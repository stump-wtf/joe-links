// safari_ci_test.go guards the SPEC-0015 TestFlight CI invariants: the safari
// job must exist in ci.yml, must only run on release tags, must depend on
// lint and test without anything depending on it (a signing failure may never
// block the binary release or the image push), must fail fast naming every
// missing signing secret, and must derive the uploaded version and build
// number from the tag. Issue #134.
package packaging_test

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ciJob returns the text of one job from ci.yml, from its "  <name>:" line up
// to the next top-level job key, so assertions cannot pass by matching another
// job's steps.
func ciJob(t *testing.T, ci, name string) string {
	t.Helper()
	start := regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:\n`)
	loc := start.FindStringIndex(ci)
	if loc == nil {
		t.Fatalf("ci.yml has no %q job", name)
	}
	rest := ci[loc[1]:]
	next := regexp.MustCompile(`(?m)^  [a-z][a-z0-9_-]*:\n`).FindStringIndex(rest)
	if next == nil {
		return rest
	}
	return rest[:next[0]]
}

func TestSafariJobRunsOnReleaseTagsOnly(t *testing.T) {
	ci := readCIWorkflow(t)
	job := ciJob(t, ci, "safari")

	if !strings.Contains(job, "needs: [lint, test]") {
		t.Error("safari job must need lint and test (SPEC-0015 REQ \"CI Job Integration\")")
	}
	if !strings.Contains(job, "runs-on: macos-latest") {
		t.Error("safari job must run on a macOS runner: xcodebuild is not on the Linux images")
	}
	if !strings.Contains(job, "startsWith(github.ref, 'refs/tags/v')") {
		t.Error("safari job must be gated on tag pushes (SPEC-0015 REQ \"CI Job Integration\")")
	}
	// A v1.2.3-rc.1 tag would otherwise upload build 123 and then make the
	// final v1.2.3 upload (also 123) a duplicate App Store Connect rejects.
	if !strings.Contains(job, "!contains(github.ref_name, '-')") {
		t.Error("safari job must skip prerelease tags, whose derived build number collides with the release build")
	}
	if !regexp.MustCompile(`(?m)^\s+timeout-minutes:\s*\d+`).MatchString(job) {
		t.Error("safari job must cap its macOS runner time; a hung xcodebuild otherwise runs to the 360-minute limit")
	}
}

func TestSafariJobNeverBlocksReleaseJobs(t *testing.T) {
	ci := readCIWorkflow(t)

	// Nothing may depend on safari: SPEC-0015 says its failure must not block
	// the binary release or the Docker image.
	for _, name := range []string{"build", "docker", "docs", "lint", "test"} {
		job := ciJob(t, ci, name)
		needs := regexp.MustCompile(`(?m)^\s+needs:.*`).FindAllString(job, -1)
		for _, n := range needs {
			if strings.Contains(n, "safari") {
				t.Errorf("%s job must not need safari: %s", name, strings.TrimSpace(n))
			}
		}
	}
}

func TestSafariJobFailsFastNamingMissingSecrets(t *testing.T) {
	ci := readCIWorkflow(t)
	job := ciJob(t, ci, "safari")

	// SPEC-0015 REQ "Code Signing Configuration": a missing secret must fail
	// the job with a message identifying it, before any archive is built.
	start := strings.Index(job, "- name: Check signing secrets\n")
	if start < 0 {
		t.Fatal("safari job must have a \"Check signing secrets\" step that runs before xcodebuild")
	}
	step := job[start:]
	if next := strings.Index(step, "\n      - name: "); next >= 0 {
		step = step[:next]
	}
	run := strings.Index(step, "run: |")
	if run < 0 {
		t.Fatal("the signing-secret check must be a shell step")
	}
	env, body := step[:run], step[run:]
	for _, secret := range []string{
		"APPLE_CERT_P12",
		"APPLE_CERT_PASSWORD",
		"APPLE_TEAM_ID",
		"APP_STORE_CONNECT_API_KEY_ID",
		"APP_STORE_CONNECT_API_ISSUER_ID",
		"APP_STORE_CONNECT_API_KEY_P8",
	} {
		// The value is mapped into env so the script can test whether it is
		// set, and the name appears in the script so the failure can name it.
		// Expanding ${{ secrets.X }} in the script instead would let the check
		// pass only by accident.
		if !strings.Contains(env, secret+": ${{ secrets."+secret+" }}") {
			t.Errorf("the signing-secret check must map %s into its env", secret)
		}
		if !strings.Contains(body, secret) {
			t.Errorf("the signing-secret check must name %s when it is missing", secret)
		}
	}
	if !strings.Contains(body, "::error::") {
		t.Error("the signing-secret check must emit an ::error:: annotation naming the missing secrets")
	}
	if !regexp.MustCompile(`(?m)^\s+exit 1\s*$`).MatchString(body) {
		t.Error("the signing-secret check must exit non-zero when a secret is missing")
	}
	if strings.Contains(body, "${{ secrets.") {
		t.Error("the signing-secret check must not expand a secret inline; test the mapped env vars")
	}
}

func TestSafariJobDerivesVersionAndBuildNumberFromTag(t *testing.T) {
	ci := readCIWorkflow(t)
	job := ciJob(t, ci, "safari")

	if !strings.Contains(job, "integrations/apple/testflight-build-number.sh") {
		t.Error("safari job must derive the build number with integrations/apple/testflight-build-number.sh")
	}
	// Both archives need the overrides: the committed project pins version 1.0
	// build 1, which App Store Connect would treat as a re-upload forever.
	for _, setting := range []string{"MARKETING_VERSION=", "CURRENT_PROJECT_VERSION="} {
		if got := strings.Count(job, setting+"\"${{ steps.build_number.outputs"); got != 2 {
			t.Errorf("both archives must override %s from the tag (got %d, want 2)", setting, got)
		}
	}
}

func TestSafariJobSignsAndUploadsBothPlatforms(t *testing.T) {
	ci := readCIWorkflow(t)
	job := ciJob(t, ci, "safari")

	for _, target := range []string{`-target "joe-links (iOS)"`, `-target "joe-links (macOS)"`} {
		if !strings.Contains(job, target) {
			t.Errorf("safari job must archive the %s target (SPEC-0015 REQ \"TestFlight Upload\")", target)
		}
	}
	if !strings.Contains(job, "security import") {
		t.Error("safari job must import the distribution certificate from APPLE_CERT_P12")
	}
	if !strings.Contains(job, "-exportArchive") {
		t.Error("safari job must export the archives (iOS .ipa and macOS .pkg) before uploading")
	}
	// -exportArchive takes only -archivePath, -exportPath and
	// -exportOptionsPlist; there is no -exportFormat flag, and passing one
	// exits EX_USAGE before any export runs. The .ipa/.pkg format is derived
	// from the archive itself via the app-store-connect method in the plist.
	// Scoped to the invocation block (not the step comment, which names the
	// flag to explain why it is absent).
	export := job[strings.Index(job, "- name: Export"):]
	if next := strings.Index(export, "\n      - name: "); next >= 0 {
		export = export[:next]
	}
	invocation := export[strings.Index(export, "xcodebuild -exportArchive"):]
	if strings.Contains(invocation, "-exportFormat") {
		t.Error("export step must not pass -exportFormat: xcodebuild -exportArchive has no such flag and exits EX_USAGE on it")
	}
	if !strings.Contains(job, "altool --upload-app") {
		t.Error("safari job must upload with xcrun altool --upload-app (SPEC-0015 design)")
	}
	if !strings.Contains(job, "--apiIssuer") {
		t.Error("uploads must authenticate with the App Store Connect API issuer")
	}
	// The exported bundle is what TestFlight accepts; an empty export dir must
	// fail rather than upload nothing and pass.
	if !strings.Contains(job, "if [ ! -e \"${artifacts[0]}\" ]") {
		t.Error("upload step must fail when an export produced no artifact")
	}
	if !strings.Contains(job, "security delete-keychain") {
		t.Error("safari job must delete the signing keychain it created")
	}
}

func TestTestFlightBuildNumberFromTag(t *testing.T) {
	script := filepath.Join("..", "..", "integrations", "apple", "testflight-build-number.sh")

	// SPEC-0015 REQ "TestFlight Upload": the build number is derived from the
	// tag so a re-run of CI cannot repeat it (v0.2.21 -> 221).
	good := []struct {
		tag  string
		want string
	}{
		{"v0.2.21", "221"},
		{"v1.0.0", "100"},
		{"v1.2.10", "1210"},
		{"v0.0.5", "5"},
		{"v0.10.0", "100"},
	}
	for _, tc := range good {
		out, err := exec.Command(script, tc.tag).CombinedOutput()
		if err != nil {
			t.Errorf("%s: %v (%s)", tc.tag, err, out)
			continue
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("build number for %s = %s, want %s", tc.tag, got, tc.want)
		}
	}

	bad := []string{"v1.2.3-rc.1", "v1.2", "latest", "v0.0.0", ""}
	for _, tag := range bad {
		out, err := exec.Command(script, tag).CombinedOutput()
		if err == nil {
			t.Errorf("%q must be rejected, got build number %s", tag, strings.TrimSpace(string(out)))
		}
	}
}
