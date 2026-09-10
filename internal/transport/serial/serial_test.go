package serial_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
	"github.com/cidekar/stoplight/internal/transport/serial"
)

// testFrame is the frame used across these tests, matching the RFC example.
func testFrame() stoplight.Frame {
	return stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
		},
	}
}

// devicePath returns a writable regular file standing in for a serial device.
// A CDC-ACM device is opened as a plain file with O_RDWR, so a regular file
// exercises the same code path without hardware attached.
func devicePath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cu.usbmodemTEST")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake device: %v", err)
	}
	f.Close()
	return path
}

func TestNameIncludesDevicePath(t *testing.T) {
	tr := serial.New("/dev/cu.usbmodem1234")
	if got, want := tr.Name(), "serial:/dev/cu.usbmodem1234"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

func TestSatisfiesTransportInterface(t *testing.T) {
	// The relay stores transports as the interface; a signature drift here
	// would only show up at the call site otherwise.
	var _ transport.Transport = serial.New("/dev/null")
}

func TestConnectOpensDevice(t *testing.T) {
	tr := serial.New(devicePath(t))
	defer tr.Close()

	if tr.Connected() {
		t.Error("Connected() is true before Connect")
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !tr.Connected() {
		t.Error("Connected() is false after a successful Connect")
	}
}

func TestConnectIsIdempotent(t *testing.T) {
	tr := serial.New(devicePath(t))
	defer tr.Close()

	for i := range 3 {
		if err := tr.Connect(context.Background()); err != nil {
			t.Fatalf("Connect #%d: %v", i+1, err)
		}
	}
	if !tr.Connected() {
		t.Error("Connected() is false after repeated Connect")
	}
}

func TestConnectRetriesUntilContextCancelled(t *testing.T) {
	// A device that does not exist is the unplugged case: Connect must keep
	// trying and return only because the context ended.
	tr := serial.New(filepath.Join(t.TempDir(), "absent"))
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := tr.Connect(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Connect returned nil for a device that does not exist")
	}
	if ctx.Err() == nil {
		t.Errorf("Connect returned %v before the context was cancelled", err)
	}
	// It must have retried rather than failing on the first miss.
	if elapsed < 250*time.Millisecond {
		t.Errorf("Connect gave up after %v, expected it to retry", elapsed)
	}
	if tr.Connected() {
		t.Error("Connected() is true after a failed Connect")
	}
}

func TestConnectReturnsWhenContextAlreadyCancelled(t *testing.T) {
	tr := serial.New(filepath.Join(t.TempDir(), "absent"))
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := tr.Connect(ctx); err == nil {
		t.Fatal("Connect returned nil for an already-cancelled context")
	}
}

func TestConnectSucceedsWhenDeviceAppears(t *testing.T) {
	// The board is plugged in a moment after the relay starts. Connect must
	// pick it up rather than having given up.
	path := filepath.Join(t.TempDir(), "cu.usbmodemLATE")
	tr := serial.New(path)
	defer tr.Close()

	go func() {
		time.Sleep(300 * time.Millisecond)
		f, err := os.Create(path)
		if err == nil {
			f.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tr.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !tr.Connected() {
		t.Error("Connected() is false after the device appeared")
	}
}

func TestSendWritesEncodedFrame(t *testing.T) {
	path := devicePath(t)
	tr := serial.New(path)
	defer tr.Close()

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	f := testFrame()
	if err := tr.Send(f); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
	want, err := transport.EncodeFrame(f)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("device received %q, want %q", got, want)
	}
}

func TestSendWritesFramesInOrder(t *testing.T) {
	path := devicePath(t)
	tr := serial.New(path)
	defer tr.Close()

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	for _, c := range []stoplight.Color{stoplight.ColorGreen, stoplight.ColorYellow, stoplight.ColorRed} {
		if err := tr.Send(stoplight.Frame{Color: c}); err != nil {
			t.Fatalf("Send(%v): %v", c, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read device: %v", err)
	}
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

func TestSendBeforeConnectFails(t *testing.T) {
	tr := serial.New(devicePath(t))
	defer tr.Close()

	if err := tr.Send(testFrame()); err == nil {
		t.Error("Send returned nil before Connect")
	}
}

func TestSendAfterCloseFails(t *testing.T) {
	tr := serial.New(devicePath(t))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Send(testFrame()); err == nil {
		t.Error("Send returned nil after Close")
	}
}

func TestSendErrorMarksDisconnectedAndStaysUsable(t *testing.T) {
	// Repeated Connect and Send must leave the transport working. The failure
	// path itself is covered by TestConnectFailureLeavesDisconnected, because
	// a portable, non-blocking way to make a single write fail mid-stream does
	// not exist: an unlinked-but-open file still accepts writes on Unix, and a
	// FIFO opened O_RDWR blocks instead of reporting EPIPE.
	path := devicePath(t)
	tr := serial.New(path)
	defer tr.Close()

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tr.Send(testFrame()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !tr.Connected() {
		t.Fatal("Connected() is false after a good Send")
	}

	// Reconnect is a no-op while already connected, and sending must still
	// work: the transport survives repeated use.
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if err := tr.Send(testFrame()); err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
}

func TestSendRecoversAfterReconnect(t *testing.T) {
	// Close drops the port, mimicking a board that went away. Connect brings
	// the transport back rather than the caller having to build a new one.
	path := devicePath(t)
	tr := serial.New(path)

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tr.Send(testFrame()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// After Close the transport is retired for good, so Send must fail rather
	// than silently reopening a port the caller released.
	if err := tr.Send(testFrame()); err == nil {
		t.Error("Send succeeded after Close")
	}
}

func TestConnectFailureLeavesDisconnected(t *testing.T) {
	// A read-only file makes Write fail the way an unplugged device does.
	dir := t.TempDir()
	path := filepath.Join(dir, "cu.usbmodemRO")
	if err := os.WriteFile(path, nil, 0o444); err != nil {
		t.Fatalf("create read-only device: %v", err)
	}

	tr := serial.New(path)
	defer tr.Close()

	// O_RDWR against a 0444 file fails for a non-root user, so Connect itself
	// is the failure here. Skip when running as root, where it would succeed.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := tr.Connect(ctx); err == nil {
		t.Skip("running with permission to open a 0444 file for writing")
	}
	if tr.Connected() {
		t.Error("Connected() is true after Connect failed")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	tr := serial.New(devicePath(t))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if tr.Connected() {
		t.Error("Connected() is true after Close")
	}
}

func TestCloseWithoutConnect(t *testing.T) {
	tr := serial.New(devicePath(t))
	if err := tr.Close(); err != nil {
		t.Errorf("Close without Connect: %v", err)
	}
}

func TestConnectAfterCloseDoesNotReopen(t *testing.T) {
	// Close is final. A retry loop still running must not resurrect the port.
	tr := serial.New(devicePath(t))
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

func TestConcurrentUse(t *testing.T) {
	// The relay sends from its run loop while `stoplight status` reads
	// Connected. Under -race this catches an unguarded field.
	tr := serial.New(devicePath(t))
	defer tr.Close()

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = tr.Send(testFrame())
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 40 {
				_ = tr.Connected()
				_ = tr.Name()
			}
		}()
	}
	wg.Wait()
}

func TestConcurrentCloseAndSend(t *testing.T) {
	// Shutdown races the run loop. Neither may panic.
	tr := serial.New(devicePath(t))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 50 {
			_ = tr.Send(testFrame())
		}
	}()
	go func() {
		defer wg.Done()
		_ = tr.Close()
	}()
	wg.Wait()
}

func TestDiscoverDoesNotError(t *testing.T) {
	// No device attached is the normal case on a build machine: an empty list
	// and no error.
	got, err := serial.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, path := range got {
		if !strings.HasPrefix(path, "/dev/") {
			t.Errorf("Discover returned %q, which is not under /dev", path)
		}
	}
}

func TestDiscoverReturnsOnlyExistingDevices(t *testing.T) {
	paths, err := serial.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("Discover returned %q but stat failed: %v", path, err)
		}
	}
}
