// adr_test.go keeps ADR numbers unique in this repo. Both automation identities
// cut a branch off a moving main and take the next number that looks free, so
// two lanes racing the same gap add different records under one number — and
// because the paths differ, the second merge is clean and nothing notices
// until review asks someone to ratify a number that names two decisions. See
// issue #345 and ADR-0025.
package packaging_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joestump/joe-links/internal/adrguard"
)

func TestADRNumbersAreUnique(t *testing.T) {
	claims, err := adrguard.Scan(filepath.Join("..", "..", "docs", "adrs"))
	if err != nil {
		t.Fatalf("scanning docs/adrs: %v", err)
	}
	for _, collision := range adrguard.Collisions(claims) {
		t.Error(collision.Message() +
			"\n  one number must name one decision: give the later record the next number that is free on main")
	}
}

func TestLintChainRunsADRCollisionCheck(t *testing.T) {
	mk := readRepoFile(t, "Makefile")

	// The gate has to run on the entry point a lane runs before it pushes, not
	// only inside the test suite, so a collision is caught by `make lint` too.
	if !regexp.MustCompile(`(?m)^lint:.*\badr-check\b`).MatchString(mk) {
		t.Error("Makefile: the lint chain must run adr-check so the ADR collision gate runs on every `make lint` (issue #345)")
	}
}

func TestBaseComparisonIsWired(t *testing.T) {
	// A branch cannot see a record main gained after the branch was cut, so the
	// gate also compares the merge — and that comparison has to run somewhere a
	// lane cannot forget. Governing: ADR-0026, issue #345.
	mk := readRepoFile(t, "Makefile")
	if !regexp.MustCompile(`(?m)^adr-vs-base:$`).MatchString(mk) {
		t.Error("Makefile: adr-vs-base must exist so the base comparison has one entry point")
	}
	if !strings.Contains(mk, "adr-check -base") {
		t.Error("Makefile: adr-vs-base must run `adr-check -base` rather than a second implementation of the rule")
	}
	if regexp.MustCompile(`(?m)^lint:.*adr-vs-base`).MatchString(mk) {
		t.Error("Makefile: the base comparison must stay out of `lint`, which has to work without a fetched base (ADR-0026)")
	}

	gate := readRepoFile(t, filepath.Join(".gitea", "workflows", "adr-gate.yml"))
	if !strings.Contains(gate, "make adr-vs-base") {
		t.Error(".gitea/workflows/adr-gate.yml: the canonical forge must run the comparison on every pull request")
	}
	if !strings.Contains(gate, "fetch-depth: 0") {
		t.Error(".gitea/workflows/adr-gate.yml: the comparison needs the history both sides share, so the checkout must not be shallow")
	}
}
