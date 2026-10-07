package relay

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// Regression tests for the relay's lifecycle and resource handling: HTTP
// deadlines, transport reconnection, re-entrant Run, socket unlinking, accept
// backoff, and cleanup after a failed bind. Each one fails against the code as
// it was before the corresponding fix.

// ---------------------------------------------------------------------------
// 1. The HTTP server bounds a slow request.
// ---------------------------------------------------------------------------

// A client that sends complete headers promising a body, then one byte and
// silence, must be released by the server. Before ReadTimeout was set it held
// a goroutine and a descriptor until the process exited.
//
// This is the HTTP twin of TestSocketReadDeadlineReleasesSilentConnection. The
// timeouts are cut to milliseconds through Config so the test is fast; the
// production values are the defaults exercised by every other test here.
func TestHTTPReadTimeoutReleasesStalledRequest(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	r.cfg.httpTimeouts = httpTimeouts{
		readHeader: 200 * time.Millisecond,
		read:       300 * time.Millisecond,
		write:      300 * time.Millisecond,
		idle:       300 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Complete headers promising 100 bytes, then a single byte of body and
	// nothing more. This is the exact shape that used to park a goroutine:
	// ReadHeaderTimeout is satisfied, so only a whole-request deadline can
	// break the stall.
	request := "POST " + SessionPath + " HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 100\r\n" +
		"\r\n" +
		"{"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// The server must hang up on its own. The read deadline here is the test's
	// safety net and is far longer than the server's, so a failure means the
	// server never released the connection rather than that this side gave up.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf[:])
		if err != nil {
			// EOF or a reset is the server releasing us, which is the point.
			// A timeout on our own deadline is the failure.
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				t.Fatal("server never released a stalled request: ReadTimeout is not set")
			}
			break
		}
		if n == 0 {
			break
		}
		// A 408 body counts as a release too; keep reading until EOF.
	}

	// The relay is still healthy: the stalled connection took nothing with it.
	if code := postSession(t, addr, `{"session_id":"after","event":"blocked"}`); code != 204 {
		t.Errorf("status after a stalled request = %d, want 204", code)
	}

	cancel()
	<-done
}

// The production defaults are all set, not just the header one. A test that
// overrides them cannot prove the shipped configuration is bounded, so assert
// the defaults directly.
func TestHTTPTimeoutDefaultsAreAllSet(t *testing.T) {
	got := httpTimeouts{}.withDefaults()

	if got.readHeader != defaultReadHeaderTimeout {
		t.Errorf("readHeader = %v, want %v", got.readHeader, defaultReadHeaderTimeout)
	}
	if got.read != defaultReadTimeout {
		t.Errorf("read = %v, want %v", got.read, defaultReadTimeout)
	}
	if got.write != defaultWriteTimeout {
		t.Errorf("write = %v, want %v", got.write, defaultWriteTimeout)
	}
	if got.idle != defaultIdleTimeout {
		t.Errorf("idle = %v, want %v", got.idle, defaultIdleTimeout)
	}

	// A body deadline that does not cover the headers would be nonsense.
	if got.read < got.readHeader {
		t.Errorf("read %v is shorter than readHeader %v", got.read, got.readHeader)
	}

	// An override must survive withDefaults untouched.
	custom := httpTimeouts{readHeader: time.Second, read: 2 * time.Second, write: 3 * time.Second, idle: 4 * time.Second}
	if custom.withDefaults() != custom {
		t.Errorf("withDefaults changed a fully specified value: %+v", custom.withDefaults())
	}
}

// ---------------------------------------------------------------------------
// 2. The relay reconnects a transport that dropped.
// ---------------------------------------------------------------------------

// droppingTransport imitates the serial transport's real failure mode: a Send
// that fails drops the handle, so Connected reports false until something
// calls Connect again. Nothing in the relay used to do that, so the light
// stayed dark until the process restarted.
type droppingTransport struct {
	mu        sync.Mutex
	frames    []stoplight.Frame
	connected bool

	// failNextSend makes the next Send fail and disconnect, once.
	failNextSend bool

	// failNextSendKeepConnected makes the next Send fail once while leaving the
	// link up. It is the transport contract's "this frame was lost, not that
	// the transport is dead" case, which a dropped handle does not model: there
	// is no edge for the loop to see, so only an unacked-frame resend recovers
	// it.
	failNextSendKeepConnected bool

	connects atomic.Int64
	sends    atomic.Int64
	closes   atomic.Int64

	// connectErr, while set, makes Connect fail. It stands in for a light that
	// is still unplugged when the reconnect attempt runs.
	connectErr error
}

