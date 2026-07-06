package pgsql

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k3s-io/kine/pkg/tls"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestPrepareDSNKeytab(t *testing.T) {
	logHook := logtest.NewGlobal()

	t.Run("keytab is extracted from the DSN and stripped", func(t *testing.T) {
		logHook.Reset()
		// An unrelated KRB5_KTNAME must be ignored when the DSN carries the param.
		t.Setenv(keytabEnvVar, "/env/should-be-ignored.keytab")
		dsn, krb, err := prepareDSN(
			"kine%40KINE.TEST@db:5432/kine?sslmode=disable&krbsrvname=postgres&krb5_keytab=/etc/kine-keytab/krb5.keytab",
			tls.Config{},
		)
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbKeytab || krb.keytab != "/etc/kine-keytab/krb5.keytab" {
			t.Fatalf("krb = %+v; want keytab mode with %q", krb, "/etc/kine-keytab/krb5.keytab")
		}

		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("failed to parse returned DSN %q: %v", dsn, err)
		}
		q := u.Query()
		if q.Has(keytabParam) {
			t.Fatalf("DSN still carries %q: %q", keytabParam, dsn)
		}
		// Other libpq parameters must survive.
		if q.Get("sslmode") != "disable" || q.Get("krbsrvname") != "postgres" {
			t.Fatalf("DSN dropped other query parameters: %q", dsn)
		}
	})

	t.Run("keytab falls back to KRB5_KTNAME when the DSN omits it", func(t *testing.T) {
		logHook.Reset()
		t.Setenv(keytabEnvVar, "/env/krb5.keytab")
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?sslmode=disable", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbKeytab || krb.keytab != "/env/krb5.keytab" {
			t.Fatalf("krb = %+v; want keytab mode with %q", krb, "/env/krb5.keytab")
		}
		assertNoKeytabWarning(t, logHook)
	})

	t.Run("KRB5_KTNAME FILE: prefix is stripped", func(t *testing.T) {
		logHook.Reset()
		t.Setenv(keytabEnvVar, "FILE:/env/krb5.keytab")
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbKeytab || krb.keytab != "/env/krb5.keytab" {
			t.Fatalf("krb = %+v; want keytab mode with %q", krb, "/env/krb5.keytab")
		}
		assertNoKeytabWarning(t, logHook)
	})

	t.Run("unset KRB5_KTNAME disables GSSAPI without warning", func(t *testing.T) {
		logHook.Reset()
		// t.Setenv records the original value for restoration on cleanup; unset it
		// for the body so we exercise the genuinely-absent path, not set-but-empty.
		t.Setenv(keytabEnvVar, "")
		os.Unsetenv(keytabEnvVar)
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?sslmode=disable", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbDisabled {
			t.Fatalf("krb = %+v; want GSSAPI disabled", krb)
		}
		assertNoKeytabWarning(t, logHook)
	})

	t.Run("empty KRB5_KTNAME disables GSSAPI and warns", func(t *testing.T) {
		logHook.Reset()
		t.Setenv(keytabEnvVar, "")
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?sslmode=disable", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbDisabled {
			t.Fatalf("krb = %+v; want GSSAPI disabled", krb)
		}
		assertKeytabWarning(t, logHook)
	})

	t.Run("KRB5_KTNAME with only a FILE: prefix disables GSSAPI and warns", func(t *testing.T) {
		logHook.Reset()
		t.Setenv(keytabEnvVar, "FILE:")
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?sslmode=disable", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbDisabled {
			t.Fatalf("krb = %+v; want GSSAPI disabled", krb)
		}
		assertKeytabWarning(t, logHook)
	})
}

