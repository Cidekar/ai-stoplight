package ble

// These tests cover the paths that matter in the field and cannot be reached
// with a radio in the room: a light that is switched off, one that goes away
// mid-frame, and a frame too large for one write. They live in package ble
// rather than ble_test because they need newWithConnector, the seam that lets
// a test supply a link it controls.
//
// Every test here is bounded. Nothing waits on an unbuffered channel or a
// radio without a deadline, because a hanging test blocks the suite for
// everyone.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// frame is the frame these tests write. Its content does not matter beyond
// encoding to more than one chunk's worth of bytes in some tests, which
// bigFrame handles.
func frame() stoplight.Frame {
	return stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
		},
	}
}

// bigFrame builds a frame near the largest the relay can produce: eight
// sessions with UUID-length ids and long labels, which is what MaxFrameSessions
// allows and roughly 1200 bytes on the wire.
func bigFrame() stoplight.Frame {
	f := stoplight.Frame{Color: stoplight.ColorRed}
	for i := range 8 {
		f.Sessions = append(f.Sessions, stoplight.FrameSession{
			ID:    fmt.Sprintf("f47ac10b-58cc-4372-a567-0e02b2c3d4%02d", i),
			Label: fmt.Sprintf("service-with-a-long-branch-name-%d/feature/some-work", i),
			State: "needs you",
			Color: stoplight.ColorRed,
		})
	}
	return f
}

// errLightGone stands in for the error a stack returns once the peripheral is
// out of range.
var errLightGone = errors.New("peripheral disconnected")

// fakeLink is an in-memory stand-in for a GATT characteristic. Its behaviour is
// set by the caller: fail says the next write errors, short says it reports
// fewer bytes than it was given. It is safe for concurrent use, since Send and
// Close race in the relay.
type fakeLink struct {
	mu      sync.Mutex
	writes  [][]byte
	fail    bool
	short   bool
	failAt  int // when > 0, the write at this 1-based index fails
	count   int
	mtu     int
	closes  int
	onWrite func()
}

func (l *fakeLink) WriteChunk(b []byte) (int, error) {
	l.mu.Lock()
	hook := l.onWrite
	l.mu.Unlock()

	// The hook runs outside the link lock so it can call back into the
	// transport, which is how the Close-during-Send test gets its overlap.
	if hook != nil {
		hook()
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.count++

	if l.fail || (l.failAt > 0 && l.count == l.failAt) {
		return 0, errLightGone
	}
	if l.short {
		// A partial write: some bytes land, the rest do not, and no error is
		// reported. This is what a congested link does.
		n := len(b) / 2
		l.writes = append(l.writes, append([]byte(nil), b[:n]...))
		return n, nil
	}
	l.writes = append(l.writes, append([]byte(nil), b...))
	return len(b), nil
}

func (l *fakeLink) ChunkSize() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mtu
}

func (l *fakeLink) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closes++
	return nil
}

func (l *fakeLink) setFail(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail = v
}

func (l *fakeLink) setShort(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.short = v
}

// bytesWritten is every chunk concatenated: what the peripheral would have in
// its line buffer after reassembly.
func (l *fakeLink) bytesWritten() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []byte
	for _, w := range l.writes {
		out = append(out, w...)
	}
	return out
}

// chunks returns the individual writes, so a test can assert on the boundaries
// rather than only on the reassembled result.
func (l *fakeLink) chunks() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]byte, len(l.writes))
	for i, w := range l.writes {
		out[i] = append([]byte(nil), w...)
	}
	return out
}

func (l *fakeLink) closeCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closes
}

// linkFactory hands out fakeLinks and records every one it made, so a test can
// assert that Connect established a new link rather than reusing a dead one.
type linkFactory struct {
	mu    sync.Mutex
	links []*fakeLink
	mtu   int
	err   error
	calls int
}

