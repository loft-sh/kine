//go:build cgo
// +build cgo

package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/k3s-io/kine/pkg/drivers"
	"github.com/k3s-io/kine/pkg/drivers/generic"
)

func TestMigrateReleasesItsConnections(t *testing.T) {
	var wg sync.WaitGroup
	_, dialect, err := NewVariant(t.Context(), &wg, "sqlite3", &drivers.Config{
		DataSourceName: filepath.Join(t.TempDir(), "test.db") + "?_journal=WAL&cache=shared&_busy_timeout=30000&_txlock=immediate",
		// Use a single connection to catch any connection leaks in Migrate
		ConnectionPoolConfig: generic.ConnectionPoolConfig{
			MaxOpen: 1,
			MaxIdle: 1,
		},
	})
	if err != nil {
		t.Fatalf("failed to create dialect: %v", err)
	}
	t.Cleanup(wg.Wait)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := dialect.DB.ExecContext(ctx, `SELECT 1`); err != nil {
		t.Fatalf("database unusable after Migrate: %v", err)
	}
}
