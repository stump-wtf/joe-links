// Regression tests for issue #321: chi's deprecated middleware.RealIP rewrote
// r.RemoteAddr from True-Client-IP / X-Real-IP / X-Forwarded-For no matter who
// sent them, so a request chose the IP it was recorded under. These drive the
// real NewRouter and read back the recorded click event, so the assertions
// cover the whole middleware stack rather than a helper in isolation.
//
// Governing: ADR-0023
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/joestump/joe-links/internal/auth"
	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newClientIPTestEnv builds the production router on an in-memory database with
// one public link, varying only the trusted-proxy configuration.
func newClientIPTestEnv(t *testing.T, trustedProxies []string) (http.Handler, chan store.ClickEvent) {
	t.Helper()
	db := testutil.NewTestDB(t)
	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)

	u, err := us.Upsert(context.Background(), "test", "sub-ip", "ip@example.com", "IP Tester", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := ls.Create(context.Background(), "example", "https://example.com/target", u.ID, "", "", "public"); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	clickCh := make(chan store.ClickEvent, 8)
	sm := scs.New()
	router := NewRouter(Deps{
		SessionManager: sm,
		AuthHandlers:   auth.NewHandlers(nil, sm, us, "", nil, "groups", true),
		AuthMiddleware: auth.NewMiddleware(sm, us),
		LinkStore:      ls,
		OwnershipStore: owns,
		TagStore:       tags,
		UserStore:      us,
		TokenStore:     auth.NewSQLTokenStore(db),
		KeywordStore:   store.NewKeywordStore(db),
		ClickStore:     store.NewClickStore(db),
		ClickCh:        clickCh,
		TrustedProxies: trustedProxies,
	})
	return router, clickCh
}

// recordRedirectClick follows one anonymous redirect and returns the IP hash it
// was recorded under.
func recordRedirectClick(t *testing.T, router http.Handler, clickCh chan store.ClickEvent, remoteAddr string, headers map[string]string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/example", nil)
	req.RemoteAddr = remoteAddr
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("GET /example status = %d, want %d", w.Code, http.StatusFound)
	}
	select {
	case ev := <-clickCh:
		return ev.IPHash
	case <-time.After(2 * time.Second):
		t.Fatal("no click event recorded")
		return ""
	}
}

// With no proxy configured to be trusted, no caller-supplied header is
// believed: the recorded IP is the one that opened the connection.
func TestClientIP_SpoofedHeadersIgnoredWhenNoProxyTrusted(t *testing.T) {
	router, clickCh := newClientIPTestEnv(t, nil)

	const peer = "192.0.2.50:40000"
	for _, headers := range []map[string]string{
		{"X-Forwarded-For": "203.0.113.7"},
		{"X-Real-IP": "203.0.113.8"},
		{"True-Client-IP": "203.0.113.9"},
		{"X-Forwarded-For": "203.0.113.7, 192.0.2.50"},
	} {
		got := recordRedirectClick(t, router, clickCh, peer, headers)
		if want := store.HashIP("192.0.2.50"); got != want {
			t.Errorf("headers %v recorded hash %s, want the peer address's hash %s", headers, got, want)
		}
		if spoofed := store.HashIP("203.0.113.7"); got == spoofed {
			t.Errorf("headers %v let the client choose the recorded IP (hash %s)", headers, spoofed)
		}
	}
}

// Behind a trusted proxy the hop's X-Forwarded-For is read, skipping the
// trusted entries, so analytics still distinguish real visitors.
func TestClientIP_TrustedProxyChainIsRead(t *testing.T) {
	router, clickCh := newClientIPTestEnv(t, []string{"192.0.2.0/24"})

	got := recordRedirectClick(t, router, clickCh, "192.0.2.50:40000", map[string]string{
		"X-Forwarded-For": "203.0.113.7, 192.0.2.50",
	})
	if want := store.HashIP("203.0.113.7"); got != want {
		t.Errorf("recorded hash = %s, want the client's hash %s from behind the trusted hop", got, want)
	}
}

// A trusted-proxy list is not a licence to believe the header from anyone: when
// the connection comes from outside the list, the peer address wins even though
// the header is well-formed.
func TestClientIP_UntrustedPeerCannotSpoofDespiteProxyList(t *testing.T) {
	router, clickCh := newClientIPTestEnv(t, []string{"192.0.2.0/24"})

	got := recordRedirectClick(t, router, clickCh, "198.51.100.9:51234", map[string]string{
		"X-Forwarded-For": "203.0.113.7",
	})
	if want := store.HashIP("198.51.100.9"); got != want {
		t.Errorf("recorded hash = %s, want the connecting peer's hash %s", got, want)
	}
	if spoofed := store.HashIP("203.0.113.7"); got == spoofed {
		t.Errorf("a peer outside the trusted list rewrote its recorded IP (hash %s)", spoofed)
	}
}

// A proxy that sets only X-Real-IP, not X-Forwarded-For, must not become a way
// back in: the header is never read, and with nothing resolved the click is
// recorded under the proxy's own address.
func TestClientIP_RealIPHeaderNeverReadEvenFromTrustedPeer(t *testing.T) {
	router, clickCh := newClientIPTestEnv(t, []string{"192.0.2.0/24"})

	got := recordRedirectClick(t, router, clickCh, "192.0.2.50:40000", map[string]string{
		"X-Real-IP": "203.0.113.8",
	})
	if want := store.HashIP("192.0.2.50"); got != want {
		t.Errorf("recorded hash = %s, want the peer's hash %s; X-Real-IP must not be believed", got, want)
	}
}
