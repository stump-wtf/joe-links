// Governing: SPEC-0002 REQ "Link Store Interface", ADR-0005, ADR-0002
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Link represents a row in the links table.
type Link struct {
	ID          string     `db:"id"`
	Slug        string     `db:"slug"`
	URL         string     `db:"url"`
	Title       string     `db:"title"`
	Description string     `db:"description"`
	Visibility  string     `db:"visibility"` // Governing: SPEC-0010 REQ "Visibility Column on Links Table"
	ExpiresAt   *time.Time `db:"expires_at"`  // Governing: SPEC-0020 REQ "Link Expiration" — NULL means never expires
	ArchivedAt  *time.Time `db:"archived_at"` // Governing: SPEC-0020 REQ "Archive State" — NULL means not archived
	CreatedAt   time.Time  `db:"created_at"`
	UpdatedAt   time.Time  `db:"updated_at"`
}

// Derived lifecycle states. Lifecycle state is computed from the two nullable
// timestamps at read time — there is no status column (ADR-0020).
// Governing: SPEC-0020, ADR-0020
const (
	LifecycleActive   = "active"
	LifecycleExpired  = "expired"
	LifecycleArchived = "archived"
)

// IsExpired reports whether the link's expires_at is set and has passed.
// Governing: SPEC-0020 REQ "Link Expiration" — expiry is derived at read time
func (l *Link) IsExpired(now time.Time) bool {
	return l.ExpiresAt != nil && !l.ExpiresAt.After(now)
}

// IsArchived reports whether the link is archived (archived_at set).
// Governing: SPEC-0020 REQ "Archive State"
func (l *Link) IsArchived() bool { return l.ArchivedAt != nil }

// LifecycleState derives the link's lifecycle state: archived when archived_at
// is set (archived wins when both apply), else expired when expires_at <= now,
// else active.
// Governing: SPEC-0020 REQ "Archive State" scenario "Archived Beats Expired in Derived State", ADR-0020
func (l *Link) LifecycleState(now time.Time) string {
	switch {
	case l.IsArchived():
		return LifecycleArchived
	case l.IsExpired(now):
		return LifecycleExpired
	default:
		return LifecycleActive
	}
}

// ShareRecord represents a row in the link_shares table.
// Governing: SPEC-0010 REQ "Link Shares Table"
type ShareRecord struct {
	LinkID    string    `db:"link_id"`
	UserID    string    `db:"user_id"`
	SharedBy  string    `db:"shared_by"`
	CreatedAt time.Time `db:"created_at"`
}

// LinkStore is the sqlx-backed link data access layer. No handler may query
// the DB directly; all link access goes through this store.
// Governing: SPEC-0002 REQ "Link Store Interface"
type LinkStore struct {
	db   *sqlx.DB
	owns *OwnershipStore
	tags *TagStore
}

func NewLinkStore(db *sqlx.DB, owns *OwnershipStore, tags *TagStore) *LinkStore {
	return &LinkStore{db: db, owns: owns, tags: tags}
}

// q rebinds ? placeholders to the driver's native format ($1,$2,... for PostgreSQL).
func (s *LinkStore) q(query string) string { return s.db.Rebind(query) }

// agg returns a dialect-appropriate string aggregation expression with deduplication.
// PostgreSQL: STRING_AGG(DISTINCT col, ',') — SQLite/MySQL: GROUP_CONCAT(DISTINCT col)
func (s *LinkStore) aggDistinct(col string) string {
	if s.db.DriverName() == "postgres" {
		return "COALESCE(STRING_AGG(DISTINCT " + col + ", ','), '')"
	}
	return "COALESCE(GROUP_CONCAT(DISTINCT " + col + "), '')"
}

// Create inserts a new link and registers ownerID as the primary owner.
// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
func (s *LinkStore) Create(ctx context.Context, slug, url, ownerID, title, description, visibility string) (*Link, error) {
	// Governing: SPEC-0002 REQ "Links Table" — reject over-length title/description before insert
	if err := ValidateLinkText(title, description); err != nil {
		return nil, err
	}
	// Belt-and-braces: handlers validate first, but the store is the single
	// source of truth for slug format + reservation (#204) — a future direct
	// caller must not be able to bypass it.
	if err := ValidateSlugFormat(slug); err != nil {
		return nil, err
	}
	// Same backstop for the destination URL scheme (issue #265).
	if err := ValidateLinkURL(url); err != nil {
		return nil, err
	}
	if visibility == "" {
		visibility = "public"
	}
	id := uuid.New().String()
	now := time.Now().UTC()

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, tx.Rebind(`
		INSERT INTO links (id, slug, url, title, description, visibility, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`), id, slug, url, title, description, visibility, now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}

	_, err = tx.ExecContext(ctx, tx.Rebind(`
		INSERT INTO link_owners (link_id, user_id, is_primary, created_at) VALUES (?, ?, 1, ?)
	`), id, ownerID, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return s.GetByID(ctx, id)
}

