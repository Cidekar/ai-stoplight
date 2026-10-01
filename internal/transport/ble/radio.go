package ble

// The radio half of the package: everything that touches
// tinygo.org/x/bluetooth. It is separated from ble.go so the transport's logic
// -- retry, chunking, connection state -- can be read and tested without the
// library in the way.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	bt "tinygo.org/x/bluetooth"
)

// ServiceUUID and FrameCharUUID are the parsed forms of the string constants.
// They are parsed once at init because ParseUUID on a constant cannot fail,
// and making every call site handle an impossible error would be noise.
var (
	ServiceUUID   = mustParseUUID(ServiceUUIDString)
	FrameCharUUID = mustParseUUID(FrameCharUUIDString)
)

func mustParseUUID(s string) bt.UUID {
	u, err := bt.ParseUUID(s)
	if err != nil {
		// Unreachable: the arguments are the constants above. A panic here
		// means someone edited one into something malformed, which is a build
		// mistake and should stop the process immediately.
		panic("ble: malformed UUID constant " + s + ": " + err.Error())
	}
	return u
}

// enableMu guards adapter setup. bluetooth.DefaultAdapter is a process-wide
// singleton and its Enable is not safe to call twice concurrently: the darwin
// backend rejects a second call outright with "already calling Enable
// function". The mutex serialises the call, and enableState caches only the
// answers that cannot change within a process.
//
// A sync.Once was wrong here. It cached every answer for the life of the
// process, including the two that are not permanent: the radio switched off,
// and a first Enable that timed out waiting for CoreBluetooth. The relay runs
// as a login item (stoplight install --ble) and starts while Bluetooth is off,
// or before the user has answered the one-time permission prompt; the Once then
// pinned "unavailable" forever and the light was never found until the relay
// was restarted. That contradicted ErrUnavailable's own promise -- "worth
// retrying, because switching Bluetooth on fixes it" -- so only the terminal
// answers are kept and a retryable one is returned without being cached.
var (
	enableMu    sync.Mutex
	enableState struct {
		// done is set once a terminal answer is reached: Enable succeeded, or
		// it was refused by permission. Both are fixed for the process, so a
		// later call returns err without touching the radio again.
		done bool
		err  error
	}
)

// adapterEnabler is the library call enableAdapter drives, pulled out as a
// variable so the retry-and-cache logic can be tested without a radio: the
// transient-error path that matters here cannot be provoked from a test that
// needs real hardware to be absent one moment and present the next.
var adapterEnabler = func() error { return bt.DefaultAdapter.Enable() }

// swapEnabler replaces adapterEnabler for a test and returns a function that
// restores it. It is here, in non-test code, only so the restore cannot be
// forgotten; nothing in production calls it.
func swapEnabler(fn func() error) (restore func()) {
	prev := adapterEnabler
	adapterEnabler = fn
	return func() { adapterEnabler = prev }
}

// resetAdapterState clears the cached enable result for a test.
func resetAdapterState() {
	enableMu.Lock()
	defer enableMu.Unlock()
	enableState.done = false
	enableState.err = nil
}

// scanMu serialises whole scans against each other, for the same reason
// enableOnce serialises Enable: DefaultAdapter is process-wide state.
//
// Scan and StopScan communicate through an adapter field that neither of them
// locks. Scan writes it on the way in and clears it on the way out; StopScan
// reads it. Two scans that overlap therefore race on that field, and the
// second one also loses its stop signal to the first, which is what leaves a
// scan blocked until its own deadline.
//
// This does NOT make a single scan race free. Within one scan the stop
// goroutine still reads that field while Scan is writing it, and no locking on
// this side can fix that: the field is unexported, and the library offers no
// signal for when it has been published or cleared. Only calling StopScan from
// inside the Scan callback avoids it, and that is unavailable here because a
// scan which sees no device never runs the callback yet still has to time out.
// See the note on Discover.
//
// One scan at a time is not a real restriction. A host radio can only run a
// single discovery session anyway, so overlapping scans were never going to
// return independent results, and the caller-visible behaviour is unchanged:
// the second scan starts when the first has finished, still bounded by its own
// context.
var scanMu sync.Mutex

