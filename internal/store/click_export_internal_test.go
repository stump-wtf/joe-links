// Story #277 — CSV export encoding internals (SPEC-0021).
// Governing: SPEC-0021 REQ "CSV Export", Security Requirements "CSV Injection"
package store

import (
	"testing"
	"time"
)

// Scenario support: Formula cell neutralized — the guard prefixes any cell
// whose first non-whitespace/control character is =, +, -, or @ (after
// skipping the leading run of TAB/CR/LF/spaces/controls), and any cell that
// begins with TAB or CR, per OWASP guidance. The route-level scenario test
// lives in internal/api/export_test.go; this pins the cell-level contract.
// Governing: SPEC-0021 Security Requirements "CSV Injection"
func TestCSVGuardCell_FormulaCellNeutralized(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"plain text", "plain text"},
		{"https://example.com/x", "https://example.com/x"},
		{"=SUM(A1:A9)", "'=SUM(A1:A9)"},
		{"+1", "'+1"},
		{"-1", "'-1"},
		{"@cmd", "'@cmd"},
		{"  =spaced", "'  =spaced"},
		{" \t\r\n=mixed-ws", "' \t\r\n=mixed-ws"},
		{"\t=tab", "'\t=tab"},
		// Begins with TAB/CR: escaped even when no formula character follows.
		{"\tplain", "'\tplain"},
		{"\rplain", "'\rplain"},
		// LF-leading cells only escape when a formula character follows the run.
		{"\nplain", "\nplain"},
		{"\n=lf-formula", "'\n=lf-formula"},
		// A formula character later in the cell is inert.
		{"a=b", "a=b"},
		{"Mozilla/5.0 (X11; Linux x86_64)", "Mozilla/5.0 (X11; Linux x86_64)"},
	}
	for _, tc := range cases {
		if got := csvGuardCell(tc.in); got != tc.want {
			t.Errorf("csvGuardCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The opaque cursor round-trips a (clicked_at, id) keyset position and
// rejects malformed input — resumption never rides a bare timestamp.
// Governing: SPEC-0021 REQ "CSV Export"
func TestClickExportCursor_RoundTrip(t *testing.T) {
	ts := time.Date(2026, 7, 7, 8, 9, 10, 123456789, time.UTC)
	cursor := EncodeClickExportCursor(ts, "row-id-42")

	gotTS, gotID, err := DecodeClickExportCursor(cursor)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !gotTS.Equal(ts) || gotID != "row-id-42" {
		t.Errorf("round trip = (%v, %q), want (%v, %q)", gotTS, gotID, ts, "row-id-42")
	}

	for _, bad := range []string{"", "!!!", "aGVsbG8=", "MjAyNi0wNy0wN1QwODowOToxMFo="} {
		if _, _, err := DecodeClickExportCursor(bad); err == nil {
			t.Errorf("DecodeClickExportCursor(%q) = nil error, want malformed-cursor error", bad)
		}
	}
}

// referrerHost groups by URL host only — scheme, path, query, fragment, and
// port discarded; empty or unparseable referrers fall to Direct / unknown.
// Governing: SPEC-0021 REQ "Click Breakdowns"
func TestReferrerHost_Grouping(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "Direct / unknown"},
		{"not a url", "Direct / unknown"},
		{"/relative/path", "Direct / unknown"},
		{"https://a.example/x", "a.example"},
		{"https://a.example/y?z=1", "a.example"},
		{"https://A.EXAMPLE/case", "a.example"},
		{"https://a.example:8443/port", "a.example"},
		{"http://b.example", "b.example"},
	}
	for _, tc := range cases {
		if got := referrerHost(tc.in); got != tc.want {
			t.Errorf("referrerHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
