package relay

import (
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// StatusResponse is the JSON body of GET /v1/status. It is the wire form of
// Status: the in-process type carries Go values, and this one carries what a
// client over HTTP can read without knowing Go.
//
// The two differ deliberately. Uptime is a time.Duration, which marshals to a
// nanosecond count no other language would guess at, so it goes out as whole
// seconds. Sessions carry unexported-by-convention fields a client cannot use,
// so they are narrowed to StatusSession.
type StatusResponse struct {
	// Transport names the configured transport, for example
	// "serial:/dev/cu.usbmodem1234", "ble" or "virtual".
	Transport string `json:"transport"`

	// Connected reports whether the transport currently reaches a light.
	// False is expected rather than an error: the light may be off, asleep,
	// or out of range.
	Connected bool `json:"connected"`

	// Aggregate is the most urgent colour across Sessions, "off" when there
	// are none. It marshals as a name, never a number, matching the frame
	// format the firmware reads.
	Aggregate stoplight.Color `json:"aggregate"`

	// UptimeSeconds is how long Run has been executing, in whole seconds.
	// It is zero before Run starts.
	UptimeSeconds int64 `json:"uptime_seconds"`

	// Sessions are the live sessions, ordered by start time. It is never
	// null: a client must not have to special-case an empty desk.
	Sessions []StatusSession `json:"sessions"`
}

// StatusSession is one live session in a StatusResponse.
type StatusSession struct {
	// ID is the opaque producer-assigned identifier, stable for the life of
	// the session. It is what `stoplight task --session-id` names.
	ID string `json:"id"`

	// Label is what the screen shows: the override when one is set, the
	// derived or producer-sent label otherwise.
	Label string `json:"label"`

	// State is the lifecycle word under the label: "idle", "working",
	// "needs you" or "done".
	State string `json:"state"`

	// Color is this session's own colour, which may be less urgent than the
	// aggregate. It marshals as a name, never a number.
	Color stoplight.Color `json:"color"`

	// Provider is free text naming what produced the session, for example
	// "claude-code" or "ci". It is empty when the producer sent none.
	Provider string `json:"provider,omitempty"`

	// Started fixes the rotation order on the device, in RFC 3339 UTC.
	Started time.Time `json:"started"`

	// LastSeen is when the last report arrived, in RFC 3339 UTC. It drives
	// the silence timeout.
	LastSeen time.Time `json:"last_seen"`
}

// TaskRequest is the JSON body of POST /v1/task.
type TaskRequest struct {
	// SessionID names the session to relabel. Required.
	SessionID string `json:"session_id"`

	// Label is the text to show in place of the derived label. An EMPTY
	// label CLEARS the override, so the derived label shows again. That is
	// how `stoplight task` undoes itself without a second verb.
	Label string `json:"label"`
}

// response converts a Status snapshot into its wire form.
//
// Times are normalised to UTC so a client never has to reason about the
// relay's local zone, and the session slice is allocated rather than left nil
// so that an empty desk marshals as [] instead of null.
func (s Status) response() StatusResponse {
	sessions := make([]StatusSession, 0, len(s.Sessions))
	for i := range s.Sessions {
		session := &s.Sessions[i]
		sessions = append(sessions, StatusSession{
			ID:       session.ID,
			Label:    session.Display(),
			State:    session.State.Label(),
			Color:    session.State.Color(),
			Provider: session.Provider,
			Started:  session.Started.UTC(),
			LastSeen: session.LastSeen.UTC(),
		})
	}

	return StatusResponse{
		Transport:     s.Transport,
		Connected:     s.Connected,
		Aggregate:     s.Aggregate,
		UptimeSeconds: int64(s.Uptime / time.Second),
		Sessions:      sessions,
	}
}

// Status is a snapshot of what the relay is doing, as printed by
// `stoplight status`. It is a copy taken under lock: reading it does not block
// ingest, and mutating it does not affect the relay.
type Status struct {
	// Transport names the configured transport in logs and status output,
	// for example "ble", "serial" or "virtual".
	Transport string

	// Connected reports whether the transport currently reaches a light.
	// False is an expected condition rather than an error: the light may be
	// off, asleep, or out of range.
	Connected bool

	// Sessions are the live sessions, ordered by start time.
	Sessions []stoplight.Session

	// Aggregate is the most urgent colour across Sessions, and is ColorOff
	// when there are none. It never depends on which session the screen
	// happens to be showing.
	Aggregate stoplight.Color

	// Uptime is how long Run has been executing. It is zero before Run
	// starts.
	Uptime time.Duration
}
