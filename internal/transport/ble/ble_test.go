package ble_test

// The public surface: the interface fit, the names, and the UUID constants the
// firmware is written against. Nothing here touches a radio.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/transport"
	"github.com/cidekar/stoplight/internal/transport/ble"
)

func TestSatisfiesTransportInterface(t *testing.T) {
	// The relay stores transports as the interface; a signature drift here
	// would only show up at the call site otherwise.
	var _ transport.Transport = ble.New("StoplightA4")
}

func TestNameIncludesDeviceName(t *testing.T) {
	if got, want := ble.New("StoplightA4").Name(), "ble:StoplightA4"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}

// An unnamed transport still has to identify itself in `stoplight status`, so
// the name says which transport it is rather than trailing off after the colon.
func TestNameWithoutADeviceName(t *testing.T) {
	got := ble.New("").Name()
	if !strings.HasPrefix(got, "ble:") {
		t.Errorf("Name() = %q, want it to start with ble:", got)
	}
	if got == "ble:" {
		t.Error("Name() = \"ble:\", want it to name the target too")
	}
}

func TestConnectedIsFalseBeforeConnect(t *testing.T) {
	tr := ble.New("StoplightA4")
	defer tr.Close()
	if tr.Connected() {
		t.Error("Connected() is true before Connect")
	}
}

// Close on a transport that never connected must not touch the radio or panic.
func TestCloseWithoutConnect(t *testing.T) {
	if err := ble.New("StoplightA4").Close(); err != nil {
		t.Errorf("Close without Connect: %v", err)
	}
}

// The UUIDs are the contract with the firmware. A typo would make the light
// invisible to the scan, and the failure would look like broken hardware, so
// they are asserted rather than trusted.
func TestServiceUUIDsAreWellFormed(t *testing.T) {
	cases := []struct {
		name, s string
	}{
		{"service", ble.ServiceUUIDString},
		{"characteristic", ble.FrameCharUUIDString},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.s) != 36 {
				t.Errorf("%q is %d characters, want a 36-character UUID", tc.s, len(tc.s))
			}
			if strings.ToLower(tc.s) != tc.s {
				t.Errorf("%q is not lowercase; the firmware compares strings", tc.s)
			}
		})
	}

	if ble.ServiceUUIDString == ble.FrameCharUUIDString {
		t.Error("the service and characteristic UUIDs are the same")
	}
	// The parsed forms are what the scan and the discovery actually use.
	if ble.ServiceUUID.String() != ble.ServiceUUIDString {
		t.Errorf("ServiceUUID = %q, want %q", ble.ServiceUUID.String(), ble.ServiceUUIDString)
	}
	if ble.FrameCharUUID.String() != ble.FrameCharUUIDString {
		t.Errorf("FrameCharUUID = %q, want %q", ble.FrameCharUUID.String(), ble.FrameCharUUIDString)
	}
}

// The firmware is written against ChunkContract, so it must name both UUIDs.
// A UUID changed in one place and not the other is the mistake this catches.
func TestChunkContractNamesTheUUIDs(t *testing.T) {
	for _, want := range []string{ble.ServiceUUIDString, ble.FrameCharUUIDString} {
		if !strings.Contains(ble.ChunkContract, want) {
			t.Errorf("ChunkContract does not mention %s", want)
		}
	}
}

// Discover must not hang or panic on a machine with no radio, which is every
// build machine. Whatever it returns, it has to return.
func TestDiscoverReturnsPromptlyOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		devices, err := ble.Discover(ctx)
		// Either answer is fine on a machine that may or may not have a radio;
		// only hanging is a failure. Devices without an error must be usable.
		if err == nil {
			for _, d := range devices {
				if d.Address == "" {
					t.Error("Discover returned a device with no address")
				}
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("Discover did not return on a cancelled context")
	}
}
