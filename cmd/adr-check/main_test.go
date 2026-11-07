// End-to-end fixture for the base comparison: a throwaway repository whose main
// gained a design record after a branch was cut from it, which is the shape
// issue #345 reports — one number, two decisions, and neither tree showing it.
// The fixtures run real git, so the plumbing that lists the two sides is tested
// and not only the rule.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	caseFoldRecord = "docs/adrs/ADR-0002-keyword-matching-case-folding.md"
	oidcRecord     = "docs/adrs/ADR-0002-optional-oidc-local-admin.md"
)

func TestRunAgainstBaseFailsOnOneNumberInTwoBranches(t *testing.T) {
	repo := newADRRepo(t)
	commitRecord(t, repo, "feature", caseFoldRecord, "0002", "Keyword Matching Case Folding")
	commitRecord(t, repo, "main", oidcRecord, "0002", "Optional OIDC With A Local Admin Login")
	checkout(t, repo, "feature")

	t.Chdir(repo)
	err := run("docs/adrs", "main")
	if err == nil {
		t.Fatal("a branch that adds a second record under a number main already claims must fail the gate")
	}
	if !strings.Contains(err.Error(), "claimed twice") {
		t.Errorf("error should name the collision, got: %v", err)
	}
}

func TestRunAgainstBasePassesOnCleanContent(t *testing.T) {
	repo := newADRRepo(t)
	commitRecord(t, repo, "feature", "docs/adrs/ADR-0026-distinct-decision.md", "0026", "A Distinct Decision")
	commitRecord(t, repo, "main", oidcRecord, "0002", "Optional OIDC With A Local Admin Login")
	checkout(t, repo, "feature")

	t.Chdir(repo)
	if err := run("docs/adrs", "main"); err != nil {
		t.Fatalf("distinct numbers on the two sides must pass, got: %v", err)
	}
}

func TestRunAgainstBaseSaysWhichSideEachRecordIsOn(t *testing.T) {
	repo := newADRRepo(t)
	commitRecord(t, repo, "feature", caseFoldRecord, "0002", "Keyword Matching Case Folding")
	commitRecord(t, repo, "main", oidcRecord, "0002", "Optional OIDC With A Local Admin Login")
	checkout(t, repo, "feature")

	t.Chdir(repo)
	var out strings.Builder
	if err := runOn(&out, "docs/adrs", "main"); err == nil {
		t.Fatal("a branch that adds a second record under a number main already claims must fail the gate")
	}

	for _, want := range []string{caseFoldRecord, oidcRecord, "(on main)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the failure must name %s so a lane knows which tree to fix, got:\n%s", want, out.String())
		}
	}
}

func TestRunAgainstUnknownBaseFailsLoudly(t *testing.T) {
	repo := newADRRepo(t)
	checkout(t, repo, "feature")

	t.Chdir(repo)
	err := run("docs/adrs", "refs/heads/no-such-base")
	if err == nil {
		t.Fatal("a base ref that cannot be resolved must not read as a passing comparison")
	}
	if !strings.Contains(err.Error(), "fetch the base branch") {
		t.Errorf("error should say how to make the comparison possible, got: %v", err)
	}
}

func TestRunAgainstBaseReadsACommittedRecordFromTheSideThatHoldsIt(t *testing.T) {
	// A record committed on the branch but deleted from the working tree is
	// the branch's own, so the gate reads it from HEAD and says so; reading
	// every absent path from the base would misattribute it.
	repo := newADRRepo(t)
	commitRecord(t, repo, "feature", caseFoldRecord, "0002", "Keyword Matching Case Folding")
	commitRecord(t, repo, "main", oidcRecord, "0002", "Optional OIDC With A Local Admin Login")
	checkout(t, repo, "feature")
	if err := os.Remove(filepath.Join(repo, filepath.FromSlash(caseFoldRecord))); err != nil {
		t.Fatal(err)
	}

	t.Chdir(repo)
	var out strings.Builder
	if err := runOn(&out, "docs/adrs", "main"); err == nil {
		t.Fatal("a committed record deleted from the working tree must still fail the gate")
	}
	if !strings.Contains(out.String(), caseFoldRecord+" (on HEAD)") {
		t.Errorf("the branch's own record must be reported from the side that holds it, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), caseFoldRecord+" (on main)") {
		t.Errorf("a record the branch committed must not be reported as living on main, got:\n%s", out.String())
	}
}

// newADRRepo makes a repository with one design record committed on main and a
// branch cut from it, so the two sides have a divergence point like real lanes
// do.
func newADRRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "symbolic-ref", "HEAD", "refs/heads/main")
	writeRecordFile(t, repo, "docs/adrs/ADR-0001-technology-stack.md", "0001", "Technology Stack")
	commitAll(t, repo, "one record on main")
	runGit(t, repo, "checkout", "-q", "-b", "feature")
	return repo
}

// commitRecord adds one record to ref and commits it, leaving the repository on
// that ref.
func commitRecord(t *testing.T, repo, ref, path, number, title string) {
	t.Helper()
	checkout(t, repo, ref)
	writeRecordFile(t, repo, path, number, title)
	commitAll(t, repo, "add "+path)
}

func checkout(t *testing.T, repo, ref string) {
	t.Helper()
	runGit(t, repo, "checkout", "-q", ref)
}

func writeRecordFile(t *testing.T, repo, path, number, title string) {
	t.Helper()
	body := "---\nstatus: proposed\n---\n\n# ADR-" + number + ": " + title + "\n\n## Context and Problem Statement\n"
	writeFile(t, filepath.Join(repo, filepath.FromSlash(path)), body)
}

func commitAll(t *testing.T, repo, message string) {
	t.Helper()
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "-q", "-m", message)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// The fixture must not inherit the machine's identity, hooks or signing.
	cmd.Env = append(cmd.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