func TestPrepareDSNCCache(t *testing.T) {
	t.Run("explicit cache path selects ccache mode and is stripped", func(t *testing.T) {
		dsn, krb, err := prepareDSN(
			"kine%40KINE.TEST@db:5432/kine?sslmode=disable&krb5_ccache=/tmp/krb5cc_kine",
			tls.Config{},
		)
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbCCache || krb.ccache != "/tmp/krb5cc_kine" {
			t.Fatalf("krb = %+v; want ccache mode with %q", krb, "/tmp/krb5cc_kine")
		}

		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("failed to parse returned DSN %q: %v", dsn, err)
		}
		q := u.Query()
		if q.Has(ccacheParam) {
			t.Fatalf("DSN still carries %q: %q", ccacheParam, dsn)
		}
		// Other libpq parameters must survive.
		if q.Get("sslmode") != "disable" {
			t.Fatalf("DSN dropped other query parameters: %q", dsn)
		}
	})

	t.Run("present but empty value still selects ccache mode", func(t *testing.T) {
		// A bare krb5_ccache (no value) means "use KRB5CCNAME, else the default".
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?krb5_ccache=", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbCCache || krb.ccache != "" {
			t.Fatalf("krb = %+v; want ccache mode with an empty path", krb)
		}
	})

	t.Run("ccache wins over the KRB5_KTNAME environment fallback", func(t *testing.T) {
		// A DSN selector always beats the environment keytab fallback.
		t.Setenv(keytabEnvVar, "/env/krb5.keytab")
		_, krb, err := prepareDSN("postgres:postgres@db:5432/kine?krb5_ccache=/tmp/cc", tls.Config{})
		if err != nil {
			t.Fatalf("prepareDSN returned unexpected error: %v", err)
		}
		if krb.mode != krbCCache || krb.ccache != "/tmp/cc" {
			t.Fatalf("krb = %+v; want ccache mode with %q", krb, "/tmp/cc")
		}
	})

	t.Run("keytab and ccache together are rejected", func(t *testing.T) {
		_, _, err := prepareDSN(
			"postgres:postgres@db:5432/kine?krb5_keytab=/etc/krb5.keytab&krb5_ccache=/tmp/cc",
			tls.Config{},
		)
		if err == nil {
			t.Fatal("prepareDSN with both krb5_keytab and krb5_ccache should fail")
		}
	})
}

// assertKeytabWarning fails the test unless a warning mentioning keytabEnvVar was
// logged, verifying that a set-but-empty KRB5_KTNAME is surfaced to operators.
func assertKeytabWarning(t *testing.T, hook *logtest.Hook) {
	t.Helper()
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, keytabEnvVar) {
			return
		}
	}
	t.Fatalf("expected a warning mentioning %s; got entries: %v", keytabEnvVar, entryMessages(hook))
}

// assertNoKeytabWarning fails the test if any warning mentioning keytabEnvVar was
// logged, guarding against spurious diagnostics on the valid and unset paths.
func assertNoKeytabWarning(t *testing.T, hook *logtest.Hook) {
	t.Helper()
	for _, e := range hook.AllEntries() {
		if e.Level == logrus.WarnLevel && strings.Contains(e.Message, keytabEnvVar) {
			t.Fatalf("unexpected warning mentioning %s: %q", keytabEnvVar, e.Message)
		}
	}
}

func entryMessages(hook *logtest.Hook) []string {
	entries := hook.AllEntries()
	msgs := make([]string, 0, len(entries))
	for _, e := range entries {
		msgs = append(msgs, e.Message)
	}
	return msgs
}

func TestEnsureLockTimeout(t *testing.T) {
	// Applies the default when the DSN does not set one.
	cfg, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/kine?sslmode=disable")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	ensureLockTimeout(cfg)
	if got := cfg.ConnConfig.RuntimeParams["lock_timeout"]; got != "10000" {
		t.Fatalf("default lock_timeout = %q, want %q", got, "10000")
	}

	// Respects an operator-supplied value from the DSN.
	cfg2, err := pgxpool.ParseConfig("postgres://user:pass@localhost:5432/kine?sslmode=disable&lock_timeout=3000")
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	ensureLockTimeout(cfg2)
	if got := cfg2.ConnConfig.RuntimeParams["lock_timeout"]; got != "3000" {
		t.Fatalf("operator lock_timeout = %q, want %q (must not be overridden)", got, "3000")
	}
}