// CreateFull creates a link together with its primary owner, tags, and share
// grants in a single transaction: if any write fails, no partial link exists.
// Tag names are deduplicated by their upserted tag ID so duplicate spellings
// of the same tag cannot violate the link_tags primary key and roll back the
// write. Share user IDs must be pre-resolved to existing users by the caller.
// Governing: SPEC-0018 REQ "Database Operation Standards", ADR-0018
// Governing: SPEC-0020 REQ "Link Expiration" — optional expires_at persisted at create
func (s *LinkStore) CreateFull(ctx context.Context, slug, url, ownerID, title, description, visibility string, expiresAt *time.Time, tagNames, shareUserIDs []string, sharedBy string) (*Link, error) {
	// Governing: SPEC-0002 REQ "Links Table" — reject over-length title/description before insert
	if err := ValidateLinkText(title, description); err != nil {
		return nil, err
	}
	// Belt-and-braces: handlers validate first, but the store is the single
	// source of truth for slug format + reservation (#204) — a future direct
	// caller must not be able to bypass it.
	if err := ValidateSlugFormat(slug); err != nil {
		return nil, err
	}
	// Same backstop for the destination URL scheme and the tag list — hostile
	// tag names and non-http(s) URLs are rejected before any row is written
	// (issues #251, #265).
	if err := ValidateLinkURL(url); err != nil {
		return nil, err
	}
	if err := ValidateTagNames(tagNames); err != nil {
		return nil, err
	}
	// A new past expiration must never be persisted (create has no stored value).
	// Governing: SPEC-0020 REQ "Link Expiration" scenario "Past Expiration Rejected"
	if err := ValidateExpiresAt(expiresAt, nil, time.Now().UTC()); err != nil {
		return nil, err
	}
	expiresAt = NormalizeExpiresAt(expiresAt)
	if visibility == "" {
		visibility = "public"
	}
	id := uuid.New().String()
	now := time.Now().UTC()

	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("create full link: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, tx.Rebind(`
		INSERT INTO links (id, slug, url, title, description, visibility, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), id, slug, url, title, description, visibility, expiresAt, now, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("create full link: insert link: %w", err)
	}

	_, err = tx.ExecContext(ctx, tx.Rebind(`
		INSERT INTO link_owners (link_id, user_id, is_primary, created_at) VALUES (?, ?, 1, ?)
	`), id, ownerID, now)
	if err != nil {
		return nil, fmt.Errorf("create full link: insert owner: %w", err)
	}

	seenTags := make(map[string]bool, len(tagNames))
	for _, name := range tagNames {
		tag, err := s.tags.upsertTx(ctx, tx, name)
		if err != nil {
			return nil, fmt.Errorf("create full link: upsert tag %q: %w", name, err)
		}
		if seenTags[tag.ID] {
			continue
		}
		seenTags[tag.ID] = true
		_, err = tx.ExecContext(ctx, tx.Rebind(`
			INSERT INTO link_tags (link_id, tag_id) VALUES (?, ?)
		`), id, tag.ID)
		if err != nil {
			return nil, fmt.Errorf("create full link: insert link_tag: %w", err)
		}
	}

	seenShares := make(map[string]bool, len(shareUserIDs))
	for _, uid := range shareUserIDs {
		if seenShares[uid] {
			continue
		}
		seenShares[uid] = true
		_, err = tx.ExecContext(ctx, tx.Rebind(`
			INSERT INTO link_shares (link_id, user_id, shared_by) VALUES (?, ?, ?)
		`), id, uid, sharedBy)
		if err != nil {
			return nil, fmt.Errorf("create full link: insert share: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("create full link: commit: %w", err)
	}

	return s.GetByID(ctx, id)
}

// GetBySlug returns the link matching slug, or ErrNotFound.
// Governing: SPEC-0002 REQ "Link Store Interface" — WHEN GetBySlug called with missing slug THEN returns sentinel ErrNotFound
func (s *LinkStore) GetBySlug(ctx context.Context, slug string) (*Link, error) {
	var l Link
	err := s.db.GetContext(ctx, &l, s.q(`SELECT * FROM links WHERE slug = ?`), slug)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// GetByID returns the link matching id, or ErrNotFound.
func (s *LinkStore) GetByID(ctx context.Context, id string) (*Link, error) {
	var l Link
	err := s.db.GetContext(ctx, &l, s.q(`SELECT * FROM links WHERE id = ?`), id)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// ListByOwner returns all links where userID appears in link_owners.
// Governing: SPEC-0002 REQ "Link Store Interface" — WHEN ListByOwner called with user ID THEN returns all links where user appears in link_owners
func (s *LinkStore) ListByOwner(ctx context.Context, ownerID string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_owners lo ON lo.link_id = l.id
		WHERE lo.user_id = ?
		ORDER BY l.slug ASC
	`), ownerID)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListAll returns all links ordered by slug.
func (s *LinkStore) ListAll(ctx context.Context) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, `SELECT * FROM links ORDER BY slug ASC`)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListAllPaginated returns links ordered by (slug, id), starting after the
// given keyset cursor. It fetches up to limit rows; pass cursorSlug/cursorID
// from the last row of the previous page (empty for the first page).
// Governing: SPEC-0005 REQ "Pagination"
func (s *LinkStore) ListAllPaginated(ctx context.Context, limit int, cursorSlug, cursorID string) ([]*Link, error) {
	var links []*Link
	if cursorSlug == "" && cursorID == "" {
		err := s.db.SelectContext(ctx, &links, s.q(`
			SELECT * FROM links
			ORDER BY slug ASC, id ASC
			LIMIT ?
		`), limit)
		return links, err
	}
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT * FROM links
		WHERE slug > ? OR (slug = ? AND id > ?)
		ORDER BY slug ASC, id ASC
		LIMIT ?
	`), cursorSlug, cursorSlug, cursorID, limit)
	return links, err
}

// ListByOwnerOrSharedPaginated returns links where userID is an owner or has a
// share record, ordered by (slug, id) and keyset-paginated.
// Governing: SPEC-0005 REQ "Pagination", SPEC-0010 REQ "REST API Visibility Field"
func (s *LinkStore) ListByOwnerOrSharedPaginated(ctx context.Context, userID string, limit int, cursorSlug, cursorID string) ([]*Link, error) {
	var links []*Link
	if cursorSlug == "" && cursorID == "" {
		err := s.db.SelectContext(ctx, &links, s.q(`
			SELECT DISTINCT l.* FROM links l
			LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
			LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
			WHERE lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL
			ORDER BY l.slug ASC, l.id ASC
			LIMIT ?
		`), userID, userID, limit)
		return links, err
	}
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT DISTINCT l.* FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE (lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)
		  AND (l.slug > ? OR (l.slug = ? AND l.id > ?))
		ORDER BY l.slug ASC, l.id ASC
		LIMIT ?
	`), userID, userID, cursorSlug, cursorSlug, cursorID, limit)
	return links, err
}

// SearchByOwner returns links owned by userID whose slug, url, or description
// contain the search term (case-insensitive LIKE). Returns all owner links if q is empty.
// Governing: SPEC-0004 REQ "User Dashboard" — HTMX debounced search
func (s *LinkStore) SearchByOwner(ctx context.Context, ownerID, q string) ([]*Link, error) {
	if q == "" {
		return s.ListByOwner(ctx, ownerID)
	}
	var links []*Link
	// LIKE metacharacters in the search term are escaped so user input matches
	// literally (issue #265).
	pattern := "%" + escapeLike(q) + "%"
	// LOWER(col) LIKE LOWER(?) so matching is case-insensitive on PostgreSQL
	// too (plain LIKE is case-sensitive there; SQLite/MySQL already fold ASCII).
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_owners lo ON lo.link_id = l.id
		WHERE lo.user_id = ?
		  AND (LOWER(l.slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.url) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.description) LIKE LOWER(?) ESCAPE '!')
		ORDER BY l.slug ASC
	`), ownerID, pattern, pattern, pattern)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// SearchAll returns all links whose slug, url, or description contain the
// search term (case-insensitive LIKE). Returns all links if q is empty.
// Governing: SPEC-0004 REQ "User Dashboard" — HTMX debounced search (admin view)
func (s *LinkStore) SearchAll(ctx context.Context, q string) ([]*Link, error) {
	if q == "" {
		return s.ListAll(ctx)
	}
	var links []*Link
	// LIKE metacharacters escaped — see SearchByOwner (issue #265).
	pattern := "%" + escapeLike(q) + "%"
	// LOWER(col) LIKE LOWER(?) — see SearchByOwner for the PostgreSQL rationale.
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT * FROM links
		WHERE LOWER(slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(url) LIKE LOWER(?) ESCAPE '!' OR LOWER(description) LIKE LOWER(?) ESCAPE '!'
		ORDER BY slug ASC
	`), pattern, pattern, pattern)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListByOwnerAndTag returns links owned by userID that have the given tag slug.
// Governing: SPEC-0004 REQ "User Dashboard" — tag filter
func (s *LinkStore) ListByOwnerAndTag(ctx context.Context, ownerID, tagSlug string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_owners lo ON lo.link_id = l.id
		INNER JOIN link_tags lt ON lt.link_id = l.id
		INNER JOIN tags t ON t.id = lt.tag_id
		WHERE lo.user_id = ? AND t.slug = ?
		ORDER BY l.slug ASC
	`), ownerID, tagSlug)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// Update modifies an existing link's url, title, description, visibility, and
// expiration. Callers pass the final desired expires_at (overlaying the stored
// value themselves when the surface treats "omitted" as "unchanged"); nil
// clears it.
// Governing: SPEC-0001 REQ "Short Link Management" — slug is immutable after creation.
// Governing: SPEC-0010 REQ "Visibility Selector in Link Forms"
// Governing: SPEC-0020 REQ "Link Expiration" — set/clear/validate lives in the store layer
func (s *LinkStore) Update(ctx context.Context, id, url, title, description, visibility string, expiresAt *time.Time) (*Link, error) {
	// Governing: SPEC-0002 REQ "Links Table" — reject over-length title/description before update
	if err := ValidateLinkText(title, description); err != nil {
		return nil, err
	}
	// Scheme allowlist backstop, matching Create/CreateFull (issue #265).
	if err := ValidateLinkURL(url); err != nil {
		return nil, err
	}
	// Expiration backstop: a NEW past value is rejected; the stored value
	// round-tripped unchanged (as the edit form and full-resource PUTs do) is
	// accepted, so expired links stay editable (SPEC-0020).
	// Governing: SPEC-0020 REQ "Link Expiration" scenarios "Past Expiration
	// Rejected", "Expired Link Stays Editable", "Expiration Cleared on Edit"
	stored, err := s.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := ValidateExpiresAt(expiresAt, stored.ExpiresAt, time.Now().UTC()); err != nil {
		return nil, err
	}
	expiresAt = NormalizeExpiresAt(expiresAt)
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, s.q(`
		UPDATE links SET url = ?, title = ?, description = ?, visibility = ?, expires_at = ?, updated_at = ? WHERE id = ?
	`), url, title, description, visibility, expiresAt, now, id)
	if err != nil {
		return nil, err
	}
	return s.GetByID(ctx, id)
}

