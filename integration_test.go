package main

// End-to-end tests for the whole pipeline: producer -> HTTP or unix socket ->
// relay -> tracker -> transport -> rendered frame.
//
// Every package below internal/ is unit tested in isolation. Nothing there
// proves the pieces agree with each other, and nothing there proves the
// examples in readme.md actually work. These tests run a real relay on a real
// port with a real socket, and drive it the way the documentation tells a user
// to.
//
// Two rules shape this file. No test may hang: every relay is cancelled by
// t.Cleanup, every poll has a deadline, and the whole suite finishes in well
// under a second. And no test may assume a port or a path: the port is
// reserved from the operating system, and the socket lives in a short
// os.MkdirTemp path because macOS caps a unix socket path at about 104 bytes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/light"
	"github.com/cidekar/stoplight/internal/relay"
	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// pollTimeout bounds every wait-for-condition loop. The budget is deliberately
// generous: under -race on a machine running the rest of the suite in
// parallel, every goroutine handoff costs multiples of what it does on an idle
// box. A wait's job is to catch a hang, not to measure speed, so the only cost
// of a large budget is how long a genuinely broken test takes to report.
//
// It was two seconds, which was calibrated on an idle machine and made the
// root package fail under full-suite load while passing on its own.
const pollTimeout = 30 * time.Second

// pollStep is how often a wait-for-condition loop re-checks. Short enough that
// a passing test costs nothing, long enough not to spin.
const pollStep = time.Millisecond

// recorder is a transport that keeps every frame it is given, counts sends,
// and can be told to fail. It stands in for a light so that the assertions can
// be made on what the device would actually have received.
//
// It is safe for concurrent use: the relay sends from its run loop and from
// both ingest paths, and the test reads from the test goroutine.
type recorder struct {
	mu     sync.Mutex
	frames []stoplight.Frame

	sends atomic.Int64

	// sendErr, when set, makes every Send fail. A dead transport must not
	// stop the relay from accepting reports.
	sendErr error

	// connected is what Connected reports. The virtual light is always
	// reachable; a radio is not.
	connected bool
}

func (r *recorder) Connect(ctx context.Context) error { return nil }
func (r *recorder) Close() error                      { return nil }
func (r *recorder) Name() string                      { return "recorder" }

// Connected reads under the lock because the relay's run loop polls it on its
// own goroutine while a test may be flipping it with setConnected.
func (r *recorder) Connected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.connected
}

// setConnected simulates a light going out of range or coming back, so a test
// can drive the difference between "the relay is down" and "the relay is up but
// the light is not".
func (r *recorder) setConnected(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connected = v
}

func (r *recorder) Send(f stoplight.Frame) error {
	r.sends.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sendErr != nil {
		return r.sendErr
	}
	r.frames = append(r.frames, f)
	return nil
}

// sendCount is the number of times Send was called, including failed sends.
func (r *recorder) sendCount() int { return int(r.sends.Load()) }

// peekFrame returns the most recent frame sent so far, and whether there was
// one. It samples without waiting, which is what a negative assertion needs.
func (r *recorder) peekFrame() (stoplight.Frame, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.frames) == 0 {
		return stoplight.Frame{}, false
	}
	return r.frames[len(r.frames)-1], true
}

// lastFrame returns the most recent frame that was sent successfully, waiting
// for one to arrive if none has yet.
//
// Waiting is required, not a convenience. Relay.transmit does not write to the
// transport: it signals a sender goroutine that owns the wire, and that
// goroutine reads the frame at send time. So an ingest call returning 204
// guarantees the tracker was updated, and guarantees nothing at all about the
// transport having been written to yet.
//
// This used to sample once. On an idle machine the sender almost always won
// the race, so it passed; under full-suite load it lost often enough to fail
// roughly one run in ten with "no frame was ever sent to the transport". The
// test was asserting something the design never promised.
func (r *recorder) lastFrame(t *testing.T) stoplight.Frame {
	t.Helper()
	var f stoplight.Frame
	deadline := time.Now().Add(pollTimeout)
	for {
		var ok bool
		if f, ok = r.peekFrame(); ok {
			return f
		}
		if time.Now().After(deadline) {
			t.Fatalf("no frame was ever sent to the transport within %v", pollTimeout)
		}
		time.Sleep(pollStep)
	}
}

// frameCount is the number of frames that arrived intact.
func (r *recorder) frameCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

// harness is a running relay plus everything a test needs to talk to it.
type harness struct {
	t     *testing.T
	addr  string // host:port of the HTTP listener
	sock  string // path of the unix socket
	light *recorder
}

// options tunes the relay a harness builds. The zero value gives a relay with
// expiry effectively disabled, which is what most tests want.
type options struct {
	sessionTimeout time.Duration
	sweepInterval  time.Duration
	sendErr        error
}

// reservePort asks the operating system for a free loopback port, then gives
// it straight back. Binding and closing is the only reliable way to name a
// port nothing is using: the relay does not expose the address it bound, so a
// test cannot pass :0 and read the result back.
//
// There is a race in principle, because another process could take the port
// between the close and the relay's bind. In practice the operating system
// does not immediately re-issue a port it just handed out, and the alternative
// of hardcoding 7373 is guaranteed to collide with a developer's own relay.
func reservePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}
	return addr
}

// shortSocketPath returns a socket path under a short temporary directory.
// t.TempDir() embeds the test name and can exceed the ~104 byte cap macOS puts
// on a unix socket path, which fails at bind time with a message that does not
// mention length.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "sock")
}

