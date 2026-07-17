// Package packaging_test guards the Docker packaging invariants fixed in
// issue #212: the compose host-port mapping must not interpolate the
// JOE_HTTP_ADDR bind address, the in-container bind must stay pinned to
// :8080, and the final image must be pinned, non-root, and health-checked.
package packaging_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readRepoFile reads a file relative to the repository root (two levels up
// from this package).
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

func TestComposePortMappingDecoupledFromBindAddr(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yml")

	// The host-port slot must interpolate JOE_HOST_PORT, never the bind
	// address (".env.example used to ship JOE_HTTP_ADDR=:8080, which parsed
	// as host port ":8080" only by accident).
	if !strings.Contains(compose, `"${JOE_HOST_PORT:-8080}:8080"`) {
		t.Error(`docker-compose.yml must map ports via "${JOE_HOST_PORT:-8080}:8080"`)
	}
	if strings.Contains(compose, "${JOE_HTTP_ADDR") {
		t.Error("docker-compose.yml must not interpolate JOE_HTTP_ADDR (it is a bind address, not a host port)")
	}

	// env_file forwards everything from .env, so the in-container bind
	// address must be pinned to the container side of the port mapping.
	if !regexp.MustCompile(`JOE_HTTP_ADDR:\s*":8080"`).MatchString(compose) {
		t.Error(`docker-compose.yml must pin environment JOE_HTTP_ADDR: ":8080" so .env cannot break the mapping`)
	}
}

func TestComposeHasHealthcheck(t *testing.T) {
	compose := readRepoFile(t, "docker-compose.yml")
	if !strings.Contains(compose, "healthcheck:") {
		t.Error("docker-compose.yml app service must define a healthcheck")
	}
}

func TestDockerfileHardened(t *testing.T) {
	dockerfile := readRepoFile(t, "Dockerfile")

	if regexp.MustCompile(`(?m)^FROM .*:latest\b`).MatchString(dockerfile) {
		t.Error("Dockerfile must not build FROM a :latest tag; pin a version for reproducible builds")
	}
	if !regexp.MustCompile(`(?m)^FROM alpine:[0-9]+\.[0-9]+`).MatchString(dockerfile) {
		t.Error("Dockerfile final stage must pin a numeric alpine version (e.g. FROM alpine:3.22)")
	}
	if !regexp.MustCompile(`(?m)^USER `).MatchString(dockerfile) {
		t.Error("Dockerfile must switch to a non-root USER before CMD")
	}
	if !regexp.MustCompile(`(?m)^HEALTHCHECK`).MatchString(dockerfile) {
		t.Error("Dockerfile must define a HEALTHCHECK")
	}
	// The named volume at /data inherits ownership from the image, so the
	// image must chown it for the non-root user's SQLite DSN to be writable.
	if !strings.Contains(dockerfile, "chown joe:joe /data") {
		t.Error("Dockerfile must pre-create /data owned by the runtime user")
	}
}

func TestDockerignoreExcludesHeavyContext(t *testing.T) {
	ignore := readRepoFile(t, ".dockerignore")

	lines := map[string]bool{}
	for _, line := range strings.Split(ignore, "\n") {
		lines[strings.TrimSpace(line)] = true
	}
	for _, want := range []string{".git", "node_modules", "docs-site"} {
		if !lines[want] {
			t.Errorf(".dockerignore must exclude %q from the build context", want)
		}
	}
	// docs/swagger is a Go package imported by the binary — excluding all of
	// docs/ would break the build.
	if lines["docs"] || lines["docs/"] {
		t.Error(".dockerignore must not exclude docs/ wholesale (docs/swagger is compiled into the binary)")
	}
}

func TestEnvExampleDoesNotForwardBindAddr(t *testing.T) {
	env := readRepoFile(t, ".env.example")

	// An active JOE_HTTP_ADDR in .env would be forwarded into the container
	// by env_file and used to fight the pinned compose value historically;
	// it must ship commented out.
	if regexp.MustCompile(`(?m)^JOE_HTTP_ADDR=`).MatchString(env) {
		t.Error(".env.example must not set JOE_HTTP_ADDR (comment it out; compose pins the in-container bind)")
	}
	if !regexp.MustCompile(`(?m)^JOE_HOST_PORT=`).MatchString(env) {
		t.Error(".env.example must document JOE_HOST_PORT for the compose port mapping")
	}
}