func (f *linkFactory) connect(ctx context.Context, _ string) (Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	mtu := f.mtu
	if mtu == 0 {
		mtu = 20
	}
	l := &fakeLink{mtu: mtu}
	f.links = append(f.links, l)
	return l, nil
}

func (f *linkFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.links)
}

func (f *linkFactory) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *linkFactory) last() *fakeLink {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.links) == 0 {
		return nil
	}
	return f.links[len(f.links)-1]
}

func (f *linkFactory) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// connected builds a transport that is already linked to a fake light.
func connected(t *testing.T) (*Transport, *linkFactory) {
	t.Helper()
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return tr, f
}

// deadline fails the test if fn does not finish in time, instead of letting it
// hang the whole package.
func deadline(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v", what, d)
	}
}

// --- Connect: retry, backoff and cancellation -------------------------------

// 1. A light that is switched off must not make Connect give up. It retries
// until the context ends, which is the contract Relay.Run depends on.
func TestConnectRetriesUntilContextCancelled(t *testing.T) {
	f := &linkFactory{}
	f.setErr(ErrNotFound)
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()

	start := time.Now()
	var err error
	deadline(t, 5*time.Second, "Connect", func() { err = tr.Connect(ctx) })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Connect returned nil for a light that is not there")
	}
	if ctx.Err() == nil {
		t.Errorf("Connect returned %v before the context was cancelled", err)
	}
	// It must have retried rather than failing on the first miss.
	if elapsed < initialBackoff {
		t.Errorf("Connect gave up after %v, expected it to retry", elapsed)
	}
	if got := f.attempts(); got < 2 {
		t.Errorf("Connect made %d attempts, want it to retry", got)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a failed Connect")
	}
}

// 2. The backoff must actually back off, or a light that is off becomes a
// tight scan loop that drains the battery on both ends.
func TestConnectBackoffGrows(t *testing.T) {
	f := &linkFactory{}
	f.setErr(ErrNotFound)
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	// In a window of this length a fixed 250ms retry would make about 8
	// attempts; doubling makes 4 (at 0, 250, 750 and 1750ms).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	deadline(t, 6*time.Second, "Connect", func() { _ = tr.Connect(ctx) })

	attempts := f.attempts()
	if attempts < 2 {
		t.Fatalf("Connect made %d attempts, want several", attempts)
	}
	if attempts > 6 {
		t.Errorf("Connect made %d attempts in 2s; the backoff is not growing", attempts)
	}
}

// 3. A context cancelled while the retry loop is sleeping must return at once,
// not after the remaining backoff. The relay cancels on SIGTERM and the
// service manager does not wait long.
func TestConnectReturnsPromptlyOnCancelDuringBackoff(t *testing.T) {
	f := &linkFactory{}
	f.setErr(ErrNotFound)
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel while the loop is certain to be inside a backoff sleep rather
	// than inside an attempt.
	//
	// cancelledAt is recorded by the goroutine that cancels, so the measurement
	// below starts at the cancel itself rather than at the start of Connect.
	// Timing from the start would fold in the 300ms wait and however long the
	// scheduler took to run this goroutine, and under load that inflates the
	// figure for reasons that have nothing to do with what is being tested.
	var (
		cancelMu    sync.Mutex
		cancelledAt time.Time
	)
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancelMu.Lock()
		cancelledAt = time.Now()
		cancelMu.Unlock()
		cancel()
	}()

	var err error
	deadline(t, 30*time.Second, "Connect", func() { err = tr.Connect(ctx) })
	returnedAt := time.Now()

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Connect error = %v, want context.Canceled", err)
	}

	cancelMu.Lock()
	cancelled := cancelledAt
	cancelMu.Unlock()
	if cancelled.IsZero() {
		t.Fatal("Connect returned before the context was cancelled")
	}

	// The point of the test: the wait is interruptible, so the cancel is
	// noticed part way through a backoff rather than after it has run out.
	//
	// By the time the cancel lands the loop has failed several times, so the
	// backoff it is sitting in is already at or near the 5s cap. A budget of
	// 2s is therefore comfortably inside one backoff -- which is what proves
	// the timer is selected on -- while still leaving room for a loaded
	// machine to schedule the return.
	if noticed := returnedAt.Sub(cancelled); noticed > 2*time.Second {
		t.Errorf("Connect took %v after the cancel to return; the backoff wait "+
			"is not interruptible", noticed)
	}
}

