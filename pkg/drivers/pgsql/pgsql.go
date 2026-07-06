package pgsql

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/k3s-io/kine/pkg/dialer"
	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/drivers/generic"
	"github.com/k3s-io/kine/pkg/identity"
	"github.com/k3s-io/kine/pkg/logstructured"
	"github.com/k3s-io/kine/pkg/logstructured/sqllog"
	"github.com/k3s-io/kine/pkg/query"
	"github.com/k3s-io/kine/pkg/server"
	"github.com/k3s-io/kine/pkg/tls"
	"github.com/k3s-io/kine/pkg/util"
	"github.com/sirupsen/logrus"
)

const (
	defaultDSN = "postgres://postgres:postgres@localhost/"

	// defaultLockTimeout bounds how long a statement waits on a row lock. The watch poll fills gaps
	// in the id sequence with an INSERT at the missing id; if a transaction holds that id, an
	// unbounded wait blocks the single poll goroutine and stalls every watcher. Bounding it lets the
	// poll self-heal; normal writes never wait this long on a lock. Operators can override it per
	// datasource with a lock_timeout query parameter in the DSN.
	defaultLockTimeout = 10 * time.Second

	// keytabParam is the kine-specific DSN query parameter carrying the path to
	// a Kerberos keytab. Its presence enables GSSAPI authentication; it is
	// stripped from the DSN before connecting so it is not forwarded to
	// PostgreSQL as a runtime parameter. It takes precedence over keytabEnvVar.
	keytabParam = "krb5_keytab"

	// keytabEnvVar is the standard Kerberos environment variable holding the
	// keytab path, used as a fallback when keytabParam is absent. Its value may
	// carry an optional "FILE:" type prefix, which is stripped.
	keytabEnvVar = "KRB5_KTNAME"

	// ccacheParam is the kine-specific DSN query parameter that enables GSSAPI
	// authentication from a Kerberos credential cache. Its presence selects
	// ccache mode; its value is an optional explicit cache path (an empty value
	// resolves the cache from KRB5CCNAME, else the MIT default
	// /tmp/krb5cc_<uid>). It is mutually exclusive with keytabParam and is
	// stripped from the DSN before connecting.
	//
	// Unlike keytabParam there is no environment-variable fallback that turns
	// ccache mode on: KRB5CCNAME only locates the cache once ccache mode is
	// selected, it does not by itself enable GSSAPI. KRB5CCNAME is commonly
	// present in an environment and must not silently hijack password auth.
	ccacheParam = "krb5_ccache"
)

var (
	schema = []string{
		`CREATE TABLE IF NOT EXISTS kine
 			(
				id BIGSERIAL PRIMARY KEY,
				name text COLLATE "C",
				created INTEGER,
				deleted INTEGER,
				create_revision BIGINT,
				prev_revision BIGINT,
 				lease INTEGER,
 				value bytea,
 				old_value bytea
 			);`,

		`CREATE INDEX IF NOT EXISTS kine_name_index ON kine (name)`,
		`CREATE INDEX IF NOT EXISTS kine_name_id_index ON kine (name,id)`,
		`CREATE INDEX IF NOT EXISTS kine_id_deleted_index ON kine (id,deleted)`,
		`CREATE INDEX IF NOT EXISTS kine_prev_revision_index ON kine (prev_revision)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS kine_name_prev_revision_uindex ON kine (name, prev_revision)`,
		`CREATE INDEX IF NOT EXISTS kine_list_query_index on kine(name, id DESC, deleted)`,
	}
	schemaMigrations = []string{
		`ALTER TABLE kine ALTER COLUMN id SET DATA TYPE BIGINT, ALTER COLUMN create_revision SET DATA TYPE BIGINT, ALTER COLUMN prev_revision SET DATA TYPE BIGINT; ALTER SEQUENCE kine_id_seq AS BIGINT`,
		// It is important to set the collation to "C" to ensure that LIKE and COMPARISON
		// queries use the index.
		`ALTER TABLE kine ALTER COLUMN name SET DATA TYPE TEXT COLLATE "C" USING name::TEXT COLLATE "C"`,
	}
	createDB = `CREATE DATABASE "%s";`
)

