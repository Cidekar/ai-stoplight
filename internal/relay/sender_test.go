package relay

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// slowTransport delays every send by a settable amount, so two concurrent
// sends genuinely overlap. It records frames in completion order, which is
// what the wire would carry.
//
// The delay is per-send and set by the test before the send happens, which is
// how a stale frame is made to finish last.
type slowTransport struct {
	mu     sync.Mutex
	frames []stoplight.Frame

	// delay is consulted for every send. It is a function so a test can vary
	// the delay by frame content, which is what forces a specific interleaving.
	delayFor func(stoplight.Frame) time.Duration

	// before runs at the start of every send, before the frame is recorded. A
	// test uses it to hold a send open and pin down an interleaving exactly,
	// rather than hoping a sleep lands the right way.
	before func(stoplight.Frame)
}

func (s *slowTransport) Connect(context.Context) error { return nil }
func (s *slowTransport) Connected() bool               { return true }
func (s *slowTransport) Close() error                  { return nil }
func (s *slowTransport) Name() string                  { return "slow" }

func (s *slowTransport) Send(f stoplight.Frame) error {
	if s.before != nil {
		s.before(f)
	}
	if s.delayFor != nil {
		time.Sleep(s.delayFor(f))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, f)
	return nil
}

// sent returns the frames in the order they landed on the wire.
func (s *slowTransport) sent() []stoplight.Frame {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stoplight.Frame(nil), s.frames...)
}

// last returns the final frame on the wire, which is what the lamp is showing.
func (s *slowTransport) last() (stoplight.Frame, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.frames) == 0 {
		return stoplight.Frame{}, false
	}
	return s.frames[len(s.frames)-1], true
}

// runRelay starts a relay on a slow transport and returns its address and a
// stop function. Every test here goes through the real listeners, because the
// defect is in how concurrent requests reach the transport.
func runRelay(t *testing.T, tr *slowTransport) (r *Relay, addr string, stop func()) {
	t.Helper()

	r, err := NewRelay(Config{
		Transport:     tr,
		ListenAddr:    "127.0.0.1:0",
		SocketPath:    tempSocket(t),
		LogPath:       t.TempDir() + "/relay.log",
		SweepInterval: time.Hour, // never sweep: the test drives every change
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	addr = waitForAddr(t, r)

	return r, addr, func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after cancellation")
		}
	}
}