func (d *droppingTransport) Connect(ctx context.Context) error {
	d.connects.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.connectErr != nil {
		return d.connectErr
	}
	d.connected = true
	return nil
}

func (d *droppingTransport) Send(f stoplight.Frame) error {
	d.sends.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failNextSend {
		// Exactly what serial.Send does on a write error: drop the handle so
		// Connected tells the truth, and leave the transport reopenable.
		d.failNextSend = false
		d.connected = false
		return errors.New("write to device failed")
	}
	if d.failNextSendKeepConnected {
		// A transient write error that does not disconnect: the frame is lost
		// but the link is still up.
		d.failNextSendKeepConnected = false
		return errors.New("transient write error")
	}
	if !d.connected {
		return errors.New("not connected")
	}
	d.frames = append(d.frames, f)
	return nil
}

func (d *droppingTransport) Connected() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connected
}

func (d *droppingTransport) Close() error { d.closes.Add(1); return nil }
func (d *droppingTransport) Name() string { return "dropping" }

func (d *droppingTransport) setFailNextSend() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNextSend = true
}

func (d *droppingTransport) setFailNextSendKeepConnected() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNextSendKeepConnected = true
}

func (d *droppingTransport) setConnectErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connectErr = err
}

func (d *droppingTransport) frameCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.frames)
}

func (d *droppingTransport) lastFrame() (stoplight.Frame, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.frames) == 0 {
		return stoplight.Frame{}, false
	}
	return d.frames[len(d.frames)-1], true
}

// newFastReconnectRelay builds a relay whose reconnect poll fires quickly, so
// a reconnect test finishes in well under the suite's budget.
func newFastReconnectRelay(t *testing.T, tr *droppingTransport) *Relay {
	t.Helper()
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	return r
}

