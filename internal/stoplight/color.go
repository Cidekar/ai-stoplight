// Package stoplight holds the core types and the state machine that decides
// what the light shows. It imports nothing from the rest of the project, so
// the invariants that make a glance trustworthy can be tested in isolation.
package stoplight

import (
	"encoding/json"
	"fmt"
)

// Color is what a lamp shows. Ordered by urgency, so the zero value is the
// least urgent and taking the maximum over a set gives the aggregate.
type Color uint8

// The lamp colours, in ascending order of urgency.
const (
	ColorOff Color = iota
	ColorGreen
	ColorYellow
	ColorRed
)

// String returns the wire name of the colour: "off", "green", "yellow" or
// "red". An out-of-range value reports as "off" rather than panicking,
// because a bad colour must never take the relay down.
func (c Color) String() string {
	switch c {
	case ColorGreen:
		return "green"
	case ColorYellow:
		return "yellow"
	case ColorRed:
		return "red"
	default:
		return "off"
	}
}

// MarshalJSON writes the colour as its wire name. RFC 1 specifies the frame
// field as an enum of strings, not the numeric value Go would emit by default.
func (c Color) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.String())
}

// UnmarshalJSON reads a colour from its wire name. Unknown names are an error
// here, unlike unknown events: this hop is private between the relay and the
// reference firmware, so a name we do not know is a bug rather than a newer
// producer.
func (c *Color) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	parsed, ok := ParseColor(name)
	if !ok {
		return fmt.Errorf("stoplight: unknown color %q", name)
	}
	*c = parsed
	return nil
}

// ParseColor maps a wire name onto a Color, reporting whether it was known.
func ParseColor(s string) (Color, bool) {
	switch s {
	case "off":
		return ColorOff, true
	case "green":
		return ColorGreen, true
	case "yellow":
		return ColorYellow, true
	case "red":
		return ColorRed, true
	default:
		return ColorOff, false
	}
}

// State is where a session is in its lifecycle.
type State uint8

// The session states. Idle and Done are both green but stay distinct, because
// the screen shows different text and only Done implies work completed.
const (
	StateIdle State = iota
	StateWorking
	StateBlocked
	StateDone
	StateExpired
)

// Color maps a state onto a lamp colour. Expired contributes no colour, so it
// drops out of the aggregate rather than holding the lamp.
func (s State) Color() Color {
	switch s {
	case StateIdle, StateDone:
		return ColorGreen
	case StateWorking:
		return ColorYellow
	case StateBlocked:
		return ColorRed
	default:
		return ColorOff
	}
}

// Label is the short word shown under the session name.
func (s State) Label() string {
	switch s {
	case StateWorking:
		return "working"
	case StateBlocked:
		return "needs you"
	case StateDone:
		return "done"
	case StateExpired:
		return "expired"
	default:
		return "idle"
	}
}

// Live reports whether the session still counts towards the aggregate. An
// expired session is dropped from the set.
//
// This is an ALLOWLIST over the four live states, matching Color and Label.
// Written as a denylist it disagreed with both: State(99) was live, coloured
// off and labelled idle, three answers to one question about the same value.
// The default direction matters here in a way it does not for the other two.
// An unknown colour showing off is harmless; an unknown state counted LIVE
// enters the aggregate and survives dropExpiredLocked, so a corrupt value
// would sit in the session map holding a place on the screen forever.
func (s State) Live() bool {
	switch s {
	case StateIdle, StateWorking, StateBlocked, StateDone:
		return true
	default:
		return false
	}
}

// Event is something a producer reported, or a timeout the relay raised.
type Event uint8

// The events. The first five arrive over the wire under the lowercase names in
// RFC 1; EventTimeout is internal and is raised only by Tracker.Sweep.
const (
	EventIdle Event = iota
	EventStarted
	EventBlocked
	EventFinished
	EventEnded
	EventTimeout
)

// String returns the wire name of the event. EventTimeout has no wire form and
// reports as "timeout" for logs only.
func (e Event) String() string {
	switch e {
	case EventStarted:
		return "started"
	case EventBlocked:
		return "blocked"
	case EventFinished:
		return "finished"
	case EventEnded:
		return "ended"
	case EventTimeout:
		return "timeout"
	default:
		return "idle"
	}
}