// start brings up a relay on a free port with a short socket path, waits until
// it answers, and registers the shutdown. Every test goes through here, so no
// test can leak a running relay or hang waiting for one that never bound.
func start(t *testing.T, opt options) *harness {
	t.Helper()

	if opt.sessionTimeout == 0 {
		// Long enough that no test expires a session by accident. Expiry is
		// tested deliberately, with a timeout measured in milliseconds.
		opt.sessionTimeout = time.Hour
	}
	if opt.sweepInterval == 0 {
		opt.sweepInterval = 5 * time.Millisecond
	}

	h := &harness{
		t:     t,
		addr:  reservePort(t),
		sock:  shortSocketPath(t),
		light: &recorder{connected: true, sendErr: opt.sendErr},
	}

	r, err := relay.NewRelay(relay.Config{
		Transport:      h.light,
		ListenAddr:     h.addr,
		SocketPath:     h.sock,
		SessionTimeout: opt.sessionTimeout,
		SweepInterval:  opt.sweepInterval,
		// Logging to a file keeps the relay's own chatter out of the test
		// output, which matters most for the dead-transport test where every
		// send logs a failure.
		LogPath: filepath.Join(t.TempDir(), "relay.log"),
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	// t.Context() is cancelled when the test ends, so the relay can never
	// outlive it even if an assertion fails early.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			// Run returns nil on a clean shutdown. A transport failure must
			// never surface here: that is the whole point of the run loop.
			if err != nil {
				t.Errorf("Run returned %v, want nil", err)
			}
		case <-time.After(pollTimeout):
			t.Error("relay did not shut down within the deadline")
		}
	})

	h.waitReady(done)
	return h
}

// waitReady blocks until the relay answers on both listeners, or fails the
// test. It also watches done, so a relay that failed to bind reports the bind
// error rather than timing out.
func (h *harness) waitReady(done <-chan error) {
	h.t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		select {
		case err := <-done:
			h.t.Fatalf("relay exited before it was ready: %v", err)
		default:
		}
		if h.httpReady() && h.socketReady() {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatal("relay never became ready")
		}
		time.Sleep(pollStep)
	}
}

