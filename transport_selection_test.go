package main

// Transport selection: the flag combinations, the precedence between them, and
// the messages a fallback prints. These call selectTransport directly rather
// than through run, so no relay binds a port and no radio is touched: every
// case here is decided before any hardware is opened.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cidekar/stoplight/internal/transport/ble"
)

// TestSelectTransportExplicitChoices proves an explicit flag is honoured and
// names the transport it produced. An explicit choice must never be quietly
// swapped for something else.
func TestSelectTransportExplicitChoices(t *testing.T) {
	cases := []struct {
		name   string
		choice transportChoice
		want   string
	}{
		{"virtual", transportChoice{virtual: true}, "virtual"},
		{"serial", transportChoice{device: "/dev/cu.usbmodemTEST"}, "serial:/dev/cu.usbmodemTEST"},
		{"ble", transportChoice{ble: true}, "ble:"},
		{"ble with a name", transportChoice{bleName: "StoplightA4"}, "ble:StoplightA4"},
		// --ble-name implies --ble, so passing both is not a conflict.
		{"ble and a name", transportChoice{ble: true, bleName: "StoplightA4"}, "ble:StoplightA4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tr, err := selectTransport(context.Background(), tc.choice, &out)
			if err != nil {
				t.Fatalf("selectTransport: %v", err)
			}
			if tr == nil {
				t.Fatal("selectTransport returned a nil transport and no error")
			}
			defer tr.Close()

			if got := tr.Name(); !strings.HasPrefix(got, tc.want) {
				t.Errorf("Name() = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}

// TestSelectTransportRejectsConflictingFlags proves two different lights cannot
// be asked for at once, and that the message names both flags. A message that
// names one leaves the reader guessing which to drop.
func TestSelectTransportRejectsConflictingFlags(t *testing.T) {
	cases := []struct {
		name   string
		choice transportChoice
		names  []string
	}{
		{
			"virtual and serial",
			transportChoice{virtual: true, device: "/dev/null"},
			[]string{"--virtual", "--serial"},
		},
		{
			"virtual and ble",
			transportChoice{virtual: true, ble: true},
			[]string{"--virtual", "--ble"},
		},
		{
			"virtual and a ble name",
			transportChoice{virtual: true, bleName: "StoplightA4"},
			[]string{"--virtual", "--ble"},
		},
		{
			"serial and ble",
			transportChoice{device: "/dev/null", ble: true},
			[]string{"--serial", "--ble"},
		},
		{
			"serial and a ble name",
			transportChoice{device: "/dev/null", bleName: "StoplightA4"},
			[]string{"--serial", "--ble"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			tr, err := selectTransport(context.Background(), tc.choice, &out)
			if err == nil {
				if tr != nil {
					tr.Close()
				}
				t.Fatal("selectTransport accepted two conflicting transports")
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

// TestSelectTransportBLEDoesNotFallBack proves --ble is a decision rather than
// a hint. The transport retries forever, so a light switched on later is still
// found; silently handing back a serial or virtual light instead would hide
// that from somebody who asked for Bluetooth on purpose.
func TestSelectTransportBLEDoesNotFallBack(t *testing.T) {
	var out bytes.Buffer
	tr, err := selectTransport(context.Background(), transportChoice{ble: true}, &out)
	if err != nil {
		t.Fatalf("selectTransport: %v", err)
	}
	defer tr.Close()

	if got := tr.Name(); !strings.HasPrefix(got, "ble:") {
		t.Errorf("Name() = %q, want a BLE transport", got)
	}
	// It must not have scanned: --ble goes straight to the transport, whose
	// own Connect does the scanning with the relay's context.
	if out.Len() != 0 {
		t.Errorf("--ble printed %q, want it to say nothing", out.String())
	}
}

// TestSelectTransportAutoDiscoveryNeverFails proves auto-discovery always
// yields a working transport. The relay must start whatever hardware is or is
// not attached, because a relay that refuses to start looks like a crash to
// the service manager, which restarts it, which loops.
//
// Which transport comes back depends on what is plugged into the machine
// running the test, so this asserts the invariant rather than a specific one.
//
// The context is cancelled before the call. That does not weaken the test: the
// point is that every discovery path, including the ones that find nothing,
// still ends in a usable transport rather than an error, and a cancelled scan
// is exactly the "found nothing" path. It is also the only way to run this
// under -race. A real Bluetooth scan reports a data race inside
// tinygo.org/x/bluetooth, whose Scan and StopScan share an unsynchronised
// field with no way to order them from outside the library, so a test that
// scans for real fails the race detector on every run for a defect that is not
// this project's to fix.
func TestSelectTransportAutoDiscoveryNeverFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	tr, err := selectTransport(ctx, transportChoice{}, &out)
	if err != nil {
		t.Fatalf("auto-discovery failed: %v", err)
	}
	if tr == nil {
		t.Fatal("auto-discovery returned a nil transport and no error")
	}
	defer tr.Close()

	if tr.Name() == "" {
		t.Error("the selected transport has no name")
	}

	// A fallback must say so. A silent fallback to the virtual light is
	// indistinguishable from broken hardware.
	if tr.Name() == "virtual" && out.Len() == 0 {
		t.Error("fell back to the virtual light without saying so")
	}
}

// TestBLEChunkContractIsReachableFromTheCLI is a canary. The firmware is
// written against these constants, and the CLI is what a contributor runs to
// find them, so a rename that leaves the docs pointing at nothing should fail
// here rather than in somebody's soldering session.
func TestBLEChunkContractIsReachableFromTheCLI(t *testing.T) {
	if ble.ServiceUUIDString == "" || ble.FrameCharUUIDString == "" {
		t.Fatal("the BLE UUID constants are empty")
	}
	if !strings.Contains(ble.ChunkContract, "newline") {
		t.Error("ChunkContract does not describe the newline delimiter")
	}
}
