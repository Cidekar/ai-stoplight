package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// fakeTransport counts frames and can be made to fail on every send, which is
// how the run loop's survival is tested.
type fakeTransport struct {
	mu        sync.Mutex
	frames    []stoplight.Frame
	sendErr   error
	connected bool

	sends      atomic.Int64
	closes     atomic.Int64
	connects   atomic.Int64
	connectErr error

	// blockConnect holds Connect until ctx is cancelled, imitating a light
	// that never appears.
	blockConnect bool

	// failConnects makes the first n Connect calls fail, after which they
	// succeed as normal. It exists so a test can place the
	// disconnected-to-connected edge STRICTLY AFTER the run loop has started.
	//
	// Run calls startConnect(retry=false) concurrently with loop, and loop
	// samples wasConnected as its first act. A fake whose very first Connect
	// succeeds races that sample: when the connect wins, loop begins with
	// wasConnected already true and never observes an edge at all. Failing the
	// startup connect removes the race, because the edge can then only happen
	// on a later tick that the loop is already watching.
	failConnects int
}

// Connect models the one contract every real transport keeps: a Connect that
// returns nil has opened the device, so Connected() reports true afterwards.
// droppingTransport in lifecycle_test.go and the serial transport both behave
// this way.
//
// This used to return nil while leaving connected false. That is a state no
// real transport reaches, and it made the relay's reconnect loop take its
// !connected branch on EVERY tick: each tick started a fresh retry Connect,
// each of which logged on success. Any assertion counting reconnect log lines
// then scaled with elapsed wall time rather than with relay behaviour.
//
// A transport that must never come back is modelled with blockConnect or
// connectErr, both of which correctly leave Connected() false because neither
// returns a nil error.
func (f *fakeTransport) Connect(ctx context.Context) error {
	n := f.connects.Add(1)
	if f.blockConnect {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.connectErr != nil {
		return f.connectErr
	}
	// An early attempt that is meant to fail leaves the transport disconnected,
	// which is exactly what a real transport does when the device is not there
	// yet. Returning an error is what keeps Connected() false honestly.
	if int64(f.failConnects) >= n {
		return errors.New("fake: device not ready")
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Send(frame stoplight.Frame) error {
	f.sends.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.frames = append(f.frames, frame)
	return nil
}

func (f *fakeTransport) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeTransport) setConnected(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = v
}

func (f *fakeTransport) Close() error { f.closes.Add(1); return nil }
func (f *fakeTransport) Name() string { return "fake" }

func (f *fakeTransport) sendCount() int { return int(f.sends.Load()) }

func (f *fakeTransport) lastFrame() (stoplight.Frame, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		return stoplight.Frame{}, false
	}
	return f.frames[len(f.frames)-1], true
}

// newTestRelay builds a relay on an unused loopback port with a socket in a
// temporary directory, so tests never touch the real ~/.local/state path.
func newTestRelay(t *testing.T, tr *fakeTransport) *Relay {
	t.Helper()
	if tr == nil {
		tr = &fakeTransport{}
	}
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

func TestNewRelayDefaults(t *testing.T) {
	r, err := NewRelay(Config{Transport: &fakeTransport{}})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	if r.cfg.SessionTimeout != DefaultSessionTimeout {
		t.Errorf("SessionTimeout = %v, want %v", r.cfg.SessionTimeout, DefaultSessionTimeout)
	}
	if r.cfg.SweepInterval != DefaultSweepInterval {
		t.Errorf("SweepInterval = %v, want %v", r.cfg.SweepInterval, DefaultSweepInterval)
	}
	if r.cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", r.cfg.ListenAddr, DefaultListenAddr)
	}
	if filepath.Base(r.cfg.SocketPath) != "sock" {
		t.Errorf("SocketPath = %q, want it to end in sock", r.cfg.SocketPath)
	}
}

func TestNewRelayRequiresTransport(t *testing.T) {
	if _, err := NewRelay(Config{}); err == nil {
		t.Fatal("NewRelay with no transport: want error, got nil")
	}
}

// A non-loopback listen address is a security failure, not a preference:
// RFC 1 section 12 forbids it without explicit configuration.
func TestNewRelayRejectsNonLoopback(t *testing.T) {
	// Find a real non-loopback address on this machine so the test is not
	// asserting on a name that happens not to resolve.
	var external string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("InterfaceAddrs: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			external = ipnet.IP.String()
			break
		}
	}

	cases := []struct {
		name string
		addr string
	}{
		{"wildcard", "0.0.0.0:7373"},
		{"ipv6 wildcard", "[::]:7373"},
		{"empty host", ":7373"},
		{"public ip", "8.8.8.8:7373"},
		{"no port", "127.0.0.1"},
	}
	if external != "" {
		cases = append(cases, struct {
			name string
			addr string
		}{"this machine's lan ip", net.JoinHostPort(external, "7373")})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRelay(Config{Transport: &fakeTransport{}, ListenAddr: tc.addr})
			if err == nil {
				t.Fatalf("NewRelay(%q): want error, got nil", tc.addr)
			}
		})
	}
}

func TestNewRelayAcceptsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7373", "localhost:7373", "[::1]:7373", "127.0.0.2:7373"} {
		t.Run(addr, func(t *testing.T) {
			_, err := NewRelay(Config{
				Transport:  &fakeTransport{},
				ListenAddr: addr,
				SocketPath: tempSocket(t),
			})
			if err != nil {
				t.Fatalf("NewRelay(%q): %v", addr, err)
			}
		})
	}
}

// The single most important behaviour in the relay: a transport that fails
// every send must not bring the process down.
func TestRunSurvivesTransportSendFailure(t *testing.T) {
	tr := &fakeTransport{sendErr: errors.New("radio is off"), connected: false}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	addr := waitForAddr(t, r)

	// Drive many state changes, every one of which fails to send.
	for i := range 20 {
		code := postSession(t, addr, fmt.Sprintf(`{"session_id":"s%d","event":"blocked"}`, i))
		if code != 204 {
			t.Fatalf("post %d: status = %d, want 204", i, code)
		}
	}

	// Converge on the sender having actually tried, rather than sleeping a
	// fixed 80ms and assuming the sender goroutine was scheduled inside it. On
	// a loaded machine it need not be, and the assertion below would then fail
	// a relay that was working correctly and merely slow.
	waitFor(t, 10*time.Second, "the relay to attempt a send", func() bool {
		return tr.sendCount() > 0
	})

	// Run must still be alive after all those failures. This is checked after
	// the convergence above so that "Run exited" is distinguished from "Run had
	// not got round to sending yet".
	select {
	case err := <-done:
		t.Fatalf("Run returned while the transport was failing: %v", err)
	default:
	}

	// It still shuts down cleanly on cancel.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A transport that never connects must not stop ingest from working.
func TestRunServesWhileTransportNeverConnects(t *testing.T) {
	tr := &fakeTransport{blockConnect: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	addr := waitForAddr(t, r)
	// started before blocked, as RFC 1 section 3 requires of producers.
	if code := postSession(t, addr, `{"session_id":"a","event":"started"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}
	if got := r.Status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// Frames go out only when Apply or Sweep reports a change. A quiet desk is a
// quiet radio.
//
// # Why every count is reached by converging rather than read straight after a
// post
//
// A 204 means the tracker applied the report, NOT that the frame reached the
// transport. transmit only signals the sender goroutine, which does the write.
// Reading sendCount immediately after a post therefore races that goroutine:
// under load it had not been scheduled yet and the count read one low, which
// failed a relay that was behaving correctly and merely had not caught up.
//
// The negative assertion is the subtle one. "Repeating an event sends nothing"
// cannot be converged on, because there is no event to wait for: the correct
// behaviour is that nothing happens. Waiting for a send that must never arrive
// would hang. So the repeats are bounded by first reaching a known-quiet state,
// then giving the sender a real opportunity to send wrongly, then asserting the
// count did not move. That direction is safe under load: extra time can only
// give a broken relay MORE chance to send the frame this asserts it must not.
func TestFramesOnlySentOnChange(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	// First blocked event is a change, so exactly one frame must reach the
	// transport. Converge on it rather than assuming the sender already ran.
	postSession(t, addr, `{"session_id":"a","event":"blocked"}`)
	waitFor(t, 10*time.Second, "the first change to reach the transport", func() bool {
		return tr.sendCount() >= 1
	})
	afterFirst := tr.sendCount()

	// Repeating the same event changes nothing, so nothing is sent.
	for range 5 {
		postSession(t, addr, `{"session_id":"a","event":"blocked"}`)
	}
	// All five repeats have been applied. Give the sender a generous window in
	// which a wrongly-signalled frame would land, then assert none did.
	time.Sleep(250 * time.Millisecond)
	if got := tr.sendCount(); got != afterFirst {
		t.Errorf("send count = %d after repeats, want %d: an unchanged frame was sent", got, afterFirst)
	}

	// A genuinely new state sends again.
	postSession(t, addr, `{"session_id":"a","event":"started"}`)
	waitFor(t, 10*time.Second, "the real change to reach the transport", func() bool {
		return tr.sendCount() >= afterFirst+1
	})
	// And it sends exactly once more, not repeatedly. The settle window catches
	// a relay that transmits on a timer rather than on change.
	time.Sleep(250 * time.Millisecond)
	if got := tr.sendCount(); got != afterFirst+1 {
		t.Errorf("send count = %d after a real change, want %d", got, afterFirst+1)
	}

	cancel()
	<-done
}

// An unknown event is accepted and ignored, so it must not move the light.
func TestUnknownEventCausesNoSend(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	if code := postSession(t, addr, `{"session_id":"a","event":"teleported"}`); code != 204 {
		t.Fatalf("status = %d, want 204", code)
	}
	// A 204 only means the report was accepted, and the sender writes on its
	// own goroutine. Reading the count straight away would pass even for a
	// relay that was about to send wrongly, so give that send a real window in
	// which to appear before asserting it never did.
	time.Sleep(250 * time.Millisecond)
	if got := tr.sendCount(); got != 0 {
		t.Errorf("send count = %d, want 0: an unknown event must not move the light", got)
	}
	if got := len(r.Status().Sessions); got != 0 {
		t.Errorf("sessions = %d, want 0", got)
	}

	cancel()
	<-done
}

// On reconnect the relay pushes the current frame, so the light catches up
// rather than showing state from before it dropped.
//
// blockConnect is what holds the transport down. Connect now marks the fake
// connected on success, as a real transport does, so a plain
// fakeTransport{connected: false} would be connected by the startup Connect
// before the state below is posted. The disconnected-to-connected edge this
// test exists to exercise would then never happen: the relay would already be
// connected, the posts would go out normally, and setConnected(true) would
// change nothing. Blocking Connect keeps the transport genuinely down until
// the test brings it back by hand.
func TestReconnectResendsCurrentFrame(t *testing.T) {
	tr := &fakeTransport{connected: false, blockConnect: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	postSession(t, addr, `{"session_id":"a","event":"started","label":"auth"}`)
	postSession(t, addr, `{"session_id":"a","event":"blocked","label":"auth"}`)

	// Let the sends triggered by those two posts finish before the baseline is
	// taken. Reading the count while one was still in flight made the baseline
	// too low, and the wait below then satisfied itself on that leftover send
	// rather than on the reconnect resend this test is about: the assertion
	// would pass without the reconnect having pushed anything at all.
	waitFor(t, 10*time.Second, "the ingest sends to settle", func() bool {
		n := tr.sendCount()
		time.Sleep(50 * time.Millisecond)
		return tr.sendCount() == n
	})
	before := tr.sendCount()

	// The light comes back into range on its own, with no Connect from the
	// relay: the loop must notice the edge and push the current frame.
	tr.setConnected(true)

	waitFor(t, 10*time.Second, "a frame to be resent after reconnect", func() bool {
		return tr.sendCount() > before
	})

	frame, ok := tr.lastFrame()
	if !ok {
		t.Fatal("no frame recorded")
	}
	if frame.Color != stoplight.ColorRed {
		t.Errorf("resent frame colour = %v, want red", frame.Color)
	}

	cancel()
	<-done
}

// One reconnect must log once and send once.
//
// There were two reconnect-and-resend paths: the run loop watching the
// disconnected-to-connected edge, and startConnect's retry acting on a
// successful Connect. Both emitted the identical line, so a single reconnect
// through Connect logged twice and put the frame on the wire twice.
func TestReconnectThroughConnectResendsOnlyOnce(t *testing.T) {
	// Disconnected at the start, and Connect brings it back, which is the case
	// where both paths used to fire.
	//
	// The fake starts disconnected and its Connect marks it connected, which is
	// what every real transport does. That is what makes this test about the
	// relay rather than about the clock: the transport comes back exactly once,
	// so a correct relay announces it exactly once no matter how long the test
	// runs. A fake whose Connect left Connected() false would make the loop
	// retry on every tick and the count grow with elapsed wall time.
	//
	// failConnects: 1 puts the edge strictly after the run loop is watching.
	// Run's startup connect races loop's first wasConnected sample, so letting
	// that first attempt succeed would sometimes mean loop starts already
	// connected, observes no edge, and logs nothing. Failing it once makes the
	// transition happen on a later tick, which the loop is guaranteed to see.
	tr := &fakeTransport{connected: false, failConnects: 1}
	logPath := filepath.Join(t.TempDir(), "relay.log")
	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		SweepInterval: time.Hour, // no sweep sends to confuse the count
		LogPath:       logPath,
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	postSession(t, addr, `{"session_id":"a","event":"blocked"}`)

	// Let the reconnect ticker observe the edge. Connect flips connected to
	// true, so the loop and startConnect both see the same transition.
	waitFor(t, 10*time.Second, "the transport to connect", func() bool {
		return tr.connects.Load() > 0 && tr.Connected()
	})

	// Wait for the resend to actually reach the log before counting. Sleeping
	// a fixed span and counting whatever had arrived could read the log before
	// the single legitimate line was written and pass for the wrong reason: a
	// count of 0 satisfies "at most 1" just as well as the correct 1 does.
	waitFor(t, 10*time.Second, "the reconnect to be logged", func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(data), "reconnected, resending frame")
	})

	// Two further reconnect ticks, so a duplicate would have had every chance
	// to land. This one stays a sleep on purpose: the assertion is that a
	// second line NEVER appears, and there is no event to converge on for
	// something that must not happen. Waiting longer only strengthens it.
	time.Sleep(2*reconnectPollInterval + 500*time.Millisecond)

	cancel()
	<-done

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if got := strings.Count(string(data), "reconnected, resending frame"); got > 1 {
		t.Errorf("reconnect logged %d times, want at most 1:\n%s", got, data)
	}
}

// The sweep expires a silent session on its own, because producers crash
// without saying goodbye.
func TestSweepExpiresSilentSession(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r, err := NewRelay(Config{
		Transport:      tr,
		ListenAddr:     "127.0.0.1:0",
		SocketPath:     tempSocket(t),
		SessionTimeout: 20 * time.Millisecond,
		SweepInterval:  5 * time.Millisecond,
		LogPath:        filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	postSession(t, addr, `{"session_id":"a","event":"started"}`)
	postSession(t, addr, `{"session_id":"a","event":"blocked"}`)
	if got := r.Status().Aggregate; got != stoplight.ColorRed {
		t.Fatalf("aggregate = %v, want red", got)
	}

	// Wait past the timeout; the sweep must clear it without any event.
	deadline := time.After(5 * time.Second)
	for {
		if r.Status().Aggregate == stoplight.ColorOff {
			break
		}
		select {
		case <-deadline:
			t.Fatal("silent session was never expired")
		case <-time.After(5 * time.Millisecond):
		}
	}

	// No live sessions means lamps off, not green.
	if got := len(r.Status().Sessions); got != 0 {
		t.Errorf("sessions = %d, want 0", got)
	}

	cancel()
	<-done
}

func TestStatusReportsTransportAndUptime(t *testing.T) {
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	// Before Run, uptime is zero rather than a huge number from the zero time.
	if got := r.Status().Uptime; got != 0 {
		t.Errorf("uptime before Run = %v, want 0", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	postSession(t, addr, `{"session_id":"a","event":"started","label":"api","provider":"ci"}`)

	st := r.Status()
	if st.Transport != "fake" {
		t.Errorf("transport = %q, want fake", st.Transport)
	}
	if !st.Connected {
		t.Error("connected = false, want true")
	}
	if st.Uptime <= 0 {
		t.Errorf("uptime = %v, want positive", st.Uptime)
	}
	if len(st.Sessions) != 1 || st.Sessions[0].ID != "a" {
		t.Errorf("sessions = %+v, want one session with ID a", st.Sessions)
	}
	if st.Aggregate != stoplight.ColorYellow {
		t.Errorf("aggregate = %v, want yellow", st.Aggregate)
	}

	cancel()
	<-done
}

// Shutdown must close the transport and stop serving the socket.
//
// The name used to say "RemovesSocket", which contradicted the code: Run
// deliberately does NOT unlink the socket file, because closing the listener
// already did and an unconditional remove could delete a file a restarted
// relay had just bound. The body never checked for removal either. It dials
// the path and requires the dial to fail, which is satisfied by the listener
// closing, so only the name was wrong.
func TestShutdownClosesTransportAndStopsServingSocket(t *testing.T) {
	tr := &fakeTransport{}
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
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	if tr.closes.Load() == 0 {
		t.Error("transport was not closed")
	}
	if _, err := netDial(socket); err == nil {
		t.Error("socket still accepting connections after shutdown")
	}
}

// A silent socket connection must not hold shutdown open.
//
// handleConn sets an absolute five second read deadline, and cancelling a
// context does not interrupt a read that is already blocked. One connection
// that opened and said nothing therefore delayed Run's return by nearly the
// whole five seconds. That matters because a supervisor with a shorter stop
// timeout SIGKILLs the relay in that window, which skips Transport.Close and
// leaves the serial port held for the next start.
func TestShutdownIsNotDelayedByASilentSocketConnection(t *testing.T) {
	tr := &fakeTransport{}
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
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	waitForAddr(t, r)

	// Open a connection and say nothing, so the handler is parked in Read.
	conn, err := netDial(socket)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()

	// Let the handler goroutine actually reach the blocking read.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	elapsed := time.Since(start)

	// Comfortably under socketReadTimeout, but loose enough not to flake on a
	// loaded machine. The bug produced ~4.9s here.
	if limit := socketReadTimeout / 2; elapsed > limit {
		t.Errorf("shutdown took %v with one silent connection, want under %v", elapsed, limit)
	}
}

// A stale socket file from a crashed run must not stop the relay binding.
func TestListenSocketRemovesStaleFile(t *testing.T) {
	socket := tempSocket(t, "sock")

	// Leave a stale socket behind, as a crash would.
	stale, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	stale.Close()
	// Closing a unix listener normally unlinks it, so recreate the file to
	// be certain a leftover path is what is being tested.
	if err := writeFile(socket); err != nil {
		t.Fatalf("recreate stale file: %v", err)
	}

	listener, err := listenSocket(socket)
	if err != nil {
		t.Fatalf("listenSocket over a stale file: %v", err)
	}
	listener.Close()
}

// listenSocket must create the parent directory, because the default path
// lives under ~/.local/state which may not exist yet.
func TestListenSocketCreatesParentDirs(t *testing.T) {
	socket := tempSocket(t, "a", "b", "c", "sock")
	listener, err := listenSocket(socket)
	if err != nil {
		t.Fatalf("listenSocket: %v", err)
	}
	defer listener.Close()
	if _, err := netDial(socket); err != nil {
		t.Errorf("dial the created socket: %v", err)
	}
}

func TestRunReturnsErrorOnUnbindableAddress(t *testing.T) {
	// Occupy a port, then ask the relay for the same one.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer busy.Close()

	r, err := NewRelay(Config{
		Transport:  &fakeTransport{},
		ListenAddr: busy.Addr().String(),
		SocketPath: tempSocket(t),
		LogPath:    filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// A port clash is a configuration problem the operator must fix, unlike a
	// transport failure, so Run does return here.
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Run on a busy port: want error, got nil")
	}
}
