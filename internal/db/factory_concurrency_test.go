// Governing: SPEC-0001 REQ "Pluggable Database Backend", ADR-0002
package db_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/db"
)

// TestNewSQLiteConcurrentWritersWait is the companion regression test for #191:
// without busy_timeout a second writer hit SQLITE_BUSY immediately and surfaced
// as a 500 (which is why internal/api has isDBLockError at all). The pragma
// added to the SQLite DSN must make that writer wait for the lock instead.
func TestNewSQLiteConcurrentWritersWait(t *testing.T) {
	conn, err := db.New("sqlite3", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO links (id, slug, url) VALUES ('l1', 'one', 'https://example.com/1')`); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	ctx := context.Background()
	holder, err := conn.Conn(ctx)
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = holder.Close() }()
	writer, err := conn.Conn(ctx)
	if err != nil {
		t.Fatalf("writer conn: %v", err)
	}
	defer func() { _ = writer.Close() }()

	tx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO links (id, slug, url) VALUES ('l2', 'two', 'https://example.com/2')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("locked insert: %v", err)
	}

	// Whoever reaches the release first commits the holding transaction: the
	// goroutine after holdFor, or the test itself on the way out.
	const holdFor = 300 * time.Millisecond
	var commitOnce sync.Once
	commitErr := make(chan error, 1)
	release := func() {
		commitOnce.Do(func() { commitErr <- tx.Commit() })
	}
	defer release()
	go func() {
		time.Sleep(holdFor)
		release()
	}()

	start := time.Now()
	_, err = writer.ExecContext(ctx, `INSERT INTO links (id, slug, url) VALUES ('l3', 'three', 'https://example.com/3')`)
	waited := time.Since(start)
	if err != nil {
		t.Fatalf("second writer failed while the first held the write lock (waited %s): %v", waited, err)
	}
	release()
	if err := <-commitErr; err != nil {
		t.Fatalf("commit of holding transaction: %v", err)
	}

	if waited < holdFor/2 {
		t.Errorf("second writer returned after %s, want at least %s: it did not wait for the lock", waited, holdFor/2)
	}
	if waited > 5*time.Second {
		t.Errorf("second writer waited %s, want well under the 5s busy_timeout", waited)
	}

	var links int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&links); err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 3 {
		t.Errorf("links rows after both writes = %d, want 3", links)
	}
}
