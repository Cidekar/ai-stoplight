package serial

// These tests cover the write-failure paths, which are the ones that matter
// most in the field: a board that is unplugged, reset or browned out mid
// session. They live in package serial rather than serial_test because they
// need newWithOpener, the seam that lets a test supply a writer it controls.
//
// Every test here is bounded. Nothing waits on an unbuffered channel or a
// network read without a deadline, because a hanging test blocks the suite for
// everyone.

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// frame is the frame these tests write. Its content does not matter; only that
// it encodes to more than zero bytes.
func frame() stoplight.Frame {
	return stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
		},
	}
}

// errUnplugged stands in for the EIO a real device returns once it is gone.
var errUnplugged = errors.New("device not configured")

// fakePort is an in-memory stand-in for the device file. Its behaviour is set
// by the caller: fail says the next write returns errUnplugged, short says it
// reports fewer bytes than it was given. It is safe for concurrent use, since
// Send and Close race in the relay.
type fakePort struct {
	mu      sync.Mutex
	written []byte
	fail    bool
	short   bool
	closes  int
	onWrite func()
}

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	hook := p.onWrite
	p.mu.Unlock()

	// The hook runs outside the port lock so it can call back into the
	// transport, which is how the Close-during-Send test gets its overlap.
	if hook != nil {
		hook()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return 0, errUnplugged
	}
	if p.short {
		// A partial write: some bytes land, the rest do not, and no error is
		// reported. This is what a device does when its buffer fills.
		n := len(b) / 2
		p.written = append(p.written, b[:n]...)
		return n, nil
	}
	p.written = append(p.written, b...)
	return len(b), nil
}

func (p *fakePort) Read([]byte) (int, error) { return 0, io.EOF }

func (p *fakePort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

func (p *fakePort) setFail(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fail = v
}

func (p *fakePort) setShort(v bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.short = v
}

func (p *fakePort) bytesWritten() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.written...)
}

func (p *fakePort) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

// portFactory hands out fakePorts and records every one it made, so a test can
// assert that Connect reopened the device rather than reusing a dead handle.
type portFactory struct {
	mu    sync.Mutex
	ports []*fakePort
}

func (f *portFactory) open(string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := &fakePort{}
	f.ports = append(f.ports, p)
	return p, nil
}

func (f *portFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ports)
}

func (f *portFactory) last() *fakePort {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ports) == 0 {
		return nil
	}
	return f.ports[len(f.ports)-1]
}

// connected builds a transport that is already open on a fake port.
func connected(t *testing.T) (*Transport, *portFactory) {
	t.Helper()
	f := &portFactory{}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", f.open)
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

// 1. A write that fails mid-stream must report the error, drop the port, and
// leave the transport reusable. This is the unplugged board: the relay calls
// Connect again and carries on.
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
	if !errors.Is(err, errUnplugged) {
		t.Errorf("Send error = %v, want it to wrap %v", err, errUnplugged)
	}
	if !strings.Contains(err.Error(), "/dev/cu.usbmodemFAKE") {
		t.Errorf("Send error = %q, want it to name the device", err)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a failed write")
	}
	if first.closeCount() != 1 {
		t.Errorf("failed port closed %d times, want 1", first.closeCount())
	}

	// The transport is not dead, only disconnected: reconnect opens a fresh
	// port and the next frame lands on it.
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !tr.Connected() {
		t.Fatal("Connected() is false after reconnect")
	}
	if f.count() != 2 {
		t.Errorf("opened %d ports, want 2 (the dead handle must not be reused)", f.count())
	}
	if err := tr.Send(frame()); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	if len(f.last().bytesWritten()) == 0 {
		t.Error("the reopened port received nothing")
	}
}

// 2. A short write reports no error but truncates the frame. The firmware
// splits on the trailing newline, so a truncated frame swallows the next one.
// Send must call that an error rather than accept it.
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

// 3. Close arriving while a Send is inside Write must not panic or deadlock.
// The port's write hook calls Close from another goroutine and waits for it,
// so the two really do overlap rather than merely interleave.
func TestCloseDuringInFlightSend(t *testing.T) {
	f := &portFactory{}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", f.open)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)

	port := f.last()
	port.mu.Lock()
	port.onWrite = func() {
		// Fire Close while this Write is still running. Close blocks on the
		// transport mutex that Send holds, which is exactly the overlap under
		// test; do not wait for it here or the two would deadlock by design.
		go func() {
			close(closeStarted)
			closeDone <- tr.Close()
		}()
		select {
		case <-closeStarted:
		case <-time.After(time.Second):
		}
		// Give the Close goroutine a moment to reach the mutex.
		time.Sleep(20 * time.Millisecond)
	}
	port.mu.Unlock()

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
	// Whatever the interleaving, a later Send must fail cleanly, not panic on
	// a nil port.
	if err := tr.Send(frame()); err == nil {
		t.Error("Send returned nil after Close")
	}
}