// 4. An already-cancelled context must be honoured before any radio work.
func TestConnectReturnsWhenContextAlreadyCancelled(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := tr.Connect(ctx); err == nil {
		t.Fatal("Connect returned nil for an already-cancelled context")
	}
	if f.attempts() != 0 {
		t.Errorf("Connect made %d attempts on a cancelled context, want 0", f.attempts())
	}
}

// 5. The light is switched on a moment after the relay starts. Connect must
// pick it up rather than having given up.
func TestConnectSucceedsWhenLightAppears(t *testing.T) {
	f := &linkFactory{}
	f.setErr(ErrNotFound)
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	go func() {
		time.Sleep(300 * time.Millisecond)
		f.setErr(nil)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()

	var err error
	deadline(t, 9500*time.Millisecond, "Connect", func() { err = tr.Connect(ctx) })
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !tr.Connected() {
		t.Error("Connected() is false after the light appeared")
	}
}

// 6. A permission refusal is the one error worth stopping for: only the user
// can reverse it, and a relay spinning silently on a denied prompt is worse
// than one that says why it stopped.
func TestConnectStopsOnPermissionDenied(t *testing.T) {
	f := &linkFactory{}
	f.setErr(fmt.Errorf("%w: user said no", ErrPermissionDenied))
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	// The context is given a long life so that it cannot be what ends Connect.
	// The check below that it never expired is then meaningful: Connect must
	// return because it recognised the refusal, not because it ran out of time.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	var err error
	deadline(t, 30*time.Second, "Connect", func() { err = tr.Connect(ctx) })

	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Connect error = %v, want it to wrap ErrPermissionDenied", err)
	}
	if ctx.Err() != nil {
		t.Error("Connect waited for the context instead of returning at once")
	}
	// A refusal must not be retried, so the return is immediate in the sense
	// that matters: no backoff is waited out. The single-attempt check below
	// is the real proof of that; this bound only catches a Connect that sat
	// there, and it is loose so that a loaded machine cannot fail it.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Connect took %v to report a permission refusal", elapsed)
	}
	if got := f.attempts(); got != 1 {
		t.Errorf("Connect made %d attempts on a permission refusal, want 1", got)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a permission refusal")
	}
}

// 7. The radio being switched off is not a permission problem: switching it on
// fixes it, so Connect must keep trying.
func TestConnectRetriesWhenRadioUnavailable(t *testing.T) {
	f := &linkFactory{}
	f.setErr(fmt.Errorf("%w: powered off", ErrUnavailable))
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()

	deadline(t, 4*time.Second, "Connect", func() { _ = tr.Connect(ctx) })

	if got := f.attempts(); got < 2 {
		t.Errorf("Connect made %d attempts with the radio off, want it to retry", got)
	}
}

// 8. Connect while already linked is a no-op, not a second connection. The
// relay's poll loop calls it whenever it sees a gap.
func TestConnectIsIdempotent(t *testing.T) {
	tr, f := connected(t)
	defer tr.Close()

	for i := range 3 {
		if err := tr.Connect(context.Background()); err != nil {
			t.Fatalf("Connect #%d: %v", i+2, err)
		}
	}
	if got := f.count(); got != 1 {
		t.Errorf("opened %d links, want 1", got)
	}
	if !tr.Connected() {
		t.Error("Connected() is false after repeated Connect")
	}
}

