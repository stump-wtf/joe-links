package handler

// Story #276 — viewer-local timestamp rendering (SPEC-0021).
//
// Tests are named after the spec scenarios they implement so the spec↔test
// mapping is auditable:
//   - "Local rendering with UTC hover" (the server side of the contract: the
//     <time datetime> markup plus the static Intl.DateTimeFormat snippet that
//     performs the client-side rewrite)
//   - "No-JS fallback"
//   - "HTMX fragment localized"
//
// Governing: SPEC-0021 REQ "Viewer-Local Timestamps", ADR-0021

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/joe-links/web"
)

// localtimeJS reads the embedded snippet exactly as it is served.
func localtimeJS(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(web.StaticFS, "static/js/localtime.js")
	if err != nil {
		t.Fatalf("read embedded localtime.js: %v", err)
	}
	return string(b)
}

// yesterdayNoonUTC is a deterministic in-window click instant.
func yesterdayNoonUTC() time.Time {
	return utcMidnightToday().AddDate(0, 0, -1).Add(14 * time.Hour)
}

// Scenario: Local rendering with UTC hover — the click timestamp is emitted
// as <time datetime="{RFC3339 UTC}">{UTC text}</time>, and the static snippet
// shipped on every page rewrites it to the viewer's local timezone via
// Intl.DateTimeFormat, setting title to preserve the UTC form.
// Governing: SPEC-0021 REQ "Viewer-Local Timestamps"
func TestViewerLocalTimestamps_LocalRenderingWithUTCHover(t *testing.T) {
	env := newStatsChartEnv(t)
	ts := yesterdayNoonUTC()
	seedClickAt(t, env.db, env.link.ID, ts, "")

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// Server-stamped machine-readable UTC instant (never user-controlled).
	want := `<time datetime="` + ts.Format(time.RFC3339) + `"`
	if !strings.Contains(body, want) {
		t.Errorf("stats page missing %q; body=%s", want, body)
	}
	// The localizer ships on the page.
	if !strings.Contains(body, "/static/js/localtime.js") {
		t.Errorf("base layout must include the localtime snippet")
	}

	// The snippet rewrites text to viewer-local via Intl.DateTimeFormat and
	// preserves the UTC form in title.
	js := localtimeJS(t)
	for _, needle := range []string{"Intl.DateTimeFormat", `setAttribute("title"`, "textContent", "time[datetime]"} {
		if !strings.Contains(js, needle) {
			t.Errorf("localtime.js missing %q", needle)
		}
	}
}

// Scenario: No-JS fallback — with JavaScript unavailable nothing rewrites the
// element, so the server-emitted UTC text is what the viewer reads (current
// v1 behavior preserved).
// Governing: SPEC-0021 REQ "Viewer-Local Timestamps"
func TestViewerLocalTimestamps_NoJSFallbackShowsUTC(t *testing.T) {
	env := newStatsChartEnv(t)
	ts := yesterdayNoonUTC()
	seedClickAt(t, env.db, env.link.ID, ts, "")

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, false)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	re := regexp.MustCompile(`<time[^>]*>([^<]+)</time>`)
	m := re.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("stats page missing <time> element; body=%s", w.Body.String())
	}
	text := strings.TrimSpace(m[1])
	if !strings.HasSuffix(text, " UTC") {
		t.Errorf("no-JS fallback text = %q, want UTC-labeled", text)
	}
	if !strings.Contains(text, ts.Format("Jan 2, 2006 3:04 PM")) {
		t.Errorf("no-JS fallback text = %q, want the UTC rendering of %s", text, ts)
	}
}

// Scenario: HTMX fragment localized — a refreshed fragment carries the same
// <time datetime> markup, and the snippet re-runs on htmx:afterSwap so the
// swapped timestamps localize too.
// Governing: SPEC-0021 REQ "Viewer-Local Timestamps"
func TestViewerLocalTimestamps_HTMXFragmentLocalized(t *testing.T) {
	env := newStatsChartEnv(t)
	ts := yesterdayNoonUTC()
	seedClickAt(t, env.db, env.link.ID, ts, "")

	w := env.get(t, "/dashboard/links/"+env.link.ID+"/stats", env.owner, true)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<html") {
		t.Fatalf("HX-Request must render a fragment, not a full page")
	}
	if !strings.Contains(body, `<time datetime="`+ts.Format(time.RFC3339)+`"`) {
		t.Errorf("fragment missing <time datetime> markup; body=%s", body)
	}

	js := localtimeJS(t)
	if !strings.Contains(js, "htmx:afterSwap") {
		t.Errorf("localtime.js must re-run on htmx:afterSwap for fragment localization")
	}
	// Spec hygiene: no innerHTML writes, no network requests. (".innerHTML"
	// matches the property sink without tripping on the code comment that
	// documents the ban.)
	for _, banned := range []string{".innerHTML", "fetch(", "XMLHttpRequest"} {
		if strings.Contains(js, banned) {
			t.Errorf("localtime.js must not use %q", banned)
		}
	}
}
