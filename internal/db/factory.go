// Governing: SPEC-0001 REQ "Pluggable Database Backend", ADR-0002
package db

import (
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// New opens a database connection for the given driver and DSN.
// Supported drivers: sqlite3, mysql, postgres.
func New(driver, dsn string) (*sqlx.DB, error) {
	switch driver {
	case "sqlite3":
		// modernc/sqlite uses "sqlite" as the driver name (CGO-free)
		db, err := sqlx.Open("sqlite", sqliteDSN(dsn))
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}
		return db, nil
	case "mysql":
		db, err := sqlx.Open("mysql", dsn)
		if err != nil {
			return nil, fmt.Errorf("open mysql: %w", err)
		}
		return db, nil
	case "postgres":
		db, err := sqlx.Open("postgres", dsn)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		return db, nil
	default:
		return nil, fmt.Errorf("unsupported DB driver %q: must be sqlite3, mysql, or postgres", driver)
	}
}

// sqliteDSN appends the pragmas every connection needs, using modernc/sqlite's
// _pragma DSN syntax so the driver applies them to each pooled connection —
// a post-open Exec would only configure whichever single connection served it.
//   - foreign_keys defaults to OFF per connection in SQLite; without it every
//     ON DELETE CASCADE / SET NULL in the schema is inert.
//   - busy_timeout makes concurrent writers wait instead of failing
//     immediately with SQLITE_BUSY.
//   - journal_mode(WAL) allows readers to proceed while a writer is active.
func sqliteDSN(dsn string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}
