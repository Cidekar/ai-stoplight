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
// The caps are load-bearing rather than cosmetic. The device parses one frame
// per line into a fixed buffer, and a line too long is dropped whole. Because
// frames are sent only on change, ONE oversized frame is not one lost update:
// every later frame is oversized too, and the lamp holds its last colour
// indefinitely. MaxFrameSessions caps how MANY sessions a frame carries; these
// cap how LARGE each one can be, and neither is sufficient alone.
const (
	MaxSessionIDLen = 128
	MaxLabelLen     = 256
	MaxProviderLen  = 64
	MaxCwdLen       = 4096
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
