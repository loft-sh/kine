package endpoint_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	_ "github.com/k3s-io/kine/pkg/drivers/pgsql" // register the postgres driver
	"github.com/k3s-io/kine/pkg/endpoint"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// watchGapEnv is a Postgres DSN, e.g. "postgres://postgres:kine@127.0.0.1:5432/postgres?sslmode=disable".
const watchGapEnv = "KINE_TEST_PG_DSN"

// TestWatchGapDoesNotStallPostgres holds a transaction that occupies a gap in the id sequence and
// asserts the watch poll keeps advancing instead of blocking on the gap-fill for the holder's full
// duration. Needs Postgres (KINE_TEST_PG_DSN); skipped otherwise.
func TestWatchGapDoesNotStallPostgres(t *testing.T) {
	dsn := os.Getenv(watchGapEnv)
	if dsn == "" {
		t.Skipf("set %s to a Postgres DSN to run this test", watchGapEnv)
	}

	const holdDuration = 15 * time.Second
	const maxFreeze = 8 * time.Second

	// Bound the lock wait well under the hold (via the DSN, the operator-facing knob) so a
	// regression to an unbounded wait fails the assertion. The driver default is 10s.
	kineDSN := withLockTimeout(t, dsn, "2000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start kine in-process against Postgres on a loopback TCP port.
	wg := &sync.WaitGroup{}
	etcdCfg, err := endpoint.Listen(ctx, endpoint.Config{
		Listener:            "tcp://127.0.0.1:0",
		Endpoint:            kineDSN,
		WaitGroup:           wg,
		NotifyInterval:      1 * time.Second,
		EmulatedETCDVersion: "3.5.13",
		CompactInterval:     5 * time.Minute,
		CompactTimeout:      5 * time.Second,
		CompactMinRetain:    1000,
		CompactBatchSize:    1000,
		PollBatchSize:       500,
	})
	if err != nil {
		t.Fatalf("start kine: %v", err)
	}
	defer wg.Wait()
	defer cancel()

	cli, err := clientv3.New(clientv3.Config{Endpoints: etcdCfg.Endpoints, DialTimeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("kine client: %v", err)
	}
	defer cli.Close()

	prefix := fmt.Sprintf("/watchgap/%d", time.Now().UnixNano())
	create := func(key string) {
		if _, err := cli.Txn(ctx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, "v")).
			Commit(); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}
	getRev := func() int64 {
		gr, err := cli.Get(ctx, prefix+"-revprobe")
		if err != nil {
			t.Fatalf("get rev: %v", err)
		}
		return gr.Header.Revision
	}

	// A watcher must be active for kine's poll loop to run (and drive the reported revision).
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	established := make(chan struct{})
	wch := cli.Watch(wctx, prefix, clientv3.WithPrefix(), clientv3.WithProgressNotify(), clientv3.WithCreatedNotify())
	go func() {
		var once sync.Once
		for resp := range wch {
			if resp.Created {
				once.Do(func() { close(established) })
			}
		}
	}()
	select {
	case <-established:
	case <-time.After(15 * time.Second):
		t.Fatal("watch never established")
	}

	// Healthy baseline. Capture baseRev only after the poll has caught up on the base creates: a
	// fixed sleep can capture it early, and later base-create catch-up would then masquerade as
	// recovery during the measurement phase (passing even on unbounded-wait code).
	for i := 0; i < 4; i++ {
		create(fmt.Sprintf("%s/base/%d", prefix, i))
	}
	baseRev := waitStableRev(t, getRev)

	// Inject a held gap: a transaction that consumes the next BIGSERIAL id and holds it
	// uncommitted, then rolls back. This is what a slow/contended write looks like to the poll.
	holdCtx, holdCancel := context.WithCancel(context.Background())
	defer holdCancel()
	holdDone := make(chan struct{})
	inserted := make(chan struct{})
	go func() {
		defer close(holdDone)
		conn, err := pgx.Connect(holdCtx, dsn)
		if err != nil {
			t.Errorf("hold connect: %v", err)
			return
		}
		defer conn.Close(context.Background())
		tx, err := conn.Begin(holdCtx)
		if err != nil {
			t.Errorf("hold begin: %v", err)
			return
		}
		_, err = tx.Exec(holdCtx,
			`INSERT INTO kine(name,created,deleted,create_revision,prev_revision,lease,value,old_value)
			 VALUES($1,1,0,0,424242,0,'\x00'::bytea,NULL)`,
			fmt.Sprintf("watchgap-stuck-%d", time.Now().UnixNano()))
		if err != nil {
			t.Errorf("hold insert: %v", err)
			return
		}
		close(inserted) // the gap id is now grabbed and held uncommitted
		select {
		case <-holdCtx.Done():
		case <-time.After(holdDuration):
		}
		_ = tx.Rollback(context.Background())
	}()

	// Wait for the gap INSERT to grab its id (so the during-creates land after the gap), and fail
	// loudly if the goroutine died before injecting the gap rather than testing an ungapped DB.
	select {
	case <-inserted:
	case <-holdDone:
		t.Fatal("gap goroutine exited before its INSERT grabbed an id")
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the gap INSERT")
	}

	// Commit writes beyond the gap so the reported revision has somewhere to advance to.
	for i := 0; i < 4; i++ {
		create(fmt.Sprintf("%s/during/%d", prefix, i))
	}

	// The poll should self-heal (advance past the gap) well before the holder releases.
	measureStart := time.Now()
	deadline := measureStart.Add(holdDuration + 2*time.Second)
	var freeze time.Duration
	recovered := false
	for time.Now().Before(deadline) {
		if getRev() > baseRev {
			freeze = time.Since(measureStart)
			recovered = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	holdCancel()
	<-holdDone

	if !recovered {
		t.Fatalf("reported revision never advanced past %d while a gap was held: watch stalled", baseRev)
	}
	if freeze >= maxFreeze {
		t.Fatalf("reported revision froze for %s (>= %s) while a gap was held: lock_timeout not bounding the poll", freeze.Round(100*time.Millisecond), maxFreeze)
	}
	t.Logf("watch self-healed in %s while the gap was held for up to %s", freeze.Round(100*time.Millisecond), holdDuration)
}

// waitStableRev polls getRev until two consecutive reads match, ensuring the poll has caught up on
// all prior writes before the value is used as a baseline.
func waitStableRev(t *testing.T, getRev func() int64) int64 {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last := getRev()
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		cur := getRev()
		if cur == last && cur > 0 {
			return cur
		}
		last = cur
	}
	t.Fatalf("revision did not stabilize within timeout (last %d)", last)
	return 0
}

// withLockTimeout returns dsn with a lock_timeout query parameter (in milliseconds) added.
func withLockTimeout(t *testing.T, dsn, ms string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	q := u.Query()
	q.Set("lock_timeout", ms)
	u.RawQuery = q.Encode()
	return u.String()
}