// UpdateVisibility sets the visibility field on a link.
// Governing: SPEC-0010 REQ "Visibility Column on Links Table", REQ "Admin Visibility Override"
func (s *LinkStore) UpdateVisibility(ctx context.Context, id, visibility string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, s.q(`UPDATE links SET visibility = ?, updated_at = ? WHERE id = ?`),
		visibility, now, id)
	return err
}

// ListByOwnerOrShared returns links where userID is an owner or has a share record.
// Governing: SPEC-0010 REQ "REST API Visibility Field"
func (s *LinkStore) ListByOwnerOrShared(ctx context.Context, userID string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT DISTINCT l.* FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL
		ORDER BY l.slug ASC
	`), userID, userID)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListByURL returns links whose URL exactly matches the given string.
// Admins see all matches; regular users see owned, shared, or public links.
func (s *LinkStore) ListByURL(ctx context.Context, url, userID string, isAdmin bool) ([]*Link, error) {
	var links []*Link
	if isAdmin {
		err := s.db.SelectContext(ctx, &links, s.q(`
			SELECT * FROM links WHERE url = ? ORDER BY created_at DESC
		`), url)
		return links, err
	}
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT DISTINCT l.* FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE l.url = ? AND (l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)
		ORDER BY l.created_at DESC
	`), userID, userID, url)
	return links, err
}

