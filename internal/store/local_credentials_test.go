// Governing: SPEC-0001 REQ "Local Admin Authentication", ADR-0024
package store

import (
	"context"
	"testing"

	"github.com/joestump/joe-links/internal/testutil"
)

func TestLocalCredentialStore_RoundTrip(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := NewUserStore(db)
	creds := NewLocalCredentialStore(db)
	ctx := context.Background()

	user, err := users.Upsert(ctx, LocalProvider, "admin", "admin@local", "admin", "admin")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := creds.SetPasswordHash(ctx, user.ID, "pbkdf2-sha256$600000$AAA$BBB"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}

	cred, err := creds.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername: %v", err)
	}
	if cred.User.ID != user.ID {
		t.Errorf("user = %s, want %s", cred.User.ID, user.ID)
	}
	if cred.PasswordHash != "pbkdf2-sha256$600000$AAA$BBB" {
		t.Errorf("hash = %q, want the stored value", cred.PasswordHash)
	}

	if err := creds.SetPasswordHash(ctx, user.ID, "pbkdf2-sha256$600000$CCC$DDD"); err != nil {
		t.Fatalf("SetPasswordHash(rotate): %v", err)
	}
	cred, err = creds.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetByUsername after rotation: %v", err)
	}
	if cred.PasswordHash != "pbkdf2-sha256$600000$CCC$DDD" {
		t.Errorf("hash = %q, want the rotated value to replace it", cred.PasswordHash)
	}

	if err := creds.Delete(ctx, user.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := creds.Delete(ctx, user.ID); err != ErrNotFound {
		t.Errorf("Delete of a missing row = %v, want ErrNotFound", err)
	}
}

// An account with no credential row must not be distinguishable from an unknown
// username: both report ErrNotFound, so the login path cannot leak which local
// accounts exist.
func TestLocalCredentialStore_NoCredentialRowIsNotFound(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := NewUserStore(db)
	creds := NewLocalCredentialStore(db)
	ctx := context.Background()

	if _, err := users.Upsert(ctx, LocalProvider, "orphan", "orphan@local", "orphan", "user"); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := creds.GetByUsername(ctx, "orphan"); err != ErrNotFound {
		t.Errorf("GetByUsername(account without a credential) = %v, want ErrNotFound", err)
	}
	if _, err := creds.GetByUsername(ctx, "nobody"); err != ErrNotFound {
		t.Errorf("GetByUsername(unknown) = %v, want ErrNotFound", err)
	}
}

// The username is scoped to provider 'local', so it can never resolve to an
// OIDC identity whose subject happens to match.
func TestLocalCredentialStore_DoesNotResolveOIDCUsers(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := NewUserStore(db)
	creds := NewLocalCredentialStore(db)
	ctx := context.Background()

	oidcUser, err := users.Upsert(ctx, "https://issuer.example.com", "admin", "admin@example.com", "Admin", "admin")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := creds.SetPasswordHash(ctx, oidcUser.ID, "pbkdf2-sha256$600000$AAA$BBB"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	if _, err := creds.GetByUsername(ctx, "admin"); err != ErrNotFound {
		t.Errorf("GetByUsername resolved an OIDC identity: %v", err)
	}
}

// Deleting the user must take its credential with it, so a removed account
// cannot leave a password hash behind.
func TestLocalCredentialStore_CascadesOnUserDelete(t *testing.T) {
	db := testutil.NewTestDB(t)
	users := NewUserStore(db)
	creds := NewLocalCredentialStore(db)
	ctx := context.Background()

	user, err := users.Upsert(ctx, LocalProvider, "admin", "admin@local", "admin", "admin")
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := creds.SetPasswordHash(ctx, user.ID, "pbkdf2-sha256$600000$AAA$BBB"); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	if err := users.DeleteUserWithLinks(ctx, user.ID, user.ID, "delete"); err != nil {
		t.Fatalf("DeleteUserWithLinks: %v", err)
	}
	if _, err := creds.GetByUsername(ctx, "admin"); err != ErrNotFound {
		t.Errorf("credential survived the user delete: %v", err)
	}
}
