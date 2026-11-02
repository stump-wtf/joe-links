// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

// LocalProvider is the users.provider value for accounts that authenticate
// with a local password instead of an identity provider (ADR-0024). OIDC rows
// keep their issuer as provider, so a local username can never collide with an
// OIDC subject.
const LocalProvider = "local"

// LocalCredentialStore reads and writes the password hash for local-mode
// accounts. Only those accounts have a row here: OIDC users stay passwordless,
// which is why the hash is not a column on users.
type LocalCredentialStore struct {
	db *sqlx.DB
}

// NewLocalCredentialStore creates a LocalCredentialStore backed by db.
func NewLocalCredentialStore(db *sqlx.DB) *LocalCredentialStore {
	return &LocalCredentialStore{db: db}
}

// q rebinds ? placeholders to the driver's native form.
func (s *LocalCredentialStore) q(query string) string { return s.db.Rebind(query) }

// LocalCredential is a local account paired with its stored password hash.
type LocalCredential struct {
	User         *User
	PasswordHash string
}

// SetPasswordHash stores hash for userID, replacing any existing hash. Startup
// calls it on every boot in local mode, so rotating JOE_ADMIN_PASSWORD takes
// effect on the next restart instead of leaving a stale hash in the database.
func (s *LocalCredentialStore) SetPasswordHash(ctx context.Context, userID, hash string) error {
	now := time.Now().UTC()
	// MySQL has no ON CONFLICT ... DO UPDATE (ADR-0002 — contrast users.Upsert).
	if s.db.DriverName() == "mysql" {
		res, err := s.db.ExecContext(ctx, s.q(`UPDATE local_credentials SET password_hash = ?, updated_at = ? WHERE user_id = ?`), hash, now, userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			return nil
		}
		_, err = s.db.ExecContext(ctx, s.q(`
			INSERT INTO local_credentials (user_id, password_hash, updated_at)
			VALUES (?, ?, ?)
		`), userID, hash, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO local_credentials (user_id, password_hash, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			password_hash = excluded.password_hash,
			updated_at = excluded.updated_at
	`), userID, hash, now)
	return err
}

// GetByUsername returns the local account whose username is username, with its
// stored password hash. It returns ErrNotFound when no local account has that
// username, and when the account exists but has no credential row — a caller
// must not be able to tell the two apart, so neither case is distinguishable.
func (s *LocalCredentialStore) GetByUsername(ctx context.Context, username string) (*LocalCredential, error) {
	var row struct {
		UserID       string `db:"id"`
		PasswordHash string `db:"password_hash"`
	}
	err := s.db.GetContext(ctx, &row, s.q(`
		SELECT u.id, c.password_hash
		FROM users u
		JOIN local_credentials c ON c.user_id = u.id
		WHERE u.provider = ? AND u.subject = ?
	`), LocalProvider, username)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	user, err := NewUserStore(s.db).GetByID(ctx, row.UserID)
	if err != nil {
		return nil, err
	}
	return &LocalCredential{User: user, PasswordHash: row.PasswordHash}, nil
}

// Delete removes the credential for userID. Deleting the user cascades, so this
// exists for an operator removing local auth while keeping the account.
func (s *LocalCredentialStore) Delete(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM local_credentials WHERE user_id = ?`), userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}
