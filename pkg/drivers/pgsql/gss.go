package pgsql

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strings"

	"github.com/go-krb5/krb5/client"
	"github.com/go-krb5/krb5/config"
	"github.com/go-krb5/krb5/credentials"
	"github.com/go-krb5/krb5/keytab"
	"github.com/go-krb5/krb5/spnego"
)

// gss implements the pgconn.GSS interface (GetInitToken, GetInitTokenFromSPN,
// Continue) on top of the pure-Go go-krb5/krb5 library. It is the in-tree
// replacement for github.com/otan/gopgkrb5, whose krb5 engine
// (github.com/jcmturner/gokrb5) is unmaintained. The two libraries share an
// identical API, so this is a near-verbatim port of gopgkrb5's keytab/password/
// ccache provider, minus the Windows/SSPI path (go-krb5/krb5 is pure Go and
// works on all platforms, and kine ships Linux only).
type gss struct {
	cli      *client.Client
	ktPath   string
	ccPath   string
	realm    string
	username string
	password string
	settings []func(*client.Settings)
}

// newGSSWithCCache creates a GSS provider that authenticates from a Kerberos
// credential cache. ccPath is an explicit cache file; when empty the cache is
// resolved from KRB5CCNAME (else /tmp/krb5cc_<uid>) by resolveCCachePath.
func newGSSWithCCache(ccPath string, settings ...func(*client.Settings)) (*gss, error) {
	g := &gss{ccPath: ccPath, settings: settings}
	if err := g.init(); err != nil {
		return nil, err
	}
	return g, nil
}

// newGSSWithKeytab creates a GSS provider that authenticates from a keytab.
// This is the path kine uses (the keytab is taken from the DSN or KRB5_KTNAME).
func newGSSWithKeytab(username, realm, ktPath string, settings ...func(*client.Settings)) (*gss, error) {
	g := &gss{ktPath: ktPath, username: username, realm: realm, settings: settings}
	if err := g.init(); err != nil {
		return nil, err
	}
	return g, nil
}

// newGSSWithPassword creates a GSS provider that authenticates from a password.
func newGSSWithPassword(username, realm, password string, settings ...func(*client.Settings)) (*gss, error) {
	g := &gss{password: password, username: username, realm: realm, settings: settings}
	if err := g.init(); err != nil {
		return nil, err
	}
	return g, nil
}

// loadKrb5Config resolves the krb5 configuration path ($KRB5_CONFIG, else
// /etc/krb5.conf) and loads it. It is the single home of that fallback so the
// GSS provider (init) and default-realm resolution (defaultRealm in
// kerberos.go) cannot drift apart. The resolved path is returned alongside the
// parsed config so callers can name the exact file in diagnostics.
func loadKrb5Config() (path string, cfg *config.Config, err error) {
	path, ok := os.LookupEnv("KRB5_CONFIG")
	if !ok {
		path = "/etc/krb5.conf"
	}
	cfg, err = config.Load(path)
	return path, cfg, err
}

// resolveCCachePath determines which Kerberos credential cache file to read.
// Precedence: an explicit path (the krb5_ccache DSN parameter) wins; otherwise
// KRB5CCNAME is consulted; otherwise the MIT default /tmp/krb5cc_<uid> is used.
// Only file-type caches are supported: an optional "FILE:" prefix is stripped,
// and a KRB5CCNAME naming another cache type (DIR:, KEYRING:, MEMORY:, ...) is
// ignored in favour of the default, matching the provider's prior behaviour.
//
// The current-user lookup (user.Current, which reads /etc/passwd) happens only
// on the default branch and only when it is actually needed, so the keytab and
// password providers never depend on /etc/passwd being readable, in distroless
// or scratch containers, for example.
func resolveCCachePath(explicit string) (string, error) {
	if p := strings.TrimPrefix(explicit, "FILE:"); p != "" {
		return p, nil
	}
	if p, ok := strings.CutPrefix(os.Getenv("KRB5CCNAME"), "FILE:"); ok && p != "" {
		return p, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("kerberos: cannot resolve the default credential cache path; set KRB5CCNAME or the krb5_ccache DSN parameter: %w", err)
	}
	return "/tmp/krb5cc_" + u.Uid, nil
}

func (g *gss) init() error {
	_, cfg, err := loadKrb5Config()
	if err != nil {
		return err
	}

	// By default we disable the encryption-type match check; callers can
	// override the settings when creating the provider.
	settings := []func(*client.Settings){client.DisablePAFXFAST(true)}
	if len(g.settings) != 0 {
		settings = g.settings
	}

	// Build the client from a keytab, a password, or the credential cache, in
	// that order of preference.
	var cl *client.Client
	switch {
	case g.ktPath != "":
		kt, err := keytab.Load(g.ktPath)
		if err != nil {
			return err
		}
		cl = client.NewWithKeytab(g.username, g.realm, kt, cfg, settings...)
	case g.password != "":
		cl = client.NewWithPassword(g.username, g.realm, g.password, cfg, settings...)
	default:
		ccpath, err := resolveCCachePath(g.ccPath)
		if err != nil {
			return err
		}

		ccache, err := credentials.LoadCCache(ccpath)
		if err != nil {
			return err
		}

		cl, err = client.NewFromCCache(ccache, cfg, settings...)
		if err != nil {
			return err
		}
	}

	// Unlike gopgkrb5, which ignored this error, we surface it: a bad keytab or
	// an unreachable KDC fails here at provider creation instead of silently
	// later during the handshake.
	if err := cl.Login(); err != nil {
		return err
	}

	g.cli = cl

	return nil
}

// GetInitToken implements the pgconn.GSS interface.
func (g *gss) GetInitToken(host, service string) ([]byte, error) {
	// Resolve the hostname down to its A record if the krb5 config asks for it;
	// the KDC usually holds one principal per canonical host, not per alias.
	if g.cli.Config.LibDefaults.DNSCanonicalizeHostname {
		var err error
		host, err = canonicalizeHostname(host)
		if err != nil {
			return nil, err
		}
	}

	return g.GetInitTokenFromSPN(service + "/" + host)
}

// GetInitTokenFromSPN implements the pgconn.GSS interface.
func (g *gss) GetInitTokenFromSPN(spn string) ([]byte, error) {
	s := spnego.SPNEGOClient(g.cli, spn)

	st, err := s.InitSecContext()
	if err != nil {
		return nil, fmt.Errorf("kerberos error (InitSecContext): %w", err)
	}

	b, err := st.Marshal()
	if err != nil {
		return nil, fmt.Errorf("kerberos error (marshaling token): %w", err)
	}

	return b, nil
}

// Continue implements the pgconn.GSS interface.
func (g *gss) Continue(inToken []byte) (done bool, outToken []byte, err error) {
	t := &spnego.SPNEGOToken{}
	if err := t.Unmarshal(inToken); err != nil {
		return true, nil, fmt.Errorf("kerberos error (unmarshaling token): %w", err)
	}

	state := t.NegTokenResp.State()
	if state != spnego.NegStateAcceptCompleted {
		return true, nil, fmt.Errorf("kerberos: expected state 'Completed' - got %d", state)
	}

	return true, nil, nil
}

// canonicalizeHostname resolves host to its CNAME target, used when the krb5
// config sets dns_canonicalize_hostname.
func canonicalizeHostname(host string) (string, error) {
	name, err := net.LookupCNAME(host)
	if err != nil {
		return "", err
	}

	name = strings.TrimSuffix(name, ".")
	if name != "" {
		return name, nil
	}

	return host, nil
}
