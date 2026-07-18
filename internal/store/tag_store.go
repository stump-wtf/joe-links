// Governing: SPEC-0002 REQ "Link Store Interface", ADR-0005
package store

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

var tagSlugStripRe = regexp.MustCompile(`[^a-z0-9-]`)

// Tag represents a row in the tags table.
type Tag struct {
	ID        string    `db:"id"`
	Name      string    `db:"name"`
	Slug      string    `db:"slug"`
	CreatedAt time.Time `db:"created_at"`
}

// TagStore is the sqlx-backed tag data access layer.
// Governing: SPEC-0002 REQ "Link Store Interface", ADR-0005
type TagStore struct {
	db *sqlx.DB
}

func NewTagStore(db *sqlx.DB) *TagStore {
	return &TagStore{db: db}
}

// q rebinds ? placeholders to the driver's native format ($1,$2,... for PostgreSQL).
func (s *TagStore) q(query string) string { return s.db.Rebind(query) }

// DeriveTagSlug derives a URL-safe slug from a tag name:
// lowercase, replace spaces/underscores with hyphens, strip non-[a-z0-9-].
// Governing: ADR-0005 (tag slug derivation)
func DeriveTagSlug(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	s = tagSlugStripRe.ReplaceAllString(s, "")
	// Collapse consecutive hyphens.
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	return s
}

// Upsert creates a tag if it doesn't exist (by slug), or returns the existing one.
func (s *TagStore) Upsert(ctx context.Context, name string) (*Tag, error) {
	// Belt-and-braces: handlers validate first, but the upsert pair is the
	// single choke point every tag write funnels through — a future direct
	// caller must not be able to persist a hostile display name (issue #251).
	if err := ValidateTagName(name); err != nil {
		return nil, err
	}
	slug := DeriveTagSlug(name)

	var existing Tag
	err := s.db.GetContext(ctx, &existing, s.q(`SELECT * FROM tags WHERE slug = ?`), slug)
	if err == nil {
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	id := uuid.New().String()
	now := time.Now().UTC()
	_, err = s.db.ExecContext(ctx, s.q(`
		INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)
	`), id, strings.TrimSpace(name), slug, now)
	if err != nil {
		// Race condition: another goroutine inserted first. Re-fetch.
		if isUniqueConstraintError(err) {
			err = s.db.GetContext(ctx, &existing, s.q(`SELECT * FROM tags WHERE slug = ?`), slug)
			if err != nil {
				return nil, err
			}
			return &existing, nil
		}
		return nil, err
	}

	return &Tag{ID: id, Name: strings.TrimSpace(name), Slug: slug, CreatedAt: now}, nil
}

// upsertTx is the transactional variant used by LinkStore.SetTags.
func (s *TagStore) upsertTx(ctx context.Context, tx *sqlx.Tx, name string) (*Tag, error) {
	// Same intake backstop as Upsert (issue #251).
	if err := ValidateTagName(name); err != nil {
		return nil, err
	}
	slug := DeriveTagSlug(name)

	var existing Tag
	err := tx.GetContext(ctx, &existing, tx.Rebind(`SELECT * FROM tags WHERE slug = ?`), slug)
	if err == nil {
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}

	id := uuid.New().String()
	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, tx.Rebind(`
		INSERT INTO tags (id, name, slug, created_at) VALUES (?, ?, ?, ?)
	`), id, strings.TrimSpace(name), slug, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			err = tx.GetContext(ctx, &existing, tx.Rebind(`SELECT * FROM tags WHERE slug = ?`), slug)
			if err != nil {
				return nil, err
			}
			return &existing, nil
		}
		return nil, err
	}

	return &Tag{ID: id, Name: strings.TrimSpace(name), Slug: slug, CreatedAt: now}, nil
}

// SearchByPrefix returns tags whose name starts with the given prefix. Case
// sensitivity of the match follows the driver's LIKE semantics: ASCII
// case-insensitive on sqlite3 and mysql (default collations), case-sensitive
// on postgres. LIKE metacharacters in the prefix are escaped so user input
// matches literally (issue #265).
// Governing: SPEC-0004 REQ "New Link Form" — tag autocomplete
func (s *TagStore) SearchByPrefix(ctx context.Context, prefix string) ([]*Tag, error) {
	var tags []*Tag
	pattern := escapeLike(prefix) + "%"
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT * FROM tags WHERE name LIKE ? ESCAPE '!' ORDER BY name ASC LIMIT 10
	`), pattern)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// SearchByPrefixVisible returns tags whose name starts with the given prefix
// and that have at least one link visible to userID: public links, links
// they own or co-own, or links shared with them. Case sensitivity of the
// prefix match follows the driver's LIKE semantics (see SearchByPrefix). Tags
// whose links are all invisible to the viewer are omitted, as are tags with
// no links at all. Pass an empty userID for anonymous viewers (public links
// only). Admins see everything and should use SearchByPrefix instead.
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
// Governing: SPEC-0004 REQ "New Link Form" — tag autocomplete
func (s *TagStore) SearchByPrefixVisible(ctx context.Context, prefix, userID string) ([]*Tag, error) {
	var tags []*Tag
	pattern := escapeLike(prefix) + "%"
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT DISTINCT t.* FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		INNER JOIN links l ON l.id = lt.link_id
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE t.name LIKE ? ESCAPE '!'
		  AND (l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)
		ORDER BY t.name ASC LIMIT 10
	`), userID, userID, pattern)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// GetBySlug returns the tag matching slug, or ErrNotFound.