// httpReady reports whether the status endpoint answers.
func (h *harness) httpReady() bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get("http://" + h.addr + relay.StatusPath)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// socketReady reports whether the unix socket accepts a connection.
func (h *harness) socketReady() bool {
	conn, err := net.DialTimeout("unix", h.sock, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// post sends a raw body to an endpoint over HTTP and returns the status code.
// Every request carries a deadline, so no test can hang on a relay that
// stopped answering.
func (h *harness) post(path, body string) int {
	h.t.Helper()
	client := &http.Client{Timeout: pollTimeout}
	resp, err := client.Post("http://"+h.addr+path, "application/json", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// session posts one report to the ingest endpoint and asserts the documented
// 204. Producers are told to ignore the response, but a test must not.
func (h *harness) session(body string) {
	h.t.Helper()
	if code := h.post(relay.SessionPath, body); code != http.StatusNoContent {
		h.t.Fatalf("POST %s %s: status = %d, want %d", relay.SessionPath, body, code, http.StatusNoContent)
	}
}

// socketSession writes one report to the unix socket. There is no response by
// design, so the only thing to check is that the write succeeded. The
// connection carries a deadline for the same reason every HTTP call does.
func (h *harness) socketSession(body string) {
	h.t.Helper()
	conn, err := net.DialTimeout("unix", h.sock, pollTimeout)
	if err != nil {
		h.t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(pollTimeout)); err != nil {
		h.t.Fatalf("socket deadline: %v", err)
	}
	if _, err := io.WriteString(conn, body); err != nil {
		h.t.Fatalf("write socket: %v", err)
	}
	// The relay reads until EOF, so half-closing is what tells it the object
	// is complete. Without this the read waits for the five second socket
	// timeout before the report is applied.
	if unixConn, ok := conn.(*net.UnixConn); ok {
		if err := unixConn.CloseWrite(); err != nil {
			h.t.Fatalf("close write half: %v", err)
		}
	}
}

// status fetches and decodes GET /v1/status, the snapshot a client reads.
func (h *harness) status() relay.StatusResponse {
	h.t.Helper()
	client := &http.Client{Timeout: pollTimeout}
	resp, err := client.Get("http://" + h.addr + relay.StatusPath)
	if err != nil {
		h.t.Fatalf("get status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s: status = %d, want 200", relay.StatusPath, resp.StatusCode)
	}
	var out relay.StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode status: %v", err)
	}
	return out
}

// waitAggregate blocks until the aggregate colour reaches want, then returns.
// Ingest over HTTP is synchronous, but the sweep is not, so an assertion about
// expiry has to wait rather than sample once.
func (h *harness) waitAggregate(want stoplight.Color) {
	h.t.Helper()
	var got stoplight.Color
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		got = h.status().Aggregate
		if got == want {
			return
		}
		time.Sleep(pollStep)
	}
	h.t.Fatalf("aggregate = %v, want %v", got, want)
}

// waitSocketApplied blocks until the socket's write has been applied to the
// tracker. The socket answers nothing, so there is no other way to know the
// report landed.
func (h *harness) waitSocketApplied(sessionID string) {
	h.t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		for _, s := range h.status().Sessions {
			if s.ID == sessionID {
				return
			}
		}
		time.Sleep(pollStep)
	}
	h.t.Fatalf("session %q never appeared after a socket write", sessionID)
}

// rendered draws the last frame the transport received, exactly as the virtual
// light would draw it to a terminal. Asserting on this rather than on the
// frame struct is what makes these tests end to end: it is the output a user
// actually looks at.
//
// It waits for a frame to exist but does not wait for any particular content.
// Use waitLamp when the assertion is about a colour the relay has just been
// asked to move to, because the write to the transport happens on the sender
// goroutine and so trails the ingest call that caused it.
func (h *harness) rendered(t *testing.T) string {
	t.Helper()
	return light.Render(h.light.lastFrame(t))
}

// waitRendered blocks until the drawn frame satisfies cond, then returns it.
// what names the condition so a timeout says what never arrived.
//
// This is the general form of waitLamp, for assertions about labels and other
// frame content rather than the lamp colour. The reason a wait is needed at
// all is the same: an ingest call returns once the tracker has changed, and
// the frame carrying that change is written to the transport afterwards, by
// the sender goroutine.
func (h *harness) waitRendered(t *testing.T, what string, cond func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	var art string
	for {
		if f, ok := h.light.peekFrame(); ok {
			art = light.Render(f)
			if cond(art) {
				return art
			}
		}
		if time.Now().After(deadline) {
			if art == "" {
				t.Fatalf("no frame reached the transport within %v while waiting for %s",
					pollTimeout, what)
			}
			t.Fatalf("timed out after %v waiting for %s\nlast frame drawn:\n%s",
				pollTimeout, what, art)
		}
		time.Sleep(pollStep)
	}
}

// waitSends blocks until the transport has been asked to send exactly n
// frames, and fails if it settles anywhere else.
//
// A test that wants to prove a change DID transmit has to wait for the send
// rather than sample for it, for the same reason every other wait here exists.
// Overshoot is still caught: the count only ever grows, so a value past n is
// reported rather than waited out.
func (h *harness) waitSends(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for {
		got := h.light.sendCount()
		if got == n {
			return
		}
		if got > n {
			t.Fatalf("the transport was sent %d frames, want %d", got, n)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transport was sent %d frames within %v, want %d", got, pollTimeout, n)
		}
		time.Sleep(pollStep)
	}
}

// waitLamp blocks until the lamp shows want, then returns the art it settled
// on so a caller can make further assertions about the same frame.
//
// This is the colour-aware counterpart to rendered. Sampling the transport
// once after an ingest call is a race: 204 means the tracker changed, and the
// frame carrying that change reaches the wire slightly later, on the sender
// goroutine. Under load the gap is wide enough to read the previous frame.
func (h *harness) waitLamp(t *testing.T, want stoplight.Color) string {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	var art string
	for {
		if f, ok := h.light.peekFrame(); ok {
			art = light.Render(f)
			if lampIs(art, want) {
				return art
			}
		}
		if time.Now().After(deadline) {
			if art == "" {
				t.Fatalf("no frame reached the transport within %v while waiting for lamp %v",
					pollTimeout, want)
			}
			// Fall through to assertLamp so the failure reads as a colour
			// mismatch with the art attached, exactly as it did before.
			assertLamp(t, art, want)
			t.FailNow()
		}
		time.Sleep(pollStep)
	}
}

// lampIs reports whether art lights want and no other lamp. It is the silent
// form of assertLamp's check, so a wait can test the same condition it will
// later assert.
func lampIs(art string, want stoplight.Color) bool {
	for _, c := range []stoplight.Color{stoplight.ColorRed, stoplight.ColorYellow, stoplight.ColorGreen} {
		lit := strings.Contains(art, strings.ToUpper(c.String()))
		if lit != (c == want) {
			return false
		}
	}
	if want == stoplight.ColorOff && strings.Contains(art, "●") {
		return false
	}
	return true
}

// assertLamp checks that the rendered art lights the wanted lamp and no other.
// The virtual light writes the colour name beside the lit lamp and leaves the
// other two housings empty, so the name appearing once is the assertion.
func assertLamp(t *testing.T, art string, want stoplight.Color) {
	t.Helper()
	for _, c := range []stoplight.Color{stoplight.ColorRed, stoplight.ColorYellow, stoplight.ColorGreen} {
		name := strings.ToUpper(c.String())
		lit := strings.Contains(art, name)
		if c == want && !lit {
			t.Errorf("lamp %s is not lit, want it lit\n%s", name, art)
		}
		if c != want && lit {
			t.Errorf("lamp %s is lit, want it dark\n%s", name, art)
		}
	}
	if want == stoplight.ColorOff && strings.Contains(art, "●") {
		t.Errorf("a lamp is lit but the aggregate is off\n%s", art)
	}
}

// TestIntegrationReadmeCurlExample runs the copy-and-paste example from
// readme.md. It is the first thing any user tries, so if it does not produce a
// red lamp reading "auth-api" the documentation is wrong and the project's
// front door is broken.
//
// The body is byte for byte the one the readme prints. Only the address is
// changed, because a test must not assume port 7373 is free.
func TestIntegrationReadmeCurlExample(t *testing.T) {
	h := start(t, options{})

	// Exactly the payload from readme.md.
	h.session(`{"session_id":"job-1","event":"blocked","label":"auth-api"}`)

	art := h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "auth-api") {
		t.Errorf("rendered frame does not show the label auth-api\n%s", art)
	}
	if !strings.Contains(art, "needs you") {
		t.Errorf("rendered frame does not show the blocked state\n%s", art)
	}

	// The status endpoint must agree with the light. A user who cannot see
	// the light reads this instead.
	st := h.status()
	if st.Aggregate != stoplight.ColorRed {
		t.Errorf("status aggregate = %v, want red", st.Aggregate)
	}
	if len(st.Sessions) != 1 || st.Sessions[0].ID != "job-1" || st.Sessions[0].Label != "auth-api" {
		t.Errorf("status sessions = %+v, want one job-1 labelled auth-api", st.Sessions)
	}
}