// Delete removes a link by ID. CASCADE deletes handle link_owners and link_tags.
func (s *LinkStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM links WHERE id = ?`), id)
	return err
}

// AddOwner adds userID as a co-owner of linkID.
// Returns ErrDuplicateOwner if already present.
func (s *LinkStore) AddOwner(ctx context.Context, linkID, userID string) error {
	err := s.owns.AddOwner(linkID, userID)
	if err == ErrAlreadyOwner {
		return ErrDuplicateOwner
	}
	return err
}

// RemoveOwner removes userID from link_owners. Primary owners cannot be removed.
func (s *LinkStore) RemoveOwner(ctx context.Context, linkID, userID string) error {
	return s.owns.RemoveOwner(linkID, userID)
}

// SetTags replaces the tag set for a link. Tags are upserted by name and
// deduplicated by their upserted tag ID — i.e. by derived slug (ADR-0005), so
// spelling variants of the same tag ("Jira", "jira") cannot collide on the
// link_tags primary key and roll back the whole transaction. First occurrence
// wins. Mirrors CreateFull's dedupe (issue #198).
func (s *LinkStore) SetTags(ctx context.Context, linkID string, tagNames []string) error {
	// Reject hostile names and oversized tag lists before any row is touched,
	// so a failed validation cannot leave the link with its tags cleared
	// (issues #251, #265).
	if err := ValidateTagNames(tagNames); err != nil {
		return err
	}
	tx, err := s.db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Clear existing tags for this link.
	_, err = tx.ExecContext(ctx, tx.Rebind(`DELETE FROM link_tags WHERE link_id = ?`), linkID)
	if err != nil {
		return err
	}

	// Upsert each tag and link it, skipping duplicates by tag ID.
	seen := make(map[string]bool, len(tagNames))
	for _, name := range tagNames {
		tag, err := s.tags.upsertTx(ctx, tx, name)
		if err != nil {
			return err
		}
		if seen[tag.ID] {
			continue
		}
		seen[tag.ID] = true
		_, err = tx.ExecContext(ctx, tx.Rebind(`
			INSERT INTO link_tags (link_id, tag_id) VALUES (?, ?)
		`), linkID, tag.ID)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// ListTags returns all tags associated with a link.
func (s *LinkStore) ListTags(ctx context.Context, linkID string) ([]*Tag, error) {
	var tags []*Tag
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT t.* FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		WHERE lt.link_id = ?
		ORDER BY t.name ASC
	`), linkID)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// AdminLink is a Link augmented with owner display names and tag names for admin views.
// Governing: SPEC-0011 REQ "Admin Links Screen", ADR-0005
type AdminLink struct {
	Link
	Owners    string `db:"owners"`     // comma-separated owner display names
	Tags      string `db:"tags"`       // comma-separated tag names
	OwnerSlug string `db:"owner_slug"` // primary owner's display_name_slug (populated in public views)
	IsOwner   bool   `db:"is_owner"`   // true if the querying user is an owner (populated in public views)
}

// TagList returns tag names as a slice for template iteration.
func (a *AdminLink) TagList() []string {
	if a.Tags == "" {
		return nil
	}
	return strings.Split(a.Tags, ",")
}