func New(ctx context.Context, wg *sync.WaitGroup, cfg *drivers.Config) (bool, server.Backend, error) {
	parsedDSN, krb, err := prepareDSN(cfg.DataSourceName, cfg.BackendTLSConfig)
	if err != nil {
		return false, nil, err
	}

	config, err := pgx.ParseConfig(parsedDSN)
	if err != nil {
		return false, nil, err
	}
	config.DialFunc = dialer.CachingDialer.DialContext

	// Bound lock waits so a held id gap can't stall the watch poll (see defaultLockTimeout).
	ensureLockTimeout(config)

	// GSSAPI (Kerberos) authentication is enabled by a keytab (krb5_keytab /
	// KRB5_KTNAME) or a credential cache (krb5_ccache) selected from the DSN;
	// the client principal is carried in the DSN user-part. This must run after
	// the DSN is parsed and before any connection is opened (createDBIfNotExist
	// copies config, inheriting the rewritten user).
	if krb.mode != krbDisabled {
		if err := configureKerberos(krb, config); err != nil {
			return false, nil, err
		}
	}

	opts := prepareOptions(cfg)
	connector := stdlib.GetConnector(*config, opts...)
	if err := createDBIfNotExist(ctx, cfg, config); err != nil {
		return false, nil, err
	}

	dialect, err := generic.OpenDB(ctx, wg, "pgx", connector, cfg.ConnectionPoolConfig, "$", true, cfg.MetricsRegisterer)
	if err != nil {
		return false, nil, err
	}

	dialect.GetSizeSQL = query.New(`SELECT pg_total_relation_size('kine')`, "$", true, "GetSize")
	dialect.CompactSQL = query.New(`
		DELETE FROM kine AS kv
		USING	(
			SELECT kp.prev_revision AS id
			FROM kine AS kp
			WHERE
				kp.name != 'compact_rev_key' AND
				kp.prev_revision != 0 AND
				kp.id <= $1
			UNION
			SELECT kd.id AS id
			FROM kine AS kd
			WHERE
				kd.deleted != 0 AND
				kd.id <= $2
		) AS ks
		WHERE kv.id = ks.id`, "$", true, "Compact")
	dialect.FillRetryDuration = time.Millisecond + 5
	dialect.InsertRetry = func(err error) bool {
		if err, ok := err.(*pgconn.PgError); ok && err.Code == pgerrcode.UniqueViolation && err.ConstraintName == "kine_pkey" {
			return true
		}
		return false
	}
	dialect.TranslateErr = func(err error) error {
		if err, ok := err.(*pgconn.PgError); ok && err.Code == pgerrcode.UniqueViolation {
			return server.ErrKeyExists
		}
		return err
	}
	dialect.ErrCode = func(err error) string {
		if err == nil {
			return ""
		}
		if err, ok := err.(*pgconn.PgError); ok {
			return err.Code
		}
		return err.Error()
	}
	dialect.TranslateStartKeyFunc = func(startKey string) string {
		// replace trailing null with 0x1a (substitute) as postgres does not allow null byte in UTF8 strings
		if s, ok := strings.CutSuffix(startKey, "\x00"); ok {
			return s + "\x1a"
		}
		return startKey
	}

	if err := setup(ctx, dialect.DB); err != nil {
		return false, nil, err
	}

	dialect.Migrate(context.Background())
	return true, logstructured.New(sqllog.New(dialect, cfg.CompactInterval, cfg.CompactIntervalJitter, cfg.CompactTimeout, cfg.CompactMinRetain, cfg.CompactBatchSize, cfg.PollBatchSize)), nil
}

