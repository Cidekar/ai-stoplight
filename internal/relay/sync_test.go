package relay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/adapter"
	"github.com/cidekar/stoplight/internal/stoplight"
)

func TestHandleSyncStatusCodes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "a full declaration",
			body: `{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","sessions":[{"session_id":"a","event":"started"}]}`,
			want: http.StatusNoContent,
		},
		{
			name: "an empty list is a declaration, not an error",
			body: `{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","sessions":[]}`,
			want: http.StatusNoContent,
		},
		{
			name: "a missing sessions key declares nothing live",
			body: `{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z"}`,
			want: http.StatusNoContent,
		},
		{
			name: "an unknown event is ignored, not rejected",
			body: `{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","sessions":[{"session_id":"a","event":"teleported"}]}`,
			want: http.StatusNoContent,
		},
		{
			name: "unknown fields are ignored",
			body: `{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","version":9,"sessions":[]}`,
			want: http.StatusNoContent,
		},
		{
			name: "no provider names nobody to be authoritative for",
			body: `{"observed_at":"2026-09-10T12:00:00Z","sessions":[]}`,
			want: http.StatusBadRequest,
		},
		{
			name: "no observed_at cannot be ordered",
			body: `{"provider":"claude-code","sessions":[]}`,
			want: http.StatusBadRequest,
		},
		{
			name: "an unparseable observed_at",
			body: `{"provider":"claude-code","observed_at":"yesterday","sessions":[]}`,
			want: http.StatusBadRequest,
		},
		{
			name: "malformed JSON",
			body: `{"provider":`,
			want: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRelay(t, nil)
			req := httptest.NewRequest(http.MethodPost, SyncPath, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			r.httpHandler().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestHandleSyncMethodNotAllowed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			r := newTestRelay(t, nil)
			req := httptest.NewRequest(method, SyncPath, nil)
			rec := httptest.NewRecorder()

			r.httpHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
		})
	}
}

// The cap is larger than ingest's because a sync carries many sessions, but it
// is still a cap.
func TestHandleSyncBodyTooLarge(t *testing.T) {
	r := newTestRelay(t, nil)

	var b strings.Builder
	b.WriteString(`{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","sessions":[`)
	for i := range 4000 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"session_id":"s%d","event":"started","label":"%s"}`, i, strings.Repeat("x", 60))
	}
	b.WriteString(`]}`)

	if b.Len() <= MaxSyncBodyBytes {
		t.Fatalf("test body is %d bytes, needs to exceed %d", b.Len(), MaxSyncBodyBytes)
	}

	req := httptest.NewRequest(http.MethodPost, SyncPath, strings.NewReader(b.String()))
	rec := httptest.NewRecorder()
	r.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// A body that ingest would reject as too large is accepted here. The two caps
// differ on purpose, and a sync of a handful of sessions clears 8KB easily.
func TestHandleSyncAcceptsMoreThanIngestWould(t *testing.T) {
	r := newTestRelay(t, nil)

	var b strings.Builder
	b.WriteString(`{"provider":"claude-code","observed_at":"2026-09-10T12:00:00Z","sessions":[`)
	for i := range 100 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"session_id":"s%d","event":"started","label":"%s"}`, i, strings.Repeat("y", 60))
	}
	b.WriteString(`]}`)

	if b.Len() <= MaxBodyBytes {
		t.Fatalf("test body is %d bytes, needs to exceed MaxBodyBytes %d", b.Len(), MaxBodyBytes)
	}

	req := httptest.NewRequest(http.MethodPost, SyncPath, strings.NewReader(b.String()))
	rec := httptest.NewRecorder()
	r.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204: a sync larger than the ingest cap must still be accepted", rec.Code)
	}
}

