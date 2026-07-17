// Governing: SPEC-0016 REQ "Click Recording", ADR-0016
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/joe-links/internal/store"
	"github.com/joestump/joe-links/internal/testutil"
)

// newClickWriterEnv creates a migrated in-memory DB seeded with a user and a
// link, and returns the ClickStore plus the seeded link ID.
func newClickWriterEnv(t *testing.T) (*store.ClickStore, string) {
	t.Helper()
	db := testutil.NewTestDB(t)

	owns := store.NewOwnershipStore(db)
	tags := store.NewTagStore(db)
	ls := store.NewLinkStore(db, owns, tags)
	us := store.NewUserStore(db)
	cs := store.NewClickStore(db)

	ctx := context.Background()
	u, err := us.Upsert(ctx, "test", "sub1", "serve-test@example.com", "Serve Tester", "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	link, err := ls.Create(ctx, "serve-test-link", "https://example.com", u.ID, "Test Link", "", "")
	if err != nil {
		t.Fatalf("seed link: %v", err)
	}
	return cs, link.ID
}

// startClickWriter runs runClickWriter as serve.go does and returns the
// channel to send on plus the done channel that closes when the writer exits.
func startClickWriter(cs *store.ClickStore) (chan store.ClickEvent, chan struct{}) {
	clickCh := make(chan store.ClickEvent, 256)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		runClickWriter(context.Background(), clickCh, cs)
	}()
	return clickCh, writerDone
}

// TestRunClickWriter_DrainsQueueOnClose verifies the drain contract: every
// event buffered in the channel at close time is persisted before the writer
// signals done — nothing queued may be dropped at shutdown.
func TestRunClickWriter_DrainsQueueOnClose(t *testing.T) {
	cs, linkID := newClickWriterEnv(t)
	clickCh, writerDone := startClickWriter(cs)

	const n = 50
	for i := 0; i < n; i++ {
		clickCh <- store.ClickEvent{
			LinkID:    linkID,
			IPHash:    fmt.Sprintf("hash-%d", i),
			UserAgent: "TestAgent/1.0",
		}
	}
	close(clickCh)

	select {
	case <-writerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("click writer did not drain and exit after channel close")
	}

	stats, err := cs.GetClickStats(context.Background(), linkID)
	if err != nil {
		t.Fatalf("GetClickStats: %v", err)
	}
	if stats.Total != n {
		t.Errorf("persisted clicks after drain = %d, want %d", stats.Total, n)
	}
}

// TestServeAndDrain_ShutdownOrdering verifies the shutdown sequence: a request
// still in flight when the signal arrives must be able to send its click
// without panicking (srv.Shutdown completes before clickCh is closed), and
// that click must be persisted before serveAndDrain returns (writer drain is
// awaited before the caller can close the database).
func TestServeAndDrain_ShutdownOrdering(t *testing.T) {
	cs, linkID := newClickWriterEnv(t)
	clickCh, writerDone := startClickWriter(cs)

	handlerEntered := make(chan struct{})
	release := make(chan struct{})
	var panicked atomic.Bool

	// Mimics ResolveHandler's non-blocking send (internal/handler/resolve.go),
	// held mid-request so the send happens after shutdown has been signaled.
	mux := http.NewServeMux()
	mux.HandleFunc("/click", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				panicked.Store(true) // send on closed channel
			}
		}()
		close(handlerEntered)
		<-release
		select {
		case clickCh <- store.ClickEvent{LinkID: linkID, IPHash: "in-flight", UserAgent: "TestAgent/1.0"}:
		default:
		}
		w.WriteHeader(http.StatusFound)
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drainErr := make(chan error, 1)
	go func() {
		drainErr <- serveAndDrain(ctx, srv, ln, clickCh, writerDone, 10*time.Second)
	}()

	reqErr := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/click")
		if resp != nil {
			_ = resp.Body.Close()
		}
		reqErr <- err
	}()

	<-handlerEntered
	cancel() // simulate SIGTERM while the request is in flight
	// Give the shutdown path time to reach any premature close(clickCh)
	// before the handler attempts its send.
	time.Sleep(100 * time.Millisecond)
	close(release)

	if err := <-reqErr; err != nil {
		t.Errorf("in-flight request failed during shutdown: %v", err)
	}
	select {
	case err := <-drainErr:
		if err != nil {
			t.Errorf("serveAndDrain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveAndDrain did not return after shutdown")
	}

	if panicked.Load() {
		t.Fatal("handler panicked sending click during shutdown (clickCh closed before srv.Shutdown completed)")
	}

	// serveAndDrain has returned, so the drain must already be complete.
	stats, err := cs.GetClickStats(context.Background(), linkID)
	if err != nil {
		t.Fatalf("GetClickStats: %v", err)
	}
	if stats.Total != 1 {
		t.Errorf("persisted clicks after shutdown = %d, want 1 (in-flight click lost)", stats.Total)
	}
}

// TestServeAndDrain_BoundedDrainWait verifies that a writer which never
// signals done (e.g. a hung database connection at shutdown) cannot wedge
// serveAndDrain forever: the drain wait is bounded by shutdownTimeout so the
// process can still exit without SIGKILL.
func TestServeAndDrain_BoundedDrainWait(t *testing.T) {
	clickCh := make(chan store.ClickEvent, 1)
	writerDone := make(chan struct{}) // never closed: simulates a hung drain

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.NewServeMux()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown immediately; no requests in flight

	drainErr := make(chan error, 1)
	go func() {
		drainErr <- serveAndDrain(ctx, srv, ln, clickCh, writerDone, 200*time.Millisecond)
	}()

	select {
	case err := <-drainErr:
		if err != nil {
			t.Errorf("serveAndDrain: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serveAndDrain blocked on a hung writer; drain wait is not bounded")
	}
}
