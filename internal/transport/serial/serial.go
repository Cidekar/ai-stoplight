// Package serial drives a light over a USB serial port. It is the fallback
// when BLE is unavailable, and the transport used for firmware debugging,
// because a wired link fails in ways you can see.
//
// The device is opened as an ordinary file rather than through a serial
// library. A USB CDC-ACM device ignores baud rate and framing: the host and
// the device negotiate those over USB itself, so the line settings a serial
// library would configure change nothing. Opening the file keeps the project
// on the standard library.
package serial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// Retry pacing for Connect. A light that is unplugged is an expected
// condition, not a failure, so the backoff caps and keeps trying rather than
// escalating or giving up.
const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// devicePatterns are the globs Discover searches. macOS exposes a USB CDC
// device as /dev/cu.usbmodem*; Linux uses ttyACM for CDC-ACM and ttyUSB for
// the FTDI and CH340 bridges on cheaper boards.
//
// On macOS the cu ("call-up") device is correct rather than tty: opening a tty
// device blocks until DCD is asserted, which a USB board never does, so an
// open against /dev/tty.usbmodem* would hang forever.
var devicePatterns = []string{
	"/dev/cu.usbmodem*",
	"/dev/ttyACM*",
	"/dev/ttyUSB*",
}

// opener opens a device and returns the stream to write frames to. It exists
// so tests can supply a writer that fails on demand: an unplugged board is the
// most important failure path in this package, and there is no portable,
// non-blocking way to make a real file write fail mid-stream.
type opener func(devicePath string) (io.ReadWriteCloser, error)

// Transport writes frames to a serial device. It is safe for concurrent use.
type Transport struct {
	devicePath string
	open       opener

	mu     sync.Mutex
	port   io.ReadWriteCloser
	closed bool
}

// compile-time check that Transport satisfies the interface.
var _ transport.Transport = (*Transport)(nil)

// New returns a serial transport for the device at devicePath. It does not
// touch the device; call Connect for that.
func New(devicePath string) *Transport {
	return newWithOpener(devicePath, openDevice)
}

// newWithOpener is New with the device open replaced. Tests use it to inject a
// writer they control; production code calls New.
func newWithOpener(devicePath string, open opener) *Transport {
	return &Transport{devicePath: devicePath, open: open}
}

// openDevice is the real open: a CDC-ACM device is an ordinary file.
func openDevice(devicePath string) (io.ReadWriteCloser, error) {
	return os.OpenFile(devicePath, os.O_RDWR, 0)
}

// Discover lists the serial devices that could be a light. It returns the
// paths that currently exist, sorted, and never errors on a pattern matching
// nothing: an empty result means no device is plugged in, which is normal.
func Discover() ([]string, error) {
	var found []string
	for _, pattern := range devicePatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			// The patterns above are constant and well-formed, so this only
			// fires if one is edited into something invalid.
			return nil, fmt.Errorf("serial: bad device pattern %q: %w", pattern, err)
		}
		found = append(found, matches...)
	}
	return found, nil
}

// Connect opens the device, retrying with backoff until it succeeds or ctx is
// cancelled. It never gives up on its own: a light that is unplugged, asleep
// or still enumerating is an expected condition, and the relay must stay up
// through it.
func (t *Transport) Connect(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := t.tryOpen()
		if err == nil {
			return nil
		}
		if errors.Is(err, errClosed) {
			// Close was called; retrying would reopen a port nobody wants.
			return err
		}

		// Wait out the backoff, but stay responsive to cancellation. A timer
		// rather than time.Sleep, so a cancelled context returns at once
		// instead of after the full delay.
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// errClosed reports that the transport was closed, so the retry loop stops.
var errClosed = errors.New("serial: transport closed")

// tryOpen attempts a single open of the device.
func (t *Transport) tryOpen() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errClosed
	}
	if t.port != nil {
		return nil // already connected
	}

	port, err := t.open(t.devicePath)
	if err != nil {
		return fmt.Errorf("serial: open %s: %w", t.devicePath, err)
	}
	t.port = port
	return nil
}

// Send writes one frame to the device. A write error marks the transport
// disconnected and returns the error, but leaves the transport usable: the
// caller can call Connect again to reopen the port. That is the difference
// between one lost frame and a dead transport.
func (t *Transport) Send(f stoplight.Frame) error {
	// Encode outside the lock. Marshalling does not touch the port, and
	// holding the mutex across it would serialise work that need not be.
	data, err := transport.EncodeFrame(f)
	if err != nil {
		return fmt.Errorf("serial: encode frame: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errClosed
	}
	if t.port == nil {
		return fmt.Errorf("serial: not connected to %s", t.devicePath)
	}

	n, err := t.port.Write(data)
	if err == nil && n < len(data) {
		// A short write with no error truncates the frame, and the firmware
		// splits on the newline that never arrived, so the next frame merges
		// into this one. Treat it as the failure it is.
		err = fmt.Errorf("%w: wrote %d of %d bytes", io.ErrShortWrite, n, len(data))
	}
	if err != nil {
		// Unplugging the device makes every later write fail. Drop the handle
		// so Connected reports the truth and Connect can reopen it.
		t.port.Close()
		t.port = nil
		return fmt.Errorf("serial: write to %s: %w", t.devicePath, err)
	}
	return nil
}

// Connected reports whether the port is currently open.
func (t *Transport) Connected() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.port != nil
}

// Close releases the port. It is idempotent: closing twice is not an error,
// because shutdown paths often overlap and the second call must not panic.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	if t.port == nil {
		return nil
	}
	port := t.port
	t.port = nil
	return port.Close()
}

// Name identifies the transport in logs and `stoplight status`. It includes
// the device path, because a machine with two boards attached otherwise gives
// two identical lines.
func (t *Transport) Name() string {
	return "serial:" + t.devicePath
}
