// Package light renders frames to a terminal instead of to hardware. The
// virtual light implements transport.Transport, so the relay, the state
// machine and the adapters can all be exercised end to end with no device
// attached. This is how the project is tested.
package light

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// Lamp glyphs. The lit lamp is filled and the dark ones are empty, so the
// state survives a screenshot, a log file, and a reader who cannot see colour.
const (
	lampLit  = "( ● )"
	lampDark = "(   )"
)

// lampOrder is top to bottom on a real stoplight: red, yellow, green.
var lampOrder = []stoplight.Color{
	stoplight.ColorRed,
	stoplight.ColorYellow,
	stoplight.ColorGreen,
}

// lampWidth is the inside width of the lamp housing: two spaces of padding
// around the five-cell glyph.
const lampWidth = 9

// Transport renders each frame to a writer as ASCII art. It is safe for
// concurrent use: the mutex keeps two goroutines from interleaving half-drawn
// frames into the same writer.
type Transport struct {
	mu sync.Mutex
	w  io.Writer
}

// compile-time check that Transport satisfies the interface.
var _ transport.Transport = (*Transport)(nil)

// NewVirtual returns a light that draws frames to w.
func NewVirtual(w io.Writer) *Transport {
	return &Transport{w: w}
}

// NewDiscard returns a light that draws nothing. It exists for tests that need
// a working transport but do not care what it displays.
func NewDiscard() *Transport {
	return &Transport{w: io.Discard}
}

// Connect returns nil at once. There is nothing to connect to, and a virtual
// light must never be the reason a test blocks.
func (t *Transport) Connect(ctx context.Context) error {
	return nil
}

// Connected always reports true. The terminal is always there.
func (t *Transport) Connected() bool {
	return true
}

// Close is a no-op and is safe to call more than once. The writer belongs to
// the caller, so closing it here would surprise whoever passed it in.
func (t *Transport) Close() error {
	return nil
}

// Name identifies the transport in logs and `stoplight status`.
func (t *Transport) Name() string {
	return "virtual"
}

// Send renders one frame to the writer.
func (t *Transport) Send(f stoplight.Frame) error {
	out := Render(f)

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := io.WriteString(t.w, out); err != nil {
		return fmt.Errorf("virtual: write frame: %w", err)
	}
	return nil
}

// Render draws a frame as ASCII art and returns it. It is a pure function of
// the frame, which is what makes the virtual light testable: the same frame
// always produces the same string, byte for byte.
func Render(f stoplight.Frame) string {
	rows := sessionRows(f.Sessions)

	// The box is as wide as its widest line, so the borders always meet.
	inner := lampWidth
	for _, row := range rows {
		if w := width(row); w > inner {
			inner = w
		}
	}

	var b strings.Builder
	b.WriteString("┌" + strings.Repeat("─", inner+2) + "┐\n")

	// The lamps. Only the aggregate colour is lit, and ColorOff lights none:
	// an empty desk is not a finished task, so nothing glows.
	for _, lamp := range lampOrder {
		glyph := lampDark
		label := ""
		if lamp == f.Color {
			glyph = lampLit
			label = strings.ToUpper(lamp.String())
		}
		// The housing stays a fixed width even when the session list makes the
		// box wider, so the lamps read as a stoplight rather than stretching
		// into a banner.
		housing := pad(" "+glyph+" ", inner)
		line := "│ " + housing + " │"
		if label != "" {
			line += " " + label
		}
		b.WriteString(line + "\n")
	}

	// A frame with no sessions stops at the lamps. Drawing an empty
	// compartment below them would suggest the screen had failed.
	if len(rows) == 0 {
		b.WriteString("└" + strings.Repeat("─", inner+2) + "┘\n")
		return b.String()
	}

	b.WriteString("├" + strings.Repeat("─", inner+2) + "┤\n")
	for _, row := range rows {
		b.WriteString("│ " + pad(row, inner) + " │\n")
	}
	b.WriteString("└" + strings.Repeat("─", inner+2) + "┘\n")
	return b.String()
}

// sessionRows formats the session list, one row per session, each carrying its
// position in the rotation. The position is shown because the real device
// rotates through the list and a reader needs to know how much they are not
// seeing.
func sessionRows(sessions []stoplight.FrameSession) []string {
	if len(sessions) == 0 {
		return nil
	}

	total := len(sessions)
	positions := make([]string, total)
	labels := make([]string, total)
	for i, s := range sessions {
		positions[i] = fmt.Sprintf("%d/%d", i+1, total)
		labels[i] = s.Label
	}

	// Columns are padded to a common width so the states line up down the
	// screen, which is what makes the list scannable.
	posWidth := maxWidth(positions)
	labelWidth := maxWidth(labels)

	rows := make([]string, total)
	for i, s := range sessions {
		rows[i] = pad(positions[i], posWidth) + " " +
			pad(labels[i], labelWidth) + "  " + s.State
	}
	return rows
}

// maxWidth returns the width of the widest string in ss.
func maxWidth(ss []string) int {
	max := 0
	for _, s := range ss {
		if w := width(s); w > max {
			max = w
		}
	}
	return max
}

// pad right-pads s with spaces to w cells. A string already at or over the
// width is returned unchanged, so a long label pushes the box wider rather
// than being truncated into something ambiguous.
func pad(s string, w int) string {
	if n := w - width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// width returns how many terminal cells s occupies. It counts runes rather
// than bytes, because the box-drawing and lamp glyphs are multi-byte and a
// byte count would misalign every border.
func width(s string) int {
	return utf8.RuneCountInString(s)
}