// 9. Close is final. A retry loop still running must not resurrect the link.
func TestConnectAfterCloseDoesNotReopen(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Connect(context.Background()); err == nil {
		t.Error("Connect reopened a closed transport")
	}
	if tr.Connected() {
		t.Error("Connected() is true after Connect on a closed transport")
	}
}

// --- Send: failure, recovery, reuse -----------------------------------------

// 10. A write that fails mid-stream must report the error, drop the link, and
// leave the transport reusable. This is the light carried out of range: the
// relay calls Connect again and carries on.
func TestSendWriteFailureDisconnectsAndStaysUsable(t *testing.T) {
	tr, f := connected(t)
	defer tr.Close()

	// One good frame first, so the failure really is mid-stream.
	if err := tr.Send(frame()); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if !tr.Connected() {
		t.Fatal("Connected() is false after a good Send")
	}

	first := f.last()
	first.setFail(true)

	err := tr.Send(frame())
	if err == nil {
		t.Fatal("Send returned nil when the write failed")
	}
	if !errors.Is(err, errLightGone) {
		t.Errorf("Send error = %v, want it to wrap %v", err, errLightGone)
	}
	if !strings.Contains(err.Error(), "StoplightTEST") {
		t.Errorf("Send error = %q, want it to name the light", err)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a failed write")
	}
	if first.closeCount() != 1 {
		t.Errorf("failed link closed %d times, want 1", first.closeCount())
	}

	// The transport is not dead, only disconnected: reconnect establishes a
	// fresh link and the next frame lands on it.
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !tr.Connected() {
		t.Fatal("Connected() is false after reconnect")
	}
	if f.count() != 2 {
		t.Errorf("opened %d links, want 2 (the dead link must not be reused)", f.count())
	}
	if err := tr.Send(frame()); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	if len(f.last().bytesWritten()) == 0 {
		t.Error("the reopened link received nothing")
	}
}

// 11. A short write reports no error but drops bytes out of the middle of a
// frame. The firmware would splice the remainder onto a corrupt line, so Send
// must call it an error rather than carry on with the next chunk.
func TestSendShortWriteIsAnError(t *testing.T) {
	tr, f := connected(t)
	defer tr.Close()

	f.last().setShort(true)

	err := tr.Send(frame())
	if err == nil {
		t.Fatal("Send returned nil for a short write")
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("Send error = %v, want it to wrap io.ErrShortWrite", err)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a short write")
	}

	// And it recovers, same as any other write failure.
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := tr.Send(frame()); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
}

// 12. A failure partway through a chunked frame must abandon the rest of that
// frame rather than writing its tail onto a link that is already broken.
func TestSendStopsAtTheFirstFailedChunk(t *testing.T) {
	f := &linkFactory{mtu: minChunkSize}
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	link := f.last()
	link.mu.Lock()
	link.failAt = 3 // the third chunk of many
	link.mu.Unlock()

	err := tr.Send(bigFrame())
	if err == nil {
		t.Fatal("Send returned nil when a middle chunk failed")
	}
	if !errors.Is(err, errLightGone) {
		t.Errorf("Send error = %v, want it to wrap %v", err, errLightGone)
	}
	if got := len(link.chunks()); got != 2 {
		t.Errorf("wrote %d chunks after the third failed, want 2", got)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a chunk failed")
	}
}

// 13. Send before Connect must return a clear error, not panic on a nil link.
// The relay can be asked for a frame before the light has been found.
func TestSendBeforeConnectReturnsClearError(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send before Connect panicked: %v", r)
		}
	}()

	err := tr.Send(frame())
	if err == nil {
		t.Fatal("Send returned nil before Connect")
	}
	if !errors.Is(err, ErrNotConnected) {
		t.Errorf("Send error = %v, want it to wrap ErrNotConnected", err)
	}
	if !strings.Contains(err.Error(), "StoplightTEST") {
		t.Errorf("Send error = %q, want it to name the light", err)
	}
	if f.count() != 0 {
		t.Errorf("Send opened %d links; it must not connect on its own", f.count())
	}
}

