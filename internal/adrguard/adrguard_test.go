// Fixture tests for the ADR number collision gate (issue #345): a pair of
// records claiming one number must fail with both paths named, and a clean set
// must pass. The fixtures are written into a temp directory so the check is
// exercised on both shapes, not only on whatever docs/adrs happens to hold.
package adrguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRecord drops one design record into dir using the repo's own shape:
// frontmatter, then an H1 carrying the number and its title.
func writeRecord(t *testing.T, dir, name, number, title string) string {
	t.Helper()
	body := "---\nstatus: proposed\ndate: 2026-11-05\n---\n\n# ADR-" + number + ": " + title + "\n\n## Context and Problem Statement\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

func TestCollisionsFailsOnSameNumberPair(t *testing.T) {
	dir := t.TempDir()
	caseFold := writeRecord(t, dir, "ADR-0027-keyword-matching-case-folding.md", "0024", "Keyword Matching Case Folding")
	oidc := writeRecord(t, dir, "ADR-0027-optional-oidc-local-admin.md", "0024", "Optional OIDC With A Local Admin Login")

	claims, err := Scan(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	got := Collisions(claims)
	if len(got) != 1 {
		t.Fatalf("want one collision from a same-number pair, got %d: %+v", len(got), got)
	}

	collision := got[0]
	if collision.Number != "24" {
		t.Errorf("collision number = %q, want %q (leading zeros are not identity)", collision.Number, "24")
	}
	if !collision.TitlesDiffer() {
		t.Error("two records under one number with different titles must report different decisions")
	}

	message := collision.Message()
	for _, want := range []string{caseFold, oidc} {
		if !strings.Contains(message, want) {
			t.Errorf("message must name %s, got: %s", want, message)
		}
	}
}

func TestCollisionsPassesOnCleanSet(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "ADR-0001-technology-stack.md", "0001", "Technology Stack")
	writeRecord(t, dir, "ADR-0009-api-token-authentication.md", "0009", "API Token Authentication")
	writeRecord(t, dir, "ADR-0023-client-ip-trust-model.md", "0023", "Client IP Trust Model")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Index\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	claims, err := Scan(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	if got := Collisions(claims); len(got) != 0 {
		t.Fatalf("a set of distinct numbers must pass, got: %s", messages(got))
	}
}

func TestScanIgnoresNamesThatClaimNothing(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ADR-template.md", "adr-0024-example.md", "ADR-24-late.md", "notes.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("# Not a record\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	claims, err := Scan(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}

	byPath := map[string]Claim{}
	for _, claim := range claims {
		byPath[filepath.Base(claim.Path)] = claim
	}
	if _, ok := byPath["ADR-template.md"]; ok {
		t.Error("ADR-template.md carries no number and must claim nothing")
	}
	if _, ok := byPath["notes.md"]; ok {
		t.Error("an unnumbered file must claim nothing")
	}
	// A lower-case name and an unpadded number are still records, and they
	// claim the same number as ADR-0027 would.
	for _, name := range []string{"adr-0024-example.md", "ADR-24-late.md"} {
		claim, ok := byPath[name]
		if !ok {
			t.Fatalf("%s should have been scanned as a record", name)
		}
		if claim.Number != "24" {
			t.Errorf("%s number = %q, want %q", name, claim.Number, "24")
		}
	}

	if got := Collisions(claims); len(got) != 1 || len(got[0].Claims) != 2 {
		t.Fatalf("the two records above claim one number, got: %s", messages(got))
	}
}

func TestTitleFallsBackToFileNameSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ADR-0002-pluggable-database-and-migrations.md")
	if err := os.WriteFile(path, []byte("## Context\n\nNo heading here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	claims, err := Scan(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	if len(claims) != 1 {
		t.Fatalf("want one claim, got %d", len(claims))
	}
	if want := "pluggable database and migrations"; claims[0].Title != want {
		t.Errorf("title = %q, want the file name slug %q", claims[0].Title, want)
	}
}

func TestTitlesDifferOnSameDecisionWrittenTwice(t *testing.T) {
	dir := t.TempDir()
	writeRecord(t, dir, "ADR-0030-alpha.md", "0030", "Shared Decision")
	writeRecord(t, dir, "ADR-0030-beta.md", "0030", "Shared Decision")

	claims, err := Scan(dir)
	if err != nil {
		t.Fatalf("scanning %s: %v", dir, err)
	}
	got := Collisions(claims)
	if len(got) != 1 {
		t.Fatalf("want one collision, got %d: %+v", len(got), got)
	}
	if got[0].TitlesDiffer() {
		t.Error("identical titles are the same decision written twice, not two decisions")
	}
	if msg := got[0].Message(); !strings.Contains(msg, "claimed by 2 files") {
		t.Errorf("message should describe duplicate files, got: %s", msg)
	}
}

func TestCollisionsScopedPerDirectory(t *testing.T) {
	root := t.TempDir()
	adrs := filepath.Join(root, "adrs")
	specs := filepath.Join(root, "specs")
	for _, dir := range []string{adrs, specs} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeRecord(t, dir, "ADR-0040-something.md", "0040", "Something")
	}

	var claims []Claim
	for _, dir := range []string{adrs, specs} {
		found, err := Scan(dir)
		if err != nil {
			t.Fatalf("scanning %s: %v", dir, err)
		}
		claims = append(claims, found...)
	}
	if got := Collisions(claims); len(got) != 0 {
		t.Fatalf("one number in two directories is not a collision, got: %s", messages(got))
	}
}

func messages(collisions []Collision) string {
	parts := make([]string, len(collisions))
	for i, collision := range collisions {
		parts[i] = collision.Message()
	}
	return strings.Join(parts, "\n")
}
