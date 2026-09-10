package transport_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

func TestEncodeFrameMatchesWireFormat(t *testing.T) {
	f := stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
		},
	}

	got, err := transport.EncodeFrame(f)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	// The exact bytes the firmware parses, from the RFC.
	want := `{"color":"red","sessions":[{"id":"a1","label":"auth-api","state":"needs you","color":"red"}]}` + "\n"
	if string(got) != want {
		t.Errorf("wire form mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestEncodeFrameEndsWithSingleNewline(t *testing.T) {
	// The newline delimits frames on the device. Exactly one, at the end.
	got, err := transport.EncodeFrame(stoplight.Frame{Color: stoplight.ColorOff})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if n := strings.Count(string(got), "\n"); n != 1 {
		t.Errorf("newline count = %d, want 1 (got %q)", n, got)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Errorf("frame does not end with newline: %q", got)
	}
}

func TestEncodeFrameColorsAreStrings(t *testing.T) {
	// A numeric colour would reach the firmware as 3 rather than "red".
	for _, tc := range []struct {
		color stoplight.Color
		want  string
	}{
		{stoplight.ColorOff, `"off"`},
		{stoplight.ColorGreen, `"green"`},
		{stoplight.ColorYellow, `"yellow"`},
		{stoplight.ColorRed, `"red"`},
	} {
		got, err := transport.EncodeFrame(stoplight.Frame{Color: tc.color})
		if err != nil {
			t.Fatalf("EncodeFrame(%v): %v", tc.color, err)
		}
		if !strings.Contains(string(got), `"color":`+tc.want) {
			t.Errorf("colour %v encoded as %q, want %s", tc.color, got, tc.want)
		}
	}
}

func TestEncodeFrameEmptySessions(t *testing.T) {
	// Lamps dark, screen cleared. sessions must still be present as [] so the
	// firmware can tell "no sessions" from "field omitted".
	got, err := transport.EncodeFrame(stoplight.Frame{
		Color:    stoplight.ColorOff,
		Sessions: []stoplight.FrameSession{},
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	want := `{"color":"off","sessions":[]}` + "\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeFrameDoesNotEscapeHTML(t *testing.T) {
	// A label with & or < must survive intact; the device font cannot render
	// & as anything useful.
	got, err := transport.EncodeFrame(stoplight.Frame{
		Color: stoplight.ColorGreen,
		Sessions: []stoplight.FrameSession{
			{ID: "x", Label: "a&b<c>d", State: "done", Color: stoplight.ColorGreen},
		},
	})
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if !strings.Contains(string(got), "a&b<c>d") {
		t.Errorf("label was escaped: %q", got)
	}
}

func TestEncodeFrameRoundTrips(t *testing.T) {
	want := stoplight.Frame{
		Color: stoplight.ColorYellow,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth", State: "working", Color: stoplight.ColorYellow},
			{ID: "b2", Label: "ci", State: "done", Color: stoplight.ColorGreen},
		},
	}

	data, err := transport.EncodeFrame(want)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	var got stoplight.Frame
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("round trip changed the frame:\n got %+v\nwant %+v", got, want)
	}
}

func TestEncodeFrameIsSafeForConcurrentUse(t *testing.T) {
	// Every transport encodes on its own goroutine, so a shared buffer inside
	// EncodeFrame would corrupt frames under -race.
	f := stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
		},
	}
	want, err := transport.EncodeFrame(f)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := transport.EncodeFrame(f)
			if err != nil {
				t.Errorf("EncodeFrame: %v", err)
				return
			}
			if string(got) != string(want) {
				t.Errorf("concurrent encode differed: %q", got)
			}
		}()
	}
	wg.Wait()
}
