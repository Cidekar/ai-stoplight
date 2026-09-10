package light_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cidekar/stoplight/internal/light"
	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

func TestSatisfiesTransportInterface(t *testing.T) {
	var _ transport.Transport = light.NewVirtual(&bytes.Buffer{})
	var _ transport.Transport = light.NewDiscard()
}

func TestName(t *testing.T) {
	if got, want := light.NewVirtual(&bytes.Buffer{}).Name(), "virtual"; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
	if got, want := light.NewDiscard().Name(), "virtual"; got != want {
		t.Errorf("NewDiscard().Name() = %q, want %q", got, want)
	}
}

func TestConnectReturnsImmediately(t *testing.T) {
	tr := light.NewVirtual(&bytes.Buffer{})
	if err := tr.Connect(context.Background()); err != nil {
		t.Errorf("Connect: %v", err)
	}
}

func TestConnectIgnoresCancelledContext(t *testing.T) {
	// The virtual light must never be the reason a test blocks or fails, even
	// when the relay is shutting down.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := light.NewVirtual(&bytes.Buffer{}).Connect(ctx); err != nil {
		t.Errorf("Connect with a cancelled context: %v", err)
	}
}

func TestConnectedIsAlwaysTrue(t *testing.T) {
	tr := light.NewVirtual(&bytes.Buffer{})
	if !tr.Connected() {
		t.Error("Connected() is false before Connect")
	}
	tr.Connect(context.Background())
	if !tr.Connected() {
		t.Error("Connected() is false after Connect")
	}
	tr.Close()
	if !tr.Connected() {
		t.Error("Connected() is false after Close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	tr := light.NewVirtual(&bytes.Buffer{})
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCloseDoesNotCloseTheWriter(t *testing.T) {
	// The writer belongs to the caller. Closing it here would surprise
	// whoever passed in os.Stdout.
	var buf bytes.Buffer
	tr := light.NewVirtual(&buf)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := tr.Send(stoplight.Frame{Color: stoplight.ColorGreen}); err != nil {
		t.Errorf("Send after Close: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("nothing written after Close; the writer was released")
	}
}

func TestSendRendersRedWithTwoSessions(t *testing.T) {
	var buf bytes.Buffer
	tr := light.NewVirtual(&buf)

	err := tr.Send(stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
			{ID: "b2", Label: "stoplight", State: "working", Color: stoplight.ColorYellow},
		},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	want := strings.Join([]string{
		"┌──────────────────────────┐",
		"│  ( ● )                   │ RED",
		"│  (   )                   │",
		"│  (   )                   │",
		"├──────────────────────────┤",
		"│ 1/2 auth-api   needs you │",
		"│ 2/2 stoplight  working   │",
		"└──────────────────────────┘",
		"",
	}, "\n")

	if got := buf.String(); got != want {
		t.Errorf("rendered frame mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderLampPositions(t *testing.T) {
	// Red is top, yellow middle, green bottom, as on a real stoplight. Only
	// the aggregate is lit.
	for _, tc := range []struct {
		color    stoplight.Color
		wantLine int // index of the lit lamp among the three lamp lines
		wantName string
	}{
		{stoplight.ColorRed, 0, "RED"},
		{stoplight.ColorYellow, 1, "YELLOW"},
		{stoplight.ColorGreen, 2, "GREEN"},
	} {
		out := light.Render(stoplight.Frame{Color: tc.color})
		lines := strings.Split(out, "\n")
		// lines[0] is the top border, so the lamps are lines 1..3.
		lamps := lines[1:4]

		for i, line := range lamps {
			lit := strings.Contains(line, "●")
			if want := i == tc.wantLine; lit != want {
				t.Errorf("%v: lamp line %d lit = %v, want %v (%q)", tc.color, i, lit, want, line)
			}
		}
		if !strings.Contains(lamps[tc.wantLine], tc.wantName) {
			t.Errorf("%v: lit lamp is not labelled %s: %q", tc.color, tc.wantName, lamps[tc.wantLine])
		}
	}
}

func TestRenderOffLightsNothing(t *testing.T) {
	// No live sessions means lamps off, not green. An empty desk is not a
	// finished task.
	out := light.Render(stoplight.Frame{Color: stoplight.ColorOff})

	if strings.Contains(out, "●") {
		t.Errorf("a lamp is lit for ColorOff:\n%s", out)
	}
	for _, name := range []string{"RED", "YELLOW", "GREEN"} {
		if strings.Contains(out, name) {
			t.Errorf("ColorOff frame names %s:\n%s", name, out)
		}
	}

	want := strings.Join([]string{
		"┌───────────┐",
		"│  (   )    │",
		"│  (   )    │",
		"│  (   )    │",
		"└───────────┘",
		"",
	}, "\n")
	if out != want {
		t.Errorf("empty frame mismatch\ngot:\n%s\nwant:\n%s", out, want)
	}
}

func TestRenderSingleSession(t *testing.T) {
	out := light.Render(stoplight.Frame{
		Color: stoplight.ColorGreen,
		Sessions: []stoplight.FrameSession{
			{ID: "z", Label: "ci", State: "done", Color: stoplight.ColorGreen},
		},
	})

	want := strings.Join([]string{
		"┌──────────────┐",
		"│  (   )       │",
		"│  (   )       │",
		"│  ( ● )       │ GREEN",
		"├──────────────┤",
		"│ 1/1 ci  done │",
		"└──────────────┘",
		"",
	}, "\n")
	if out != want {
		t.Errorf("single-session frame mismatch\ngot:\n%s\nwant:\n%s", out, want)
	}
}

func TestRenderShowsRotationPosition(t *testing.T) {
	// The device rotates through the list, so a reader needs to know how much
	// they are not seeing.
	out := light.Render(stoplight.Frame{
		Color: stoplight.ColorYellow,
		Sessions: []stoplight.FrameSession{
			{ID: "a", Label: "one", State: "working", Color: stoplight.ColorYellow},
			{ID: "b", Label: "two", State: "idle", Color: stoplight.ColorGreen},
			{ID: "c", Label: "three", State: "done", Color: stoplight.ColorGreen},
		},
	})

	for _, want := range []string{"1/3", "2/3", "3/3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing rotation position %s:\n%s", want, out)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	f := stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed},
			{ID: "b2", Label: "stoplight", State: "working", Color: stoplight.ColorYellow},
		},
	}
	first := light.Render(f)
	for range 20 {
		if got := light.Render(f); got != first {
			t.Fatalf("Render is not deterministic:\n%s\nvs\n%s", got, first)
		}
	}
}

func TestRenderBoxIsRectangular(t *testing.T) {
	// Every line must be the same display width, or the borders do not meet.
	// Rune counts, because the box glyphs are multi-byte.
	frames := []stoplight.Frame{
		{Color: stoplight.ColorOff},
		{Color: stoplight.ColorRed, Sessions: []stoplight.FrameSession{
			{ID: "a", Label: "short", State: "done", Color: stoplight.ColorGreen},
		}},
		{Color: stoplight.ColorYellow, Sessions: []stoplight.FrameSession{
			{ID: "a", Label: "a-very-long-session-label", State: "needs you", Color: stoplight.ColorRed},
			{ID: "b", Label: "x", State: "idle", Color: stoplight.ColorGreen},
		}},
	}

	for i, f := range frames {
		out := light.Render(f)
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")

		want := -1
		for _, line := range lines {
			// The lamp label sits outside the box, so measure only up to the
			// closing border.
			box := line
			if idx := strings.LastIndex(line, "│"); idx >= 0 {
				box = line[:idx+len("│")]
			}
			n := len([]rune(box))
			if want == -1 {
				want = n
				continue
			}
			if n != want {
				t.Errorf("frame %d: line %q is %d cells, want %d\n%s", i, box, n, want, out)
			}
		}
	}
}

func TestRenderLongLabelWidensBox(t *testing.T) {
	// A long label must widen the box rather than being truncated into
	// something ambiguous.
	long := "an-extremely-long-session-label"
	out := light.Render(stoplight.Frame{
		Color: stoplight.ColorRed,
		Sessions: []stoplight.FrameSession{
			{ID: "a", Label: long, State: "needs you", Color: stoplight.ColorRed},
		},
	})
	if !strings.Contains(out, long) {
		t.Errorf("long label was truncated:\n%s", out)
	}
}

func TestRenderEndsWithNewline(t *testing.T) {
	out := light.Render(stoplight.Frame{Color: stoplight.ColorGreen})
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("rendered frame does not end with a newline: %q", out)
	}
}

func TestSendAppendsEachFrame(t *testing.T) {
	// Successive frames accumulate, so a test can watch a sequence.
	var buf bytes.Buffer
	tr := light.NewVirtual(&buf)

	for _, c := range []stoplight.Color{stoplight.ColorGreen, stoplight.ColorRed} {
		if err := tr.Send(stoplight.Frame{Color: c}); err != nil {
			t.Fatalf("Send(%v): %v", c, err)
		}
	}

	out := buf.String()
	if !strings.Contains(out, "GREEN") || !strings.Contains(out, "RED") {
		t.Errorf("both frames should be present:\n%s", out)
	}
	if got := strings.Count(out, "┌"); got != 2 {
		t.Errorf("got %d frames, want 2:\n%s", got, out)
	}
}

func TestSendMatchesRender(t *testing.T) {
	f := stoplight.Frame{
		Color: stoplight.ColorYellow,
		Sessions: []stoplight.FrameSession{
			{ID: "a", Label: "build", State: "working", Color: stoplight.ColorYellow},
		},
	}
	var buf bytes.Buffer
	if err := light.NewVirtual(&buf).Send(f); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got, want := buf.String(), light.Render(f); got != want {
		t.Errorf("Send wrote %q, Render returned %q", got, want)
	}
}

func TestSendReportsWriterError(t *testing.T) {
	tr := light.NewVirtual(failingWriter{})
	if err := tr.Send(stoplight.Frame{Color: stoplight.ColorRed}); err == nil {
		t.Error("Send returned nil for a writer that always fails")
	}
}

// failingWriter fails every write, standing in for a closed pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestNewDiscardWritesNothing(t *testing.T) {
	tr := light.NewDiscard()
	if err := tr.Send(stoplight.Frame{Color: stoplight.ColorRed}); err != nil {
		t.Errorf("Send: %v", err)
	}
	if err := tr.Connect(context.Background()); err != nil {
		t.Errorf("Connect: %v", err)
	}
	if !tr.Connected() {
		t.Error("Connected() is false")
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestConcurrentSend(t *testing.T) {
	// Two goroutines sending at once must not interleave half-drawn frames.
	var buf bytes.Buffer
	tr := light.NewVirtual(&buf)

	var wg sync.WaitGroup
	const senders, each = 8, 25
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if err := tr.Send(stoplight.Frame{Color: stoplight.ColorRed}); err != nil {
					t.Errorf("Send: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Every frame must be intact: one top border per frame, and the total
	// output must be an exact multiple of a single frame.
	one := light.Render(stoplight.Frame{Color: stoplight.ColorRed})
	if got, want := buf.Len(), len(one)*senders*each; got != want {
		t.Errorf("output is %d bytes, want %d; frames interleaved", got, want)
	}
	if got, want := strings.Count(buf.String(), "┌"), senders*each; got != want {
		t.Errorf("got %d frames, want %d", got, want)
	}
}
