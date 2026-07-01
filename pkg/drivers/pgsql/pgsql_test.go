package pgsql

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

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