// TestIntegrationReadmeShellWrapper walks the stoplight_run wrapper from
// readme.md: started before the command, then finished or blocked depending on
// the exit code. The wrapper reuses one session ID for both, which is what
// makes the lamp move rather than accumulate sessions.
func TestIntegrationReadmeShellWrapper(t *testing.T) {
	h := start(t, options{})

	// The wrapper's id is "wrap:$$" and its label is the basename of $PWD.
	const id = "wrap:4242"
	const label = "stoplight"

	body := func(event string) string {
		return fmt.Sprintf(`{"session_id":%q,"event":%q,"label":%q}`, id, event, label)
	}

	// _sl started; "$@"
	h.session(body("started"))
	art := h.waitLamp(t, stoplight.ColorYellow)
	if !strings.Contains(art, label) {
		t.Errorf("rendered frame does not show the wrapper label\n%s", art)
	}
	if !strings.Contains(art, "working") {
		t.Errorf("rendered frame does not show the working state\n%s", art)
	}

	// rc == 0, so: _sl finished
	h.session(body("finished"))
	art = h.waitLamp(t, stoplight.ColorGreen)
	if !strings.Contains(art, "done") {
		t.Errorf("rendered frame does not show the done state\n%s", art)
	}

	// The wrapper must not leak a second session by reusing the ID.
	if got := len(h.status().Sessions); got != 1 {
		t.Errorf("session count = %d, want 1 for one wrapped command", got)
	}

	// The failing branch of the wrapper: a second run of the same command
	// that exits non-zero goes red.
	h.session(body("started"))
	h.session(body("blocked"))
	h.waitLamp(t, stoplight.ColorRed)
}

// TestIntegrationMultiSessionAggregation is the core promise: many agents, one
// light, most urgent wins. It also pins the rule that provider is free text
// which never influences the aggregate.
func TestIntegrationMultiSessionAggregation(t *testing.T) {
	h := start(t, options{})

	h.session(`{"session_id":"s-blocked","event":"blocked","provider":"claude-code","label":"auth"}`)
	h.session(`{"session_id":"s-working","event":"started","provider":"deepseek","label":"index"}`)
	h.session(`{"session_id":"s-done","event":"finished","provider":"ci","label":"build"}`)

	// Red wins over yellow and green.
	art := h.waitLamp(t, stoplight.ColorRed)

	// All three appear on the screen, whatever the lamp shows. The rotation
	// is how a user finds out which agent is the one that needs them.
	for _, label := range []string{"auth", "index", "build"} {
		if !strings.Contains(art, label) {
			t.Errorf("rendered frame is missing session %q\n%s", label, art)
		}
	}

	frame := h.light.lastFrame(t)
	if len(frame.Sessions) != 3 {
		t.Fatalf("frame carries %d sessions, want 3", len(frame.Sessions))
	}

	// Each session keeps its own colour even though the lamp shows only the
	// aggregate.
	want := map[string]stoplight.Color{
		"s-blocked": stoplight.ColorRed,
		"s-working": stoplight.ColorYellow,
		"s-done":    stoplight.ColorGreen,
	}
	for _, s := range frame.Sessions {
		if got := want[s.ID]; got != s.Color {
			t.Errorf("session %s colour = %v, want %v", s.ID, s.Color, got)
		}
	}

	// Provider is carried through to status but must not move the lamp.
	providers := map[string]string{}
	for _, s := range h.status().Sessions {
		providers[s.ID] = s.Provider
	}
	if providers["s-blocked"] != "claude-code" || providers["s-working"] != "deepseek" || providers["s-done"] != "ci" {
		t.Errorf("providers = %v, want the three reported values", providers)
	}

	// Changing only the provider on the blocked session must change nothing
	// about the colour: one light, one aggregate.
	before := h.light.sendCount()
	h.session(`{"session_id":"s-blocked","event":"blocked","provider":"something-else","label":"auth"}`)
	if got := h.status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("aggregate after a provider change = %v, want red", got)
	}
	if after := h.light.sendCount(); after != before {
		t.Errorf("a provider-only change transmitted %d frames, want 0", after-before)
	}

	// Resolve the blocked one with `finished`. This is the ordinary path: the
	// human answers the prompt and the agent completes its turn. The lamp
	// drops to yellow, because the still-working session now dominates.
	//
	// This transition used to be a no-op, which left the lamp red after every
	// answered prompt. It is asserted here rather than only in the state
	// machine's own tests because the symptom is only visible end to end.
	h.session(`{"session_id":"s-blocked","event":"finished","label":"auth"}`)
	h.waitLamp(t, stoplight.ColorYellow)

	// Finish the working one and the desk is green: work done, nothing
	// waiting.
	h.session(`{"session_id":"s-working","event":"finished","label":"index"}`)
	h.waitLamp(t, stoplight.ColorGreen)
	if got := len(h.status().Sessions); got != 3 {
		t.Errorf("session count = %d, want all 3 still live", got)
	}

	// The other way out of a block is `started`: the human answers and the
	// agent resumes rather than finishing. Red must clear either way.
	h.session(`{"session_id":"s-done","event":"blocked","label":"build"}`)
	h.waitLamp(t, stoplight.ColorRed)
	h.session(`{"session_id":"s-done","event":"started","label":"build"}`)
	h.waitLamp(t, stoplight.ColorYellow)
}