// 14. Send after Close must fail rather than silently reconnecting a link the
// caller released.
func TestSendAfterCloseFails(t *testing.T) {
	tr, _ := connected(t)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Send(frame()); err == nil {
		t.Error("Send returned nil after Close")
	}
}

// 15. A light that stays out of range fails every write. Relay.Run retries
// forever, so the transport must never latch into a permanently unusable
// state: after any number of failures, one good write still succeeds.
func TestRepeatedSendFailuresNeverPermanentlyBreakTransport(t *testing.T) {
	inner := &linkFactory{}
	failing := &failingFactory{inner: inner}
	tr := newWithConnector("StoplightTEST", failing.connect)
	defer tr.Close()

	const rounds = 25
	for i := range rounds {
		if err := tr.Connect(context.Background()); err != nil {
			t.Fatalf("Connect on round %d: %v", i, err)
		}
		if !tr.Connected() {
			t.Fatalf("Connected() is false after Connect on round %d", i)
		}
		if err := tr.Send(frame()); err == nil {
			t.Fatalf("Send succeeded on round %d, want the injected failure", i)
		}
		if tr.Connected() {
			t.Fatalf("Connected() is true after a failed Send on round %d", i)
		}
	}
	if got := inner.count(); got != rounds {
		t.Errorf("opened %d links over %d rounds, want one per round", got, rounds)
	}

	// The light comes back. Nothing about the earlier failures may stop this.
	failing.heal()
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect after recovery: %v", err)
	}
	if err := tr.Send(frame()); err != nil {
		t.Fatalf("Send after recovery: %v", err)
	}
	if !tr.Connected() {
		t.Error("Connected() is false after a good Send")
	}
	if len(inner.last().bytesWritten()) == 0 {
		t.Error("the recovered link received nothing")
	}
}

// failingFactory hands out links that fail every write until heal is called.
type failingFactory struct {
	inner  *linkFactory
	healed atomic.Bool
}

func (f *failingFactory) connect(ctx context.Context, name string) (Link, error) {
	l, err := f.inner.connect(ctx, name)
	if err != nil {
		return nil, err
	}
	l.(*fakeLink).setFail(!f.healed.Load())
	return l, nil
}

func (f *failingFactory) heal() { f.healed.Store(true) }

// --- Chunking ---------------------------------------------------------------

// 16. A frame larger than the MTU must arrive complete and in order, whatever
// the chunk size. The reassembled bytes are what the firmware's line buffer
// holds, and they must equal the frame exactly, newline included.
func TestSendChunksLargeFrame(t *testing.T) {
	// Sizes spanning the clamp: the 20-byte floor, a typical negotiated value,
	// and the 244-byte ceiling.
	for _, mtu := range []int{minChunkSize, 23, 100, maxChunkSize} {
		t.Run(fmt.Sprintf("mtu=%d", mtu), func(t *testing.T) {
			f := &linkFactory{mtu: mtu}
			tr := newWithConnector("StoplightTEST", f.connect)
			defer tr.Close()
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatalf("Connect: %v", err)
			}

			fr := bigFrame()
			want, err := transport.EncodeFrame(fr)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if len(want) <= mtu {
				t.Fatalf("test frame is %d bytes, not larger than the %d-byte MTU", len(want), mtu)
			}

			if err := tr.Send(fr); err != nil {
				t.Fatalf("Send: %v", err)
			}

			link := f.last()
			got := link.bytesWritten()
			if string(got) != string(want) {
				t.Errorf("reassembled %d bytes, want %d", len(got), len(want))
			}
			// The delimiter is the whole point: without it the firmware never
			// completes the frame.
			if len(got) == 0 || got[len(got)-1] != '\n' {
				t.Error("the reassembled frame does not end in a newline")
			}

			chunks := link.chunks()
			if len(chunks) < 2 {
				t.Fatalf("wrote %d chunks for a %d-byte frame at MTU %d, want several",
					len(chunks), len(want), mtu)
			}
			for i, c := range chunks {
				if len(c) > mtu {
					t.Errorf("chunk %d is %d bytes, over the %d-byte MTU", i, len(c), mtu)
				}
				if len(c) == 0 {
					t.Errorf("chunk %d is empty", i)
				}
			}
			// Every chunk but the last must be full, or the transport is
			// making more round trips than it needs.
			for i, c := range chunks[:len(chunks)-1] {
				if len(c) != mtu {
					t.Errorf("chunk %d is %d bytes, want a full %d", i, len(c), mtu)
				}
			}
		})
	}
}

