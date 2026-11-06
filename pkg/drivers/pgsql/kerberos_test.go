package pgsql

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// writeKrb5Conf writes a krb5.conf with the given contents to a temp file and
// returns its path, for use with t.Setenv("KRB5_CONFIG", ...).
func writeKrb5Conf(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "krb5.conf")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("failed to write krb5.conf: %v", err)
	}
	return path
}

func TestSplitPrincipal(t *testing.T) {
	tests := []struct {
		name      string
		principal string
		wantUser  string
		wantRealm string
		wantErr   bool
	}{
		{name: "valid", principal: "kine@KINE.TEST", wantUser: "kine", wantRealm: "KINE.TEST"},
		{name: "valid with dots", principal: "svc.kine@EXAMPLE.COM", wantUser: "svc.kine", wantRealm: "EXAMPLE.COM"},
		{name: "missing realm", principal: "kine", wantErr: true},
		{name: "empty user", principal: "@KINE.TEST", wantErr: true},
		{name: "empty realm", principal: "kine@", wantErr: true},
		{name: "empty string", principal: "", wantErr: true},
		{name: "double at", principal: "kine@KINE@TEST", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, realm, err := splitPrincipal(tt.principal)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitPrincipal(%q) = (%q, %q, nil); want error", tt.principal, user, realm)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitPrincipal(%q) returned unexpected error: %v", tt.principal, err)
			}
			if user != tt.wantUser || realm != tt.wantRealm {
				t.Fatalf("splitPrincipal(%q) = (%q, %q); want (%q, %q)", tt.principal, user, realm, tt.wantUser, tt.wantRealm)
			}
		})
	}
}

func TestPrincipalFromUser(t *testing.T) {
	const realmConf = "[libdefaults]\n  default_realm = KINE.TEST\n"
	const noRealmConf = "[libdefaults]\n  dns_canonicalize_hostname = false\n"

	t.Run("explicit realm in user-part", func(t *testing.T) {
		// An explicit realm wins and no krb5.conf lookup is needed.
		t.Setenv("KRB5_CONFIG", writeKrb5Conf(t, noRealmConf))
		user, realm, err := principalFromUser("kine@OTHER.REALM")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user != "kine" || realm != "OTHER.REALM" {
			t.Fatalf("got (%q, %q); want (kine, OTHER.REALM)", user, realm)
		}
	})

	t.Run("realm from default_realm when absent", func(t *testing.T) {
		t.Setenv("KRB5_CONFIG", writeKrb5Conf(t, realmConf))
		user, realm, err := principalFromUser("kine")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user != "kine" || realm != "KINE.TEST" {
			t.Fatalf("got (%q, %q); want (kine, KINE.TEST)", user, realm)
		}
	})

	t.Run("no realm and no default_realm is an error", func(t *testing.T) {
		t.Setenv("KRB5_CONFIG", writeKrb5Conf(t, noRealmConf))
		if _, _, err := principalFromUser("kine"); err == nil {
			t.Fatal("principalFromUser should fail when no realm can be determined")
		}
	})

	t.Run("unreadable krb5 config is an error", func(t *testing.T) {
		t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.conf"))
		if _, _, err := principalFromUser("kine"); err == nil {
			t.Fatal("principalFromUser should fail when the krb5 config cannot be loaded")
		}
	})

	t.Run("empty user-part is an error", func(t *testing.T) {
		if _, _, err := principalFromUser(""); err == nil {
			t.Fatal("principalFromUser should fail on an empty user-part")
		}
	})
}

// mustParseConfig parses a postgres DSN into a *pgx.ConnConfig, failing the test
// on error.
func mustParseConfig(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	cc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", dsn, err)
	}
	return cc
}

