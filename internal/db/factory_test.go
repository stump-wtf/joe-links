// Governing: SPEC-0001 REQ "Pluggable Database Backend", ADR-0002
package db_test

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"

	"github.com/joestump/joe-links/internal/db"
	"github.com/joestump/joe-links/internal/db/migrations"
)

// TestNewSQLitePragmas verifies that db.New applies foreign_keys,
// busy_timeout, and WAL to every pooled connection, not just the first one.
func TestNewSQLitePragmas(t *testing.T) {
	conn, err := db.New("sqlite3", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Pin several distinct pool connections open at once so each goes
	// through the driver's connection setup independently.
	ctx := context.Background()
	const poolSize = 4
	conns := make([]*sql.Conn, 0, poolSize)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < poolSize; i++ {
		c, err := conn.Conn(ctx)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}

	for i, c := range conns {
		var foreignKeys, busyTimeout int
		var journalMode string
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatalf("conn %d: query foreign_keys: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
			t.Fatalf("conn %d: query busy_timeout: %v", i, err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
			t.Fatalf("conn %d: query journal_mode: %v", i, err)
		}
		if foreignKeys != 1 {
			t.Errorf("conn %d: foreign_keys = %d, want 1", i, foreignKeys)
		}
		if busyTimeout != 5000 {
			t.Errorf("conn %d: busy_timeout = %d, want 5000", i, busyTimeout)
		}
		if journalMode != "wal" {
			t.Errorf("conn %d: journal_mode = %q, want \"wal\"", i, journalMode)
		}
	}
}

// TestNewSQLiteDSNWithQueryParams verifies pragmas are appended correctly
// when the configured DSN already carries query parameters.
func TestNewSQLiteDSNWithQueryParams(t *testing.T) {
	conn, err := db.New("sqlite3", "file:"+filepath.Join(t.TempDir(), "app.db")+"?mode=rwc")
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var foreignKeys int
	if err := conn.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("query foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys = %d, want 1", foreignKeys)
	}
}

// TestNewSQLiteCascades is a regression test for #191: with foreign_keys off
// (the SQLite per-connection default), every ON DELETE CASCADE / SET NULL in
// the schema was inert and parent deletes orphaned child rows.
func TestNewSQLiteCascades(t *testing.T) {
	conn, err := db.New("sqlite3", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seed := []string{
		`INSERT INTO users (id, provider, subject, email, display_name, display_name_slug) VALUES ('u1', 'test', 'sub-1', 'u1@example.com', 'User One', 'user-one')`,
		`INSERT INTO users (id, provider, subject, email, display_name, display_name_slug) VALUES ('u2', 'test', 'sub-2', 'u2@example.com', 'User Two', 'user-two')`,
		`INSERT INTO links (id, slug, url) VALUES ('l1', 'one', 'https://example.com/1')`,
		`INSERT INTO links (id, slug, url) VALUES ('l2', 'two', 'https://example.com/2')`,
		`INSERT INTO tags (id, name, slug) VALUES ('t1', 'Test', 'test')`,
		`INSERT INTO link_owners (link_id, user_id, is_primary, created_at) VALUES ('l1', 'u1', 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO link_owners (link_id, user_id, is_primary, created_at) VALUES ('l2', 'u1', 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO link_tags (link_id, tag_id) VALUES ('l1', 't1')`,
		`INSERT INTO link_shares (link_id, user_id, shared_by) VALUES ('l1', 'u2', 'u1')`,
		`INSERT INTO link_clicks (id, link_id, user_id, ip_hash) VALUES ('c1', 'l1', 'u2', 'h1')`,
		`INSERT INTO link_clicks (id, link_id, user_id, ip_hash) VALUES ('c2', 'l2', 'u2', 'h2')`,
		`INSERT INTO api_tokens (id, user_id, name, token_hash) VALUES ('tok1', 'u2', 'test', 'hash-1')`,
	}
	for _, q := range seed {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	// Deleting a link must cascade to its owners, tags, shares, and clicks.
	if _, err := conn.Exec(`DELETE FROM links WHERE id = 'l1'`); err != nil {
		t.Fatalf("delete link: %v", err)
	}
	for table, want := range map[string]int{
		`SELECT COUNT(*) FROM link_owners WHERE link_id = 'l1'`: 0,
		`SELECT COUNT(*) FROM link_tags WHERE link_id = 'l1'`:   0,
		`SELECT COUNT(*) FROM link_shares WHERE link_id = 'l1'`: 0,
		`SELECT COUNT(*) FROM link_clicks WHERE link_id = 'l1'`: 0,
		`SELECT COUNT(*) FROM link_clicks WHERE link_id = 'l2'`: 1,
	} {
		var got int
		if err := conn.QueryRow(table).Scan(&got); err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", table, got, want)
		}
	}

	// Deleting a user must cascade to api_tokens and null out link_clicks.user_id.
	if _, err := conn.Exec(`DELETE FROM users WHERE id = 'u2'`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var tokens int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM api_tokens WHERE user_id = 'u2'`).Scan(&tokens); err != nil {
		t.Fatalf("count api_tokens: %v", err)
	}
	if tokens != 0 {
		t.Errorf("api_tokens for deleted user = %d, want 0", tokens)
	}
	var clickUser sql.NullString
	if err := conn.QueryRow(`SELECT user_id FROM link_clicks WHERE id = 'c2'`).Scan(&clickUser); err != nil {
		t.Fatalf("query click user_id: %v", err)
	}
	if clickUser.Valid {
		t.Errorf("link_clicks.user_id = %q, want NULL (ON DELETE SET NULL)", clickUser.String)
	}
}

// TestCleanupOrphansMigration verifies migration 00014 removes child rows
// orphaned by historical SQLite deployments that ran with foreign_keys=OFF.
func TestCleanupOrphansMigration(t *testing.T) {
	// Open without db.New so foreign_keys stays off, mirroring the
	// deployments that produced the orphans in the first place.
	conn, err := sqlx.Open("sqlite", filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}
	migrations.SetDialect("sqlite3")
	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub migrations fs: %v", err)
	}
	goose.SetBaseFS(sub)
	defer goose.SetBaseFS(nil)

	if err := goose.UpTo(conn.DB, ".", 13); err != nil {
		t.Fatalf("migrate to 00013: %v", err)
	}

	seed := []string{
		`INSERT INTO users (id, provider, subject, email) VALUES ('u1', 'test', 'sub-1', 'u1@example.com')`,
		`INSERT INTO links (id, slug, url) VALUES ('l1', 'one', 'https://example.com/1')`,
		`INSERT INTO tags (id, name, slug) VALUES ('t1', 'Test', 'test')`,
		// Live rows that must survive the cleanup.
		`INSERT INTO link_owners (link_id, user_id, is_primary, created_at) VALUES ('l1', 'u1', 1, CURRENT_TIMESTAMP)`,
		`INSERT INTO link_tags (link_id, tag_id) VALUES ('l1', 't1')`,
		`INSERT INTO link_clicks (id, link_id, user_id, ip_hash) VALUES ('c-live', 'l1', 'u1', 'h1')`,
		`INSERT INTO api_tokens (id, user_id, name, token_hash) VALUES ('tok-live', 'u1', 'test', 'hash-1')`,
		// Orphans a real deployment could have accumulated (FKs were off).
		`INSERT INTO link_owners (link_id, user_id, is_primary) VALUES ('ghost-link', 'u1', 1)`,
		`INSERT INTO link_owners (link_id, user_id, is_primary) VALUES ('l1', 'ghost-user', 0)`,
		`INSERT INTO link_tags (link_id, tag_id) VALUES ('ghost-link', 't1')`,
		`INSERT INTO link_tags (link_id, tag_id) VALUES ('l1', 'ghost-tag')`,
		`INSERT INTO link_shares (link_id, user_id, shared_by) VALUES ('ghost-link', 'u1', 'u1')`,
		`INSERT INTO link_shares (link_id, user_id, shared_by) VALUES ('l1', 'ghost-user', 'u1')`,
		`INSERT INTO link_clicks (id, link_id, user_id, ip_hash) VALUES ('c-orphan', 'ghost-link', NULL, 'h2')`,
		`INSERT INTO link_clicks (id, link_id, user_id, ip_hash) VALUES ('c-ghost-user', 'l1', 'ghost-user', 'h3')`,
		`INSERT INTO api_tokens (id, user_id, name, token_hash) VALUES ('tok-orphan', 'ghost-user', 'test', 'hash-2')`,
	}
	for _, q := range seed {
		if _, err := conn.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}

	if err := goose.Up(conn.DB, "."); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM link_owners`:                             1, // ('l1','u1') survives
		`SELECT COUNT(*) FROM link_tags`:                               1, // ('l1','t1') survives
		`SELECT COUNT(*) FROM link_shares`:                             0,
		`SELECT COUNT(*) FROM link_clicks`:                             2, // c-live + c-ghost-user survive
		`SELECT COUNT(*) FROM link_clicks WHERE user_id IS NOT NULL`:   1, // c-ghost-user nulled, c-live kept
		`SELECT COUNT(*) FROM api_tokens`:                              1, // tok-live survives
		`SELECT COUNT(*) FROM api_tokens WHERE user_id = 'ghost-user'`: 0,
	} {
		var got int
		if err := conn.QueryRow(query).Scan(&got); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}
}