// ListAllAdmin returns all links with owner display names and tags joined, ordered by slug.
// Supports optional search query filtering by slug, URL, title, or owner display name.
// Governing: SPEC-0011 REQ "Admin Links Screen"
func (s *LinkStore) ListAllAdmin(ctx context.Context, q string) ([]*AdminLink, error) {
	query := fmt.Sprintf(`
		SELECT l.*,
			%s AS owners,
			%s AS tags
		FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id
		LEFT JOIN users u ON u.id = lo.user_id
		LEFT JOIN link_tags lt ON lt.link_id = l.id
		LEFT JOIN tags t ON t.id = lt.tag_id`,
		s.aggDistinct("u.display_name"),
		s.aggDistinct("t.name"),
	)
	var args []interface{}
	if q != "" {
		// LIKE metacharacters escaped — see SearchByOwner (issue #265).
		pattern := "%" + escapeLike(q) + "%"
		// The owner-name filter is an EXISTS subquery, not a WHERE on the joined
		// u.display_name: filtering the joined rows would drop every non-matching
		// owner/tag row before GROUP_CONCAT/STRING_AGG runs, truncating the
		// aggregated owners and tags columns to the matching subset (issue #205).
		// LOWER(col) LIKE LOWER(?) keeps matching case-insensitive on PostgreSQL,
		// where plain LIKE is case-sensitive (SQLite/MySQL already match ASCII
		// case-insensitively, so this is a no-op there).
		query += ` WHERE LOWER(l.slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.url) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.title) LIKE LOWER(?) ESCAPE '!'
			OR EXISTS (
				SELECT 1 FROM link_owners lo2
				INNER JOIN users u2 ON u2.id = lo2.user_id
				WHERE lo2.link_id = l.id AND LOWER(u2.display_name) LIKE LOWER(?) ESCAPE '!'
			)`
		args = append(args, pattern, pattern, pattern, pattern)
	}
	query += ` GROUP BY l.id ORDER BY l.slug ASC`

	var links []*AdminLink
	err := s.db.SelectContext(ctx, &links, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// GetAdminLink returns a single link with owner display names and tags joined.
// Governing: SPEC-0011 REQ "Admin Inline Link Editing"
func (s *LinkStore) GetAdminLink(ctx context.Context, id string) (*AdminLink, error) {
	var link AdminLink
	err := s.db.GetContext(ctx, &link, s.q(fmt.Sprintf(`
		SELECT l.*,
			%s AS owners,
			%s AS tags
		FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id
		LEFT JOIN users u ON u.id = lo.user_id
		LEFT JOIN link_tags lt ON lt.link_id = l.id
		LEFT JOIN tags t ON t.id = lt.tag_id
		WHERE l.id = ?
		GROUP BY l.id`,
		s.aggDistinct("u.display_name"),
		s.aggDistinct("t.name"),
	)), id)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &link, nil
}

// ListByTag returns all links that have the given tag slug.
func (s *LinkStore) ListByTag(ctx context.Context, tagSlug string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_tags lt ON lt.link_id = l.id
		INNER JOIN tags t ON t.id = lt.tag_id
		WHERE t.slug = ?
		ORDER BY l.slug ASC
	`), tagSlug)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListVisibleByTag returns links with the given tag slug that userID may see:
// public links, links they own or co-own, and links shared with them.
// Pass an empty userID for anonymous viewers (public links only). Admins see
// everything and should use ListByTag instead.
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
func (s *LinkStore) ListVisibleByTag(ctx context.Context, tagSlug, userID string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT DISTINCT l.* FROM links l
		INNER JOIN link_tags lt ON lt.link_id = l.id
		INNER JOIN tags t ON t.id = lt.tag_id
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE t.slug = ? AND (l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)
		ORDER BY l.slug ASC
	`), userID, userID, tagSlug)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// ListPublic returns paginated public links, optionally filtered by search query.
// Returns the matching links and total count for pagination.
// Governing: SPEC-0012 REQ "Public Link Browser (GET /links)", REQ "Public Link Search"
// ListPublic returns paginated public links as AdminLink rows, optionally filtered by query.
// currentUserID is used to set IsOwner; pass "" for unauthenticated callers.
// Governing: SPEC-0012 REQ "Public Link Browser (GET /links)", REQ "Public Link Search"
func (s *LinkStore) ListPublic(ctx context.Context, currentUserID, q string, page, perPage int) ([]*AdminLink, int, error) {
	baseWhere := `WHERE l.visibility = 'public'`
	var args []interface{}
	if q != "" {
		// LIKE metacharacters escaped — see SearchByOwner (issue #265).
		pattern := "%" + escapeLike(q) + "%"
		// LOWER(col) LIKE LOWER(?) — see SearchByOwner for the PostgreSQL rationale.
		baseWhere += ` AND (LOWER(l.slug) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.url) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.title) LIKE LOWER(?) ESCAPE '!' OR LOWER(l.description) LIKE LOWER(?) ESCAPE '!')`
		args = append(args, pattern, pattern, pattern, pattern)
	}

	// Count total matching rows.
	countQuery := `SELECT COUNT(DISTINCT l.id) FROM links l ` + baseWhere
	var total int
	if err := s.db.GetContext(ctx, &total, s.q(countQuery), args...); err != nil {
		return nil, 0, err
	}

	// Fetch paginated results.
	// COALESCE(MAX(u.*), '') is used instead of bare u.* so that PostgreSQL's
	// strict GROUP BY is satisfied; the JOIN on is_primary=1 ensures at most
	// one owner row per link, so MAX() returns the same value as the bare column.
	offset := (page - 1) * perPage
	query := fmt.Sprintf(`
		SELECT l.*,
		       COALESCE(MAX(u.display_name), '') AS owners,
		       COALESCE(MAX(u.display_name_slug), '') AS owner_slug,
		       %s AS tags,
		       CASE WHEN EXISTS(
		           SELECT 1 FROM link_owners lo2 WHERE lo2.link_id = l.id AND lo2.user_id = ?
		       ) THEN 1 ELSE 0 END AS is_owner
		FROM links l
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.is_primary = 1
		LEFT JOIN users u ON u.id = lo.user_id
		LEFT JOIN link_tags lt ON lt.link_id = l.id
		LEFT JOIN tags t ON t.id = lt.tag_id
		`+baseWhere+`
		GROUP BY l.id
		ORDER BY l.created_at DESC
		LIMIT ? OFFSET ?`,
		s.aggDistinct("t.name"),
	)
	fetchArgs := append([]interface{}{currentUserID}, args...)
	fetchArgs = append(fetchArgs, perPage, offset)

	var links []*AdminLink
	if err := s.db.SelectContext(ctx, &links, s.q(query), fetchArgs...); err != nil {
		return nil, 0, err
	}
	return links, total, nil
}

// ListSharedWithUser returns links shared with the given user via link_shares.
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
func (s *LinkStore) ListSharedWithUser(ctx context.Context, userID string) ([]*Link, error) {
	var links []*Link
	err := s.db.SelectContext(ctx, &links, s.q(`
		SELECT l.* FROM links l
		INNER JOIN link_shares ls ON ls.link_id = l.id
		WHERE ls.user_id = ?
		ORDER BY l.slug ASC
	`), userID)
	if err != nil {
		return nil, err
	}
	return links, nil
}

// HasShare checks if user has a link_shares record.
// Governing: SPEC-0010 REQ "Link Shares Table"
func (s *LinkStore) HasShare(ctx context.Context, linkID, userID string) (bool, error) {
	var count int
	err := s.db.GetContext(ctx, &count,
		s.q(`SELECT COUNT(*) FROM link_shares WHERE link_id = ? AND user_id = ?`), linkID, userID)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// AddShare creates a link_shares record.
// Governing: SPEC-0010 REQ "Link Shares Table"
func (s *LinkStore) AddShare(ctx context.Context, linkID, userID, sharedBy string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO link_shares (link_id, user_id, shared_by) VALUES (?, ?, ?)
	`), linkID, userID, sharedBy)
	return err
}

// RemoveShare deletes a link_shares record.
// Governing: SPEC-0010 REQ "Link Shares Table"
func (s *LinkStore) RemoveShare(ctx context.Context, linkID, userID string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM link_shares WHERE link_id = ? AND user_id = ?
	`), linkID, userID)
	return err
}