// TestIntegrationZeroSessionsIsOff pins a stated invariant: an empty desk is
// not a finished task. Ending every session must turn the lamp off, not leave
// it green.
func TestIntegrationZeroSessionsIsOff(t *testing.T) {
	h := start(t, options{})

	h.session(`{"session_id":"a","event":"blocked"}`)
	h.session(`{"session_id":"b","event":"finished"}`)
	h.waitLamp(t, stoplight.ColorRed)

	h.session(`{"session_id":"a","event":"ended"}`)
	// One session left, and it is done, so green is correct here.
	h.waitLamp(t, stoplight.ColorGreen)

	h.session(`{"session_id":"b","event":"ended"}`)

	art := h.waitLamp(t, stoplight.ColorOff)
	// With no sessions the frame stops at the lamps: an empty compartment
	// below them would read as a broken screen.
	if strings.Contains(art, "├") {
		t.Errorf("empty frame drew a session compartment\n%s", art)
	}

	st := h.status()
	if st.Aggregate != stoplight.ColorOff {
		t.Errorf("status aggregate = %v, want off", st.Aggregate)
	}
	if len(st.Sessions) != 0 {
		t.Errorf("status sessions = %+v, want none", st.Sessions)
	}
}

// TestIntegrationBothIngestPathsAgree posts equivalent payloads over HTTP and
// over the unix socket and checks the tracker cannot tell them apart. The two
// listeners share one ingest path precisely so they can never disagree, and
// this is the test that proves the sharing is real.
func TestIntegrationBothIngestPathsAgree(t *testing.T) {
	h := start(t, options{})

	const payload = `{"session_id":"%s","event":"blocked","provider":"ci","label":"deploy"}`

	h.session(fmt.Sprintf(payload, "over-http"))
	h.socketSession(fmt.Sprintf(payload, "over-socket"))
	h.waitSocketApplied("over-socket")

	byID := map[string]relay.StatusSession{}
	for _, s := range h.status().Sessions {
		byID[s.ID] = s
	}
	viaHTTP, ok := byID["over-http"]
	if !ok {
		t.Fatal("the HTTP session is missing")
	}
	viaSocket, ok := byID["over-socket"]
	if !ok {
		t.Fatal("the socket session is missing")
	}

	// Compare everything except the fields that must differ: the ID, and the
	// timestamps, which cannot be equal for two sequential requests.
	if viaHTTP.Label != viaSocket.Label {
		t.Errorf("label: http = %q, socket = %q", viaHTTP.Label, viaSocket.Label)
	}
	if viaHTTP.State != viaSocket.State {
		t.Errorf("state: http = %q, socket = %q", viaHTTP.State, viaSocket.State)
	}
	if viaHTTP.Color != viaSocket.Color {
		t.Errorf("colour: http = %v, socket = %v", viaHTTP.Color, viaSocket.Color)
	}
	if viaHTTP.Provider != viaSocket.Provider {
		t.Errorf("provider: http = %q, socket = %q", viaHTTP.Provider, viaSocket.Provider)
	}

	// The socket must also drive the light, not merely the tracker.
	art := h.waitLamp(t, stoplight.ColorRed)
	if strings.Count(art, "deploy") != 2 {
		t.Errorf("rendered frame does not show both sessions\n%s", art)
	}

	// A session started over one path must be advanced by the other: the
	// producer may change how it talks between events.
	h.socketSession(`{"session_id":"over-http","event":"ended"}`)
	deadline := time.Now().Add(pollTimeout)
	for {
		if _, still := func() (relay.StatusSession, bool) {
			for _, s := range h.status().Sessions {
				if s.ID == "over-http" {
					return s, true
				}
			}
			return relay.StatusSession{}, false
		}(); !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a socket ended did not end a session started over HTTP")
		}
		time.Sleep(pollStep)
	}
}

// TestIntegrationSessionExpiry proves a producer that crashes without saying
// goodbye stops holding the lamp. The timeout and the sweep are measured in
// milliseconds here; in production they are minutes, and the mechanism is the
// same.
func TestIntegrationSessionExpiry(t *testing.T) {
	h := start(t, options{
		sessionTimeout: 40 * time.Millisecond,
		sweepInterval:  5 * time.Millisecond,
	})

	h.session(`{"session_id":"crashed","event":"blocked","label":"stuck"}`)
	h.waitLamp(t, stoplight.ColorRed)

	// Say nothing. The sweep must expire the session and drop the lamp with
	// no further input from anybody.
	h.waitAggregate(stoplight.ColorOff)

	if got := len(h.status().Sessions); got != 0 {
		t.Errorf("session count after expiry = %d, want 0", got)
	}
	// The expiry has to reach the light, not just the tracker. A relay that
	// expired a session but never transmitted would leave the lamp red.
	h.waitLamp(t, stoplight.ColorOff)

	// A session that keeps reporting must survive the same span. This is the
	// other half of the invariant: expiry must not evict live work.
	h.session(`{"session_id":"alive","event":"started","label":"busy"}`)
	for range 6 {
		time.Sleep(15 * time.Millisecond)
		h.session(`{"session_id":"alive","event":"started","label":"busy"}`)
	}
	if got := h.status().Aggregate; got != stoplight.ColorYellow {
		t.Errorf("aggregate for a session that kept reporting = %v, want yellow", got)
	}
}