func setup(ctx context.Context, db *sql.DB) error {
	logrus.Infof("Configuring database table schema and indexes, this may take a moment...")

	// Run schema setup and migrations on a dedicated connection with lock_timeout disabled. The
	// operational lock_timeout (see defaultLockTimeout) must not abort DDL such as the ALTER TABLE
	// row-rewrite, which legitimately waits for an AccessExclusiveLock during a rolling upgrade.
	// RESET restores the connection's startup value before it returns to the pool.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SET lock_timeout = 0"); err != nil {
		return err
	}
	defer func() { _, _ = conn.ExecContext(ctx, "RESET lock_timeout") }()

	var version string
	collationSupported := true
	if err := conn.QueryRowContext(ctx, "select version()").Scan(&version); err == nil && strings.Contains(strings.ToLower(version), "cockroachdb") {
		// CockroadDB does not seem to support "C" as a collation
		// It looks like it's using golang.org/x/text/language and ends up calling something like v, err := language.Parse("C")
		// which parses it as a BCP47 language tag instead of a collation.
		collationSupported = false
	}

	for _, stmt := range schema {
		if !collationSupported {
			stmt = strings.ReplaceAll(stmt, ` COLLATE "C"`, "")
		}
		logrus.Tracef("SETUP EXEC : %v", query.Strip(stmt))
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	// Run enabled schama migrations.
	// Note that the schema created by the `schema` var is always the latest revision;
	// migrations should handle deltas between prior schema versions.
	schemaVersion, _ := strconv.ParseUint(os.Getenv("KINE_SCHEMA_MIGRATION"), 10, 64)
	for i, stmt := range schemaMigrations {
		if i >= int(schemaVersion) {
			break
		}
		if !collationSupported {
			stmt = strings.ReplaceAll(stmt, ` COLLATE "C"`, "")
		}
		if stmt == "" {
			continue
		}
		logrus.Tracef("SETUP EXEC MIGRATION %d: %v", i, query.Strip(stmt))
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	logrus.Infof("Database tables and indexes are up to date")
	return nil
}

func createDBIfNotExist(ctx context.Context, cfg *drivers.Config, config *pgx.ConnConfig) error {
	createConfig := config.Copy()
	createConfig.Database = "postgres"
	connector := stdlib.GetConnector(*createConfig, prepareOptions(cfg)...)
	conn, err := connector.Connect(ctx)
	if err != nil {
		logrus.Warnf("failed to ensure existence of database %s: unable to connect to default postgres database: %v", createConfig.Database, err)
		return nil
	}
	conn.Close()

	db := sql.OpenDB(connector)
	defer db.Close()

	var exists bool
	err = db.QueryRow("SELECT 1 FROM pg_database WHERE datname = $1", config.Database).Scan(&exists)
	if err != nil && err != sql.ErrNoRows {
		logrus.Warnf("failed to check existence of database %s, going to attempt create: %v", config.Database, err)
	}

	if !exists {
		stmt := fmt.Sprintf(createDB, config.Database)
		logrus.Tracef("SETUP EXEC : %v", query.Strip(stmt))
		if _, err = db.Exec(stmt); err != nil {
			logrus.Warnf("failed to create database %s: %v", config.Database, err)
		} else {
			logrus.Tracef("created database: %s", config.Database)
		}
	}
	return nil
}

func prepareDSN(dataSourceName string, tlsInfo tls.Config) (dsn string, krb krbAuth, err error) {
	if len(dataSourceName) == 0 {
		dataSourceName = defaultDSN
	} else {
		dataSourceName = "postgres://" + dataSourceName
	}
	u, err := util.ParseURL(dataSourceName)
	if err != nil {
		return "", krbAuth{}, err
	}
	if len(u.Path) == 0 || u.Path == "/" {
		u.Path = "/kubernetes"
	}

	// makes quoting database and schema reference the same unquoted identifier
	// See: https://www.postgresql.org/docs/12/sql-syntax-lexical.html#:~:text=unquoted%20names%20are%20always%20folded%20to%20lower%20case
	u.Path = strings.ToLower(u.Path)

	queryMap, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", krbAuth{}, err
	}

	// Resolve the GSSAPI (Kerberos) auth source from the DSN/environment. The
	// kine-specific parameters (krb5_keytab, krb5_ccache) are dropped from the
	// query so they are not forwarded to PostgreSQL as runtime parameters.
	krb, err = kerberosAuthFromDSN(queryMap)
	if err != nil {
		return "", krbAuth{}, err
	}

	// set up tls dsn
	params := url.Values{}
	sslmode := ""
	if _, ok := queryMap["sslcert"]; tlsInfo.CertFile != "" && !ok {
		params.Add("sslcert", tlsInfo.CertFile)
		sslmode = "verify-full"
	}
	if _, ok := queryMap["sslkey"]; tlsInfo.KeyFile != "" && !ok {
		params.Add("sslkey", tlsInfo.KeyFile)
		sslmode = "verify-full"
	}
	if _, ok := queryMap["sslrootcert"]; tlsInfo.CAFile != "" && !ok {
		params.Add("sslrootcert", tlsInfo.CAFile)
		sslmode = "verify-full"
	}
	if _, ok := queryMap["sslmode"]; !ok && sslmode != "" {
		params.Add("sslmode", sslmode)
	}
	for k, v := range queryMap {
		params.Add(k, v[0])
	}
	u.RawQuery = params.Encode()
	return u.String(), krb, nil
}

