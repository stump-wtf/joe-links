// Governing: SPEC-0001 REQ "Local User Records", ADR-0003
package auth

import (
	"context"
	"testing"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

func TestComputeRole(t *testing.T) {
	tests := []struct {
		name        string
		adminEmail  string
		adminGroups []string
		groupsClaim string
		email       string
		claims      map[string]interface{}
		want        string
	}{
		{
			name:  "no admin config",
			email: "bob@example.com",
			want:  "user",
		},
		{
			name:       "admin email match",
			adminEmail: "alice@example.com",
			email:      "alice@example.com",
			want:       "admin",
		},
		{
			name:       "admin email mismatch",
			adminEmail: "alice@example.com",
			email:      "bob@example.com",
			want:       "user",
		},
		{
			name:        "group match interface slice",
			adminGroups: []string{"joe-admins"},
			email:       "bob@example.com",
			claims:      map[string]interface{}{"groups": []interface{}{"users", "joe-admins"}},
			want:        "admin",
		},
		{
			name:        "group match string slice",
			adminGroups: []string{"joe-admins"},
			email:       "bob@example.com",
			claims:      map[string]interface{}{"groups": []string{"joe-admins"}},
			want:        "admin",
		},
		{
			name:        "group mismatch",
			adminGroups: []string{"joe-admins"},
			email:       "bob@example.com",
			claims:      map[string]interface{}{"groups": []interface{}{"users"}},
			want:        "user",
		},
		{
			name:        "groups claim absent",
			adminGroups: []string{"joe-admins"},
			email:       "bob@example.com",
			claims:      map[string]interface{}{},
			want:        "user",
		},
		{
			name:        "custom groups claim name",
			adminGroups: []string{"joe-admins"},
			groupsClaim: "roles",
			email:       "bob@example.com",
			claims:      map[string]interface{}{"roles": []interface{}{"joe-admins"}},
			want:        "admin",
		},
		{
			name:        "custom claim ignores default claim",
			adminGroups: []string{"joe-admins"},
			groupsClaim: "roles",
			email:       "bob@example.com",
			claims:      map[string]interface{}{"groups": []interface{}{"joe-admins"}},
			want:        "user",
		},
		{
			name:        "non-string group entries skipped",
			adminGroups: []string{"joe-admins"},
			email:       "bob@example.com",
			claims:      map[string]interface{}{"groups": []interface{}{42, "joe-admins"}},
			want:        "admin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groupsClaim := tt.groupsClaim
			if groupsClaim == "" {
				groupsClaim = "groups"
			}
			h := &Handlers{
				adminEmail:  tt.adminEmail,
				adminGroups: tt.adminGroups,
				groupsClaim: groupsClaim,
			}
			claims := tt.claims
			if claims == nil {
				claims = map[string]interface{}{}
			}
			if got := h.computeRole(tt.email, claims); got != tt.want {
				t.Errorf("computeRole(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

// Grant-only semantics: env-driven admin config promotes but never demotes,
// so admin-UI promotions survive logins where the computed role is "user".
// Governing: SPEC-0001 REQ "Local User Records"
func TestApplyRoleGrant(t *testing.T) {
	tests := []struct {
		name     string
		stored   string // role in the users table before login
		computed string // role computed from env config at login
		want     string
	}{
		{"promotes user when env grants admin", "user", "admin", "admin"},
		{"never demotes manually promoted admin", "admin", "user", "admin"},
		{"leaves user alone without grant", "user", "user", "user"},
		{"admin with grant is a no-op", "admin", "admin", "admin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			us := store.NewUserStore(testutil.NewTestDB(t))
			ctx := context.Background()

			u, err := us.Upsert(ctx, "test", "sub1", "bob@example.com", "Bob Jones", tt.stored)
			if err != nil {
				t.Fatalf("upsert: %v", err)
			}

			h := &Handlers{users: us}
			got, err := h.applyRoleGrant(ctx, u, tt.computed)
			if err != nil {
				t.Fatalf("applyRoleGrant: %v", err)
			}
			if got.Role != tt.want {
				t.Errorf("returned role = %q, want %q", got.Role, tt.want)
			}

			// The returned record must match what is persisted.
			stored, err := us.GetByID(ctx, u.ID)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if stored.Role != tt.want {
				t.Errorf("persisted role = %q, want %q", stored.Role, tt.want)
			}
		})
	}
}