// enableAdapter powers up the host adapter and returns as soon as ctx is done
// even if the adapter has not answered. It calls the library's Enable once per
// process on success or a permission refusal, and again on anything retryable.
//
// The library's Enable takes no context and blocks for up to ten seconds
// waiting for CoreBluetooth to report a state. That wait is not hypothetical:
// on a machine where the binary has never been granted Bluetooth access, the
// state callback never fires and the full ten seconds elapse. Ten seconds is
// far longer than the three the CLI allows for auto-discovery, so the wait
// happens on a goroutine and the caller leaves when its own deadline passes.
//
// The abandoned goroutine finishes on its own and records the result under
// enableMu, so a later call gets the real answer rather than repeating the
// wait. A retryable result is handed back but not kept, so once the radio is
// switched on or the slow stack finally answers, the next pass calls Enable
// again instead of serving a stale "unavailable".
func enableAdapter(ctx context.Context) error {
	enableMu.Lock()
	if enableState.done {
		err := enableState.err
		enableMu.Unlock()
		return err
	}

	done := make(chan error, 1)
	go func() {
		// enableMu is held across the whole library call, not just the state
		// write, so a second caller cannot start a concurrent Enable the darwin
		// backend would reject. The caller below may leave on its deadline
		// first, but the lock stays held until this goroutine finishes, which is
		// what serialises the next attempt behind this one.
		defer enableMu.Unlock()
		err := classifyAdapterError(adapterEnabler())
		// Keep only the terminal answers. A retryable error (the radio off, a
		// slow stack, or an unrecognised string) is returned to this caller but
		// left uncached so the next call retries Enable.
		if err == nil || errors.Is(err, ErrPermissionDenied) {
			enableState.done = true
			enableState.err = err
		}
		done <- err
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Not a permission refusal and not a missing adapter: this is simply
		// "no answer yet". Reporting it as retryable is what lets Connect try
		// again on its next pass rather than treating a slow radio as fatal.
		return fmt.Errorf("%w: the Bluetooth adapter did not respond in time: %w",
			ErrUnavailable, ctx.Err())
	}
}

// classifyAdapterError turns the library's error strings into the sentinels
// Connect branches on.
//
// String matching is unpleasant and it is what the library leaves available:
// the darwin backend builds its errors with errors.New inside a delegate
// callback, so there is no typed error and no wrapped sentinel to test for.
// The strings come from CentralManagerDidUpdateState in adapter_darwin.go and
// from the equivalent BlueZ and WinRT paths. A miss here is not a crash: an
// unrecognised error is treated as retryable, which is the safe default for
// everything except a permission refusal.
func classifyAdapterError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "not authorized"),
		strings.Contains(msg, "denied"),
		strings.Contains(msg, "permission"):
		// The user declined the Bluetooth prompt, or a policy forbids it.
		// Retrying cannot change the answer, so say what to do about it.
		return fmt.Errorf("%w: %v\n"+
			"  grant Bluetooth access in System Settings > Privacy & Security > Bluetooth,\n"+
			"  or run with --serial to use a USB cable instead", ErrPermissionDenied, err)
	case strings.Contains(msg, "powered off"),
		strings.Contains(msg, "not supported"),
		strings.Contains(msg, "unsupported"):
		// Switching Bluetooth on fixes this, so it stays retryable.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	default:
		return fmt.Errorf("ble: enable adapter: %w", err)
	}
}

// deviceLink is a real connection to a light: an open peripheral and the
// characteristic frames are written to.
type deviceLink struct {
	device bt.Device
	char   bt.DeviceCharacteristic

	// mtu is the negotiated write payload, read once at connect. The value
	// cannot change without a reconnection, so caching it keeps Send off a
	// library call that crosses into CoreBluetooth on every chunk.
	mtu int

	once sync.Once
}

// WriteChunk writes one chunk to the characteristic.
//
// Write, not WriteWithoutResponse: a write with response is acknowledged by
// the peripheral, so a light that has gone away produces an error instead of
// bytes vanishing into the air. Frames are sent only on change, so an
// unnoticed drop would leave the lamp holding a stale colour indefinitely --
// exactly the failure the firmware's line-length work was done to avoid. The
// cost is a round trip per chunk, which for a handful of chunks a few times a
// minute is not worth optimising away.
func (l *deviceLink) WriteChunk(b []byte) (int, error) {
	return l.char.Write(b)
}

// ChunkSize reports the negotiated write payload.
func (l *deviceLink) ChunkSize() int { return l.mtu }

// Close disconnects the peripheral. Disconnecting twice makes the second call
// an error on some backends, so the once keeps Close idempotent, which the
// Transport contract requires.
func (l *deviceLink) Close() error {
	var err error
	l.once.Do(func() { err = l.device.Disconnect() })
	return err
}

// dialDevice is the real connector: enable the adapter, scan for a light,
// connect, and find the frame characteristic.
func dialDevice(ctx context.Context, deviceName string) (Link, error) {
	if err := enableAdapter(ctx); err != nil {
		return nil, err
	}

	found, err := scan(ctx, deviceName, scanTimeout)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("%w advertising %s", ErrNotFound, ServiceUUIDString)
	}

	// scan sorts by signal strength, so the first is the nearest light, which
	// on a desk with one light is the only one and on a desk with two is the
	// one in front of you. --ble-name overrides the guess.
	target := found[0]

	var addr bt.Address
	addr.Set(target.Address)

	device, err := bt.DefaultAdapter.Connect(addr, bt.ConnectionParams{})
	if err != nil {
		return nil, fmt.Errorf("ble: connect to %s: %w", target.Address, err)
	}

	link, err := openFrameChar(device)
	if err != nil {
		device.Disconnect()
		return nil, err
	}
	return link, nil
}