// ListPublicByOwner returns public links owned by userID as AdminLink rows
// (owner display name, owner slug, tags) so the shared link_list partial can
// render them identically to the public link browser. Paginated.
// currentUserID is used to set IsOwner; pass "" for unauthenticated callers.
// Returns the links, total count, and any error.
// Governing: SPEC-0012 REQ "User Profile Page (GET /u/{display_name_slug})"
// Governing: SPEC-0014 REQ "Abstract Link Widget" — same shape as ListPublic
func (s *LinkStore) ListPublicByOwner(ctx context.Context, userID, currentUserID string, page, perPage int) ([]*AdminLink, int, error) {
	// Count total matching links
	var total int
	err := s.db.GetContext(ctx, &total, s.q(`
		SELECT COUNT(DISTINCT l.id) FROM links l
		JOIN link_owners lo ON lo.link_id = l.id AND lo.is_primary = 1
		WHERE l.visibility = 'public' AND lo.user_id = ?
	`), userID)
	if err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * perPage
	var links []*AdminLink
	err = s.db.SelectContext(ctx, &links, s.q(fmt.Sprintf(`
		SELECT l.*,
		       COALESCE(MAX(u.display_name), '') AS owners,
		       COALESCE(MAX(u.display_name_slug), '') AS owner_slug,
		       %s AS tags,
		       CASE WHEN EXISTS(
		           SELECT 1 FROM link_owners lo2 WHERE lo2.link_id = l.id AND lo2.user_id = ?
		       ) THEN 1 ELSE 0 END AS is_owner
		FROM links l
		JOIN link_owners lo ON lo.link_id = l.id AND lo.is_primary = 1
		JOIN users u ON u.id = lo.user_id
		LEFT JOIN link_tags lt ON lt.link_id = l.id
		LEFT JOIN tags t ON t.id = lt.tag_id
		WHERE l.visibility = 'public'
		  AND lo.user_id = ?
		GROUP BY l.id
		ORDER BY l.created_at DESC
		LIMIT ? OFFSET ?
	`, s.aggDistinct("t.name"))), currentUserID, userID, perPage, offset)
	if err != nil {
		return nil, 0, err
	}

	return links, total, nil
}

// CountAll returns the total number of links.
// Governing: SPEC-0016 REQ "Prometheus Metrics Endpoint", ADR-0016
func (s *LinkStore) CountAll(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.GetContext(ctx, &count, s.q(`SELECT COUNT(*) FROM links`))
	return count, err
}

