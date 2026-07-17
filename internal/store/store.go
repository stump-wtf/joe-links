// Governing: SPEC-0002 REQ "Link Store Interface", ADR-0005
package store

import (
	"context"
	"errors"
)

var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("not found")

	// ErrDuplicateOwner is returned when attempting to add an owner that already exists.
	ErrDuplicateOwner = errors.New("user is already an owner of this link")
)

// NOTE: LinkStoreIface and TagStoreIface were deleted (issue #205). Nothing
// implemented or asserted them — handlers depend on the concrete *LinkStore /
// *TagStore — and their method sets had drifted from the implementations.
// Per YAGNI, reintroduce an interface only when a second implementation or a
// consumer-side seam actually needs one.

// KeywordStoreIface exposes keyword operations.
// Governing: ADR-0011 REQ "Keyword Host Discovery"
type KeywordStoreIface interface {
	List(ctx context.Context) ([]*Keyword, error)
	GetByID(ctx context.Context, id string) (*Keyword, error)
	GetByKeyword(ctx context.Context, keyword string) (*Keyword, error)
	Create(ctx context.Context, keyword, urlTemplate, description string) (*Keyword, error)
	Update(ctx context.Context, id, keyword, urlTemplate, description string) (*Keyword, error)
	Delete(ctx context.Context, id string) error
}