func TestConfigureKerberos(t *testing.T) {
	keytab := filepath.Join(t.TempDir(), "krb5.keytab")
	if err := os.WriteFile(keytab, []byte("not-a-real-keytab"), 0o600); err != nil {
		t.Fatalf("failed to write fake keytab: %v", err)
	}
	keytabAuth := func(path string) krbAuth { return krbAuth{mode: krbKeytab, keytab: path} }

	t.Run("non-principal user-part is rejected", func(t *testing.T) {
		// A bare user-part with no resolvable realm must fail.
		t.Setenv("KRB5_CONFIG", filepath.Join(t.TempDir(), "does-not-exist.conf"))
		cc := mustParseConfig(t, "postgres://no-realm-here@localhost:5432/db")
		if err := configureKerberos(keytabAuth(keytab), cc); err == nil {
			t.Fatal("configureKerberos with an unresolvable principal should fail")
		}
	})

	t.Run("missing keytab is rejected", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist.keytab")
		cc := mustParseConfig(t, "postgres://kine%40KINE.TEST@localhost:5432/db")
		if err := configureKerberos(keytabAuth(missing), cc); err == nil {
			t.Fatal("configureKerberos with a missing keytab should fail")
		}
	})

	t.Run("explicit principal rewrites the user to the bare role", func(t *testing.T) {
		// The "@" in the user-part is percent-encoded; pgx decodes it, so the
		// parsed user is the full principal before configureKerberos runs.
		cc := mustParseConfig(t, "postgres://kine%40KINE.TEST@localhost:5432/db")
		if cc.User != "kine@KINE.TEST" {
			t.Fatalf("precondition: parsed user = %q; want %q", cc.User, "kine@KINE.TEST")
		}
		if err := configureKerberos(keytabAuth(keytab), cc); err != nil {
			t.Fatalf("configureKerberos returned unexpected error: %v", err)
		}
		if cc.User != "kine" {
			t.Fatalf("configureKerberos did not rewrite user: got %q, want %q", cc.User, "kine")
		}

		// Idempotent: a second call must not error even though the GSS provider
		// is only installed once, and it must still rewrite the user.
		cc2 := mustParseConfig(t, "postgres://kine%40KINE.TEST@localhost:5432/db")
		if err := configureKerberos(keytabAuth(keytab), cc2); err != nil {
			t.Fatalf("second configureKerberos returned unexpected error: %v", err)
		}
		if cc2.User != "kine" {
			t.Fatalf("second configureKerberos did not rewrite user: got %q, want %q", cc2.User, "kine")
		}
	})

	t.Run("bare user-part derives the realm from default_realm", func(t *testing.T) {
		t.Setenv("KRB5_CONFIG", writeKrb5Conf(t, "[libdefaults]\n  default_realm = KINE.TEST\n"))
		cc := mustParseConfig(t, "postgres://kine@localhost:5432/db")
		if err := configureKerberos(keytabAuth(keytab), cc); err != nil {
			t.Fatalf("configureKerberos returned unexpected error: %v", err)
		}
		// The user-part was already the bare role, so it is unchanged.
		if cc.User != "kine" {
			t.Fatalf("configureKerberos changed the user unexpectedly: got %q, want %q", cc.User, "kine")
		}
	})

	t.Run("unset auth mode is an internal error", func(t *testing.T) {
		cc := mustParseConfig(t, "postgres://kine@localhost:5432/db")
		if err := configureKerberos(krbAuth{mode: krbDisabled}, cc); err == nil {
			t.Fatal("configureKerberos with krbDisabled should fail")
		}
	})
}

func TestConfigureKerberosCCache(t *testing.T) {
	// ccache mode takes the client identity from the cached tickets, so it needs
	// no keytab and no realm resolution; configureKerberos only registers a
	// (lazy) provider and rewrites the DSN user-part to the bare PostgreSQL role.
	// The provider closure is not invoked here, so no KDC or cache file is read.
	ccacheAuth := func(path string) krbAuth { return krbAuth{mode: krbCCache, ccache: path} }

	t.Run("principal user-part is reduced to the bare role", func(t *testing.T) {
		cc := mustParseConfig(t, "postgres://kine%40KINE.TEST@localhost:5432/db")
		if cc.User != "kine@KINE.TEST" {
			t.Fatalf("precondition: parsed user = %q; want %q", cc.User, "kine@KINE.TEST")
		}
		if err := configureKerberos(ccacheAuth(""), cc); err != nil {
			t.Fatalf("configureKerberos (ccache) returned unexpected error: %v", err)
		}
		if cc.User != "kine" {
			t.Fatalf("configureKerberos (ccache) did not rewrite user: got %q, want %q", cc.User, "kine")
		}
	})

	t.Run("bare user-part is left unchanged with an explicit cache path", func(t *testing.T) {
		cc := mustParseConfig(t, "postgres://kine@localhost:5432/db")
		if err := configureKerberos(ccacheAuth("/tmp/krb5cc_custom"), cc); err != nil {
			t.Fatalf("configureKerberos (ccache) returned unexpected error: %v", err)
		}
		if cc.User != "kine" {
			t.Fatalf("configureKerberos (ccache) changed the user unexpectedly: got %q, want %q", cc.User, "kine")
		}
	})

	t.Run("empty user-part is rejected", func(t *testing.T) {
		cc := mustParseConfig(t, "postgres://kine@localhost:5432/db")
		cc.User = ""
		if err := configureKerberos(ccacheAuth(""), cc); err == nil {
			t.Fatal("configureKerberos (ccache) with an empty user-part should fail")
		}
	})
}

func TestRoleFromUser(t *testing.T) {
	tests := []struct {
		name     string
		dsnUser  string
		wantRole string
		wantErr  bool
	}{
		{name: "full principal", dsnUser: "kine@KINE.TEST", wantRole: "kine"},
		{name: "bare role", dsnUser: "kine", wantRole: "kine"},
		{name: "role with dots", dsnUser: "svc.kine@EXAMPLE.COM", wantRole: "svc.kine"},
		{name: "empty user component", dsnUser: "@KINE.TEST", wantErr: true},
		{name: "empty string", dsnUser: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, err := roleFromUser(tt.dsnUser)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("roleFromUser(%q) = %q, nil; want error", tt.dsnUser, role)
				}
				return
			}
			if err != nil {
				t.Fatalf("roleFromUser(%q) returned unexpected error: %v", tt.dsnUser, err)
			}
			if role != tt.wantRole {
				t.Fatalf("roleFromUser(%q) = %q; want %q", tt.dsnUser, role, tt.wantRole)
			}
		})
	}
}
