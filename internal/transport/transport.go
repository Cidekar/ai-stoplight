// Package transport carries frames from the relay to a light. Serial, BLE and
// the virtual light all implement the same interface, so nothing above this
// layer cares which one is in use. That seam is what makes hardware optional:
// the whole project can be developed and tested against the virtual light.
package transport

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// Transport carries frames to a light.
type Transport interface {
	// Connect blocks until connected or ctx is cancelled. It retries
	// internally; a returned error means give up, not try again.
	Connect(ctx context.Context) error

	// Send transmits one frame. An error means this frame was lost, not that
	// the transport is dead. The caller may send the next frame regardless.
	Send(f stoplight.Frame) error

	// Connected reports whether a light is currently reachable.
	Connected() bool

	// Close releases the port or link. It is safe to call more than once.
	Close() error

	// Name identifies the transport in logs and `stoplight status`.
	Name() string
}

// EncodeFrame renders a frame as the newline-delimited JSON the firmware
// parses: one object, then a single '\n'. Every transport shares this, so the
// firmware sees an identical byte stream whether it arrived over serial or BLE.
//
// The trailing newline is the frame delimiter, not cosmetic. A reader on the
// device splits on it, so a frame without one would merge into the next.
func EncodeFrame(f stoplight.Frame) ([]byte, error) {
	// json.Marshal escapes HTML by default, which would turn a label
	// containing & or < into an escape sequence the tiny device font cannot
	// show. An encoder with escaping off keeps labels legible.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	// Encode already appends the newline that delimits the frame.
	return buf.Bytes(), nil
}
