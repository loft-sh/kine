package pgsql

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sirupsen/logrus"
)

// gssProviderOnce guards registration of the global pgx GSSAPI provider.
// pgconn.RegisterGSSProvider mutates package-level state, so we register at
// most once per process.
//
// This bakes in a single-backend assumption: the registered provider closure
// captures the auth source (keytab principal or credential cache) of the first
// configureKerberos call, and every subsequent GSSAPI connection in the process
// authenticates with that identity. kine creates exactly one postgres backend
// per process, so this holds. If that ever changes (multiple postgres backends
// with different principals in one process), the registration must be keyed on
// the auth source and reject a conflicting re-registration instead of silently
// reusing the first.
var gssProviderOnce sync.Once

// krbMode identifies the source of the Kerberos credentials used for GSSAPI
// authentication to PostgreSQL.
type krbMode int

const (
	// krbDisabled means GSSAPI is off; PostgreSQL password (or IAM) auth is used.
	krbDisabled krbMode = iota
	// krbKeytab authenticates from a keytab whose path is in krbAuth.keytab.
	krbKeytab
	// krbCCache authenticates from a Kerberos credential cache.
	krbCCache
)

// krbAuth describes how GSSAPI (Kerberos) authentication is performed. It is
// derived from the DSN and environment by prepareDSN and consumed by
// configureKerberos. The zero value disables GSSAPI.
type krbAuth struct {
	mode krbMode
	// keytab is the keytab path, set when mode == krbKeytab.
	keytab string
	// ccache is the explicit credential-cache path, optionally set when
	// mode == krbCCache; empty means resolve from KRB5CCNAME, else the default.
	ccache string
}

// configureKerberos sets up GSSAPI (Kerberos) authentication to PostgreSQL
// according to krb (derived from the DSN/environment by prepareDSN). It
// registers a pgx GSS provider and rewrites connConfig.User to the bare
// PostgreSQL role.
//
// pgx (pgconn) already implements the GSSAPI handshake; it only needs a GSS
// provider registered via pgconn.RegisterGSSProvider. The provider in gss.go
// supplies one backed by the pure-Go go-krb5/krb5 library, so no libkrb5/CGO is
// required. The krb5 configuration is read by it from $KRB5_CONFIG (default
// /etc/krb5.conf).
func configureKerberos(krb krbAuth, connConfig *pgx.ConnConfig) error {
	switch krb.mode {
	case krbKeytab:
		return configureKeytab(krb.keytab, connConfig)
	case krbCCache:
		return configureCCache(krb.ccache, connConfig)
	default:
		return errors.New("kerberos: configureKerberos called with an unset auth mode")
	}
}

// configureKeytab registers a GSS provider that authenticates from a keytab,
// using the client principal carried in the DSN user-part. The user-part is
// either the full principal "user@REALM" with the "@" percent-encoded as "%40"
// (e.g. postgres://kine%40KINE.TEST@host/db), or just "user", in which case the
// realm is taken from default_realm in the krb5 configuration.
func configureKeytab(keytab string, connConfig *pgx.ConnConfig) error {
	user, realm, err := principalFromUser(connConfig.User)
	if err != nil {
		return err
	}

	if _, err := os.Stat(keytab); err != nil {
		return fmt.Errorf("kerberos keytab %q is not accessible: %w", keytab, err)
	}

	gssProviderOnce.Do(func() {
		pgconn.RegisterGSSProvider(func() (pgconn.GSS, error) {
			return newGSSWithKeytab(user, realm, keytab)
		})
		logrus.Infof("Registered Kerberos (GSSAPI) provider for principal %s@%s using keytab %s", user, realm, keytab)
	})

	// PostgreSQL receives the bare role in the startup message; the realm is
	// stripped and matched server-side (pg_hba include_realm=0). The DSN
	// user-part carried the full principal, so reduce it to the user component.
	connConfig.User = user

	return nil
}

