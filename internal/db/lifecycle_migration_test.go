// Migration 00016 (link lifecycle columns) round-trip: Up adds nullable
// expires_at and archived_at to links, Down drops exactly what Up added.
// Plain portable SQL — no per-dialect branching (ADR-0002 / ADR-0020).
//
// Governing: SPEC-0020 REQ "Link Expiration", REQ "Archive State", ADR-0020
package db_test

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"

	"github.com/joestump/joe-links/internal/db"
)

// linkColumns returns the set of column names on the links table.
func linkColumns(t *testing.T, conn *sqlx.DB) map[string]bool {
	t.Helper()
	rows, err := conn.Query(`PRAGMA table_info(links)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return cols
}

func TestLifecycleMigration_UpDownUp(t *testing.T) {
	conn, err := db.New("sqlite3", filepath.Join(t.TempDir(), "lifecycle.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	cols := linkColumns(t, conn)
	if !cols["expires_at"] || !cols["archived_at"] {
		t.Fatalf("after Up: expires_at=%v archived_at=%v, want both present", cols["expires_at"], cols["archived_at"])
	}

	// Down to 00015 must drop both lifecycle columns (each down migration
	// drops what its up migration added). db.Migrate resets goose's BaseFS
	// after Up, so point goose back at the embedded migrations for the Down.
	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub migrations fs: %v", err)
	}
	goose.SetBaseFS(sub)
	defer goose.SetBaseFS(nil)
	if err := goose.DownTo(conn.DB, ".", 15); err != nil {
		t.Fatalf("goose down to 00015: %v", err)
	}
	cols = linkColumns(t, conn)
	if cols["expires_at"] || cols["archived_at"] {
		t.Fatalf("after Down: expires_at=%v archived_at=%v, want both absent", cols["expires_at"], cols["archived_at"])
	}

	// Re-applying is clean (Down left no residue behind).
	if err := db.Migrate(conn, "sqlite3"); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	cols = linkColumns(t, conn)
	if !cols["expires_at"] || !cols["archived_at"] {
		t.Fatalf("after re-Up: expires_at=%v archived_at=%v, want both present", cols["expires_at"], cols["archived_at"])
	}
}