func (s *TagStore) GetBySlug(ctx context.Context, slug string) (*Tag, error) {
	var t Tag
	err := s.db.GetContext(ctx, &t, s.q(`SELECT * FROM tags WHERE slug = ?`), slug)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TagWithCount is a Tag augmented with the number of links using it.
// Governing: SPEC-0004 REQ "Tag Browser" — zero-count tags MUST NOT appear
type TagWithCount struct {
	Tag
	Count int `db:"link_count"`
}

// ListWithCounts returns all tags with ≥1 link, annotated with their link count.
// Governing: SPEC-0004 REQ "Tag Browser"
func (s *TagStore) ListWithCounts(ctx context.Context) ([]*TagWithCount, error) {
	var tags []*TagWithCount
	err := s.db.SelectContext(ctx, &tags, `
		SELECT t.*, COUNT(lt.link_id) as link_count
		FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		GROUP BY t.id
		HAVING COUNT(lt.link_id) >= 1
		ORDER BY t.name ASC
	`)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// ListWithCountsVisible returns tags with ≥1 link visible to userID, with
// counts restricted to visible links: public, owned/co-owned, or shared with
// the user. Tags whose links are all invisible to the viewer are omitted.
// Pass an empty userID for anonymous viewers (public links only). Admins see
// everything and should use ListWithCounts instead.
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
// Governing: SPEC-0004 REQ "Tag Browser" — zero-count tags MUST NOT appear
func (s *TagStore) ListWithCountsVisible(ctx context.Context, userID string) ([]*TagWithCount, error) {
	var tags []*TagWithCount
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT t.*, COUNT(DISTINCT lt.link_id) as link_count
		FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		INNER JOIN links l ON l.id = lt.link_id
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL
		GROUP BY t.id
		HAVING COUNT(DISTINCT lt.link_id) >= 1
		ORDER BY t.name ASC
	`), userID, userID)
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// ListWithCountsPaginated returns tags with ≥1 link, annotated with their link
// count, ordered by (name, id) and keyset-paginated. Pass cursorName/cursorID
// from the last row of the previous page (empty for the first page).
// Governing: SPEC-0005 REQ "Pagination", SPEC-0004 REQ "Tag Browser"
func (s *TagStore) ListWithCountsPaginated(ctx context.Context, limit int, cursorName, cursorID string) ([]*TagWithCount, error) {
	var tags []*TagWithCount
	if cursorName == "" && cursorID == "" {
		err := s.db.SelectContext(ctx, &tags, s.q(`
			SELECT t.*, COUNT(lt.link_id) as link_count
			FROM tags t
			INNER JOIN link_tags lt ON lt.tag_id = t.id
			GROUP BY t.id
			HAVING COUNT(lt.link_id) >= 1
			ORDER BY t.name ASC, t.id ASC
			LIMIT ?
		`), limit)
		return tags, err
	}
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT t.*, COUNT(lt.link_id) as link_count
		FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		WHERE t.name > ? OR (t.name = ? AND t.id > ?)
		GROUP BY t.id
		HAVING COUNT(lt.link_id) >= 1
		ORDER BY t.name ASC, t.id ASC
		LIMIT ?
	`), cursorName, cursorName, cursorID, limit)
	return tags, err
}

// ListWithCountsVisiblePaginated returns tags with ≥1 link visible to userID,
// with counts restricted to visible links (public, owned/co-owned, or shared
// with the user), ordered by (name, id) and keyset-paginated. Tags whose links
// are all invisible to the viewer are omitted. Pass cursorName/cursorID from
// the last row of the previous page (empty for the first page). Admins see
// everything and should use ListWithCountsPaginated instead.
// Governing: SPEC-0010 REQ "Dashboard Visibility Filtering"
// Governing: SPEC-0005 REQ "Pagination", SPEC-0004 REQ "Tag Browser"
func (s *TagStore) ListWithCountsVisiblePaginated(ctx context.Context, userID string, limit int, cursorName, cursorID string) ([]*TagWithCount, error) {
	var tags []*TagWithCount
	if cursorName == "" && cursorID == "" {
		err := s.db.SelectContext(ctx, &tags, s.q(`
			SELECT t.*, COUNT(DISTINCT lt.link_id) as link_count
			FROM tags t
			INNER JOIN link_tags lt ON lt.tag_id = t.id
			INNER JOIN links l ON l.id = lt.link_id
			LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
			LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
			WHERE l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL
			GROUP BY t.id
			HAVING COUNT(DISTINCT lt.link_id) >= 1
			ORDER BY t.name ASC, t.id ASC
			LIMIT ?
		`), userID, userID, limit)
		return tags, err
	}
	err := s.db.SelectContext(ctx, &tags, s.q(`
		SELECT t.*, COUNT(DISTINCT lt.link_id) as link_count
		FROM tags t
		INNER JOIN link_tags lt ON lt.tag_id = t.id
		INNER JOIN links l ON l.id = lt.link_id
		LEFT JOIN link_owners lo ON lo.link_id = l.id AND lo.user_id = ?
		LEFT JOIN link_shares ls ON ls.link_id = l.id AND ls.user_id = ?
		WHERE (l.visibility = 'public' OR lo.user_id IS NOT NULL OR ls.user_id IS NOT NULL)
		  AND (t.name > ? OR (t.name = ? AND t.id > ?))
		GROUP BY t.id
		HAVING COUNT(DISTINCT lt.link_id) >= 1
		ORDER BY t.name ASC, t.id ASC
		LIMIT ?
	`), userID, userID, cursorName, cursorName, cursorID, limit)
	return tags, err
}

// ListAll returns all tags ordered by name.
func (s *TagStore) ListAll(ctx context.Context) ([]*Tag, error) {
	var tags []*Tag
	err := s.db.SelectContext(ctx, &tags, `SELECT * FROM tags ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	return tags, nil
}
