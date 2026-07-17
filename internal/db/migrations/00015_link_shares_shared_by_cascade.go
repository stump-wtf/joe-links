package migrations

// Governing: SPEC-0010 REQ "Link Shares Table", ADR-0002
// Gives link_shares.shared_by an explicit ON DELETE CASCADE disposition.
//
// Migration 00010 declared shared_by REFERENCES users(id) with no ON DELETE
// action, so any user delete that bypassed UserStore.DeleteUserWithLinks
// (which reattributes or deletes the rows first) failed with an FK violation
// once foreign keys were enforced. CASCADE matches the sibling link_id/user_id
// FKs on the same table: a share whose grantor is gone and was not reattributed
// has no surviving provenance, so it is dropped. The admin deletion flow still
// reattributes shares in "reassign" mode before the user row is deleted, so
// recipients keep their access there — the cascade is a schema-level backstop,
// not the primary mechanism.
//
// This is a Go migration because the change is dialect-specific: SQLite cannot
// ALTER a foreign key constraint (table rebuild required), PostgreSQL needs the
// auto-generated constraint name looked up and dropped, and MySQL ignored the
// column-level REFERENCES clause in 00010 entirely (inline REFERENCES
// specifications are parsed but not enforced), so it may have no constraint to
// drop and orphaned rows to clean before the real FK can be added.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3"
)

func init() {
	goose.AddMigrationContext(upLinkSharesSharedByCascade, downLinkSharesSharedByCascade)
}

func upLinkSharesSharedByCascade(ctx context.Context, tx *sql.Tx) error {
	// Remove rows whose grantor no longer exists. A no-op where the FK was
	// enforced (PostgreSQL always; SQLite since 00014 + foreign_keys(1)), but
	// MySQL never enforced the inline REFERENCES clause, so orphans may have
	// accumulated and would make the ADD CONSTRAINT below fail.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM link_shares WHERE shared_by NOT IN (SELECT id FROM users)`); err != nil {
		return fmt.Errorf("clean orphaned shared_by rows: %w", err)
	}
	return replaceSharedByFK(ctx, tx, " ON DELETE CASCADE")
}

func downLinkSharesSharedByCascade(ctx context.Context, tx *sql.Tx) error {
	// Restore the original NO ACTION semantics (bare REFERENCES clause).
	return replaceSharedByFK(ctx, tx, "")
}

// replaceSharedByFK swaps the foreign key on link_shares.shared_by for one
// with the given ON DELETE clause ("" or " ON DELETE CASCADE").
func replaceSharedByFK(ctx context.Context, tx *sql.Tx, onDelete string) error {
	switch dialect {
	case "postgres":
		name, err := findSharedByConstraint(ctx, tx, `
			SELECT con.conname
			FROM pg_constraint con
			JOIN pg_class rel ON rel.oid = con.conrelid
			JOIN pg_attribute att ON att.attrelid = rel.oid AND att.attnum = con.conkey[1]
			WHERE rel.relname = 'link_shares'
			  AND con.contype = 'f'
			  AND array_length(con.conkey, 1) = 1
			  AND att.attname = 'shared_by'`)
		if err != nil {
			return err
		}
		if name != "" {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE link_shares DROP CONSTRAINT "%s"`, name)); err != nil {
				return fmt.Errorf("drop shared_by constraint %s: %w", name, err)
			}
		}
		_, err = tx.ExecContext(ctx, `ALTER TABLE link_shares
			ADD CONSTRAINT link_shares_shared_by_fkey
			FOREIGN KEY (shared_by) REFERENCES users(id)`+onDelete)
		return err

	case "mysql":
		name, err := findSharedByConstraint(ctx, tx, `
			SELECT CONSTRAINT_NAME
			FROM information_schema.KEY_COLUMN_USAGE
			WHERE TABLE_SCHEMA = DATABASE()
			  AND TABLE_NAME = 'link_shares'
			  AND COLUMN_NAME = 'shared_by'
			  AND REFERENCED_TABLE_NAME = 'users'
			LIMIT 1`)
		if err != nil {
			return err
		}
		if name != "" {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf("ALTER TABLE link_shares DROP FOREIGN KEY `%s`", name)); err != nil {
				return fmt.Errorf("drop shared_by constraint %s: %w", name, err)
			}
		}
		_, err = tx.ExecContext(ctx, `ALTER TABLE link_shares
			ADD CONSTRAINT link_shares_shared_by_fkey
			FOREIGN KEY (shared_by) REFERENCES users(id)`+onDelete)
		return err

	default: // sqlite3 — FK constraints cannot be altered; rebuild the table.
		// link_shares is a leaf table (nothing references it), so dropping and
		// renaming inside the migration transaction is safe with foreign_keys
		// enabled; the copied rows already satisfy every constraint.
		for _, stmt := range []string{
			`CREATE TABLE link_shares_rebuild (
			    link_id    TEXT NOT NULL REFERENCES links(id) ON DELETE CASCADE,
			    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			    shared_by  TEXT NOT NULL REFERENCES users(id)` + onDelete + `,
			    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			    PRIMARY KEY (link_id, user_id)
			)`,
			`INSERT INTO link_shares_rebuild (link_id, user_id, shared_by, created_at)
			    SELECT link_id, user_id, shared_by, created_at FROM link_shares`,
			`DROP TABLE link_shares`,
			`ALTER TABLE link_shares_rebuild RENAME TO link_shares`,
			// The index on the old table went away with DROP TABLE; recreate it.
			`CREATE INDEX idx_link_shares_link_user ON link_shares(link_id, user_id)`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("rebuild link_shares: %w", err)
			}
		}
		return nil
	}
}

// findSharedByConstraint returns the name of the existing FK constraint on
// link_shares.shared_by, or "" if none exists (MySQL databases created by
// migration 00010 have none — inline REFERENCES clauses are ignored there).
func findSharedByConstraint(ctx context.Context, tx *sql.Tx, query string) (string, error) {
	var name string
	err := tx.QueryRowContext(ctx, query).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find shared_by constraint: %w", err)
	}
	return name, nil
}