// 17. The boundary cases. A frame of exactly the chunk size must go in one
// write with no empty trailing chunk, and one byte over must go in exactly
// two. The firmware must not wait for a chunk that never comes.
func TestChunkBoundaries(t *testing.T) {
	const size = 20

	cases := []struct {
		name       string
		length     int
		wantChunks int
	}{
		{"one byte", 1, 1},
		{"one under", size - 1, 1},
		{"exactly the MTU", size, 1},
		{"one over", size + 1, 2},
		{"exactly two MTUs", size * 2, 2},
		{"two MTUs plus one", size*2 + 1, 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			link := &fakeLink{mtu: size}
			data := make([]byte, tc.length)
			for i := range data {
				data[i] = byte('a' + i%26)
			}

			if err := writeChunked(link, data); err != nil {
				t.Fatalf("writeChunked: %v", err)
			}

			chunks := link.chunks()
			if len(chunks) != tc.wantChunks {
				t.Errorf("wrote %d chunks for %d bytes, want %d",
					len(chunks), tc.length, tc.wantChunks)
			}
			for i, c := range chunks {
				if len(c) == 0 {
					t.Errorf("chunk %d is empty; the peripheral must not wait for one", i)
				}
				if len(c) > size {
					t.Errorf("chunk %d is %d bytes, over the %d-byte MTU", i, len(c), size)
				}
			}
			if got := link.bytesWritten(); string(got) != string(data) {
				t.Errorf("reassembled %d bytes, want %d", len(got), len(data))
			}
		})
	}
}

// 18. An empty payload must not produce a write. It cannot happen through Send,
// because every encoded frame carries at least a newline, but writeChunked must
// not emit an empty chunk if it ever did.
func TestWriteChunkedEmptyPayloadWritesNothing(t *testing.T) {
	link := &fakeLink{mtu: 20}
	if err := writeChunked(link, nil); err != nil {
		t.Fatalf("writeChunked: %v", err)
	}
	if got := len(link.chunks()); got != 0 {
		t.Errorf("wrote %d chunks for an empty payload, want 0", got)
	}
}

// 19. A stack that reports nothing, or nonsense, must not turn every write
// into a failure. The clamp is what keeps a frame going through.
func TestChunkSizeClamp(t *testing.T) {
	cases := []struct {
		reported, want int
	}{
		{0, minChunkSize},  // no MTU reported
		{-7, minChunkSize}, // GetMTU returned less than the ATT header
		{3, minChunkSize},  // implausibly small
		{minChunkSize, minChunkSize},
		{100, 100},
		{maxChunkSize, maxChunkSize},
		{9999, maxChunkSize}, // a peripheral overstating its MTU
	}
	for _, tc := range cases {
		if got := chunkSize(tc.reported); got != tc.want {
			t.Errorf("chunkSize(%d) = %d, want %d", tc.reported, got, tc.want)
		}
	}
}

