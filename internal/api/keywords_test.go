// Governing: SPEC-0008 REQ "Keyword Host Discovery", ADR-0011
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGetConfigConfiguredShortKeyword verifies GET /config returns the
// JOE_SHORT_KEYWORD override even when it differs from the host's first label
// (the issue #210 deployment shape: "go/" advertised on links.example.com).
func TestGetConfigConfiguredShortKeyword(t *testing.T) {
	env := newTestEnvWithShortKeyword(t, "go")

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Host = "links.example.com"
	rr := httptest.NewRecorder()
	env.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp struct {
		ShortKeyword string `json:"short_keyword"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ShortKeyword != "go" {
		t.Errorf("short_keyword = %q, want %q", resp.ShortKeyword, "go")
	}
}

// TestGetConfigHostDerivedFallback verifies that without an override the
// endpoint derives the short keyword from the request host's first DNS label,
// mirroring the web UI (internal/handler/templates.go newBasePage).
func TestGetConfigHostDerivedFallback(t *testing.T) {
	env := newTestEnv(t)

	cases := []struct {
		host string
		want string
	}{
		{"go.stump.rocks", "go"},
		{"go.stump.rocks:8080", "go"}, // port must be stripped before the label split
		{"links.example.com", "links"},
		{"go", "go"}, // single-label host
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/config", nil)
		req.Host = tc.host
		rr := httptest.NewRecorder()
		env.Router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("host %q: status = %d, want 200", tc.host, rr.Code)
		}
		var resp struct {
			ShortKeyword string `json:"short_keyword"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("host %q: decode: %v", tc.host, err)
		}
		if resp.ShortKeyword != tc.want {
			t.Errorf("host %q: short_keyword = %q, want %q", tc.host, resp.ShortKeyword, tc.want)
		}
	}
}

// TestGetConfigRequiresNoAuth verifies /config sits in the public route group:
// the extension calls it before any API key is configured.
func TestGetConfigRequiresNoAuth(t *testing.T) {
	env := newTestEnvWithShortKeyword(t, "go")

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	// Deliberately no Authorization header.
	rr := httptest.NewRecorder()
	env.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status without bearer token = %d, want 200", rr.Code)
	}
}

// TestListKeywordsBareArray pins the back-compat contract relied on by older
// extensions: GET /keywords returns a bare JSON array of keyword names (the
// extension checks Array.isArray), NOT an object envelope. The short keyword
// lives on /config precisely so this shape never has to change.
func TestListKeywordsBareArray(t *testing.T) {
	env := newTestEnv(t)

	ctx := context.Background()
	if _, err := env.KeywordStore.Create(ctx, "jira", "https://jira.example.com/browse/{slug}", ""); err != nil {
		t.Fatalf("seed keyword: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/keywords", nil)
	rr := httptest.NewRecorder()
	env.Router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(body, "[") {
		t.Fatalf("body is not a bare JSON array: %s", body)
	}
	var names []string
	if err := json.Unmarshal([]byte(body), &names); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(names) != 1 || names[0] != "jira" {
		t.Errorf("names = %v, want [jira]", names)
	}
}