// configureCCache registers a GSS provider that authenticates from a Kerberos
// credential cache. Unlike keytab mode the client identity comes from the
// cached tickets, so the DSN user-part is used only as the PostgreSQL role: any
// "@REALM" suffix is stripped for the startup message and no realm resolution
// is performed. ccache is the explicit cache path from the DSN; when empty the
// cache is resolved from KRB5CCNAME (else the MIT default /tmp/krb5cc_<uid>).
func configureCCache(ccache string, connConfig *pgx.ConnConfig) error {
	role, err := roleFromUser(connConfig.User)
	if err != nil {
		return err
	}

	gssProviderOnce.Do(func() {
		pgconn.RegisterGSSProvider(func() (pgconn.GSS, error) {
			return newGSSWithCCache(ccache)
		})
		if ccache != "" {
			logrus.Infof("Registered Kerberos (GSSAPI) provider for role %s using credential cache %s", role, ccache)
		} else {
			logrus.Infof("Registered Kerberos (GSSAPI) provider for role %s using the credential cache from KRB5CCNAME (or the default /tmp/krb5cc_<uid>)", role)
		}
	})

	connConfig.User = role

	return nil
}

// principalFromUser derives the Kerberos (user, realm) from the DSN user-part.
// When the user-part contains an "@" it is parsed as a full "user@REALM"
// principal; otherwise the whole value is the user and the realm is taken from
// default_realm in the krb5 configuration.
func principalFromUser(dsnUser string) (user, realm string, err error) {
	if dsnUser == "" {
		return "", "", errors.New("invalid kerberos principal: empty DSN user-part; expected \"user\" or \"user@REALM\"")
	}
	if strings.Contains(dsnUser, "@") {
		return splitPrincipal(dsnUser)
	}
	realm, err = defaultRealm()
	if err != nil {
		return "", "", err
	}
	return dsnUser, realm, nil
}

// roleFromUser reduces the DSN user-part to the bare PostgreSQL role, dropping
// an optional "@REALM" suffix. Unlike principalFromUser it does not resolve a
// realm, since credential-cache mode takes the client identity from the cached
// tickets rather than from the DSN.
func roleFromUser(dsnUser string) (string, error) {
	if dsnUser == "" {
		return "", errors.New("invalid kerberos principal: empty DSN user-part; expected \"user\" or \"user@REALM\"")
	}
	role, _, _ := strings.Cut(dsnUser, "@")
	if role == "" {
		return "", fmt.Errorf("invalid kerberos principal %q: empty user component before \"@\"", dsnUser)
	}
	return role, nil
}

// splitPrincipal splits a Kerberos principal of the form "user@REALM" into its
// user and realm components.
func splitPrincipal(principal string) (user, realm string, err error) {
	user, realm, found := strings.Cut(principal, "@")
	if !found || user == "" || realm == "" {
		return "", "", fmt.Errorf("invalid kerberos principal %q: expected format \"user@REALM\" in the DSN user-part (percent-encode the \"@\" as \"%%40\")", principal)
	}
	if strings.Contains(realm, "@") {
		return "", "", fmt.Errorf("invalid kerberos principal %q: expected exactly one \"@\"", principal)
	}
	return user, realm, nil
}

// defaultRealm reads default_realm from the krb5 configuration. It loads the
// config through loadKrb5Config (gss.go), the same helper the GSS provider
// uses, so the realm derived here is always read from the very file the
// provider authenticates against; the two cannot resolve $KRB5_CONFIG /
// /etc/krb5.conf differently.
func defaultRealm() (string, error) {
	cfgPath, cfg, err := loadKrb5Config()
	if err != nil {
		return "", fmt.Errorf("kerberos: failed to load krb5 config %q to determine the default realm: %w", cfgPath, err)
	}
	if cfg.LibDefaults.DefaultRealm == "" {
		return "", fmt.Errorf("kerberos: no realm in the DSN user-part and no default_realm in %q; specify the principal as user@REALM (percent-encode the \"@\" as \"%%40\")", cfgPath)
	}
	return cfg.LibDefaults.DefaultRealm, nil
}