// 20. A link that reports a nonsense MTU must still deliver a whole frame,
// because the clamp turns the bad number into the safe floor.
func TestSendSucceedsWithNonsenseMTU(t *testing.T) {
	for _, mtu := range []int{0, -1, 100000} {
		t.Run(fmt.Sprintf("mtu=%d", mtu), func(t *testing.T) {
			f := &linkFactory{mtu: mtu}
			tr := newWithConnector("StoplightTEST", f.connect)
			defer tr.Close()
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatalf("Connect: %v", err)
			}

			fr := bigFrame()
			if err := tr.Send(fr); err != nil {
				t.Fatalf("Send: %v", err)
			}
			want, err := transport.EncodeFrame(fr)
			if err != nil {
				t.Fatalf("EncodeFrame: %v", err)
			}
			if got := f.last().bytesWritten(); string(got) != string(want) {
				t.Errorf("reassembled %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// 21. Frames must arrive in order and stay separable. The firmware splits on
// newlines, so three frames must reassemble into three lines however they were
// chunked.
func TestSendFramesReassembleIntoSeparateLines(t *testing.T) {
	f := &linkFactory{mtu: minChunkSize}
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for _, c := range []stoplight.Color{stoplight.ColorGreen, stoplight.ColorYellow, stoplight.ColorRed} {
		if err := tr.Send(stoplight.Frame{Color: c}); err != nil {
			t.Fatalf("Send(%v): %v", c, err)
		}
	}

	data := f.last().bytesWritten()
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d frames, want 3: %q", len(lines), data)
	}
	for i, want := range []string{`"green"`, `"yellow"`, `"red"`} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("frame %d = %q, want it to contain %s", i, lines[i], want)
		}
	}
}

// --- Close and concurrency --------------------------------------------------

// 22. Closing twice is not an error, because shutdown paths overlap.
func TestCloseIsIdempotent(t *testing.T) {
	tr, f := connected(t)
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
	if got := f.last().closeCount(); got != 1 {
		t.Errorf("link closed %d times, want 1", got)
	}
}

// 23. Close without Connect must not panic on a nil link.
func TestCloseWithoutConnect(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	if err := tr.Close(); err != nil {
		t.Errorf("Close without Connect: %v", err)
	}
}

// 24. Connected must not block behind a wedged write. This is the bug the
// serial reviewer found there -- the mutex held across Write -- and the reason
// the connection state here is an atomic. A radio write can stall for seconds,
// and `stoplight status` must still answer.
func TestConnectedDoesNotBlockOnAWedgedWrite(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	defer tr.Close()
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	release := make(chan struct{})
	inWrite := make(chan struct{})
	var once sync.Once

	link := f.last()
	link.mu.Lock()
	link.onWrite = func() {
		once.Do(func() { close(inWrite) })
		<-release
	}
	link.mu.Unlock()

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		_ = tr.Send(frame())
	}()

	select {
	case <-inWrite:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("the write never started")
	}

	// The write is stuck. Connected and Name must both answer anyway.
	answered := make(chan bool, 1)
	go func() {
		_ = tr.Name()
		answered <- tr.Connected()
	}()

	select {
	case got := <-answered:
		if !got {
			t.Error("Connected() = false while a write is in flight, want true")
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("Connected() blocked behind a wedged write")
	}

	close(release)
	select {
	case <-sendDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Send never returned")
	}
}

// 25. Shutdown races the relay's run loop. Under -race this catches an
// unguarded field; the deadline catches a lock ordering mistake. Writes fail
// partway through, so the disconnect path runs concurrently with Close too.
func TestConcurrentSendCloseAndConnectedUnderRace(t *testing.T) {
	f := &linkFactory{mtu: minChunkSize}
	tr := newWithConnector("StoplightTEST", f.connect)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 50 {
				// Errors are expected once Close lands; only a panic or a
				// race matters here.
				_ = tr.Send(frame())
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 100 {
				_ = tr.Connected()
				_ = tr.Name()
			}
		}()
	}
	// Flip the live link to failing partway through, so Send's disconnect
	// branch runs while the other goroutines are still going.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(2 * time.Millisecond)
		if l := f.last(); l != nil {
			l.setFail(true)
		}
	}()
	// Reconnect alongside everything else: Relay.Run does exactly this.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 20 {
			_ = tr.Connect(context.Background())
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(5 * time.Millisecond)
		_ = tr.Close()
		_ = tr.Close()
	}()

	close(start)
	deadline(t, 8*time.Second, "concurrent Send, Close and Connected", wg.Wait)

	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
}