// Unplug the light and plug it back in: the relay must reopen the device and
// push the current frame. Before the fix Connect had one call site that ran
// once at startup, so a transport that dropped after a failed Send stayed
// disconnected for the life of the process.
func TestTransportReconnectsAfterFailedSend(t *testing.T) {
	tr := &droppingTransport{}
	r := newFastReconnectRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	// The first connect happens at startup.
	waitFor(t, 5*time.Second, "initial connect", func() bool {
		return tr.connects.Load() >= 1
	})
	connectsAfterStart := tr.connects.Load()

	// A frame goes out normally.
	if code := postSession(t, addr, `{"session_id":"a","event":"started","label":"auth"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}
	waitFor(t, 5*time.Second, "first frame", func() bool { return tr.frameCount() >= 1 })

	// Now the board is unplugged: the next Send fails and drops the handle.
	tr.setFailNextSend()
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked","label":"auth"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}
	waitFor(t, 5*time.Second, "transport to report disconnected", func() bool {
		return !tr.Connected()
	})
	framesWhileDown := tr.frameCount()

	// The relay must call Connect again on its own. Nothing else in this test
	// touches the transport, so a second connect can only come from the relay.
	waitFor(t, 10*time.Second, "a second Connect after the drop", func() bool {
		return tr.connects.Load() > connectsAfterStart
	})

	// And the light must catch up: a frame is delivered after recovery.
	waitFor(t, 10*time.Second, "a frame after recovery", func() bool {
		return tr.frameCount() > framesWhileDown
	})

	if !tr.Connected() {
		t.Error("transport still reports disconnected after recovery")
	}
	frame, ok := tr.lastFrame()
	if !ok {
		t.Fatal("no frame recorded")
	}
	if frame.Color != stoplight.ColorRed {
		t.Errorf("recovered frame colour = %v, want red: the light must show current state", frame.Color)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A Send that fails while the link stays up loses the frame, and the relay
// must resend it on its own. Before the fix a transient write error was logged
// and forgotten: the lamp held the previous colour until the tracker next
// moved, because the transport never disconnected and so no reconnect edge
// pushed the frame again.
func TestLostFrameIsResentWhileConnected(t *testing.T) {
	tr := &droppingTransport{}
	r := newFastReconnectRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	waitFor(t, 5*time.Second, "initial connect", func() bool {
		return tr.connects.Load() >= 1
	})

	// The next send fails but the link stays up, so there is no edge to notice.
	tr.setFailNextSendKeepConnected()
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked","label":"auth"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}

	// Nothing else moves the tracker, so the red frame can only reach the light
	// if the relay retries the lost one.
	waitFor(t, 10*time.Second, "the lost frame to be resent", func() bool {
		f, ok := tr.lastFrame()
		return ok && f.Color == stoplight.ColorRed
	})

	cancel()
	<-done
}

// After the first connect the relay must push the current frame once, even
// though no state changed. Before the fix startConnect(retry=false) never
// transmitted and the loop only resent on a disconnected-to-connected edge, so
// a light that was already up at startup held its boot state until the first
// ingest.
func TestFirstFrameSentAfterStartupConnect(t *testing.T) {
	tr := &droppingTransport{}
	r := newFastReconnectRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	// No post, no state change: a frame still reaches the light because the
	// first reconnect tick sees a connected transport that has not acked.
	waitFor(t, 10*time.Second, "the startup frame to be sent", func() bool {
		return tr.frameCount() >= 1
	})

	cancel()
	<-done
}

// A reconnect that keeps failing must be logged and retried, never propagated.
// Run returning an error because the transport failed is the one thing this
// package must never do.
func TestFailingReconnectNeverEndsRun(t *testing.T) {
	tr := &droppingTransport{connectErr: errors.New("device is gone")}
	r := newFastReconnectRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	// Several reconnect attempts fail while the relay keeps serving.
	waitFor(t, 10*time.Second, "repeated reconnect attempts", func() bool {
		return tr.connects.Load() >= 3
	})

	select {
	case err := <-done:
		t.Fatalf("Run returned while reconnection was failing: %v", err)
	default:
	}

	// Ingest is unaffected by a light that will not come back.
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != 204 {
		t.Errorf("status = %d, want 204: a dead transport must not break ingest", code)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Only one Connect may be in flight at a time. A slow connect that outlasts
// several reconnect ticks must not have a second attempt stacked on top of it,
// because two opens of one device is worse than none.
func TestOnlyOneReconnectInFlight(t *testing.T) {
	release := make(chan struct{})
	tr := &slowConnectTransport{release: release}

	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
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

	// Wait out several reconnect ticks with the first connect still blocked.
	waitFor(t, 5*time.Second, "the first connect to be entered", func() bool {
		return tr.inFlight.Load() == 1
	})
	time.Sleep(3 * reconnectPollInterval)

	if got := tr.maxInFlight.Load(); got > 1 {
		t.Errorf("max concurrent Connect calls = %d, want 1", got)
	}
	if got := tr.connects.Load(); got != 1 {
		t.Errorf("Connect called %d times while one was still in flight, want 1", got)
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// slowConnectTransport blocks in Connect until released, and records how many
// calls overlapped. It stands in for a light that is slow to open, so the
// whole point is that Connect is still running when the next reconnect tick
// fires.
type slowConnectTransport struct {
	release chan struct{}

	connects    atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	connected   atomic.Bool
}

func (s *slowConnectTransport) Connect(ctx context.Context) error {
	s.connects.Add(1)
	n := s.inFlight.Add(1)
	for {
		max := s.maxInFlight.Load()
		if n <= max || s.maxInFlight.CompareAndSwap(max, n) {
			break
		}
	}
	defer s.inFlight.Add(-1)

	select {
	case <-s.release:
		// Released means the device really did open, so Connected must say so.
		// Returning nil while Connected() stayed false described a transport
		// that cannot exist, and it is the same defect that made the relay's
		// reconnect loop retry on every tick elsewhere in this suite. The
		// assertions here all run while Connect is still blocked above, so
		// this only affects the shutdown path, but the fake should not model
		// an impossible transport.
		s.connected.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *slowConnectTransport) Send(stoplight.Frame) error { return nil }
func (s *slowConnectTransport) Connected() bool            { return s.connected.Load() }
func (s *slowConnectTransport) Close() error               { return nil }
func (s *slowConnectTransport) Name() string               { return "slow" }

// ---------------------------------------------------------------------------
// 3. Run is not re-entrant.
// ---------------------------------------------------------------------------

// A second Run on a live Relay must be refused. Both used to bind (port 0
// makes that succeed), both wrote r.listener and r.socket, and the second
// listenSocket unlinked the socket file the first was serving. Whichever
// returned first closed the shared transport and log file under the other.
func TestConcurrentRunIsRefused(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := make(chan error, 1)
	go func() { first <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	// Remember what the first Run bound, so the second cannot be seen to have
	// replaced it.
	r.mu.Lock()
	firstListener := r.listener
	firstSocket := r.socket
	r.mu.Unlock()

	second := make(chan error, 1)
	go func() { second <- r.Run(ctx) }()

	select {
	case err := <-second:
		if err == nil {
			t.Fatal("a second concurrent Run returned nil, want an error")
		}
		if !errors.Is(err, ErrAlreadyRunning) {
			t.Errorf("second Run error = %v, want ErrAlreadyRunning", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second concurrent Run never returned: it is serving alongside the first")
	}

	// The first Run is untouched: same listeners, still serving.
	r.mu.Lock()
	sameListener := r.listener == firstListener
	sameSocket := r.socket == firstSocket
	r.mu.Unlock()
	if !sameListener {
		t.Error("the refused Run replaced the live HTTP listener")
	}
	if !sameSocket {
		t.Error("the refused Run replaced the live socket listener")
	}

	select {
	case err := <-first:
		t.Fatalf("the first Run exited when the second was refused: %v", err)
	default:
	}

	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != 204 {
		t.Errorf("http status = %d, want 204: the first relay must still serve", code)
	}
	// The socket the first Run bound is still live, not unlinked by the second.
	sendSocket(t, r.cfg.SocketPath, `{"session_id":"b","event":"started"}`)
	waitForSessions(t, r, 2)

	cancel()
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("first Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first Run did not return after cancel")
	}
}

// Many simultaneous Runs: exactly one may serve.
func TestOnlyOneRunSucceeds(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const attempts = 6
	results := make(chan error, attempts)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < attempts; i++ {
		go func() {
			start.Wait()
			results <- r.Run(ctx)
		}()
	}
	start.Done()

	// All but one must be refused promptly.
	refused := 0
	deadline := time.After(10 * time.Second)
	for refused < attempts-1 {
		select {
		case err := <-results:
			if err == nil {
				t.Fatal("a Run returned nil before cancel, so two were serving")
			}
			if !errors.Is(err, ErrAlreadyRunning) {
				t.Fatalf("refusal error = %v, want ErrAlreadyRunning", err)
			}
			refused++
		case <-deadline:
			t.Fatalf("only %d of %d Runs were refused", refused, attempts-1)
		}
	}

	// The survivor is serving.
	addr := waitForAddr(t, r)
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != 204 {
		t.Errorf("status = %d, want 204", code)
	}

	cancel()
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("the surviving Run = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the surviving Run did not return after cancel")
	}
}

// After a clean shutdown, Status must not report a uptime that keeps growing,
// and a fresh Run must not log to the file the previous one closed.
func TestRunClearsStateOnShutdown(t *testing.T) {
	tr := &fakeTransport{connected: true}
	logPath := filepath.Join(t.TempDir(), "relay.log")
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: 10 * time.Millisecond,
		LogPath:       logPath,
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	if r.Status().Uptime <= 0 {
		t.Error("uptime while running = 0, want positive")
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	// started is cleared, so uptime does not keep climbing for a stopped relay.
	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime after shutdown = %v, want 0", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime kept growing after shutdown: %v", got)
	}

	// logFile is nil'd and the underlying file closed, but r.logger still wraps
	// that closed descriptor. Clearing the handle is therefore NOT what makes a
	// second Run safe: this assertion used to claim it was, which cemented the
	// bug it was meant to guard. What makes it safe is that the relay is
	// one-shot, asserted below.
	r.mu.Lock()
	logFile := r.logFile
	running := r.running
	ran := r.ran
	r.mu.Unlock()
	if logFile != nil {
		t.Error("logFile is still set after shutdown, so it was never closed")
	}
	if running {
		t.Error("running is still set after shutdown")
	}
	if !ran {
		t.Error("ran is not set after a Run that served, so a second Run would be allowed")
	}

	// The real guarantee: a second Run is refused rather than silently serving
	// with a closed log file and double-closing the transport.
	if err := r.Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second Run after a clean shutdown = %v, want ErrAlreadyRunning", err)
	}
	if got := tr.closes.Load(); got != 1 {
		t.Errorf("transport closed %d times, want exactly 1", got)
	}
}

// ---------------------------------------------------------------------------
// 4. Shutdown does not unlink a socket that belongs to someone else.
// ---------------------------------------------------------------------------

// Closing the listener already unlinks the path, so the extra os.Remove could
// only delete a file another process created. A restart makes that reachable:
// relay B binds the path while A is still draining, and A's remove kills B's
// ingest.
func TestShutdownDoesNotUnlinkAnotherRelaysSocket(t *testing.T) {
	socket := tempSocket(t)

	newRelayOn := func() *Relay {
		t.Helper()
		r, err := NewRelay(Config{
			Transport:     &fakeTransport{connected: true},
			ListenAddr:    "127.0.0.1:0",
			SocketPath:    socket,
			SweepInterval: 10 * time.Millisecond,
			LogPath:       filepath.Join(t.TempDir(), "relay.log"),
		})
		if err != nil {
			t.Fatalf("NewRelay: %v", err)
		}
		return r
	}

	// Relay A starts and serves.
	relayA := newRelayOn()
	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	go func() { doneA <- relayA.Run(ctxA) }()
	waitForAddr(t, relayA)
	sendSocket(t, socket, `{"session_id":"a","event":"started"}`)
	waitForSessions(t, relayA, 1)

	// A is told to stop, exactly as `stoplight restart` does.
	cancelA()
	if err := <-doneA; err != nil {
		t.Fatalf("relay A Run: %v", err)
	}

	// Closing the listener unlinked the path on its own. That is the whole
	// reason the extra remove was unnecessary.
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file survived a clean shutdown: stat err = %v", err)
	}

	// Relay B takes over the path.
	relayB := newRelayOn()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneB := make(chan error, 1)
	go func() { doneB <- relayB.Run(ctxB) }()
	waitForAddr(t, relayB)

	// B's socket is live and stays live: A is gone and can no longer reach in
	// and unlink it.
	sendSocket(t, socket, `{"session_id":"b","event":"started"}`)
	waitForSessions(t, relayB, 1)

	if _, err := os.Stat(socket); err != nil {
		t.Errorf("relay B's socket file is missing: %v", err)
	}
	if conn, err := netDial(socket); err != nil {
		t.Errorf("relay B's socket stopped accepting: %v", err)
	} else {
		conn.Close()
	}

	cancelB()
	if err := <-doneB; err != nil {
		t.Fatalf("relay B Run: %v", err)
	}
}

// The interleaving that made the old code dangerous, driven directly: A is
// still inside Run's shutdown when B binds the same path. A must not remove
// B's socket.
//
// A's drain is stretched by a socket connection that is accepted and then held
// open, so A sits in wg.Wait while B starts.
func TestSlowShutdownDoesNotUnlinkTheNextRelaysSocket(t *testing.T) {
	socket := tempSocket(t)

	relayA, err := NewRelay(Config{
		Transport:     &fakeTransport{connected: true},
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    socket,
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay A: %v", err)
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	doneA := make(chan error, 1)
	go func() { doneA <- relayA.Run(ctxA) }()
	waitForAddr(t, relayA)

	// Open a connection and send an incomplete object. A's handler blocks in
	// its read waiting for the rest until socketReadTimeout, so A's wg.Wait
	// during shutdown takes real time, which is the window B binds in.
	//
	// The accept does not need to be observed before cancelA below. Shutdown
	// closes the socket listener only AFTER loop returns (see Relay.Run), so
	// the accept loop is still running when cancelA fires and picks up this
	// already-queued connection regardless of timing. The previous
	// time.Sleep(50ms) tried to confirm the accept up front and was only a
	// guess: under CPU contention 50ms was not always enough, which is what
	// made this test flaky.
	stall, err := netDial(socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer stall.Close()
	if _, err := stall.Write([]byte("{")); err != nil {
		t.Fatalf("write to stall conn: %v", err)
	}

	cancelA()

	// While A drains, B binds the same path.
	relayB, err := NewRelay(Config{
		Transport:     &fakeTransport{connected: true},
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    socket,
		SweepInterval: 10 * time.Millisecond,
		LogPath:       filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay B: %v", err)
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	doneB := make(chan error, 1)
	go func() { doneB <- relayB.Run(ctxB) }()
	waitForAddr(t, relayB)

	// A finishes its shutdown after B is up.
	select {
	case err := <-doneA:
		if err != nil {
			t.Fatalf("relay A Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("relay A never finished shutting down")
	}

	// B's ingest must still work. Before the fix, A's os.Remove unlinked B's
	// live socket here and this send went nowhere.
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("relay A's shutdown removed relay B's socket file: %v", err)
	}
	sendSocket(t, socket, `{"session_id":"b","event":"started"}`)
	waitForSessions(t, relayB, 1)

	cancelB()
	if err := <-doneB; err != nil {
		t.Fatalf("relay B Run: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 5. The accept loop backs off instead of spinning.
// ---------------------------------------------------------------------------

// The backoff schedule: start small, double, cap, and reset on success.
//
// The full path is hard to drive portably, because it needs a non-transient
// accept error such as EMFILE and exhausting the descriptor table inside a
// test process is not something to do to a shared test binary. The schedule is
// unit tested here and the loop's use of it is covered by
// TestAcceptBackoffPacesARepeatedlyFailingListener below.
func TestNextAcceptDelay(t *testing.T) {
	cases := []struct {
		name    string
		current time.Duration
		want    time.Duration
	}{
		{"first failure", 0, minAcceptDelay},
		{"negative is treated as first", -time.Second, minAcceptDelay},
		{"doubles", minAcceptDelay, 2 * minAcceptDelay},
		{"doubles again", 2 * minAcceptDelay, 4 * minAcceptDelay},
		{"caps rather than overshooting", 600 * time.Millisecond, maxAcceptDelay},
		{"stays at the cap", maxAcceptDelay, maxAcceptDelay},
		{"never exceeds the cap", 2 * maxAcceptDelay, maxAcceptDelay},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextAcceptDelay(tc.current); got != tc.want {
				t.Errorf("nextAcceptDelay(%v) = %v, want %v", tc.current, got, tc.want)
			}
		})
	}

	// The schedule must actually reach the cap from a cold start, and must not
	// take so many steps that a real failure spins in the meantime.
	steps := 0
	d := time.Duration(0)
	for d < maxAcceptDelay {
		d = nextAcceptDelay(d)
		steps++
		if steps > 64 {
			t.Fatal("the backoff never reaches its cap")
		}
	}
	if minAcceptDelay <= 0 {
		t.Error("minAcceptDelay must be positive, or the loop still spins")
	}
	if maxAcceptDelay < minAcceptDelay {
		t.Error("maxAcceptDelay is below minAcceptDelay")
	}
}

// A listener whose Accept always fails must be paced rather than spun. The
// error is not net.ErrClosed and the context is live, which is exactly the
// EMFILE shape: without a backoff the loop calls Accept as fast as the CPU
// allows.
//
// The assertion is on call rate, not wall time: with a 5ms first delay
// doubling to a 1s cap, a few hundred milliseconds admits only a handful of
// attempts, where an unpaced loop makes many thousands.
func TestAcceptBackoffPacesARepeatedlyFailingListener(t *testing.T) {
	r, _ := newHandlerRelay(t)

	listener := &alwaysFailingListener{err: errors.New("accept: too many open files")}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.serveSocket(ctx, listener)
	}()

	// The "is it retrying at all" half of the assertion converges: it waits for
	// the second attempt rather than sleeping a fixed 300ms and hoping the
	// backoff's 5ms and 10ms steps both fit. On a loaded machine they need not,
	// and a fixed sleep would fail a correctly paced loop for being slow.
	waitFor(t, 10*time.Second, "the accept loop to retry after a failure", func() bool {
		return listener.accepts.Load() >= 2
	})

	// The "is it spinning" half is a RATE, so it needs a real elapsed window
	// and the count measured across it. Sampling wall time around the window
	// rather than assuming it is exactly 300ms keeps the bound honest when the
	// scheduler steals time from this goroutine: a window that overran would
	// otherwise admit more backoff steps than the budget was computed for and
	// read as a spin.
	start := time.Now()
	from := listener.accepts.Load()
	time.Sleep(300 * time.Millisecond)
	elapsed := time.Since(start)
	during := listener.accepts.Load() - from

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveSocket did not return after cancel")
	}

	// With a 5ms first delay doubling to a 1s cap, the most attempts any
	// correctly paced loop can fit into the window is bounded by the smallest
	// step, and generously so. An unpaced loop clears tens of thousands. The
	// bound is loose on purpose: it fails the spin and passes any sane pacing.
	maxAttempts := int64(elapsed/(5*time.Millisecond)) + 20
	if during > maxAttempts {
		t.Errorf("accept was called %d times in %v, want at most %d: the loop is spinning",
			during, elapsed.Round(time.Millisecond), maxAttempts)
	}
}

// A run of failures must not leave the accept loop paced slowly forever: the
// delay resets once a connection gets through.
//
// The earlier version of this test injected no failure at all, so there was no
// backoff to reset and it asserted nothing about the behaviour its name
// claims. This one drives the failures explicitly: enough consecutive errors
// to push the delay to its cap, then a success, then one more failure. If the
// delay reset, that last failure waits minAcceptDelay again; if it did not, it
// waits maxAcceptDelay, and the two are 200x apart.
func TestAcceptBackoffResetsAfterASuccess(t *testing.T) {
	r, _ := newHandlerRelay(t)

	listener := &scriptedListener{
		results:  make(chan scriptedResult, 16),
		done:     make(chan struct{}),
		asked:    make(chan struct{}, 1),
		accepted: make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan struct{})
	go func() {
		defer close(served)
		r.serveSocket(ctx, listener)
	}()

	fail := errors.New("accept: too many open files")

	// Enough failures to drive the delay to maxAcceptDelay.
	// 5ms,10,20,40,80,160,320,640,1s -> capped.
	for i := 0; i < 9; i++ {
		listener.results <- scriptedResult{err: fail}
	}

	// A connection gets through, which must reset the pacing.
	local, remote := net.Pipe()
	go func() {
		// The handler reads one object; give it one and close.
		local.SetDeadline(time.Now().Add(5 * time.Second))
		local.Write([]byte(`{"session_id":"reset","event":"started"}`))
		local.Close()
	}()
	listener.results <- scriptedResult{conn: remote}

	// Wait for that success to be accepted, so the reset has definitely run
	// before the next failure is timed.
	select {
	case <-listener.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the successful connection was never accepted")
	}

	// Now time a SHORT RUN of failures after the success.
	//
	// One failure would prove nothing: whether or not the delay was reset, the
	// first wait after a success is short in the broken case too, because the
	// doubling starts from whatever the variable held. Three consecutive
	// failures cost 5+10+20ms once the delay really reset, against three waits
	// pinned at the 1s cap if it did not.
	const runLength = 3

	// Drop any stale "asked" signal, so the timing below covers only the
	// retries that follow the failures queued here.
	select {
	case <-listener.asked:
	default:
	}

	start := time.Now()
	for i := 0; i < runLength; i++ {
		listener.results <- scriptedResult{err: fail}
		select {
		case <-listener.asked:
		case <-time.After(10 * time.Second):
			t.Fatal("the accept loop stopped asking after a failure")
		}
	}
	elapsed := time.Since(start)

	cancel()
	close(listener.done)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("serveSocket did not return after cancel")
	}

	// A reset delay spends about 35ms over these three failures. An unreset one
	// spends about 3s, pinned at the cap. The bound sits well clear of both.
	if limit := maxAcceptDelay; elapsed > limit {
		t.Errorf("the loop spent %v on %d failures after a success, want under %v: the backoff did not reset",
			elapsed, runLength, limit)
	}
}

// scriptedResult is one queued outcome for scriptedListener.Accept.
type scriptedResult struct {
	conn net.Conn
	err  error
}

// scriptedListener returns exactly the results a test queues, which is what
// makes the accept pacing observable: the test controls when a failure and a
// success happen, and watches how long the loop waits between them.
type scriptedListener struct {
	results chan scriptedResult
	done    chan struct{}

	// asked is signalled every time Accept is called, so a test can time the
	// gap between a failure and the retry.
	asked chan struct{}

	// accepted is signalled when Accept hands back a real connection.
	accepted chan struct{}
}

func (l *scriptedListener) Accept() (net.Conn, error) {
	// Non-blocking notifications, so a test that is not watching cannot stall
	// the loop.
	select {
	case l.asked <- struct{}{}:
	default:
	}

	select {
	case res := <-l.results:
		if res.err != nil {
			return nil, res.err
		}
		select {
		case l.accepted <- struct{}{}:
		default:
		}
		return res.conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *scriptedListener) Close() error { return nil }

func (l *scriptedListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "scripted", Net: "unix"}
}

// alwaysFailingListener returns a non-fatal error from every Accept, imitating
// a process at its descriptor limit.
type alwaysFailingListener struct {
	err     error
	accepts atomic.Int64
	closed  atomic.Bool
}

func (l *alwaysFailingListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	if l.closed.Load() {
		return nil, net.ErrClosed
	}
	return nil, l.err
}

func (l *alwaysFailingListener) Close() error {
	l.closed.Store(true)
	return nil
}

func (l *alwaysFailingListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "failing", Net: "unix"}
}

// ---------------------------------------------------------------------------
// 6. A failed bind leaves nothing behind.
// ---------------------------------------------------------------------------

// started used to be set before either listen, so a relay that could not bind
// still reported a growing uptime. The log file NewRelay opened was never
// closed on that path either.
func TestFailedBindLeavesNoUptimeOrOpenLog(t *testing.T) {
	// Occupy a port, then ask the relay for the same one.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	logPath := filepath.Join(t.TempDir(), "relay.log")
	r, err := NewRelay(Config{
		Transport:  &fakeTransport{},
		ListenAddr: busy.Addr().String(),
		SocketPath: tempSocket(t),
		LogPath:    logPath,
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	r.mu.Lock()
	opened := r.logFile
	r.mu.Unlock()
	if opened == nil {
		t.Fatal("NewRelay did not open a log file, so this test proves nothing")
	}

	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run on a busy port: want error, got nil")
	}

	// No uptime for a relay that never started.
	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime after a failed bind = %v, want 0", got)
	}
	time.Sleep(10 * time.Millisecond)
	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime kept growing after a failed bind: %v", got)
	}

	// The log file is closed and released.
	r.mu.Lock()
	remaining := r.logFile
	running := r.running
	r.mu.Unlock()
	if remaining != nil {
		t.Error("the log file was left open after a failed bind")
	}
	if running {
		t.Error("running was left set after a failed bind, so the relay can never start")
	}
	// Writing to the handle NewRelay opened must now fail, proving it was
	// really closed rather than merely forgotten.
	if _, err := opened.Write([]byte("x")); err == nil {
		t.Error("the log file is still writable after a failed bind, so it was not closed")
	}
}

// The socket bind is the second of the two, so its failure path must clean up
// just as the first one does, including closing the HTTP listener it already
// opened.
func TestFailedSocketBindLeavesNoUptimeOrOpenLog(t *testing.T) {
	// A socket path whose parent cannot be created: an existing regular file
	// standing where a directory would have to be.
	blocker := tempSocket(t, "blocker")
	if err := writeFile(blocker); err != nil {
		t.Fatalf("create blocker file: %v", err)
	}
	socketPath := filepath.Join(blocker, "sock")

	logPath := filepath.Join(t.TempDir(), "relay.log")
	r, err := NewRelay(Config{
		Transport:  &fakeTransport{},
		ListenAddr: "127.0.0.1:0",
		SocketPath: socketPath,
		LogPath:    logPath,
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	r.mu.Lock()
	opened := r.logFile
	r.mu.Unlock()

	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run with an unbindable socket path: want error, got nil")
	}

	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime after a failed socket bind = %v, want 0", got)
	}

	r.mu.Lock()
	remaining := r.logFile
	running := r.running
	r.mu.Unlock()
	if remaining != nil {
		t.Error("the log file was left open after a failed socket bind")
	}
	if running {
		t.Error("running was left set after a failed socket bind")
	}
	if opened != nil {
		if _, err := opened.Write([]byte("x")); err == nil {
			t.Error("the log file is still writable after a failed socket bind")
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitFor polls cond until it holds or the budget runs out. Every wait in this
// file is bounded, so a regression shows as a named failure rather than a hung
// suite.
func waitFor(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(budget)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out after %v waiting for %s", budget, what)
		case <-time.After(2 * time.Millisecond):
		}
	}
}
