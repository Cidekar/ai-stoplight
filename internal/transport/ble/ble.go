// Package ble drives a light over Bluetooth Low Energy. It is the wireless
// counterpart to the serial transport, and it carries exactly the same bytes:
// one newline-delimited JSON frame per Send, encoded by transport.EncodeFrame.
// The firmware parses one format whichever radio or wire it arrived on.
//
// The Mac is the central and the ESP32 is the peripheral. This package only
// ever plays the central role: it scans, connects, discovers one service and
// one characteristic, and writes to it. Central-role work needs no entitlement
// and no bundle, so the CLI binary runs unsigned; macOS asks the user for
// Bluetooth permission once, and a refusal surfaces as a clear error rather
// than a hang. See Connect.
//
// # Why a third-party library
//
// This package is the reason the project has its first dependency. CoreBluetooth
// on macOS, BlueZ over D-Bus on Linux and WinRT on Windows are three unrelated
// Bluetooth stacks, and hand-rolling any one of them would be a larger project
// than Stoplight. tinygo.org/x/bluetooth wraps all three behind one API. See
// CONTRIBUTING.md for the dependency rule this establishes.
//
// # The library is not safe for concurrent scanning
//
// tinygo.org/x/bluetooth is not concurrency-safe on its scan path, and this is
// a defect in the library rather than a rule this package chose. On darwin,
// Adapter.Scan writes the unexported adapter field scanChan and then blocks on
// it, and Adapter.StopScan reads the same field to decide whether a scan is
// running. Neither takes a lock and the field is not atomic, so a StopScan that
// runs on another goroutine, which is the only way to stop a scan that is
// blocked, is an unsynchronised read of a field the scanning goroutine wrote.
// The race detector reports it against gap_darwin.go lines 49, 54, 66, 67 and
// 76. It is present in v0.16.0, the newest release, and in upstream HEAD, which
// is byte-identical here, so there is no version to upgrade to. The BlueZ and
// WinRT backends have the same shape.
//
// The consequence for this package is a rule the callers must keep: never run
// two scans at once, and never call into the library's scan path from more than
// one goroutine except through the stop handshake in scan. scanMu enforces the
// first half process-wide. The second half cannot be enforced from outside the
// library, because ordering a write inside Scan against a read inside StopScan
// needs a lock only the library can take.
//
// In practice the exposure is small and the failure mode is benign. Only
// Discover and dialDevice scan, both hold scanMu for the whole scan, and the
// worst outcome of the unsynchronised read is a StopScan that observes a stale
// nil and returns "not calling Scan function", which scan already retries
// through. It cannot corrupt a frame or a connection: the racing field is scan
// bookkeeping and nothing on the write path touches it.
package ble

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// The GATT contract between this transport and the firmware. Both sides are
// written against these two values, so neither may change without the other.
//
// These are random 128-bit UUIDs, not numbers assigned by the Bluetooth SIG.
// A 16-bit UUID has to be bought, and a custom 128-bit UUID is what the spec
// says to use for a vendor service, so the only requirement is that they do
// not collide with anything else on the air. They share a base with the last
// four digits distinguishing them, which makes them readable in a packet dump.
const (
	// ServiceUUIDString is the primary service the firmware advertises. The
	// scan matches on this and nothing else.
	ServiceUUIDString = "6e5d0001-b5a3-f393-e0a9-e50e24dcca9e"

	// FrameCharUUIDString is the characteristic frames are written to. It is
	// write-only from the central's point of view: the light never answers.
	FrameCharUUIDString = "6e5d0002-b5a3-f393-e0a9-e50e24dcca9e"
)

// Retry pacing for Connect, matching the serial transport. A light that is off,
// out of range or asleep is an expected condition, not a failure, so the
// backoff caps and keeps trying rather than escalating or giving up.
const (
	initialBackoff = 250 * time.Millisecond
	maxBackoff     = 5 * time.Second
)

// scanTimeout bounds one scan attempt. A scan that finds nothing must end so
// the backoff can run and the context can be checked; without a bound, Connect
// would sit inside the library's blocking Scan and ignore cancellation.
const scanTimeout = 5 * time.Second

// Chunking limits.
//
// A BLE characteristic write cannot exceed the negotiated ATT MTU minus three
// bytes of ATT header. That is 20 bytes on a link that never negotiated an
// upgrade and up to 244 on one that did, while a frame runs to roughly 1200
// bytes, so a frame does not fit in one write and must be split.
const (
	// minChunkSize is the payload of a write on the default 23-byte ATT MTU:
	// 23 minus the 3-byte header. Every BLE stack supports at least this, so
	// it is the floor when the negotiated MTU is unknown or implausible.
	minChunkSize = 20

	// maxChunkSize caps a single write regardless of what the link reports.
	// A peripheral that overstates its MTU would otherwise make every write
	// fail; 244 is the largest payload a 247-byte ATT MTU allows, which is the
	// practical ceiling on an ESP32.
	maxChunkSize = 244
)