// kerberosAuthFromDSN derives the GSSAPI auth source from the parsed DSN query,
// deleting the kine-specific parameters it consumes. Selection order:
//
//  1. krb5_keytab and krb5_ccache are mutually exclusive; setting both is an
//     error.
//  2. krb5_ccache (any value, including empty) selects credential-cache mode.
//  3. A non-empty krb5_keytab selects keytab mode.
//  4. Otherwise KRB5_KTNAME, when set, selects keytab mode.
//  5. Otherwise GSSAPI is disabled.
func kerberosAuthFromDSN(queryMap url.Values) (krbAuth, error) {
	keytab, hasKeytab := dsnParam(queryMap, keytabParam)
	ccache, hasCCache := dsnParam(queryMap, ccacheParam)

	switch {
	case hasKeytab && hasCCache:
		return krbAuth{}, fmt.Errorf("postgres DSN sets both %q and %q; specify at most one Kerberos credential source", keytabParam, ccacheParam)
	case hasCCache:
		return krbAuth{mode: krbCCache, ccache: ccache}, nil
	case keytab != "":
		return krbAuth{mode: krbKeytab, keytab: keytab}, nil
	}

	// No explicit DSN selector (or an empty krb5_keytab): fall back to the
	// KRB5_KTNAME environment variable, which enables keytab mode when set.
	if keytab := keytabFromEnv(); keytab != "" {
		return krbAuth{mode: krbKeytab, keytab: keytab}, nil
	}
	return krbAuth{mode: krbDisabled}, nil
}

// dsnParam returns the first value of key and whether it was present, deleting
// it from q. A present-but-empty parameter (e.g. "krb5_ccache" with no value)
// returns ("", true), which the caller can distinguish from an absent one.
func dsnParam(q url.Values, key string) (value string, present bool) {
	v, ok := q[key]
	if !ok {
		return "", false
	}
	delete(q, key)
	return v[0], true
}

// keytabFromEnv returns the Kerberos keytab path from the KRB5_KTNAME
// environment variable, stripping an optional "FILE:" type prefix (the standard
// KRB5_KTNAME residual format). It returns "" when the variable is unset. When
// the variable is set but resolves to an empty path (e.g. "" or a bare "FILE:"
// prefix) it logs a warning and returns "", since that is almost always a
// misconfiguration of an operator who intended to enable GSSAPI authentication.
func keytabFromEnv() string {
	v, ok := os.LookupEnv(keytabEnvVar)
	if !ok {
		return ""
	}
	keytab := strings.TrimPrefix(v, "FILE:")
	if keytab == "" {
		logrus.Warnf("environment variable %s is set (%q) but resolves to an empty keytab path; ignoring it for GSSAPI (Kerberos) authentication", keytabEnvVar, v)
		return ""
	}
	return keytab
}

// ensureLockTimeout sets the default lock_timeout on every connection (via pgx RuntimeParams),
// unless the operator already supplied one through the DSN. See defaultLockTimeout for the rationale.
func ensureLockTimeout(config *pgx.ConnConfig) {
	if config.RuntimeParams == nil {
		config.RuntimeParams = map[string]string{}
	}
	if _, ok := config.RuntimeParams["lock_timeout"]; ok {
		return // operator-provided value in the DSN takes precedence
	}
	// Postgres expects the bare value in milliseconds.
	config.RuntimeParams["lock_timeout"] = strconv.FormatInt(defaultLockTimeout.Milliseconds(), 10)
}

func prepareOptions(cfg *drivers.Config) []stdlib.OptionOpenDB {
	var opts []stdlib.OptionOpenDB
	if cfg.TokenSource != nil {
		opts = append(opts, stdlib.OptionBeforeConnect(func(ctx context.Context, config *pgx.ConnConfig) error {
			token, err := cfg.TokenSource(ctx, identity.Config{
				Endpoint: config.Host + ":" + strconv.Itoa(int(config.Port)),
				User:     config.User,
			})
			if err != nil {
				return err
			}
			config.Password = token
			return nil
		}))
	}
	return opts
}

func init() {
	drivers.Register("postgres", New)
	drivers.Register("postgresql", New)
}
