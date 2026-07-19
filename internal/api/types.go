// Governing: SPEC-0005 REQ "API Response Structures", SPEC-0007 REQ "Request/Response Type Declarations", ADR-0008
package api

import (
	"encoding/json"
	"time"
)

// OptionalTime distinguishes an omitted JSON field from an explicit null so
// PUT /links/{id} can tell "leave expires_at unchanged" (omitted) apart from
// "clear it" (null). Absent fields never invoke UnmarshalJSON, so Set stays
// false; an explicit null sets Set with a nil Time.
// Governing: SPEC-0020 REQ "Link Expiration" — "explicitly passing null on update MUST clear it"
type OptionalTime struct {
	Set  bool
	Time *time.Time
}

// UnmarshalJSON implements tri-state decoding: present-null and present-value.
func (o *OptionalTime) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Time = nil
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	o.Time = &t
	return nil
}

// ErrorResponse is the standard error shape.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// OwnerResponse represents a link owner.
type OwnerResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	IsPrimary bool   `json:"is_primary"`
}

// HealthResponse is the destination-health object attached to link resources
// for callers holding capabilities on the link (owners, co-owners, admins,
// share recipients). Until the destination health checker lands (story #274 —
// the link_health table), every link's derived state is "unchecked" with null
// details: absence of a health row means "never checked".
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP", REQ "Destination Health Checking"
type HealthResponse struct {
	Status        string     `json:"status" enums:"unchecked,ok,broken,skipped"`
	LastCheckedAt *time.Time `json:"last_checked_at"`
	LastStatus    *int       `json:"last_status"`
}

// LinkResponse is the full link resource.
// Governing: SPEC-0005 REQ "API Response Structures", SPEC-0010 REQ "REST API Visibility Field"
// Governing: SPEC-0020 REQ "Link Expiration" — expires_at is RFC 3339 or null (never expires)
// Governing: SPEC-0020 REQ "Lifecycle State in API and MCP" — archived_at,
// derived lifecycle_state, and the capability-gated health object
type LinkResponse struct {
	ID             string          `json:"id"`
	Slug           string          `json:"slug"`
	URL            string          `json:"url"`
	Title          string          `json:"title"`
	Description    string          `json:"description"`
	Visibility     string          `json:"visibility"`
	ExpiresAt      *time.Time      `json:"expires_at"`
	ArchivedAt     *time.Time      `json:"archived_at"`
	LifecycleState string          `json:"lifecycle_state" enums:"active,expired,archived"`
	Health         *HealthResponse `json:"health,omitempty"`
	Tags           []string        `json:"tags"`
	Owners         []OwnerResponse `json:"owners"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// LinkListResponse wraps a paginated list of links.
// Governing: SPEC-0005 REQ "Pagination"
type LinkListResponse struct {
	Links      []*LinkResponse `json:"links"`
	NextCursor *string         `json:"next_cursor"`
}

// CreateLinkRequest is the body for POST /api/v1/links.
// Governing: SPEC-0005 REQ "Links Collection", SPEC-0010 REQ "REST API Visibility Field"
// Governing: SPEC-0020 REQ "Link Expiration" — omitting expires_at on create yields NULL
type CreateLinkRequest struct {
	Slug        string     `json:"slug"`
	URL         string     `json:"url"`
	Title       string     `json:"title,omitempty"`
	Description string     `json:"description,omitempty"`
	Visibility  string     `json:"visibility,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
}

// UpdateLinkRequest is the body for PUT /api/v1/links/{id}.
// Governing: SPEC-0005 REQ "Link Resource" — slug is intentionally omitted (immutable).
// Governing: SPEC-0010 REQ "REST API Visibility Field"
// Governing: SPEC-0020 REQ "Link Expiration" — expires_at omitted = unchanged, null = clear
// Governing: SPEC-0020 REQ "Archive State" — archived true sets archived_at
// (if not already set), false clears it; a body with only "archived" toggles
// archive state without performing the full-resource update
type UpdateLinkRequest struct {
	URL         string       `json:"url"`
	Title       string       `json:"title,omitempty"`
	Description string       `json:"description,omitempty"`
	Visibility  string       `json:"visibility,omitempty"`
	ExpiresAt   OptionalTime `json:"expires_at,omitempty" swaggertype:"string" format:"date-time" extensions:"x-nullable"`
	Archived    *bool        `json:"archived,omitempty"`
	Tags        []string     `json:"tags,omitempty"`
}

// AddOwnerRequest is the body for POST /api/v1/links/{id}/owners.
// Governing: SPEC-0005 REQ "Co-Owner Management"
type AddOwnerRequest struct {
	Email string `json:"email"`
}

// AddShareRequest is the body for POST /api/v1/links/{id}/shares.
// Governing: SPEC-0010 REQ "Link Share Management API Endpoints"
type AddShareRequest struct {
	Email string `json:"email"`
}

// ShareResponse represents a share record in API responses.
// Governing: SPEC-0010 REQ "Link Share Management API Endpoints"
type ShareResponse struct {
	LinkID      string    `json:"link_id"`
	UserID      string    `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	SharedBy    string    `json:"shared_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// TagResponse represents a tag with its link count.
// Governing: SPEC-0005 REQ "API Response Structures"
type TagResponse struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	LinkCount int    `json:"link_count"`
}

// TagListResponse wraps a paginated list of tags.
// Governing: SPEC-0005 REQ "Pagination"
type TagListResponse struct {
	Tags       []*TagResponse `json:"tags"`
	NextCursor *string        `json:"next_cursor"`
}

// UserResponse represents a user profile.
type UserResponse struct {
	ID          string    `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"created_at"`
}

// UserListResponse wraps a paginated list of users.
// Governing: SPEC-0005 REQ "Pagination"
type UserListResponse struct {
	Users      []*UserResponse `json:"users"`
	NextCursor *string         `json:"next_cursor"`
}

// UpdateRoleRequest is the body for PUT /api/v1/admin/users/{id}/role.
// Governing: SPEC-0005 REQ "Admin Endpoints"
type UpdateRoleRequest struct {
	Role string `json:"role"`
}

// TokenResponse is the API token representation (never includes token_hash).
type TokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// TokenCreatedResponse is returned only on POST /api/v1/tokens — includes plaintext once.
type TokenCreatedResponse struct {
	TokenResponse
	Token string `json:"token"`
}

// TokenListResponse wraps a list of tokens.
type TokenListResponse struct {
	Tokens []*TokenResponse `json:"tokens"`
}

// CreateTokenRequest is the body for POST /api/v1/tokens.
type CreateTokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}