// openFrameChar discovers the frame characteristic on a connected peripheral.
func openFrameChar(device bt.Device) (*deviceLink, error) {
	services, err := device.DiscoverServices([]bt.UUID{ServiceUUID})
	if err != nil {
		return nil, fmt.Errorf("ble: discover service %s: %w", ServiceUUIDString, err)
	}
	if len(services) == 0 {
		// The device advertised the service and then did not present it. A
		// half-flashed board does this.
		return nil, fmt.Errorf("ble: device does not have service %s", ServiceUUIDString)
	}

	chars, err := services[0].DiscoverCharacteristics([]bt.UUID{FrameCharUUID})
	if err != nil {
		return nil, fmt.Errorf("ble: discover characteristic %s: %w", FrameCharUUIDString, err)
	}
	if len(chars) == 0 {
		return nil, fmt.Errorf("ble: service %s has no characteristic %s",
			ServiceUUIDString, FrameCharUUIDString)
	}

	char := chars[0]

	// A stack that cannot report an MTU is not an error: chunkSize clamps
	// whatever comes back, and zero becomes the 20-byte floor every BLE link
	// supports. A frame still gets through, only in more writes.
	mtu := 0
	if v, err := char.GetMTU(); err == nil {
		// The reported value is the ATT MTU; three bytes of it are the write
		// header, so the payload is three smaller.
		mtu = int(v) - 3
	}

	return &deviceLink{device: device, char: char, mtu: mtu}, nil
}

// Discover scans for lights and returns what it found, strongest signal first.
// It is the BLE counterpart to serial.Discover: an empty result means no light
// is switched on and in range, which is normal and not an error.
//
// A refused Bluetooth permission is an error, because a caller listing devices
// needs to know the difference between "none found" and "not allowed to look".
func Discover(ctx context.Context) ([]Device, error) {
	if err := enableAdapter(ctx); err != nil {
		return nil, err
	}
	return scan(ctx, "", scanTimeout)
}

// scan runs one bounded scan and returns the matching lights.
//
// Matching is on ServiceUUID, never on the advertised name. A name is a label
// a user can change and two boards flashed from the same source advertise the
// same one, so a name match finds the wrong light or none at all. deviceName,
// when set, filters the service matches down rather than replacing the test.
//
// The library's Scan blocks until StopScan is called and takes no context, so
// the timeout and the cancellation both have to be driven from outside it.
func scan(ctx context.Context, deviceName string, timeout time.Duration) ([]Device, error) {
	adapter := bt.DefaultAdapter

	// Held for the whole scan, including the stop handshake below, so no other
	// scan can touch the adapter's scan state while this one owns it. Taken
	// before the deadline is armed so a scan that waits its turn still gets
	// its full timeout rather than spending it queued.
	scanMu.Lock()
	defer scanMu.Unlock()

	var (
		mu    sync.Mutex
		found = map[string]Device{}
	)

	scanCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// StopScan is what unblocks Scan below. It runs from a second goroutine
	// because the callback only fires when a device is seen, and a scan that
	// sees nothing at all still has to end.
	//
	// It has to be retried, and that is not defensive coding. StopScan reports
	// "not calling Scan function" and does nothing when the adapter has not yet
	// assigned its internal scan channel, which is a window this code can land
	// in: the timeout is armed before Scan is called, so a context already past
	// its deadline stops a scan that has not started. Firing once into that
	// window leaves Scan blocked forever with no way out. Retrying until it
	// takes hold closes it.
	stopped := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(stopped)
			go func() {
				// A short poll, bounded so a genuinely stopped scan does not
				// leave this goroutine spinning for the life of the process.
				for range 200 {
					if err := adapter.StopScan(); err == nil {
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()
		})
	}

	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-scanCtx.Done():
			stop()
		case <-stopped:
		}
	}()

	err := adapter.Scan(func(_ *bt.Adapter, result bt.ScanResult) {
		// The match key, and the only one. HasServiceUUID checks the
		// advertised Service Class UUIDs.
		if !result.HasServiceUUID(ServiceUUID) {
			return
		}
		name := result.LocalName()
		if deviceName != "" && name != deviceName {
			return
		}

		mu.Lock()
		// Keyed by address so a device seen twice does not appear twice. The
		// strongest sighting wins, since RSSI on a single packet is noisy.
		addr := result.Address.String()
		if prev, seen := found[addr]; !seen || result.RSSI > prev.RSSI {
			found[addr] = Device{Address: addr, Name: name, RSSI: result.RSSI}
		}
		mu.Unlock()
	})

	stop()
	<-watchDone

	if err != nil {
		return nil, fmt.Errorf("ble: scan: %w", err)
	}
	// A cancelled parent context is the caller giving up, not an empty result.
	// The deadline expiring is an ordinary scan that found nothing.
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}

	mu.Lock()
	defer mu.Unlock()

	devices := make([]Device, 0, len(found))
	for _, d := range found {
		devices = append(devices, d)
	}
	// Strongest signal first, then by address so a tie is stable rather than
	// dependent on map iteration order.
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].RSSI != devices[j].RSSI {
			return devices[i].RSSI > devices[j].RSSI
		}
		return devices[i].Address < devices[j].Address
	})
	return devices, nil
}
