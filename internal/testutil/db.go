package testutil

import (
	"io/fs"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"

	"github.com/joestump/joe-links/internal/db"
	_ "modernc.org/sqlite"
)

// NewTestDB opens an in-memory SQLite DB and runs all goose migrations.
func NewTestDB(t *testing.T) *sqlx.DB {
	t.Helper()

	// Use a file URI with shared cache so all pool connections share the
	// same in-memory database. Each test gets a unique name to avoid
	// cross-test interference. Pragmas use modernc/sqlite's _pragma syntax
	// (mattn-style "_busy_timeout=5000" is silently ignored by this driver):
	// busy_timeout handles lock contention from async goroutines (e.g.
	// BearerTokenMiddleware.UpdateLastUsed) and foreign_keys matches
	// production (db.New enables it) so FK cascades are enforced in tests.
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	conn, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set goose dialect: %v", err)
	}

	sub, err := fs.Sub(db.Migrations, "migrations")
	if err != nil {
		t.Fatalf("sub migrations fs: %v", err)
	}

	goose.SetBaseFS(sub)
	if err := goose.Up(conn.DB, "."); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	goose.SetBaseFS(nil)

	return conn
}