// TestIntegrationSurvivesDeadTransport is the invariant that keeps the service
// out of a crash loop. A light that is off, asleep or out of range makes every
// Send fail, and the relay must go on accepting reports as though nothing
// happened.
func TestIntegrationSurvivesDeadTransport(t *testing.T) {
	h := start(t, options{sendErr: fmt.Errorf("light is unplugged")})

	// Enough traffic that a relay which died on the first failure could not
	// possibly answer the last request.
	for i := range 20 {
		h.session(fmt.Sprintf(`{"session_id":"job-%d","event":"blocked"}`, i))
	}
	// Both ingest paths must survive, not just HTTP.
	h.socketSession(`{"session_id":"via-socket","event":"started"}`)
	h.waitSocketApplied("via-socket")

	// Wait for the first attempt rather than sample for it. A failing Send is
	// still a Send, and it happens on the sender goroutine, so it trails the
	// ingest calls above.
	deadline := time.Now().Add(pollTimeout)
	for h.light.sendCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the relay never tried to send within %v, so nothing was proved about failures",
				pollTimeout)
		}
		time.Sleep(pollStep)
	}
	if got := h.light.frameCount(); got != 0 {
		t.Errorf("%d frames arrived, want 0 from a transport that always fails", got)
	}

	// State is still correct: the relay tracked everything, it simply could
	// not show it.
	st := h.status()
	if st.Aggregate != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red despite the dead transport", st.Aggregate)
	}
	if len(st.Sessions) != 21 {
		t.Errorf("session count = %d, want 21", len(st.Sessions))
	}

	// And the relay is still there. The cleanup registered by start asserts
	// that Run returns nil rather than a transport error.
	if !h.httpReady() {
		t.Error("the relay stopped answering after the transport failed")
	}
}

// TestIntegrationUnknownEventsAndFields checks the forward-compatibility rule
// from RFC 1 section 11. A producer written against a later spec must not be
// rejected, and must not corrupt anything either.
func TestIntegrationUnknownEventsAndFields(t *testing.T) {
	h := start(t, options{})

	// Establish a baseline the unknown event could damage.
	h.session(`{"session_id":"other","event":"started","label":"baseline"}`)
	baselineFrame := h.light.lastFrame(t)
	baselineSends := h.light.sendCount()

	// The unknown event, with an unknown field alongside it.
	if code := h.post(relay.SessionPath, `{"session_id":"x","event":"teleported","weird_field":123}`); code != http.StatusNoContent {
		t.Errorf("unknown event: status = %d, want 204", code)
	}

	// No session created, no colour moved, no frame sent.
	st := h.status()
	if len(st.Sessions) != 1 || st.Sessions[0].ID != "other" {
		t.Errorf("sessions after an unknown event = %+v, want only the baseline", st.Sessions)
	}
	if st.Aggregate != stoplight.ColorYellow {
		t.Errorf("aggregate after an unknown event = %v, want yellow", st.Aggregate)
	}
	if got := h.light.sendCount(); got != baselineSends {
		t.Errorf("an unknown event transmitted %d frames, want 0", got-baselineSends)
	}
	if !h.light.lastFrame(t).Equal(baselineFrame) {
		t.Error("an unknown event changed the frame")
	}

	// The same session now sends something the relay does know. If the
	// unknown event had poisoned any state, this is where it would show.
	h.session(`{"session_id":"x","event":"blocked","label":"real","unknown_field":"ignored"}`)
	art := h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "real") {
		t.Errorf("the valid event after an unknown one did not take effect\n%s", art)
	}

	// An unknown event for a session that already exists must also be inert.
	before := h.light.sendCount()
	if code := h.post(relay.SessionPath, `{"session_id":"x","event":"levitated"}`); code != http.StatusNoContent {
		t.Errorf("unknown event on a live session: status = %d, want 204", code)
	}
	if got := h.status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red after an ignored event", got)
	}
	if got := h.light.sendCount(); got != before {
		t.Errorf("an unknown event on a live session transmitted %d frames, want 0", got-before)
	}

	// A payload the producer genuinely got wrong is still a 400: leniency is
	// for events, not for missing required fields.
	if code := h.post(relay.SessionPath, `{"event":"blocked"}`); code != http.StatusBadRequest {
		t.Errorf("missing session_id: status = %d, want 400", code)
	}
	if code := h.post(relay.SessionPath, `{not json`); code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", code)
	}
}

// TestIntegrationLabelOverride exercises POST /v1/task, the endpoint behind
// `stoplight task`. An explicit label must survive later reports that carry a
// derived or producer-sent label, otherwise naming a task would last only
// until the next event.
func TestIntegrationLabelOverride(t *testing.T) {
	h := start(t, options{})

	h.session(`{"session_id":"job-9","event":"started","label":"derived-name"}`)
	h.waitRendered(t, "the derived label to reach the screen", func(art string) bool {
		return strings.Contains(art, "derived-name")
	})

	// Name the task.
	if code := h.post(relay.TaskPath, `{"session_id":"job-9","label":"ship the thing"}`); code != http.StatusNoContent {
		t.Fatalf("POST %s: status = %d, want 204", relay.TaskPath, code)
	}
	art := h.waitRendered(t, "the override to replace the derived label", func(art string) bool {
		return strings.Contains(art, "ship the thing")
	})
	if strings.Contains(art, "derived-name") {
		t.Errorf("the derived label is still on screen beside the override\n%s", art)
	}
	// The override reaches the screen only. Naming a task must never move the
	// lamp.
	assertLamp(t, art, stoplight.ColorYellow)

	// A later report carrying the old label must not win it back. This is the
	// whole point of an override.
	h.session(`{"session_id":"job-9","event":"blocked","label":"derived-name"}`)
	art = h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "ship the thing") {
		t.Errorf("the override did not survive a later report\n%s", art)
	}
	if strings.Contains(art, "derived-name") {
		t.Errorf("a later report overwrote the override\n%s", art)
	}

	// An empty label clears the override, so the derived label shows again.
	// That is how the command undoes itself without a second verb.
	if code := h.post(relay.TaskPath, `{"session_id":"job-9","label":""}`); code != http.StatusNoContent {
		t.Fatalf("clearing the override: status = %d, want 204", code)
	}
	art = h.waitRendered(t, "the derived label to come back after the override is cleared",
		func(art string) bool { return strings.Contains(art, "derived-name") })
	assertLamp(t, art, stoplight.ColorRed)

	// A task for a session nobody reported is accepted but creates NOTHING.
	// An override says what the work is called and nothing about where it got
	// to, so there is no state to create a session in: the only honest choice
	// was idle, which is green, and that put a finished-looking session on the
	// board for work that never reported. The name is parked instead.
	if code := h.post(relay.TaskPath, `{"session_id":"never-seen","label":"planned"}`); code != http.StatusNoContent {
		t.Fatalf("task for an unknown session: status = %d, want 204", code)
	}
	for _, s := range h.status().Sessions {
		if s.ID == "never-seen" {
			t.Error("a task for an unknown session created one; naming work " +
				"must not put a session on the board")
		}
	}

	// It applies the moment that session genuinely reports.
	h.session(`{"session_id":"never-seen","event":"started"}`)
	var found bool
	for _, s := range h.status().Sessions {
		if s.ID == "never-seen" {
			found = true
			if s.Label != "planned" {
				t.Errorf("label = %q, want the parked label planned", s.Label)
			}
			if s.State != "working" {
				t.Errorf("state = %q, want working", s.State)
			}
		}
	}
	if !found {
		t.Error("the session did not appear once it reported")
	}

	// A task with no session_id names nothing at all, so it is a 400.
	if code := h.post(relay.TaskPath, `{"label":"nameless"}`); code != http.StatusBadRequest {
		t.Errorf("task with no session_id: status = %d, want 400", code)
	}
}

