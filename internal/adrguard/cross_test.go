// Fixture tests for the pre-merge case of the ADR number gate (issue #345): two
// branches that never saw each other's record can put one number on two files,
// and neither tree looks guilty on its own. The fixtures are path lists plus an
// in-memory reader, so the rule is exercised on both shapes without a repository.
package adrguard

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// treeFixture is a set of design records addressed by repo-relative path, the
// shape a git tree listing hands over.
type treeFixture map[string]string

func (f treeFixture) read(path string) ([]byte, error) {
	body, ok := f[filepath.ToSlash(path)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(body), nil
}

// record builds a record body in the repo's own shape for the fixture trees.
func record(number, title string) string {
	return "---\nstatus: proposed\n---\n\n# ADR-" + number + ": " + title + "\n"
}

func TestMergedPathsSeesTheRecordMainGainedAfterTheBranch(t *testing.T) {
	diverged := []string{"docs/adrs/ADR-0001-technology-stack.md"}
	base := []string{"docs/adrs/ADR-0001-technology-stack.md", "docs/adrs/ADR-0002-optional-oidc-local-admin.md"}
	branch := []string{"docs/adrs/ADR-0001-technology-stack.md", "docs/adrs/ADR-0002-keyword-matching-case-folding.md"}

	got := MergedPaths(diverged, base, branch)
	for _, want := range []string{"docs/adrs/ADR-0002-optional-oidc-local-admin.md", "docs/adrs/ADR-0002-keyword-matching-case-folding.md"} {
		if !slices.Contains(got, want) {
			t.Errorf("a merge would leave %s behind, so it must be visible to the gate; got %v", want, got)
		}
	}
}

func TestMergedPathsTreatsARenumberAsAMove(t *testing.T) {
	// A lane renaming its own record to a free number deletes the old path, and
	// the merge deletes it too: the gate must not report the record it moved.
	diverged := []string{"docs/adrs/ADR-0002-stale-name.md"}
	base := []string{"docs/adrs/ADR-0002-stale-name.md"}
	branch := []string{"docs/adrs/ADR-0009-api-token-authentication.md"}

	got := MergedPaths(diverged, base, branch)
	if len(got) != 1 || got[0] != "docs/adrs/ADR-0009-api-token-authentication.md" {
		t.Fatalf("a moved record must leave only its new path, got %v", got)
	}
}

func TestCrossTreeCollisionFailsOnOneNumberInTwoBranches(t *testing.T) {
	mainRecords := treeFixture{
		"docs/adrs/ADR-0001-technology-stack.md":          record("0001", "Technology Stack"),
		"docs/adrs/ADR-0002-optional-oidc-local-admin.md": record("0002", "Optional OIDC With A Local Admin Login"),
	}
	branchRecords := treeFixture{
		"docs/adrs/ADR-0001-technology-stack.md":              record("0001", "Technology Stack"),
		"docs/adrs/ADR-0002-keyword-matching-case-folding.md": record("0002", "Keyword Matching Case Folding"),
	}

	branchClaims, err := ClaimsFromPaths(keysOf(branchRecords), branchRecords.read)
	if err != nil {
		t.Fatalf("reading the branch side: %v", err)
	}

	// The record main gained after this branch was cut is the one the branch
	// cannot see, so the caller reads it from the base and says so.
	onMain, err := ClaimsFromPaths(
		[]string{"docs/adrs/ADR-0002-optional-oidc-local-admin.md"},
		mainRecords.read,
	)
	if err != nil {
		t.Fatalf("reading the base side: %v", err)
	}
	for i := range onMain {
		onMain[i].Side = "origin/main"
	}

	got := Collisions(append(branchClaims, onMain...))
	if len(got) != 1 {
		t.Fatalf("one number claimed on both sides must fail once merged, got %d: %+v", len(got), got)
	}

	message := got[0].Message()
	for _, want := range []string{
		"docs/adrs/ADR-0002-optional-oidc-local-admin.md",
		"docs/adrs/ADR-0002-keyword-matching-case-folding.md",
		"on origin/main",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message must name %s, got: %s", want, message)
		}
	}
	if !got[0].TitlesDiffer() {
		t.Error("one number naming two decisions must report different decisions")
	}
}

func TestCrossTreePassesOnDistinctNumbers(t *testing.T) {
	branchRecords := treeFixture{
		"docs/adrs/ADR-0001-technology-stack.md":                      record("0001", "Technology Stack"),
		"docs/adrs/ADR-0026-adr-number-collisions-across-branches.md": record("0026", "ADR Number Collisions Across Branches Fail CI"),
	}
	onMain, err := ClaimsFromPaths([]string{"docs/adrs/ADR-0025-adr-number-collision-gate.md"},
		treeFixture{"docs/adrs/ADR-0025-adr-number-collision-gate.md": record("0025", "ADR Number Collisions Fail The Gate")}.read)
	if err != nil {
		t.Fatalf("reading the base side: %v", err)
	}
	for i := range onMain {
		onMain[i].Side = "origin/main"
	}

	branchClaims, err := ClaimsFromPaths(keysOf(branchRecords), branchRecords.read)
	if err != nil {
		t.Fatalf("reading the branch side: %v", err)
	}
	if got := Collisions(append(branchClaims, onMain...)); len(got) != 0 {
		t.Fatalf("distinct numbers on the two sides must pass, got: %s", messages(got))
	}
}

func TestCollisionsCountsOneRecordListedTwiceOnce(t *testing.T) {
	// Comparing two trees hands the same record over twice whenever the branch
	// never touched it, and one file is one decision.
	records := treeFixture{"docs/adrs/ADR-0023-client-ip-trust-model.md": record("0023", "Client IP Trust Model")}
	claims, err := ClaimsFromPaths(keysOf(records), records.read)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	both := append(append([]Claim{}, claims...), Claim{
		Path: "docs/adrs/ADR-0023-client-ip-trust-model.md", Number: "23", Digits: "0023",
		Title: "Client IP Trust Model", Side: "origin/main",
	})
	if got := Collisions(both); len(got) != 0 {
		t.Fatalf("the same path on both sides is not a collision, got: %s", messages(got))
	}
}

func TestCollisionsKeepsTheWorkingTreeCopyForAnOnDiskRecord(t *testing.T) {
	// The same on-disk record can arrive once as a working-tree claim and once
	// read back from the base, in either order. The message quotes the copy
	// the reader can open, so a file this tree holds on disk must never be
	// reported as living on the base.
	records := treeFixture{
		"docs/adrs/ADR-0023-client-ip-trust-model.md": record("0023", "Client IP Trust Model"),
		"docs/adrs/ADR-0023-a-second-decision.md":     record("0023", "A Second Decision"),
	}
	working, err := ClaimsFromPaths(keysOf(records), records.read)
	if err != nil {
		t.Fatalf("reading the working tree: %v", err)
	}

	fromBase := []Claim{{
		Path: "docs/adrs/ADR-0023-client-ip-trust-model.md", Number: "23", Digits: "0023",
		Title: "Client IP Trust Model", Side: "origin/main",
	}}

	got := Collisions(append(append([]Claim{}, working...), fromBase...))
	if len(got) != 1 {
		t.Fatalf("one number on two records must collide, got: %s", messages(got))
	}
	if message := got[0].Message(); strings.Contains(message, "(on origin/main)") {
		t.Errorf("a record this tree holds on disk must not be reported as living on the base, got: %s", message)
	}
}

func keysOf(tree treeFixture) []string {
	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	return paths
}
