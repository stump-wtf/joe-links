// Command adr-check fails when one ADR number is claimed by more than one
// design record, which is what happens when two build lanes cut branches off a
// moving main and each takes the next number that looked free there. It runs as
// part of `make lint`; the same check runs inside `go test ./...` so the CI
// test job catches it too.
//
// With -base it checks what a merge would contain instead of only the tree it
// stands in, which is the case a lane cannot see from its own branch: the record
// that took the number is already on main, and this branch was cut before it
// landed. Governing: ADR-0025 and ADR-0026, issue #345.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joestump/joe-links/internal/adrguard"
)

func main() {
	dirs := flag.String("dirs", "docs/adrs", "comma-separated design-record directories to check")
	base := flag.String("base", "", "base ref to compare against, so a number main already claims is caught on a branch that never saw the record")
	flag.Parse()

	if err := run(*dirs, *base); err != nil {
		fmt.Fprintln(os.Stderr, "adr-check: "+err.Error())
		os.Exit(1)
	}
}

func run(list, base string) error {
	return runOn(os.Stderr, list, base)
}

// runOn is run with its output placed anywhere, so a test can read what a
// developer or a CI log would see.
func runOn(out io.Writer, list, base string) error {
	dirs := splitDirs(list)

	headClaims, err := workingTreeClaims(dirs)
	if err != nil {
		return err
	}
	claims := headClaims

	if base != "" {
		merged, err := mergedAgainstBase(base, dirs, headClaims)
		if err != nil {
			return fmt.Errorf("comparing against %s: %w", base, err)
		}
		claims = merged
	}

	collisions := adrguard.Collisions(claims)
	for _, collision := range collisions {
		fmt.Fprintln(out, collision.Message())
	}
	if len(collisions) > 0 {
		if base != "" {
			return fmt.Errorf("%d ADR number(s) claimed twice once this branch is merged into %s: rebase onto %s and give the later record the next number that is free there", len(collisions), base, base)
		}
		return fmt.Errorf("%d ADR number(s) claimed twice: give the later record the next number that is free on main and update its references", len(collisions))
	}
	return nil
}

// splitDirs turns the flag's list into paths, dropping the blanks a trailing
// comma leaves behind.
func splitDirs(list string) []string {
	var dirs []string
	for _, dir := range strings.Split(list, ",") {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

// workingTreeClaims reads the records as they stand in this checkout. A
// directory without design records is a repo with nothing to collide.
func workingTreeClaims(dirs []string) ([]adrguard.Claim, error) {
	var claims []adrguard.Claim
	for _, dir := range dirs {
		found, err := adrguard.Scan(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return nil, fmt.Errorf("scanning %s: %w", dir, err)
		}
		claims = append(claims, found...)
	}
	return claims, nil
}

// mergedAgainstBase returns the claims a merge of this branch into base would
// leave behind: this branch's own records plus every record base holds that the
// branch did not delete. That is the set two open pull requests can put one
// number on without either tree ever showing it, which is what issue #345 is
// about.
func mergedAgainstBase(base string, dirs []string, head []adrguard.Claim) ([]adrguard.Claim, error) {
	diverged, err := rev("merge-base", base, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("%w (fetch the base branch so the two sides have a commit to compare from)", err)
	}
	basePaths, err := treePaths(base, dirs)
	if err != nil {
		return nil, err
	}
	divergedPaths, err := treePaths(diverged, dirs)
	if err != nil {
		return nil, err
	}
	branchPaths, err := treePaths("HEAD", dirs)
	if err != nil {
		return nil, err
	}

	// Records added in the working tree but not yet committed belong to this
	// side too: a lane should be stopped before it commits, not after.
	onDisk := make(map[string]bool, len(head))
	for _, claim := range head {
		onDisk[filepath.ToSlash(claim.Path)] = true
	}
	branchPaths = union(branchPaths, keys(onDisk))

	visible := adrguard.MergedPaths(divergedPaths, basePaths, branchPaths)

	// A record the merge would leave behind can be missing from disk two ways:
	// it lives on the base side, or it is committed here and the working tree
	// has deleted it. Each is read from the tree that actually holds it, so
	// the message names the side that owns the record instead of guessing.
	onBranch := setOf(branchPaths)
	var baseOnly, committed []string
	for _, p := range visible {
		if onDisk[p] {
			continue
		}
		if onBranch[p] {
			committed = append(committed, p)
			continue
		}
		baseOnly = append(baseOnly, p)
	}

	claims := make([]adrguard.Claim, 0, len(visible))
	claims = append(claims, head...)

	fromBranch, err := adrguard.ClaimsFromPaths(committed, gitReader("HEAD"))
	if err != nil {
		return nil, err
	}
	for i := range fromBranch {
		fromBranch[i].Side = "HEAD"
	}

	fromBase, err := adrguard.ClaimsFromPaths(baseOnly, gitReader(base))
	if err != nil {
		return nil, err
	}
	for i := range fromBase {
		fromBase[i].Side = base
	}
	claims = append(claims, fromBranch...)
	return append(claims, fromBase...), nil
}

// union merges two path lists without repeating one.
func union(a, b []string) []string {
	set := make(map[string]bool, len(a)+len(b))
	for _, p := range a {
		set[p] = true
	}
	for _, p := range b {
		set[p] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func keys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func setOf(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