// rawPost posts to the ingest endpoint and discards the result. Unlike
// postSession it takes no *testing.T, so it is safe to call from a goroutine
// the test does not join: these tests deliberately start posts that block
// until released, and a t.Fatal from such a goroutine is not allowed.
func rawPost(addr, body string) {
	resp, err := http.Post("http://"+addr+SessionPath, "application/json", strings.NewReader(body))
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// waitForWire blocks until the wire has settled: no new frame for a short
// quiet period. It bounds the wait so a broken sender fails the test rather
// than hanging it.
func waitForWire(t *testing.T, tr *slowTransport, quiet, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	seen := -1
	stable := time.Now()
	for time.Now().Before(deadline) {
		n := len(tr.sent())
		if n != seen {
			seen = n
			stable = time.Now()
		} else if time.Since(stable) >= quiet {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("wire never settled, %d frames sent", len(tr.sent()))
}

// TestConcurrentPostsEndOnTheTrackerState is the defect in its plainest form.
//
// A session goes working then blocked. The yellow send is held open while the
// red one is posted, so the two overlap exactly as two HTTP requests on two
// goroutines did. Released, the fast red frame lands first and the stale
// yellow one lands second, leaving the lamp yellow while the session is
// blocked, with nothing to correct it: the next Apply reports no change.
//
// The hold is what makes this deterministic rather than a race that usually
// passes. With one goroutine owning the transport, the yellow send cannot
// still be in flight when red is decided, so the interleaving cannot happen.
//
// The wire's last frame must match what the tracker actually holds.
func TestConcurrentPostsEndOnTheTrackerState(t *testing.T) {
	release := make(chan struct{})
	held := make(chan struct{}, 1)
	var first atomic.Bool

	tr := &slowTransport{
		delayFor: func(stoplight.Frame) time.Duration { return 0 },
	}
	// The first send is held until the test releases it. Whatever frame that
	// turns out to be, it is the one at risk of going stale while it waits.
	//
	// The flag is a CompareAndSwap rather than a sync.Once because Once holds
	// its mutex for the whole call: a second send would block inside Once
	// itself rather than run, which is the opposite of the overlap being
	// tested, and would deadlock instead of failing.
	tr.before = func(stoplight.Frame) {
		if first.CompareAndSwap(false, true) {
			held <- struct{}{}
			<-release
		}
	}

	r, addr, stop := runRelay(t, tr)
	defer stop()

	// Released on every path, including a failure before the explicit release
	// below. A held send would otherwise stall shutdown and hang the test
	// rather than failing it. This defer is registered after stop's so that it
	// runs first: shutdown must never wait on a send this test is holding.
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	// started: yellow. This send is the one that gets held. The post runs on
	// its own goroutine because the handler blocks behind the held send in the
	// pre-fix code, which is the behaviour under test.
	go rawPost(addr, `{"session_id":"a","event":"started"}`)

	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the first send never started")
	}

	// blocked: red, decided while the yellow send is still in flight.
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != http.StatusNoContent {
		t.Fatalf("blocked: status = %d, want 204", code)
	}
	waitForState(t, r, "a", stoplight.StateBlocked)

	releaseAll()

	waitForWire(t, tr, 150*time.Millisecond, 5*time.Second)

	got, ok := tr.last()
	if !ok {
		t.Fatal("nothing reached the wire")
	}
	if got.Color != stoplight.ColorRed {
		t.Fatalf("last frame on the wire = %s, want red: "+
			"the session is blocked and the lamp says otherwise\nwire: %s",
			got.Color, describe(tr.sent()))
	}
}

// TestRedIsNeverFollowedByAStaleFrame states the invariant the whole device
// exists for: once red has reached the light, nothing less urgent may follow
// it unless the tracker really did leave the blocked state.
//
// Here the blocked session never unblocks, so no non-red frame is ever
// legitimate after the first red one.
//
// # Why the blocking session's start is ordered, and the others are not
//
// This test used to post all four `started` events on unjoined goroutines and
// then post `blocked` for session "a". That is a race in the TEST, not in the
// relay: `started` moves a session to Working from ANY state, including
// Blocked, so an "a" start still in flight when `blocked` was applied landed
// afterwards and moved "a" back out of Blocked. The tracker's aggregate then
// genuinely became yellow again, the sender faithfully wrote that yellow, and
// the test called the correct frame stale.
//
// Under load that reordering happened often: about 1 run in 4 with the machine
// busy, versus never when the package ran alone. The failure was real output
// from a correct sender fed a state the test did not mean to create.
//
// The fix is to make the premise true rather than to widen a timing window.
// Session "a" is started and confirmed Working BEFORE it blocks, so nothing can
// move it back. Sessions "b", "c" and "d" still race freely, which is what
// keeps concurrent yellow sends piled up behind the hold: the overlap the test
// exists to exercise is preserved in full, and the assertion below is unchanged
// and no weaker.
//
// # Why "red reaches the wire" is a fair assertion here, and not in general
//
// The sender COALESCES: it reads tracker.Frame() at send time, so a state that
// is superseded before any send begins is legitimately never transmitted. In
// general "red appeared on the wire" is therefore NOT a guarantee the design
// makes, and asserting it against a freely-racing burst samples a timing
// window rather than testing a property. That is exactly how this test failed
// under -race with [yellow yellow yellow]: the yellow sends had all completed
// before `blocked` was applied, so red was correctly folded into nothing.
//
// A single held send removes the window instead of narrowing it. One goroutine
// owns the transport, so while the first send is parked in the hold no other
// send can begin; the test blocks session "a" and confirms the tracker holds
// Blocked while the sender is still parked. Once released, every frame the
// sender reads is red, because nothing in this test ever unblocks "a". Red
// reaching the wire is then a consequence of the construction rather than of
// the scheduler, which is what makes the stronger assertion legitimate.
func TestRedIsNeverFollowedByAStaleFrame(t *testing.T) {
	var (
		release = make(chan struct{})
		started = make(chan struct{}, 1)
		first   atomic.Bool
	)

	tr := &slowTransport{}
	// The FIRST send is held open until the test releases it, whatever colour
	// it turns out to be. That single hold is what makes this deterministic.
	//
	// One goroutine owns the transport, so while that first send is parked in
	// this hook NO other send can start. The test waits for the hold to be
	// entered before it posts `blocked`, so the tracker is guaranteed to hold
	// red before the sender is ever free to read a frame again. Nothing
	// unblocks session "a" afterwards, so every subsequent send reads red:
	// red MUST reach the wire, and no non-red frame can follow it.
	//
	// The previous construction held every non-red send behind an atomic flag
	// instead. That let the whole burst of yellow sends complete before
	// `blocked` was posted, after which the frame never changed again and the
	// wire legitimately ended [yellow yellow yellow] with red never sent. The
	// assertion then failed against a correct sender. Holding exactly one send
	// and ordering the block behind it removes that window entirely rather
	// than making it less likely.
	//
	// CompareAndSwap rather than sync.Once for the same reason as the test
	// above: Once holds its mutex for the whole call, so a second send would
	// block inside Once itself rather than proceed, turning a failure into a
	// deadlock.
	tr.before = func(f stoplight.Frame) {
		if first.CompareAndSwap(false, true) {
			started <- struct{}{}
			<-release
		}
	}

	r, addr, stop := runRelay(t, tr)
	defer stop()

	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	// Session "a" is the one that will block, so its start is ordered and
	// confirmed first: a late start for "a" is the only event that could
	// legitimately move it out of Blocked, and this test's premise is that
	// nothing does.
	//
	// The post runs on its own goroutine because the handler blocks behind the
	// held yellow send. Waiting for the tracker to show Working is what orders
	// it, not waiting for the HTTP response, which cannot arrive until release.
	go rawPost(addr, `{"session_id":"a","event":"started"}`)
	waitForState(t, r, "a", stoplight.StateWorking)

	// The remaining sessions start concurrently and are deliberately NOT
	// ordered. They keep several yellow sends racing behind the hold, which is
	// the overlap under test. None of them is the session that blocks, so
	// whenever they land they can only ever contribute yellow, never move the
	// aggregate off red.
	for _, id := range []string{"b", "c", "d"} {
		go rawPost(addr, `{"session_id":"`+id+`","event":"started"}`)
	}

	// The hold is now entered, so the single sender goroutine is parked inside
	// it. This is the ordering guarantee the whole test rests on: until
	// release, no frame can reach the wire, so `blocked` below is applied to
	// the tracker strictly before the sender reads its next frame.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first send never started")
	}

	// One session blocks. The aggregate goes red and stays red: nothing here
	// ever finishes or ends, so no non-red frame after this is ever legitimate.
	if code := postSession(t, addr, `{"session_id":"a","event":"blocked"}`); code != http.StatusNoContent {
		t.Fatalf("blocked: status = %d, want 204", code)
	}
	waitForState(t, r, "a", stoplight.StateBlocked)

	releaseAll()

	waitForWire(t, tr, 200*time.Millisecond, 10*time.Second)

	// The premise the assertion rests on: the session really is still blocked,
	// so every non-red frame after red below is a genuine ordering defect and
	// never an artefact of a late start. Checked before the frames are read so
	// a broken premise fails as itself rather than as a stale frame.
	if got := r.tracker.Aggregate(); got != stoplight.ColorRed {
		t.Fatalf("tracker aggregate = %s, want red: the test's premise broke, "+
			"a session left the blocked state\nwire: %s", got, describe(tr.sent()))
	}

	frames := tr.sent()
	redSeen := false
	for i, f := range frames {
		if f.Color == stoplight.ColorRed {
			redSeen = true
			continue
		}
		if redSeen {
			t.Fatalf("frame %d is %s after red, and no session unblocked\nwire: %s",
				i, f.Color, describe(frames))
		}
	}
	if !redSeen {
		t.Fatalf("red never reached the wire\nwire: %s", describe(frames))
	}
}

// TestSenderCoalescesRapidChanges proves the sender collapses a burst rather
// than sending one frame per request. The point is not the saving: it is that
// the sender reads the frame at send time, which is what makes a stale frame
// impossible.
//
// The final wire state must still be correct. Coalescing that lost the last
// change would be a worse defect than the one it replaced.
func TestSenderCoalescesRapidChanges(t *testing.T) {
	const n = 40

	tr := &slowTransport{
		// Slow enough that the burst piles up behind the first send.
		delayFor: func(stoplight.Frame) time.Duration { return 5 * time.Millisecond },
	}
	r, addr, stop := runRelay(t, tr)
	defer stop()

	// Each post adds a distinct session, so every one of them genuinely
	// changes the frame. De-duplication cannot explain any saving here.
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if code := postSession(t, addr, `{"session_id":"s`+itoa(i)+`","event":"started"}`); code != http.StatusNoContent {
				t.Errorf("post s%d: status = %d, want 204", i, code)
			}
		}(i)
	}
	wg.Wait()

	// Every post has been accepted, so the tracker holds all n sessions. The
	// wire is allowed to lag behind that, but only by coalescing: the frame it
	// finally settles on must be this one.
	waitForSessions(t, r, n)
	waitForWire(t, tr, 150*time.Millisecond, 10*time.Second)

	frames := tr.sent()
	if len(frames) >= n {
		t.Errorf("sends = %d for %d changes, want fewer: the sender is not coalescing", len(frames), n)
	}
	if len(frames) == 0 {
		t.Fatal("nothing reached the wire")
	}

	// Whatever was coalesced, the last frame must be the one the tracker holds
	// right now. That is the real invariant: the sender reads Frame() at send
	// time, so the wire cannot settle on an older state than the tracker's.
	//
	// The comparison is against tracker.Frame() rather than against n sessions
	// directly, because a frame carries at most MaxFrameSessions entries: the
	// device shows one session at a time and the firmware's line buffer bounds
	// the list. With n above that cap the final frame is legitimately truncated,
	// so asserting n entries here would be asserting the cap does not exist.
	last := frames[len(frames)-1]
	if want := r.tracker.Frame(); !last.Equal(want) {
		t.Errorf("last frame on the wire is not the tracker's current frame:\n got %d sessions, colour %s\nwant %d sessions, colour %s",
			len(last.Sessions), last.Color, len(want.Sessions), want.Color)
	}

	// The cap bounds the list, never the lamp: the aggregate is computed over
	// every live session, so all n working sessions must still show yellow.
	if len(last.Sessions) != stoplight.MaxFrameSessions {
		t.Errorf("last frame carries %d sessions, want %d (the frame cap)", len(last.Sessions), stoplight.MaxFrameSessions)
	}
	if last.Color != stoplight.ColorYellow {
		t.Errorf("last frame = %s, want yellow: %d sessions are working", last.Color, n)
	}
}

// TestSenderStopsWithTheRelay proves the sender goroutine does not outlive
// Run. A leaked goroutine holding the transport would keep writing to a closed
// device, and -race would report it against the next test's transport.
func TestSenderStopsWithTheRelay(t *testing.T) {
	tr := &slowTransport{}
	_, addr, stop := runRelay(t, tr)

	postSession(t, addr, `{"session_id":"a","event":"blocked"}`)
	waitForWire(t, tr, 50*time.Millisecond, 5*time.Second)

	stop()

	// After Run returns, nothing may still be sending.
	before := len(tr.sent())
	time.Sleep(100 * time.Millisecond)
	if after := len(tr.sent()); after != before {
		t.Errorf("frames sent after Run returned: %d then %d", before, after)
	}
}

// describe renders the wire as a colour sequence, so a failure says what the
// light actually did rather than dumping structs.
func describe(frames []stoplight.Frame) string {
	names := make([]string, 0, len(frames))
	for _, f := range frames {
		names = append(names, f.Color.String())
	}
	return "[" + strings.Join(names, " ") + "]"
}

// itoa keeps the session IDs readable without pulling strconv into a test file
// that needs it for nothing else.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