// 26. Two Connects racing must not leave a link orphaned. Whoever loses closes
// the link it made rather than overwriting the winner's, or the light is left
// holding a connection nothing will ever close.
func TestConcurrentConnectDoesNotOrphanALink(t *testing.T) {
	f := &linkFactory{}
	// A slow dial widens the window in which both attempts are in flight.
	slow := func(ctx context.Context, name string) (Link, error) {
		time.Sleep(20 * time.Millisecond)
		return f.connect(ctx, name)
	}
	tr := newWithConnector("StoplightTEST", slow)
	defer tr.Close()

	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = tr.Connect(context.Background())
		}()
	}
	deadline(t, 5*time.Second, "concurrent Connect", wg.Wait)

	if !tr.Connected() {
		t.Fatal("Connected() is false after concurrent Connect")
	}

	// Every link made but not kept must have been closed. One stays open: the
	// live one.
	f.mu.Lock()
	made := len(f.links)
	open := 0
	for _, l := range f.links {
		if l.closeCount() == 0 {
			open++
		}
	}
	f.mu.Unlock()

	if open != 1 {
		t.Errorf("%d of %d links are still open, want exactly 1", open, made)
	}
}

// 27. A Close landing while a dial is in flight must not leave the new link
// open. The dial happens outside the mutex, so this window is real.
func TestCloseDuringDialClosesTheNewLink(t *testing.T) {
	f := &linkFactory{}
	dialing := make(chan struct{})
	var once sync.Once

	slow := func(ctx context.Context, name string) (Link, error) {
		once.Do(func() { close(dialing) })
		time.Sleep(100 * time.Millisecond)
		return f.connect(ctx, name)
	}
	tr := newWithConnector("StoplightTEST", slow)

	connectDone := make(chan error, 1)
	go func() { connectDone <- tr.Connect(context.Background()) }()

	select {
	case <-dialing:
	case <-time.After(2 * time.Second):
		t.Fatal("the dial never started")
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-connectDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Connect never returned after Close")
	}

	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
	// The link the dial produced must not be left open on the light.
	if l := f.last(); l != nil && l.closeCount() == 0 {
		t.Error("the link established during Close was left open")
	}
}

// 28. Close during an in-flight Send must not panic or deadlock.
func TestCloseDuringInFlightSend(t *testing.T) {
	f := &linkFactory{}
	tr := newWithConnector("StoplightTEST", f.connect)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	// A frame is chunked, so the hook fires once per chunk. Only the first
	// one starts the Close; without the guard the later chunks close an
	// already-closed channel.
	var once sync.Once

	link := f.last()
	link.mu.Lock()
	link.onWrite = func() {
		// Fire Close while this write is still running. Close blocks on the
		// transport mutex that Send holds, which is exactly the overlap under
		// test; do not wait for it here or the two would deadlock by design.
		fired := false
		once.Do(func() {
			fired = true
			go func() {
				close(closeStarted)
				closeDone <- tr.Close()
			}()
		})
		if !fired {
			return
		}
		select {
		case <-closeStarted:
		case <-time.After(time.Second):
		}
		time.Sleep(20 * time.Millisecond)
	}
	link.mu.Unlock()

	deadline(t, 3*time.Second, "Send racing Close", func() {
		_ = tr.Send(frame())
	})

	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close never returned; the transport deadlocked")
	}

	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
	if err := tr.Send(frame()); err == nil {
		t.Error("Send returned nil after Close")
	}
}
