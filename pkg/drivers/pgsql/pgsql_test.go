package pgsql

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLockTimeoutMillis(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want string
	}{
		{"default", "", "10000"},
		{"seconds", "5s", "5000"},
		{"millis", "250ms", "250"},
		{"disabled", "0s", "0"},
		{"invalid falls back to default", "not-a-duration", "10000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(lockTimeoutEnv, tt.env)
			if got := lockTimeoutMillis(); got != tt.want {
				t.Fatalf("lockTimeoutMillis() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEnsureLockTimeout(t *testing.T) {
	t.Setenv(lockTimeoutEnv, "")

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
