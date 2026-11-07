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

	// Side names the tree a claim was read from, for the checks that compare
	// two of them: "main" for a record the branch never saw. It stays empty
	// when the claim came from the working tree, so a collision inside one tree
	// reads exactly as it always did.
	Side string

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

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, filepath.Join(dir, entry.Name()))
	}
	return ClaimsFromPaths(names, os.ReadFile)
}

// ClaimsFromPaths builds claims from a list of record paths, the form a git
// tree listing hands over, reading each file through read. That leaves the
// caller in charge of where contents come from: the working tree for a branch,
// `git show <ref>:<path>` for the revision it is compared against. Files whose
// names carry no number claim nothing, exactly as in Scan.
func ClaimsFromPaths(paths []string, read func(path string) ([]byte, error)) ([]Claim, error) {
	if read == nil {
		read = os.ReadFile
	}
	claims := make([]Claim, 0, len(paths))
	for _, path := range paths {
		match := adrFileName.FindStringSubmatch(filepath.Base(filepath.ToSlash(path)))
		if match == nil {
			continue
		}
		claims = append(claims, Claim{
			Path:   path,
			Number: normalize(match[1]),
			Digits: match[1],
			Title:  title(read, path, match[2]),
		})
	}

	sort.Slice(claims, func(i, j int) bool { return claims[i].Path < claims[j].Path })
	return claims, nil
}

// MergedPaths returns the record paths a merge of a branch into its base would
// leave behind: everything either side holds, minus what the branch deleted
// since the two diverged.
//
// The divergence point is what makes the answer honest. Without it a record
// added on the base after the branch was cut looks like one the branch
// deleted, and that is precisely the collision this gate exists to catch: two
// lanes that never saw each other's file. With it, a branch that renames a
// record on purpose is still just a rename.
func MergedPaths(divergedPaths, basePaths, branchPaths []string) []string {
	held := make(map[string]bool, len(divergedPaths))
	for _, path := range divergedPaths {
		held[path] = true
	}
	for _, path := range branchPaths {
		delete(held, path)
	}

	merged := make(map[string]bool, len(basePaths)+len(branchPaths))
	for _, path := range basePaths {
		merged[path] = true
	}
	for _, path := range branchPaths {
		merged[path] = true
	}
	for path := range held {
		delete(merged, path)
	}

	paths := make([]string, 0, len(merged))
	for path := range merged {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// title reads a record's H1 title, falling back to the file name's slug when
// the record has no heading a reader would recognize.
func title(read func(path string) ([]byte, error), path, slug string) string {
	data, err := read(path)
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
// number more than one file claims. The same path listed twice counts once:
// comparing two trees hands over one record twice when the branch never
// touched it, and that is one decision, not two.
func Collisions(claims []Claim) []Collision {
	groups := map[claimKey]map[string]Claim{}
	for _, claim := range claims {
		k := claimKey{dir: filepath.Dir(claim.Path), number: claim.Number}
		if groups[k] == nil {
			groups[k] = map[string]Claim{}
		}
		// A claim read from the working tree wins over one read from another
		// revision, so the message quotes the copy the reader can open. Only
		// a claim carrying a side can be replaced, and only by one that
		// carries none.
		if seen, ok := groups[k][claim.Path]; ok && seen.Side == "" {
			continue
		}
		groups[k][claim.Path] = claim
	}

	var out []Collision
	for k, byPath := range groups {
		if len(byPath) < 2 {
			continue
		}
		group := make([]Claim, 0, len(byPath))
		for _, claim := range byPath {
			group = append(group, claim)
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
// every file claiming it, and which decision each of them records. When the
// claims came from different trees the message says which side each record is
// on, because "mine is on main" is the fact a lane needs in order to fix it.
func (c Collision) Message() string {
	paths := make([]string, len(c.Claims))
	for i, claim := range c.Claims {
		paths[i] = claim.display()
	}
	if !c.TitlesDiffer() {
		return fmt.Sprintf("ADR-%s is claimed by %d files in %s: %s",
			c.Number, len(c.Claims), c.Dir, strings.Join(paths, ", "))
	}
	titles := make([]string, len(c.Claims))
	for i, claim := range c.Claims {
		titles[i] = fmt.Sprintf("%s records %q", claim.display(), claim.Title)
	}
	return fmt.Sprintf("ADR-%s is claimed by %d different decisions in %s: %s",
		c.Number, len(c.Claims), c.Dir, strings.Join(titles, "; "))
}

// display is the path plus the tree it was read from, when that differs from
// the tree the check runs in.
func (c Claim) display() string {
	if c.Side == "" {
		return c.Path
	}
	return c.Path + " (on " + c.Side + ")"
}
