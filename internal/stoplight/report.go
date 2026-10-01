package stoplight

import "errors"

// Report is one event from one producer. The wire form is specified in RFC 1
// section 5, and is identical over HTTP and the unix socket.
type Report struct {
	SessionID string `json:"session_id"`
	Event     string `json:"event"`
	Label     string `json:"label,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Detail    string `json:"detail,omitempty"` // reserved: the screen is too small
	Cwd       string `json:"cwd,omitempty"`
}

// The field length caps from RFC 1 Appendix A, in bytes.
//
// A field over its cap is TRUNCATED rather than rejected: RFC 1 section 11
// requires the relay to be lenient with a producer written against a later
// spec, so a long field must not cost the caller its light.
//
// These caps are what the relay STORES. They are generous on purpose: section
// 11 leniency means a producer's over-long id or label is kept, not rejected,
// and the stored value feeds `stoplight status`, the socket, and label
// derivation, none of which share the device's line buffer.
//
// What goes on the WIRE to the device is capped separately and more tightly by
// the MaxFrame* constants below. Keeping the two apart is the whole fix for the
// line-budget overflow: the relay can remember a long label without ever
// putting one on a line the firmware would drop whole.
const (
	MaxSessionIDLen = 128
	MaxLabelLen     = 256
	MaxProviderLen  = 64
	MaxCwdLen       = 4096
)

// The per-field caps for a session entry ON THE WIRE, in bytes. These mirror
// SL_MAX_ID and SL_MAX_LABEL in firmware/stoplight/protocol.h, which is the
// single source of truth: SL_LINE_MAX is sized there for exactly these widths,
// 145 bytes per entry, 8 entries, under the 1536-byte line buffer.
//
// They are load-bearing rather than cosmetic. The device parses one frame per
// line into a fixed buffer, and a line too long is dropped whole. Because
// frames are sent only on change, ONE oversized frame is not one lost update:
// every later frame is oversized too, and the lamp holds its last colour
// indefinitely. MaxFrameSessions caps how MANY sessions a frame carries; these
// cap how LARGE each one is on the wire, and neither is sufficient alone. The
// relay's storage caps above are far wider than SL_LINE_MAX allows for eight
// entries, so without these the worst-case frame runs to ~3.5KB and overflows.
//
// The firmware already truncates id to SL_MAX_ID and label to SL_MAX_LABEL on
// the way in (parseString in protocol.cpp), so capping here sends exactly what
// the device would keep anyway, minus the bytes that would have overflowed the
// line. SL_MAX_ID is 40, which still holds a 36-character UUID whole, so no id
// the relay actually emits is shortened; the cap only trims the headroom the
// storage cap allowed.
const (
	MaxFrameIDLen    = 40
	MaxFrameLabelLen = 48
)

// Errors returned by Report.Validate.
var (
	ErrMissingSessionID = errors.New("stoplight: session_id is required")
	ErrMissingEvent     = errors.New("stoplight: event is required")
)

// Validate checks the required fields. An unknown event name is not an error
// here: RFC 1 section 11 requires the caller to ignore it rather than reject
// it, so that a producer written against a later spec keeps working.
func (r *Report) Validate() error {
	if r.SessionID == "" {
		return ErrMissingSessionID
	}
	if r.Event == "" {
		return ErrMissingEvent
	}
	return nil
}