// ListShares returns all users with access to a link.
// Governing: SPEC-0010 REQ "Link Shares Table"
func (s *LinkStore) ListShares(ctx context.Context, linkID string) ([]ShareRecord, error) {
	var shares []ShareRecord
	err := s.db.SelectContext(ctx, &shares, s.q(`
		SELECT * FROM link_shares WHERE link_id = ? ORDER BY created_at ASC
	`), linkID)
	if err != nil {
		return nil, err
	}
	return shares, nil
}

// Suggest endpoint bounds.
// Governing: SPEC-0019 REQ "Suggest Endpoint" — result count default 5,
// maximum 10; q truncated to 64 characters.
const (
	DefaultSuggestLimit = 5
	MaxSuggestLimit     = 10
	MaxSuggestQueryLen  = 64

	// suggestCandidateCap bounds the SQL candidate fetch (ADR-0019: the
	// suggest query is "capped at a small candidate limit"). It exceeds
	// MaxSuggestLimit so the deterministic in-Go re-ranking always has the
	// full best-band candidate set to draw from: band is an integer, so the
	// SQL ORDER BY band cut is exact — a cut row never has a better band
	// than a kept row. Within the single band straddling the cap the SQL
	// slug tiebreak follows the driver's collation, which matches Go byte
	// order on sqlite (BINARY) and C-locale postgres but can diverge on
	// mysql utf8mb4 / ICU collations (mostly around '-' in slugs); on those
	// drivers a band with more than suggestCandidateCap matches may cut a
	// slug that byte-orders into the final top-limit. Accepted: it takes
	// >50 same-band matches for one query, and the result is still a
	// valid, visibility-filtered suggestion set — only the tiebreak drifts.
	suggestCandidateCap = 50
)

// Shared SQL fragments for SPEC-0019 discovery surfaces. Every discovery
// query (the suggest endpoint today, the did-you-mean 404 candidate fetch
// next) composes the same two predicates so visibility filtering cannot
// diverge per surface (ADR-0019: one authorization code path).
const (
	// linkViewerJoins attaches the viewer's ownership and share rows to links
	// aliased as l. Bind viewerID twice.
	// Governing: SPEC-0010 — the same joins used by ListVisibleByTag/ListByURL
	linkViewerJoins = `
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?`

	// linkVisibleWhere is the SPEC-0010 visibility predicate for non-admin
	// viewers: public links plus links the viewer owns/co-owns plus links
	// shared with the viewer. Requires linkViewerJoins.
	// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
	linkVisibleWhere = `(l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)`

	// linkActiveWhere is the SPEC-0020 lifecycle filter: expired and archived
	// links must never be offered by a discovery surface — suggesting a link
	// that will not resolve is worse than no suggestion. Bind now (UTC).
	// Governing: SPEC-0019 REQ "Suggest Endpoint", SPEC-0020
	linkActiveWhere = `l.archived_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > ?)`
)

// didYouMeanLengthWindow bounds the did-you-mean candidate fetch in SQL: a
// slug whose length differs from the requested slug by more than 2 cannot be
// within Levenshtein distance 2 of it (each insertion/deletion changes length
// by exactly 1), so the window is a necessary condition for the handler's
// distance bound and excludes the bulk of the corpus before any distance is
// computed (ADR-0019: bounded per-404 work, nothing cached).
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019
const didYouMeanLengthWindow = 2

// DidYouMeanCandidates returns the candidate slugs for the 404 page's
// did-you-mean block, filtered here in the shared store layer so discovery
// surfaces cannot diverge on authorization (ADR-0019: one authorization code
// path). Discoverability, not resolvability, is the governing test
// (SPEC-0010): anonymous viewers — empty viewerID, which joins to no
// ownership or share rows — are offered public slugs only, never private or
// secure ones even though a private link would resolve if the exact slug were
// known; authenticated non-admins additionally get their own/co-owned links
// and links shared with them; admins get every slug. Expired and archived
// links are excluded for all callers (SPEC-0020). Candidates are
// length-bounded in SQL to ±2 of q; the handler computes the actual
// Levenshtein distances, ordering, and result cap in Go (ADR-0019). LENGTH()
// is portable across sqlite/mysql/postgres here because valid slugs are
// ASCII (SPEC-0002), so byte and character semantics agree.
// Governing: SPEC-0019 REQ "Did-You-Mean 404 Suggestions", ADR-0019, ADR-0002
func (s *LinkStore) DidYouMeanCandidates(ctx context.Context, viewerID string, isAdmin bool, q string) ([]string, error) {
	n := len([]rune(q))
	if n == 0 {
		return []string{}, nil
	}
	minLen, maxLen := n-didYouMeanLengthWindow, n+didYouMeanLengthWindow
	now := time.Now().UTC()

	var candidates []string
	var err error
	if isAdmin {
		// Governing: SPEC-0010 REQ "Admin Visibility Override" — admins may be
		// offered any slug; the lifecycle filter still applies.
		err = s.db.SelectContext(ctx, &candidates, s.q(`
			SELECT l.slug
			FROM links l
			WHERE `+linkActiveWhere+`
			  AND LENGTH(l.slug) BETWEEN ? AND ?
			ORDER BY l.slug ASC
		`), now, minLen, maxLen)
	} else {
		err = s.db.SelectContext(ctx, &candidates, s.q(`
			SELECT DISTINCT l.slug
			FROM links l
			`+linkViewerJoins+`
			WHERE `+linkVisibleWhere+`
			  AND `+linkActiveWhere+`
			  AND LENGTH(l.slug) BETWEEN ? AND ?
			ORDER BY l.slug ASC
		`), viewerID, viewerID, now, minLen, maxLen)
	}
	if err != nil {
		return nil, err
	}
	if candidates == nil {
		candidates = []string{}
	}
	return candidates, nil
}