// ParseEvent maps a wire name onto an Event. Unknown names return ok == false
// and must be ignored rather than rejected, so a producer written against a
// later spec does not break an older relay. "timeout" is not accepted: it is
// internal, and a producer must not be able to expire its own session.
func ParseEvent(s string) (Event, bool) {
	switch s {
	case "idle":
		return EventIdle, true
	case "started":
		return EventStarted, true
	case "blocked":
		return EventBlocked, true
	case "finished":
		return EventFinished, true
	case "ended":
		return EventEnded, true
	default:
		return EventIdle, false
	}
}

// Next returns the state this event moves to, and whether it changed anything.
// A false second return means the event was a no-op, which is normal: a
// finished while already Done is not an error.
//
// Expired is terminal. Every event from it is a no-op.
func (s State) Next(e Event) (State, bool) {
	// EXPIRED IS TERMINAL. RFC 1 section 7 shows it as the end of the diagram
	// with no arrow out, and nothing may move a session back from it.
	//
	// `idle` used to be an exception, on the reasoning that a producer which
	// restarts should revive its session. That row was unreachable: Tracker
	// DELETES an expired session as soon as it is marked, so no event ever
	// arrives at one. The path a restarting producer actually takes is the one
	// the RFC intends, and it already works: the session is gone, and the next
	// report for that ID creates a fresh session via InitialState, with a new
	// Started that puts it correctly at the end of the rotation. Reviving in
	// place would instead have kept the original Started and slotted a
	// just-restarted session into the middle of the cycle.
	//
	// Terminal here also means the state machine is safe on its own, without
	// relying on the tracker to delete first.
	if s == StateExpired {
		return s, false
	}

	// These apply from any live state, so they come before the table.
	switch e {
	case EventTimeout, EventEnded:
		return StateExpired, true
	case EventIdle:
		if s == StateIdle {
			return s, false
		}
		return StateIdle, true
	}

	// `started`, `blocked` and `finished` all apply from every live state. The
	// event says what the agent is doing now, and that is true whatever it was
	// doing before. Restricting any of them by source state only ever drops a
	// light the producer asked for.
	//
	// Each case matters on its own:
	//
	//   - blocked or finished from Idle: an agent can go from sitting idle
	//     straight to needing a human, with no turn in between. A scheduled job
	//     that wakes up and immediately asks for a password, or a producer
	//     whose `started` was dropped.
	//
	//   - finished from Blocked: this is how a permission prompt ends. The
	//     human answers and the agent completes the turn. It is the ordinary
	//     path for the Claude Code adapter, whose Stop hook sends `finished`
	//     after a Notification sent `blocked`.
	//
	//   - blocked from Done: a finished session that begins a new turn can hit
	//     a permission prompt in it. RFC 1 section 8 requires the lamp to be
	//     red if any session is blocked, so this must not be swallowed.
	//
	// A state that is already the target returns false, because the session did
	// not move. Repeating an event is a keepalive, not a change, and the relay
	// only transmits on change.
	next := s
	switch e {
	case EventStarted:
		next = StateWorking
	case EventBlocked:
		next = StateBlocked
	case EventFinished:
		next = StateDone
	}
	return next, next != s
}

// InitialState returns the state a session takes when the relay sees it for the
// first time, and whether the session should be created at all.
//
// A fresh session is created directly in the state the event implies, rather
// than created as Idle and then transitioned. The two differ for `blocked`: a
// session created as Idle would stay green, and the relay would lose the very
// red light this rule exists to protect.
//
// ok is false for `ended` and for EventTimeout. Both only end a session, and
// there is nothing to expire for a session that was never tracked.
func InitialState(e Event) (State, bool) {
	switch e {
	case EventIdle:
		return StateIdle, true
	case EventStarted:
		return StateWorking, true
	case EventBlocked:
		return StateBlocked, true
	case EventFinished:
		return StateDone, true
	default: // EventEnded, EventTimeout
		return StateIdle, false
	}
}
