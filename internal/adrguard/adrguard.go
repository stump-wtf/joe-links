// Package adrguard reports design records that claim the same ADR number.
//
// Both automation identities cut a branch off a moving main and take the next
// number that looks free there, so two lanes racing the same gap pick the same
// one. The two files have different paths, so the second merge is clean and
// nothing notices until someone writes "ratify ADR-0024" and means two
// different decisions. This package turns that into a gate failure that names
// both paths.
//
// Governing: ADR-0025. See issue #345.
package adrguard

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	// adrFileName is how a numbered design record is named:
	// ADR-0024-some-decision.md. The digits are kept as written so a failure
	// message can quote the file's own name.
	adrFileName = regexp.MustCompile(`(?i)^adr-([0-9]+)-(.+?)\.md$`)

	// adrHeading is the H1 a record opens with: "# ADR-0024: Some Decision".
	adrHeading = regexp.MustCompile(`(?i)^#\s+adr-[0-9]+\s*:\s*(\S.*?)\s*$`)
)

// Claim is one file's claim on an ADR number.
type Claim struct {
	// Path is the record's path as Scan was pointed at it.
	Path string

	// Number is the claimed number with leading zeros stripped, so ADR-24 and
	// ADR-0024 are one claim rather than two.
	Number string

	// Digits is the number as it is written in the file name.
	Digits string

	// Title is the record's H1 title, or the file name's slug when the file
	// carries no ADR heading.
	Title string
}

// Collision is one ADR number claimed by more than one file in one directory.
type Collision struct {
	// Dir scopes the collision: a design record and a record in another
	// directory may share a number.
	Dir string

	// Number is the claimed number, leading zeros stripped.
	Number string

	// Claims holds every file claiming it, sorted by path.
	Claims []Claim
}

// normalize strips the leading zeros that pad a sequence number, so the gate
// compares claims rather than file names.
func normalize(digits string) string {
	n := strings.TrimLeft(digits, "0")
	if n == "" {
		return "0"
	}
	return n
}

// claimKey scopes a number to the directory holding it.
type claimKey struct{ dir, number string }

// Scan reads a directory of numbered design records. Files whose names do not
// look like ADR-<digits>-<slug>.md claim nothing: an index, a README or a
// template stays out of the gate.
func Scan(dir string) ([]Claim, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	claims := make([]Claim, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := adrFileName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		claims = append(claims, Claim{
			Path:   path,
			Number: normalize(match[1]),
			Digits: match[1],
			Title:  title(path, match[2]),
		})
	}

	sort.Slice(claims, func(i, j int) bool { return claims[i].Path < claims[j].Path })
	return claims, nil
}

// title reads a record's H1 title, falling back to the file name's slug when
// the record has no heading a reader would recognize.
func title(path, slug string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return humanize(slug)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if match := adrHeading.FindStringSubmatch(line); match != nil {
			return match[1]
		}
	}
	return humanize(slug)
}

// humanize turns a kebab-case slug into the phrase a title would use.
func humanize(slug string) string {
	return strings.ReplaceAll(slug, "-", " ")
}

// Collisions groups the claims by directory and number and returns every
// number more than one file claims.
func Collisions(claims []Claim) []Collision {
	groups := map[claimKey][]Claim{}
	for _, claim := range claims {
		k := claimKey{dir: filepath.Dir(claim.Path), number: claim.Number}
		groups[k] = append(groups[k], claim)
	}

	var out []Collision
	for k, group := range groups {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].Path < group[j].Path })
		out = append(out, Collision{Dir: k.dir, Number: k.number, Claims: group})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir < out[j].Dir
		}
		return lessNumber(out[i].Number, out[j].Number)
	})
	return out
}

// lessNumber orders numbers by magnitude rather than by string, so ADR-9 sorts
// below ADR-10.
func lessNumber(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// TitlesDiffer reports whether the colliding records describe different
// decisions. That is the case a renumber cannot settle on its own: the number
// is ambiguous, and so is which decision it names.
func (c Collision) TitlesDiffer() bool {
	if len(c.Claims) < 2 {
		return false
	}
	for _, claim := range c.Claims[1:] {
		if !strings.EqualFold(strings.TrimSpace(claim.Title), strings.TrimSpace(c.Claims[0].Title)) {
			return true
		}
	}
	return false
}

// Message spells the collision out for a developer or a CI log: the number,
// every file claiming it, and which decision each of them records.
func (c Collision) Message() string {
	paths := make([]string, len(c.Claims))
	for i, claim := range c.Claims {
		paths[i] = claim.Path
	}
	if !c.TitlesDiffer() {
		return fmt.Sprintf("ADR-%s is claimed by %d files in %s: %s",
			c.Number, len(c.Claims), c.Dir, strings.Join(paths, ", "))
	}
	titles := make([]string, len(c.Claims))
	for i, claim := range c.Claims {
		titles[i] = fmt.Sprintf("%s records %q", claim.Path, claim.Title)
	}
	return fmt.Sprintf("ADR-%s is claimed by %d different decisions in %s: %s",
		c.Number, len(c.Claims), c.Dir, strings.Join(titles, "; "))
}