// TestIntegrationTaskCommandKeepsTheLampRed drives the real `stoplight task`
// command against a real relay, which is where the defect actually lived.
//
// The endpoint was correct and TestIntegrationLabelOverride proved it, but the
// command was not calling it: cmdTask posted an `idle` report to ingest, and
// every ingest report runs a state transition. EventIdle resets ANY state to
// Idle, so naming the task you are blocked on drove the session to Idle and
// turned the lamp green. That is the exact moment the light must stay red.
//
// Nothing below touches an endpoint directly. The point is the wiring: which
// endpoint the command chooses, which no endpoint test can see.
func TestIntegrationTaskCommandKeepsTheLampRed(t *testing.T) {
	h := start(t, options{})

	// A session needs a human. The lamp is red and must stay that way.
	h.session(`{"session_id":"t1","event":"blocked","label":"derived-name"}`)
	h.waitAggregate(stoplight.ColorRed)
	h.waitLamp(t, stoplight.ColorRed)

	// Name the task through the CLI, exactly as a user would.
	code, stdout, stderr := runCLI("task", "--session-id", "t1", "--addr", h.addr, "my label")
	if code != exitOK {
		t.Fatalf("task: exit code = %d, want %d\nstderr: %s", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "my label") {
		t.Errorf("stdout does not confirm the label:\n%s", stdout)
	}

	// The state must not have moved. This is the assertion that failed before
	// the fix: the aggregate went green and the session went idle.
	if got := h.status().Aggregate; got != stoplight.ColorRed {
		t.Errorf("aggregate = %s after naming a task, want red: "+
			"relabelling a blocked session cleared the red light", got)
	}
	for _, s := range h.status().Sessions {
		if s.ID == "t1" && s.State != "needs you" {
			t.Errorf("state = %q after naming a task, want %q", s.State, "needs you")
		}
	}

	// The override must reach the rendered frame, not just the tracker: the
	// whole command exists to change what the screen says.
	art := h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "my label") {
		t.Errorf("the override is not on the rendered frame\n%s", art)
	}
	if strings.Contains(art, "derived-name") {
		t.Errorf("the derived label is still on screen beside the override\n%s", art)
	}

	// A later report carrying a derived label must not clobber the override,
	// and must not disturb the lamp either.
	h.session(`{"session_id":"t1","event":"blocked","label":"derived-name"}`)
	art = h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "my label") {
		t.Errorf("a later report clobbered the override\n%s", art)
	}

	// --clear removes the override, so the derived label shows again. The lamp
	// is still not the override's business.
	code, stdout, stderr = runCLI("task", "--session-id", "t1", "--addr", h.addr, "--clear")
	if code != exitOK {
		t.Fatalf("task --clear: exit code = %d, want %d\nstderr: %s", code, exitOK, stderr)
	}
	if !strings.Contains(stdout, "cleared") {
		t.Errorf("stdout does not confirm the clear:\n%s", stdout)
	}
	art = h.waitLamp(t, stoplight.ColorRed)
	if !strings.Contains(art, "derived-name") {
		t.Errorf("--clear did not restore the derived label\n%s", art)
	}
	if strings.Contains(art, "my label") {
		t.Errorf("--clear left the override on screen\n%s", art)
	}
}

// TestIntegrationTaskCommandReportsARejectedRequest proves the command tells
// the user when the relay refused the request.
//
// This is why task must not go through notify.Send, which discards its error to
// protect a hook from failing. task is typed by a human who is owed an answer,
// so a 400 has to be distinguishable from success.
func TestIntegrationTaskCommandReportsARejectedRequest(t *testing.T) {
	h := start(t, options{})

	// A label longer than the 8KB body cap is rejected by the relay. The
	// command must surface that rather than printing a cheerful confirmation.
	code, stdout, stderr := runCLI("task",
		"--session-id", "big",
		"--addr", h.addr,
		strings.Repeat("x", relay.MaxBodyBytes+1),
	)
	if code != exitError {
		t.Errorf("exit code = %d, want %d: a rejected request looked like success", code, exitError)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation of the rejection")
	}
	if strings.Contains(stdout, "label set to") {
		t.Errorf("stdout confirms a label the relay rejected:\n%s", stdout)
	}
}

