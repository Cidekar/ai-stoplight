package relay

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// sendSocket writes one JSON object to the socket and closes it, which is the
// whole protocol from RFC 1 section 4.2.
func sendSocket(t *testing.T, path, body string) {
	t.Helper()
	conn, err := netDial(path)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()
	// A test must never block on a socket write, however the relay misbehaves.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set socket deadline: %v", err)
	}
	if _, err := conn.Write([]byte(body)); err != nil {
		t.Fatalf("write socket: %v", err)
	}
	// Half-close so the decoder sees EOF rather than waiting for more.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		unixConn.CloseWrite()
	}
}

// startRelay runs a relay and returns its HTTP address, its socket path, and a
// stop function.
func startRelay(t *testing.T, tr *fakeTransport) (addr, socket string, stop func()) {
	t.Helper()
	socket = tempSocket(t)
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    socket,
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr = waitForAddr(t, r)

	return addr, socket, func() {
		cancel()
		<-done
	}
}

// waitForSessions polls until the tracker holds n sessions, because socket
// ingest is asynchronous: the write returns before the relay has applied it.
// The budget is generous because it costs nothing on a passing run: this
// returns the moment the count matches. It only bites on a loaded machine,
// where a tight deadline turns "the relay had not been scheduled yet" into a
// failure that reads like a lost report.
func waitForSessions(t *testing.T, r *Relay, n int) []stoplight.Session {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		sessions := r.Status().Sessions
		if len(sessions) == n {
			return sessions
		}
		select {
		case <-deadline:
			t.Fatalf("wanted %d sessions, have %d", n, len(sessions))
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// waitForState polls until the named session reaches want. Socket ingest is
// asynchronous, so the write returning does not mean the relay has applied it.
//
// The budget is generous for the same reason as waitForSessions: a convergence
// loop that succeeds immediately on a healthy run pays nothing for a long
// deadline, and a short one only manufactures failures under load.
func waitForState(t *testing.T, r *Relay, id string, want stoplight.State) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		for _, s := range r.Status().Sessions {
			if s.ID == id && s.State == want {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("session %q never reached state %v", id, want)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func TestSocketIngestAppliesReport(t *testing.T) {
	tr := &fakeTransport{connected: true}
	socket := tempSocket(t)
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    socket,
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	// started before blocked, as RFC 1 section 3 requires of producers.
	sendSocket(t, socket, `{"session_id":"sock1","event":"started","label":"api"}`)
	waitForSessions(t, r, 1)
	sendSocket(t, socket, `{"session_id":"sock1","event":"blocked","label":"api"}`)
	waitForState(t, r, "sock1", stoplight.StateBlocked)

	sessions := waitForSessions(t, r, 1)
	if sessions[0].ID != "sock1" {
		t.Errorf("session ID = %q, want sock1", sessions[0].ID)
	}
	if got := r.Status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red", got)
	}

	cancel()
	<-done
}

// The two listeners share one decode-and-apply path, so the same payload must
// produce identical tracker state whichever way it arrived.
func TestSocketAndHTTPProduceIdenticalState(t *testing.T) {
	// The full sequence a real producer sends, so the comparison covers a
	// state transition rather than just session creation.
	started := `{"session_id":"same","event":"started","label":"auth-api","provider":"deepseek","cwd":"/tmp/x"}`
	blocked := `{"session_id":"same","event":"blocked","label":"auth-api","provider":"deepseek","cwd":"/tmp/x"}`

	// Over HTTP.
	httpRelay, _ := newHandlerRelay(t)
	httpAddr, _, stopHTTP := startRelayFor(t, httpRelay)
	postSession(t, httpAddr, started)
	postSession(t, httpAddr, blocked)
	httpSessions := waitForSessions(t, httpRelay, 1)
	httpAggregate := httpRelay.Status().Aggregate
	stopHTTP()

	// Over the socket.
	socketRelay, _ := newHandlerRelay(t)
	_, socketPath, stopSocket := startRelayFor(t, socketRelay)
	sendSocket(t, socketPath, started)
	waitForState(t, socketRelay, "same", stoplight.StateWorking)
	sendSocket(t, socketPath, blocked)
	waitForState(t, socketRelay, "same", stoplight.StateBlocked)
	socketSessions := waitForSessions(t, socketRelay, 1)
	socketAggregate := socketRelay.Status().Aggregate
	stopSocket()

	// Both must have reached red through the same path.
	if httpAggregate != stoplight.ColorRed {
		t.Errorf("http aggregate = %v, want red", httpAggregate)
	}

	if httpAggregate != socketAggregate {
		t.Errorf("aggregate differs: http %v, socket %v", httpAggregate, socketAggregate)
	}
	h, s := httpSessions[0], socketSessions[0]
	if h.ID != s.ID {
		t.Errorf("ID differs: http %q, socket %q", h.ID, s.ID)
	}
	if h.Label != s.Label {
		t.Errorf("Label differs: http %q, socket %q", h.Label, s.Label)
	}
	if h.Provider != s.Provider {
		t.Errorf("Provider differs: http %q, socket %q", h.Provider, s.Provider)
	}
	if h.State != s.State {
		t.Errorf("State differs: http %v, socket %v", h.State, s.State)
	}
	if h.Dir != s.Dir {
		t.Errorf("Dir differs: http %q, socket %q", h.Dir, s.Dir)
	}
}

// startRelayFor runs an already-built relay, so a test can hold the pointer and
// inspect its state.
func startRelayFor(t *testing.T, r *Relay) (addr, socket string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr = waitForAddr(t, r)
	return addr, r.cfg.SocketPath, func() {
		cancel()
		<-done
	}
}

// A malformed payload on the socket is logged and dropped. It must not take
// the listener down, because the next producer still needs it.
func TestSocketSurvivesMalformedPayload(t *testing.T) {
	tr := &fakeTransport{connected: true}
	addr, socket, stop := startRelay(t, tr)
	defer stop()

	sendSocket(t, socket, `{"session_id":`)
	sendSocket(t, socket, `not json at all`)
	sendSocket(t, socket, `{"event":"blocked"}`) // missing session_id

	// The listener is still alive and serving.
	sendSocket(t, socket, `{"session_id":"good","event":"blocked"}`)

	deadline := time.After(5 * time.Second)
	for tr.sendCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("socket stopped working after malformed payloads")
		case <-time.After(2 * time.Millisecond):
		}
	}

	// HTTP is unaffected too.
	if code := postSession(t, addr, `{"session_id":"http","event":"started"}`); code != 204 {
		t.Errorf("http status = %d, want 204", code)
	}
}

// An unknown event over the socket is ignored, exactly as over HTTP.
func TestSocketIgnoresUnknownEvent(t *testing.T) {
	tr := &fakeTransport{connected: true}
	_, socket, stop := startRelay(t, tr)
	defer stop()

	sendSocket(t, socket, `{"session_id":"a","event":"warp_drive"}`)

	// Give the relay a chance to have applied it, then confirm it did not.
	time.Sleep(50 * time.Millisecond)
	if got := tr.sendCount(); got != 0 {
		t.Errorf("send count = %d, want 0", got)
	}
}

// The socket is created with owner-only permissions: it can drive the light.
func TestSocketPermissions(t *testing.T) {
	socket := tempSocket(t)
	listener, err := listenSocket(socket)
	if err != nil {
		t.Fatalf("listenSocket: %v", err)
	}
	defer listener.Close()

	info, err := osStat(socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}
}

// A connection that opens and says nothing must not pin a goroutine forever.
func TestSocketReadDeadlineReleasesSilentConnection(t *testing.T) {
	tr := &fakeTransport{connected: true}
	_, socket, stop := startRelay(t, tr)
	defer stop()

	conn, err := netDial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Hold it open without writing, then confirm the relay still serves. The
	// deadline is the test's own safety net: the relay's read deadline is what
	// is under test, so this side must not be what keeps the pair alive.
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set socket deadline: %v", err)
	}
	defer conn.Close()

	sendSocket(t, socket, `{"session_id":"b","event":"blocked"}`)
	deadline := time.After(5 * time.Second)
	for tr.sendCount() == 0 {
		select {
		case <-deadline:
			t.Fatal("a silent connection blocked the listener")
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// The socket honours the same 8KB cap as HTTP.
//
// Both shapes matter, and the second is the one that used to get through:
//
//   - Junk INSIDE the JSON, so the object never parses. This passed even when
//     the cap did nothing, because the decode failed on its own. Asserting only
//     this was why the old version of this test reported a working cap that was
//     in fact bypassable.
//   - A COMPLETE, valid object followed by junk. The decoder stops at the end
//     of the object, so nothing ever reads far enough to notice the size, and
//     the payload used to be truncated to its valid prefix and applied.
func TestSocketRejectsOversizedBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// Oversized and unparseable: the weak case.
			name: "oversize inside the json",
			body: `{"session_id":"a","event":"blocked","label":"` + strings.Repeat("x", MaxBodyBytes*2) + `"}`,
		},
		{
			// Oversized with a valid prefix: the bypass.
			name: "valid object then junk",
			body: `{"session_id":"a","event":"blocked"}` + strings.Repeat("x", MaxBodyBytes*2),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeTransport{connected: true}
			r, socket, stop := startRelayReturningRelay(t, tr)
			defer stop()

			// Write directly rather than through sendSocket: the relay stops
			// reading at the cap and closes, so the tail of an oversized body
			// reliably fails with EPIPE. That broken pipe is the cap working,
			// not a test failure.
			conn, err := netDial(socket)
			if err != nil {
				t.Fatalf("dial socket: %v", err)
			}
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatalf("set socket deadline: %v", err)
			}
			if _, err := conn.Write([]byte(tc.body)); err != nil {
				t.Logf("write cut short by the relay, as expected: %v", err)
			}
			conn.Close()

			time.Sleep(100 * time.Millisecond)

			// No send is necessary but not sufficient: a truncated body that
			// fails to parse also sends nothing. The tracker is what proves
			// the payload was REJECTED rather than quietly applied.
			if got := tr.sendCount(); got != 0 {
				t.Errorf("send count = %d, want 0: an oversized body reached the transport", got)
			}
			if got := r.tracker.Sessions(); len(got) != 0 {
				t.Errorf("tracker holds %d session(s), want 0: the oversized body was applied", len(got))
			}
		})
	}
}

// startRelayReturningRelay is startRelay plus the relay itself, so a test can
// assert on tracker state and not just on what reached the transport.
func startRelayReturningRelay(t *testing.T, tr *fakeTransport) (r *Relay, socket string, stop func()) {
	t.Helper()
	socket = tempSocket(t)
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    socket,
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	return r, socket, func() {
		cancel()
		<-done
	}
}