// Transport writes frames to a light over BLE. It is safe for concurrent use.
type Transport struct {
	deviceName string
	connect    connector

	// live is the connection state. It is an atomic rather than a field under
	// mu so Connected never waits on an in-flight write: a wedged Write holds
	// mu for as long as the radio takes, and `stoplight status` asking whether
	// the light is reachable must not block behind it.
	live atomic.Bool

	// mu guards the link and the closed flag. It is held across a write, which
	// serialises frames onto one characteristic, but never across a read of
	// the connection state.
	mu     sync.Mutex
	link   Link
	closed bool
}

// Link is one open connection to a light: the writable characteristic plus the
// means to hang up. It is an interface so tests can supply a link they control,
// the same seam the serial transport gets from its opener. A real link wraps a
// bluetooth.DeviceCharacteristic.
type Link interface {
	// WriteChunk writes one chunk to the GATT characteristic. It returns the
	// number of bytes accepted, and a short count is an error to the caller.
	WriteChunk(b []byte) (int, error)

	// ChunkSize reports the largest payload a single write may carry, derived
	// from the negotiated ATT MTU.
	ChunkSize() int

	// Close drops the connection. It is safe to call more than once.
	Close() error
}

// connector establishes a link to a light, or fails trying. It exists so tests
// can drive Connect, Send and Close without a radio: the failure paths here
// are the ones that matter in the field, and none of them can be provoked from
// a test that needs real hardware to be absent, present, or going away
// mid-frame on cue.
type connector func(ctx context.Context, deviceName string) (Link, error)

// Device is one light found by a scan.
type Device struct {
	// Address identifies the peripheral to the local Bluetooth stack. It is a
	// MAC on Linux and Windows and a system-assigned UUID on macOS, and it is
	// stable only for this machine, so it is a handle and not an identity.
	Address string

	// Name is the advertised local name, for example "StoplightA4". It is a
	// label for a human, never the match key: names collide and users rename
	// things, so Discover matches on ServiceUUIDString.
	Name string

	// RSSI is the signal strength in dBm, closer to zero being stronger. The
	// strongest candidate is the best guess at the light on this desk.
	RSSI int16
}

// compile-time check that Transport satisfies the interface.
var _ transport.Transport = (*Transport)(nil)

// Sentinel errors callers and tests match on.
var (
	// errClosed reports that the transport was closed, so the retry loop stops.
	errClosed = errors.New("ble: transport closed")

	// ErrNotConnected is returned by Send when there is no link. It is not
	// fatal: the relay reconnects and sends the next frame.
	ErrNotConnected = errors.New("ble: not connected")

	// ErrPermissionDenied reports that the operating system refused access to
	// Bluetooth. Retrying cannot fix it, so Connect stops rather than looping
	// forever against a decision only the user can reverse.
	ErrPermissionDenied = errors.New("ble: bluetooth permission denied")

	// ErrUnavailable reports that there is no usable Bluetooth adapter: the
	// radio is off, or the machine has none. Unlike a permission refusal this
	// is worth retrying, because switching Bluetooth on fixes it.
	ErrUnavailable = errors.New("ble: bluetooth unavailable")

	// ErrNotFound reports that a scan completed without seeing a light. It is
	// the ordinary "light is off" case, so Connect retries.
	ErrNotFound = errors.New("ble: no light found")
)

// New returns a BLE transport. deviceName narrows the match to one advertised
// local name, which matters only when two lights are in range; empty means
// take the strongest light advertising the service. It does not touch the
// radio; call Connect for that.
func New(deviceName string) *Transport {
	return newWithConnector(deviceName, dialDevice)
}

// newWithConnector is New with the radio replaced. Tests use it to inject a
// link they control; production code calls New.
func newWithConnector(deviceName string, connect connector) *Transport {
	return &Transport{deviceName: deviceName, connect: connect}
}