// TestIntegrationStatusCommandDistinguishesRelayFromLight covers the other half
// of the status fix. A relay that is not running and a relay that is running
// with no light attached are different problems with different fixes, and the
// old TCP probe could not tell them apart: it only ever learned whether the
// port opened.
func TestIntegrationStatusCommandDistinguishesRelayFromLight(t *testing.T) {
	// A relay that is up, with a transport that reports connected.
	h := start(t, options{})
	h.session(`{"session_id":"s1","event":"blocked"}`)
	h.waitAggregate(stoplight.ColorRed)

	snapshot, err := fetchStatus(h.addr)
	if err != nil {
		t.Fatalf("fetchStatus against a running relay: %v", err)
	}
	if !snapshot.Connected {
		t.Error("connected = false, want true: the recorder transport is connected")
	}
	if snapshot.Aggregate != stoplight.ColorRed {
		t.Errorf("aggregate = %s, want red", snapshot.Aggregate)
	}
	if len(snapshot.Sessions) != 1 {
		t.Errorf("sessions = %d, want 1", len(snapshot.Sessions))
	}

	// A relay that is not there at all. This must be an error, which is what
	// lets status say "not listening" rather than "no light".
	if _, err := fetchStatus(closedAddr(t)); err == nil {
		t.Error("fetchStatus against a closed port returned no error")
	}
}

// TestIntegrationStatusReportsAnUnreachableLight proves the second message: the
// relay answers, so ingest works, but no light is reachable. Reporting this as
// "not listening" would send the user to restart a service that is running.
func TestIntegrationStatusReportsAnUnreachableLight(t *testing.T) {
	h := start(t, options{})
	h.light.setConnected(false)

	snapshot, err := fetchStatus(h.addr)
	if err != nil {
		t.Fatalf("fetchStatus: %v", err)
	}
	if snapshot.Connected {
		t.Error("connected = true, want false: the light was disconnected")
	}
	if snapshot.Transport == "" {
		t.Error("transport is empty, want the transport name so status can name it")
	}
}

// TestIntegrationFramesOnlySentOnChange is what keeps a quiet desk from
// keeping a radio awake. Repeating an event must cost nothing.
func TestIntegrationFramesOnlySentOnChange(t *testing.T) {
	h := start(t, options{})

	h.session(`{"session_id":"a","event":"blocked","label":"one"}`)
	// Wait for the send rather than sample for it. The 204 means the tracker
	// changed; the frame goes out just after, on the sender goroutine.
	h.waitSends(t, 1)
	afterFirst := h.light.sendCount()

	// The identical event again. Nothing on the device would look different,
	// so nothing goes out.
	h.session(`{"session_id":"a","event":"blocked","label":"one"}`)
	if got := h.light.sendCount(); got != afterFirst {
		t.Errorf("a repeated event sent %d extra frames, want 0", got-afterFirst)
	}

	// Ten more of the same changes nothing either.
	for range 10 {
		h.session(`{"session_id":"a","event":"blocked","label":"one"}`)
	}
	if got := h.light.sendCount(); got != afterFirst {
		t.Errorf("ten repeated events sent %d extra frames, want 0", got-afterFirst)
	}

	// A repeat over the socket is the same repeat: the two paths share the
	// change detection, not just the decoder.
	h.socketSession(`{"session_id":"a","event":"blocked","label":"one"}`)
	// The socket answers nothing, so there is no reply to wait on. Waiting for
	// the report to have been applied is the closest positive signal, and it
	// is a real one rather than a fixed pause: once the session is visible in
	// the status snapshot the handler has run, so any send it was going to
	// make has been decided.
	h.waitSocketApplied("a")
	if got := h.light.sendCount(); got != afterFirst {
		t.Errorf("a repeated event over the socket sent %d extra frames, want 0", got-afterFirst)
	}

	// Something that genuinely changes the screen does transmit.
	h.session(`{"session_id":"a","event":"blocked","label":"two"}`)
	h.waitSends(t, afterFirst+1)
}

// TestIntegrationFrameSurvivesTheWire encodes the frame the way a transport
// does and decodes it again, checking nothing a user sees is lost between the
// relay and the firmware. The colour goes out as a name and the label as
// unescaped text, and both matter on a device with a tiny font.
func TestIntegrationFrameSurvivesTheWire(t *testing.T) {
	h := start(t, options{})

	// A label with a character json.Marshal would escape by default.
	h.session(`{"session_id":"job-1","event":"blocked","label":"a&b <ok>"}`)

	sent := h.light.lastFrame(t)
	encoded, err := transport.EncodeFrame(sent)
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	if !bytes.HasSuffix(encoded, []byte("\n")) {
		t.Error("the encoded frame has no trailing newline, so the device could not split frames")
	}
	if !bytes.Contains(encoded, []byte(`"color":"red"`)) {
		t.Errorf("the colour is not on the wire as a name: %s", encoded)
	}
	if !bytes.Contains(encoded, []byte("a&b <ok>")) {
		t.Errorf("the label was escaped on the wire: %s", encoded)
	}

	var back stoplight.Frame
	if err := json.Unmarshal(bytes.TrimSpace(encoded), &back); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if !back.Equal(sent) {
		t.Errorf("frame did not survive the round trip:\n sent = %+v\n back = %+v", sent, back)
	}
}
