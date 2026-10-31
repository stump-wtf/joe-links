// Tests for scripts/gitea-request-review.sh, the helper build lanes use to ask
// the sibling identity for review on a Gitea pull request.
//
// They pin two things the forge itself will not tell us: the request shape
// Gitea API 28.x actually routes (POST .../pulls/{index}/requested_reviewers
// with a JSON body, not the old .../reviewers?reviewers= query form), and the
// fact that the helper must fail when a forge answers the request without
// attaching anybody. That silent no-op is issue #336, and it is invisible from
// the exit code alone because `tea api` exits 0 for any HTTP status.
package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	testRepo  = "stump.wtf/joe-links"
	testIndex = "336"
	testUser  = "joestump"
)

// fakeForge stands in for `tea api`: it records the request it was asked to
// make and replays a canned pull-request body, so the helper can be pointed at
// both a forge that attaches the reviewer and one that no-ops.
const fakeForge = `#!/usr/bin/env bash
set -euo pipefail
endpoint=""
method="GET"
body=""
while [ $# -gt 0 ]; do
  case "$1" in
    --login) shift 2 ;;
    -X) method="$2"; shift 2 ;;
    -H) shift 2 ;;
    -d) body="$2"; shift 2 ;;
    *) endpoint="$1"; shift ;;
  esac
done
if [ "$method" = "POST" ]; then
  printf '%s\n%s\n' "$endpoint" "$body" > "$FAKE_RECORD"
  exit 0
fi
cat "$FAKE_PR_JSON"
`

// attachedPR is a pull request whose review request landed.
const attachedPR = `{"number":336,"requested_reviewers":[{"login":"joestump"}],"assignees":[]}`

// noOpPR is a pull request whose review request was answered but ignored.
const noOpPR = `{"number":336,"requested_reviewers":[],"assignees":[]}`

type forge struct {
	dir      string
	record   string
	endpoint string
	body     string
}

func newForge(t *testing.T, pullRequest string) *forge {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "fake-forge.sh")
	if err := os.WriteFile(stub, []byte(fakeForge), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pull.json"), []byte(pullRequest), 0o644); err != nil {
		t.Fatal(err)
	}
	return &forge{dir: dir, record: filepath.Join(dir, "request.txt")}
}

func (f *forge) run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t)}, args...)...)
	cmd.Env = append(cmd.Environ(),
		"GITEA_API_CMD="+filepath.Join(f.dir, "fake-forge.sh"),
		"FAKE_RECORD="+f.record,
		"FAKE_PR_JSON="+filepath.Join(f.dir, "pull.json"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		f.readRecord()
	}
	return stdout.String(), stderr.String(), err
}

func (f *forge) readRecord() {
	recorded, err := os.ReadFile(f.record)
	if err != nil {
		return
	}
	lines := strings.SplitN(strings.TrimSuffix(string(recorded), "\n"), "\n", 2)
	f.endpoint = lines[0]
	if len(lines) > 1 {
		f.body = lines[1]
	}
}

func scriptPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to find the script beside it")
	}
	return filepath.Join(filepath.Dir(thisFile), "gitea-request-review.sh")
}

func TestReviewRequestUsesTheRoutedEndpoint(t *testing.T) {
	f := newForge(t, attachedPR)
	stdout, stderr, err := f.run(t, "--host", "forge.test", "--dry-run", testRepo, testIndex, testUser)
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, stderr)
	}
	want := "POST repos/" + testRepo + "/pulls/" + testIndex + "/requested_reviewers\n" +
		`{"reviewers":["joestump"]}` + "\n"
	if stdout != want {
		t.Errorf("request shape =\n%q\nwant\n%q", stdout, want)
	}
	if strings.Contains(stdout, "/reviewers?") {
		t.Error("the query form is a no-op on Gitea 28.x and must not come back")
	}
}

func TestReviewRequestReportsTheReviewerAttached(t *testing.T) {
	requireJQ(t)
	f := newForge(t, attachedPR)
	stdout, stderr, err := f.run(t, "--host", "forge.test", testRepo, testIndex, testUser)
	if err != nil {
		t.Fatalf("request failed: %v\n%s", err, stderr)
	}
	if want := "repos/" + testRepo + "/pulls/" + testIndex + "/requested_reviewers"; f.endpoint != want {
		t.Errorf("posted to %q, want %q", f.endpoint, want)
	}
	if want := `{"reviewers":["joestump"]}`; f.body != want {
		t.Errorf("posted body %q, want %q", f.body, want)
	}
	if !strings.Contains(stdout, "verified in requested_reviewers") {
		t.Errorf("output %q does not confirm the reviewer attached", stdout)
	}
}

func TestReviewRequestFailsWhenNothingAttaches(t *testing.T) {
	requireJQ(t)
	f := newForge(t, noOpPR)
	stdout, stderr, err := f.run(t, "--host", "forge.test", testRepo, testIndex, testUser)
	if err == nil {
		t.Fatalf("a review request that attached nobody must not exit 0\n%s", stdout)
	}
	if !strings.Contains(stderr, "review request did not attach") {
		t.Errorf("stderr %q does not name the no-op", stderr)
	}
}

func TestReviewRequestRefusesNamesThatWouldBreakTheBody(t *testing.T) {
	f := newForge(t, attachedPR)
	_, stderr, err := f.run(t, "--host", "forge.test", "--dry-run", testRepo, testIndex, `joestump","x`)
	if err == nil {
		t.Fatal("a reviewer name carrying quotes must be refused")
	}
	if !strings.Contains(stderr, "invalid reviewer name") {
		t.Errorf("stderr %q does not name the bad reviewer", stderr)
	}
}

func TestReviewRequestRefusesRepoNamesThatWouldReshapeThePath(t *testing.T) {
	for _, repo := range []string{
		"../../evil/repo",
		"stump.wtf/joe-links/pulls/1/requested_reviewers",
		"stump.wtf/joe-links?reviewers=eve",
		"stump.wtf/",
		"/joe-links",
	} {
		f := newForge(t, attachedPR)
		_, stderr, err := f.run(t, "--host", "forge.test", "--dry-run", repo, testIndex, testUser)
		if err == nil {
			t.Errorf("repo %q must be refused before it reaches the request path", repo)
		}
		if !strings.Contains(stderr, "repo must be") {
			t.Errorf("stderr %q does not name the bad repo %q", stderr, repo)
		}
	}
}

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed, cannot verify requested_reviewers")
	}
}