// 4. Send before Connect must return a clear error, not panic on a nil writer.
// The relay can be asked for a frame before the board has enumerated.
func TestSendBeforeConnectReturnsClearError(t *testing.T) {
	f := &portFactory{}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", f.open)
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
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("Send error = %q, want it to say the transport is not connected", err)
	}
	if !strings.Contains(err.Error(), "/dev/cu.usbmodemFAKE") {
		t.Errorf("Send error = %q, want it to name the device", err)
	}
	if f.count() != 0 {
		t.Errorf("Send opened %d ports; it must not open the device itself", f.count())
	}
}

// 5. A board that stays unplugged fails every write. Relay.Run retries
// forever, so the transport must never latch into a permanently unusable
// state: after any number of failures, one good write still succeeds.
func TestRepeatedSendFailuresNeverPermanentlyBreakTransport(t *testing.T) {
	f := &portFactory{}
	// Every port this factory makes starts out failing, mimicking a device
	// that is present in /dev but no longer answers.
	failing := &failingFactory{inner: f}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", failing.open)
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
	if got := failing.opened(); got != rounds {
		t.Errorf("opened %d ports over %d rounds, want one per round", got, rounds)
	}

	// The board comes back. Nothing about the earlier failures may stop this.
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
	if len(failing.last().bytesWritten()) == 0 {
		t.Error("the recovered port received nothing")
	}
}

// failingFactory hands out ports that fail every write until heal is called.
type failingFactory struct {
	inner  *portFactory
	healed atomic.Bool
}

func (f *failingFactory) open(path string) (io.ReadWriteCloser, error) {
	rwc, err := f.inner.open(path)
	if err != nil {
		return nil, err
	}
	p := rwc.(*fakePort)
	p.setFail(!f.healed.Load())
	return p, nil
}

func (f *failingFactory) heal()           { f.healed.Store(true) }
func (f *failingFactory) opened() int     { return f.inner.count() }
func (f *failingFactory) last() *fakePort { return f.inner.last() }

// 6. Shutdown races the relay's run loop. Under -race this catches an
// unguarded field; the deadline catches a lock ordering mistake. Writes fail
// partway through, so the disconnect path runs concurrently with Close too.
func TestConcurrentSendAndCloseUnderRace(t *testing.T) {
	f := &portFactory{}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", f.open)
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
	// Flip the live port to failing partway through, so Send's disconnect
	// branch runs while the other goroutines are still going.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		time.Sleep(2 * time.Millisecond)
		if p := f.last(); p != nil {
			p.setFail(true)
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
	deadline(t, 4*time.Second, "concurrent Send and Close", wg.Wait)

	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
}

// wedgePort blocks inside Write until it is released. It stands in for a board
// that has stopped draining its CDC queue: on darwin the Go runtime does not
// poll a character device, so a Write to such a board blocks in the kernel with
// no deadline. Close releases the block, the way closing a real fd aborts the
// pending write.
type wedgePort struct {
	release chan struct{}
	once    sync.Once
}

func (p *wedgePort) Write(b []byte) (int, error) {
	<-p.release
	return len(b), nil
}
func (p *wedgePort) Read([]byte) (int, error) { return 0, io.EOF }
func (p *wedgePort) Close() error {
	p.once.Do(func() { close(p.release) })
	return nil
}

// 7. The regression for issue #8: a Send wedged in Write must not block
// Connected or Close. The whole fix exists so the relay's connection poll,
// `stoplight status`, and shutdown stay responsive while a board is stuck.
func TestWedgedWriteDoesNotBlockConnectedOrClose(t *testing.T) {
	wp := &wedgePort{release: make(chan struct{})}
	tr := newWithOpener("/dev/cu.usbmodemFAKE", func(string) (io.ReadWriteCloser, error) {
		return wp, nil
	})
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Start a Send that wedges inside Write and never returns on its own.
	sendReturned := make(chan struct{})
	go func() {
		defer close(sendReturned)
		_ = tr.Send(frame())
	}()
	// Give the Send time to reach Write and park there.
	time.Sleep(50 * time.Millisecond)

	// Connected answers at once, reading the atomic rather than the lock the
	// write path would hold if it took one.
	deadline(t, time.Second, "Connected during a wedged write", func() {
		if !tr.Connected() {
			t.Error("Connected() is false while the port is open")
		}
	})

	// Close returns at once too, and releasing the wedged write lets the Send
	// goroutine unwind rather than leak.
	deadline(t, time.Second, "Close during a wedged write", func() {
		if err := tr.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	deadline(t, time.Second, "wedged Send after Close", func() {
		<-sendReturned
	})
	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
}