// End to end through the endpoint: a session the sync omits is removed, which
// is the behaviour reporting one session at a time cannot express.
func TestHandleSyncRemovesTheUndeclared(t *testing.T) {
	r := newTestRelay(t, nil)
	handler := r.httpHandler()

	post := func(t *testing.T, path, body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("POST %s = %d, want 204 (%q)", path, rec.Code, rec.Body.String())
		}
	}

	// Two sessions arrive as hooks would deliver them.
	post(t, SessionPath, `{"session_id":"a","event":"started","provider":"claude-code"}`)
	post(t, SessionPath, `{"session_id":"b","event":"blocked","provider":"claude-code"}`)

	if got := r.Status().Aggregate; got != stoplight.ColorRed {
		t.Fatalf("Aggregate = %v, want red", got)
	}

	// A poll taken now sees only "a".
	observed := time.Now().UTC().Format(time.RFC3339Nano)
	post(t, SyncPath, `{"provider":"claude-code","observed_at":"`+observed+`","sessions":[{"session_id":"a","event":"started"}]}`)

	status := r.Status()
	if len(status.Sessions) != 1 {
		t.Fatalf("len(Sessions) = %d, want 1", len(status.Sessions))
	}
	if status.Sessions[0].ID != "a" {
		t.Errorf("surviving session = %q, want a", status.Sessions[0].ID)
	}
	if status.Aggregate != stoplight.ColorYellow {
		t.Errorf("Aggregate = %v, want yellow: the red session was declared gone", status.Aggregate)
	}
}

// fakePoller stands in for an adapter that can be asked for its full session
// list.
type fakePoller struct {
	provider string
	sessions []stoplight.Report
	err      error

	mu    sync.Mutex
	calls int
}

func (f *fakePoller) Name() string             { return "fake" }
func (f *fakePoller) Install(string) error     { return nil }
func (f *fakePoller) Uninstall() error         { return nil }
func (f *fakePoller) Installed() (bool, error) { return true, nil }
func (f *fakePoller) Provider() string         { return f.provider }
func (f *fakePoller) callCount() int           { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fakePoller) Poll(context.Context) ([]stoplight.Report, time.Time, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.err != nil {
		return nil, time.Time{}, f.err
	}
	return f.sessions, time.Now(), nil
}

// The relay syncs once at startup rather than waiting out the first interval.
// State is held in memory and lost on restart, and frames are sent only on
// change, so a session merely sitting idle would otherwise be invisible until
// it next moved.
func TestRunPollsAtStartup(t *testing.T) {
	poller := &fakePoller{
		provider: "claude-code",
		sessions: []stoplight.Report{{SessionID: "revived", Event: "blocked"}},
	}

	tr := &fakeTransport{}
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: time.Hour,
		PollInterval:  time.Hour, // long, so only the startup poll can fire
		Pollers:       []adapter.Poller{poller},
		LogPath:       t.TempDir() + "/relay.log",
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	deadline := time.After(3 * time.Second)
	for {
		if poller.callCount() > 0 && len(r.Status().Sessions) == 1 {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("no startup poll: calls=%d sessions=%d", poller.callCount(), len(r.Status().Sessions))
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := r.Status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("Aggregate = %v, want red: the polled session was blocked", got)
	}

	cancel()
	<-done
}

// A poll that fails must leave the tracker alone. RFC 1 section 5.4 separates
// an empty answer from no answer: conflating them would clear the whole light
// every time the agent could not be asked.
func TestRunPollFailureLeavesSessionsAlone(t *testing.T) {
	poller := &fakePoller{provider: "claude-code", err: errors.New("command not found")}

	tr := &fakeTransport{}
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: time.Hour,
		PollInterval:  10 * time.Millisecond,
		Pollers:       []adapter.Poller{poller},
		LogPath:       t.TempDir() + "/relay.log",
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// A session the hooks reported.
	r.tracker.Apply(stoplight.Report{
		SessionID: "held",
		Event:     "blocked",
		Provider:  "claude-code",
	}, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()

	// Let several failing polls go by.
	for poller.callCount() < 3 {
		select {
		case <-time.After(2 * time.Second):
			cancel()
			<-done
			t.Fatalf("poller was called %d times, want at least 3", poller.callCount())
		case <-time.After(5 * time.Millisecond):
		}
	}

	status := r.Status()
	if len(status.Sessions) != 1 {
		t.Errorf("len(Sessions) = %d, want 1: a failed poll must not remove anything", len(status.Sessions))
	}
	if status.Aggregate != stoplight.ColorRed {
		t.Errorf("Aggregate = %v, want red", status.Aggregate)
	}

	cancel()
	<-done
}

// With no pollers the relay must never poll, and must not spin: the ticker is
// one that never fires rather than a short interval with a guard inside.
func TestRunWithNoPollersDoesNotPoll(t *testing.T) {
	tr := &fakeTransport{}
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: 5 * time.Millisecond,
		LogPath:       t.TempDir() + "/relay.log",
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Running to completion without a panic or a deadlock is the assertion.
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run() error = %v", err)
	}
}