// Connect establishes a link, retrying with backoff until it succeeds or ctx is
// cancelled. It never gives up on its own for any condition the user can fix by
// switching the light on or walking back into range: that is the contract the
// relay depends on, because Relay.Run must never exit because a transport
// failed.
//
// The one exception is a permission refusal. If the user denies the macOS
// Bluetooth prompt, no amount of retrying will change the answer, and a relay
// spinning silently on a denied permission is worse than one that says why it
// stopped. Every other error, including the radio being switched off, retries.
func (t *Transport) Connect(ctx context.Context) error {
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := t.tryConnect(ctx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, errClosed):
			// Close was called; retrying would reopen a link nobody wants.
			return err
		case errors.Is(err, ErrPermissionDenied):
			// Only the user can undo this, and they are not at the keyboard
			// inside a retry loop.
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

// tryConnect attempts a single scan-and-connect.
func (t *Transport) tryConnect(ctx context.Context) error {
	// Check for a closed or already-live transport under the lock, then let it
	// go: the dial below talks to a radio and can take seconds, and holding mu
	// across it would block Send, Close and any other Connect for the whole
	// attempt.
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errClosed
	}
	if t.link != nil {
		t.mu.Unlock()
		return nil // already connected
	}
	t.mu.Unlock()

	link, err := t.connect(ctx, t.deviceName)
	if err != nil {
		return err
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Close or another Connect may have landed while the dial was in flight.
	// Whoever got there first wins and this link is dropped, rather than
	// overwriting a live link or resurrecting a closed transport.
	if t.closed {
		link.Close()
		return errClosed
	}
	if t.link != nil {
		link.Close()
		return nil
	}

	t.link = link
	t.live.Store(true)
	return nil
}

// Send writes one frame to the light. A write error marks the transport
// disconnected and returns the error, but leaves the transport usable: the
// relay calls Connect again and carries on. That is the difference between one
// lost frame and a dead transport.
//
// The frame is split into chunks that fit the link's MTU and written in order.
// See ChunkContract for the guarantees the firmware may rely on.
func (t *Transport) Send(f stoplight.Frame) error {
	// Encode outside the lock. Marshalling does not touch the radio, and
	// holding the mutex across it would serialise work that need not be.
	data, err := transport.EncodeFrame(f)
	if err != nil {
		return fmt.Errorf("ble: encode frame: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return errClosed
	}
	if t.link == nil {
		return fmt.Errorf("%w: %s", ErrNotConnected, t.target())
	}

	if err := writeChunked(t.link, data); err != nil {
		// A light switched off or carried out of range fails every later
		// write. Drop the link so Connected reports the truth and Connect can
		// establish a new one.
		t.link.Close()
		t.link = nil
		t.live.Store(false)
		return fmt.Errorf("ble: write to %s: %w", t.target(), err)
	}
	return nil
}

// writeChunked splits data into MTU-sized chunks and writes them in order,
// stopping at the first failure.
//
// A partial frame on the wire is not a problem the firmware has to reason
// about: it reassembles by scanning for the newline, and a frame whose newline
// never arrives is simply never completed, so the next frame's bytes append to
// an incomplete line the reader discards on overflow. Reporting the error here
// is what lets the relay reconnect and resend a whole frame.
func writeChunked(link Link, data []byte) error {
	size := chunkSize(link.ChunkSize())

	for off := 0; off < len(data); off += size {
		end := min(off+size, len(data))
		chunk := data[off:end]

		n, err := link.WriteChunk(chunk)
		if err != nil {
			return fmt.Errorf("chunk at offset %d: %w", off, err)
		}
		if n < len(chunk) {
			// A short write drops bytes out of the middle of a frame with no
			// error reported. The firmware would splice the remainder onto a
			// corrupt line, so treat it as the failure it is rather than
			// carrying on with the next chunk.
			return fmt.Errorf("%w: chunk at offset %d wrote %d of %d bytes",
				io.ErrShortWrite, off, n, len(chunk))
		}
	}
	return nil
}

// chunkSize clamps a reported MTU into a payload size that is safe to write.
// A peripheral that reports nonsense, or a stack that reports nothing at all,
// must not turn every write into a failure, so an implausible value falls back
// to the 20 bytes every BLE link supports.
func chunkSize(reported int) int {
	switch {
	case reported < minChunkSize:
		return minChunkSize
	case reported > maxChunkSize:
		return maxChunkSize
	default:
		return reported
	}
}

// Connected reports whether a light is currently reachable.
//
// It reads an atomic rather than taking the mutex, on purpose. Send holds the
// mutex for the whole of a chunked write, and a radio write can wedge for
// seconds; if this took the same lock, `stoplight status` and the relay's
// once-a-second connection poll would both block behind a stuck frame. The
// serial transport has that bug and this one must not copy it.
func (t *Transport) Connected() bool {
	return t.live.Load()
}

// Close drops the link. It is idempotent: closing twice is not an error,
// because shutdown paths often overlap and the second call must not panic.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.closed = true
	t.live.Store(false)
	if t.link == nil {
		return nil
	}
	link := t.link
	t.link = nil
	return link.Close()
}

// Name identifies the transport in logs and `stoplight status`, for example
// "ble:StoplightA4". It includes the device name so a desk with two lights in
// range does not give two identical lines.
func (t *Transport) Name() string {
	return "ble:" + t.target()
}

// target is the device name for messages, or a placeholder when the caller did
// not name one. An error reading "ble: write to <any>" still says which
// transport failed, which is what the reader needs.
func (t *Transport) target() string {
	if t.deviceName == "" {
		return "<any>"
	}
	return t.deviceName
}