// suggestRow is a Link plus its ranking band, computed in SQL so the
// candidate cap can never cut a better-band row in favor of a worse one.
type suggestRow struct {
	Link
	Band int `db:"band"`
}

// SuggestLinks returns up to limit links matching q for autocomplete,
// visibility-filtered for the viewer (SPEC-0010: public + owned/co-owned +
// shared; admins see all) and restricted to active links (SPEC-0020: not
// expired, not archived). Matching uses portable LIKE only, with wildcards in
// q escaped so user input matches literally; q is lowercased and truncated to
// MaxSuggestQueryLen here — not in handlers — so every caller gets identical
// matching semantics. Results are ranked slug-prefix first, then
// slug-substring, then title/description matches, ties within each band
// broken by slug ascending in byte order.
// Governing: SPEC-0019 REQ "Suggest Endpoint", ADR-0019, ADR-0002
func (s *LinkStore) SuggestLinks(ctx context.Context, viewerID string, isAdmin bool, q string, limit int) ([]*Link, error) {
	// Governing: SPEC-0019 REQ "Suggest Endpoint" — the server MUST lowercase
	// q before matching (postgres LIKE is case-sensitive; the slug corpus is
	// canonically lowercase per SPEC-0002), and q longer than 64 characters
	// MUST be truncated to 64.
	q = strings.ToLower(q)
	if r := []rune(q); len(r) > MaxSuggestQueryLen {
		q = string(r[:MaxSuggestQueryLen])
	}
	// An empty q returns an empty suggestions set, never the whole corpus.
	if q == "" {
		return []*Link{}, nil
	}
	if limit <= 0 {
		limit = DefaultSuggestLimit
	}
	if limit > MaxSuggestLimit {
		limit = MaxSuggestLimit
	}

	// LIKE metacharacters in q are escaped so % and _ match literally, not as
	// wildcards — see escapeLike (issue #265).
	// Governing: SPEC-0019 REQ "Suggest Endpoint" scenario "LIKE Wildcards Neutralized"
	prefixPat := escapeLike(q) + "%"
	subPat := "%" + escapeLike(q) + "%"
	now := time.Now().UTC()

	// Band 0 = slug prefix, 1 = slug substring, 2 = title/description match.
	// Slugs are canonically lowercase so the slug arms need no LOWER();
	// title/description matching is case-insensitive via LOWER(col) against
	// the already-lowercased pattern (see SearchByOwner for the rationale).
	// Caveat shared with SearchByOwner/ListPublic: sqlite's LOWER folds only
	// ASCII, so a non-ASCII uppercase title (e.g. "Über") matches
	// case-insensitively on postgres/mysql but not sqlite.
	const bandCase = `CASE WHEN l.slug LIKE ? ESCAPE '!' THEN 0
		WHEN l.slug LIKE ? ESCAPE '!' THEN 1 ELSE 2 END`
	const matchWhere = `(l.slug LIKE ? ESCAPE '!'
		OR LOWER(l.title) LIKE ? ESCAPE '!'
		OR LOWER(l.description) LIKE ? ESCAPE '!')`

	var rows []*suggestRow
	var err error
	if isAdmin {
		// Governing: SPEC-0010 REQ "Admin Visibility Override" — admins see all
		// links; the lifecycle filter still applies.
		err = s.db.SelectContext(ctx, &rows, s.q(`
			SELECT l.*, `+bandCase+` AS band
			FROM links l
			WHERE `+linkActiveWhere+`
			  AND `+matchWhere+`
			ORDER BY band ASC, l.slug ASC
			LIMIT ?
		`), prefixPat, subPat, now, subPat, subPat, subPat, suggestCandidateCap)
	} else {
		err = s.db.SelectContext(ctx, &rows, s.q(`
			SELECT DISTINCT l.*, `+bandCase+` AS band
			FROM links l
			`+linkViewerJoins+`
			WHERE `+linkVisibleWhere+`
			  AND `+linkActiveWhere+`
			  AND `+matchWhere+`
			ORDER BY band ASC, l.slug ASC
			LIMIT ?
		`), prefixPat, subPat, viewerID, viewerID, now, subPat, subPat, subPat, suggestCandidateCap)
	}
	if err != nil {
		return nil, err
	}

	// Final ranking in Go (ADR-0019): band precedence, then slug ascending in
	// byte order — Go string comparison is byte order on every platform,
	// whereas SQL collations differ per driver.
	// Governing: SPEC-0019 REQ "Suggest Endpoint" scenario "Prefix Match Ranks First"
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Band != rows[j].Band {
			return rows[i].Band < rows[j].Band
		}
		return rows[i].Slug < rows[j].Slug
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]*Link, len(rows))
	for i, row := range rows {
		l := row.Link
		out[i] = &l
	}
	return out, nil
}
