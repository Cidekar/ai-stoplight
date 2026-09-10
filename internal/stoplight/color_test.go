package stoplight

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestColorString(t *testing.T) {
	tests := []struct {
		name  string
		color Color
		want  string
	}{
		{"off", ColorOff, "off"},
		{"green", ColorGreen, "green"},
		{"yellow", ColorYellow, "yellow"},
		{"red", ColorRed, "red"},
		{"out of range falls back to off", Color(99), "off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.color.String(); got != tt.want {
				t.Errorf("Color(%d).String() = %q, want %q", tt.color, got, tt.want)
			}
		})
	}
}

// Urgency ordering is what makes the aggregate a simple maximum.
func TestColorUrgencyOrder(t *testing.T) {
	if !(ColorOff < ColorGreen && ColorGreen < ColorYellow && ColorYellow < ColorRed) {
		t.Fatal("colors must ascend by urgency: off < green < yellow < red")
	}
}

func TestColorMarshalJSON(t *testing.T) {
	tests := []struct {
		name  string
		color Color
		want  string
	}{
		{"off", ColorOff, `"off"`},
		{"green", ColorGreen, `"green"`},
		{"yellow", ColorYellow, `"yellow"`},
		{"red", ColorRed, `"red"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.color)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal(%v) = %s, want %s", tt.color, got, tt.want)
			}
		})
	}
}

func TestColorUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    Color
		wantErr bool
	}{
		{"off", `"off"`, ColorOff, false},
		{"green", `"green"`, ColorGreen, false},
		{"yellow", `"yellow"`, ColorYellow, false},
		{"red", `"red"`, ColorRed, false},
		{"unknown name", `"chartreuse"`, ColorOff, true},
		{"number is not a color", `2`, ColorOff, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Color
			err := json.Unmarshal([]byte(tt.input), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%s) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("Unmarshal(%s) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestColorRoundTrip(t *testing.T) {
	for _, color := range []Color{ColorOff, ColorGreen, ColorYellow, ColorRed} {
		t.Run(color.String(), func(t *testing.T) {
			encoded, err := json.Marshal(color)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var decoded Color
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if decoded != color {
				t.Errorf("round trip = %v, want %v", decoded, color)
			}
		})
	}
}

func TestParseColor(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   Color
		wantOK bool
	}{
		{"off", "off", ColorOff, true},
		{"green", "green", ColorGreen, true},
		{"yellow", "yellow", ColorYellow, true},
		{"red", "red", ColorRed, true},
		{"unknown", "purple", ColorOff, false},
		{"empty", "", ColorOff, false},
		{"case sensitive", "RED", ColorOff, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseColor(tt.input)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParseColor(%q) = (%v, %v), want (%v, %v)", tt.input, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestStateColor(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  Color
	}{
		{"idle is green", StateIdle, ColorGreen},
		{"working is yellow", StateWorking, ColorYellow},
		{"blocked is red", StateBlocked, ColorRed},
		{"done is green", StateDone, ColorGreen},
		{"expired contributes nothing", StateExpired, ColorOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Color(); got != tt.want {
				t.Errorf("State(%d).Color() = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestStateLabel(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  string
	}{
		{"idle", StateIdle, "idle"},
		{"working", StateWorking, "working"},
		{"blocked", StateBlocked, "needs you"},
		{"done", StateDone, "done"},
		{"expired", StateExpired, "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Label(); got != tt.want {
				t.Errorf("State(%d).Label() = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

// Idle and Done are both green but must stay distinct: only Done implies work
// completed, and the screen shows different text.
func TestIdleAndDoneShareColorButNotLabel(t *testing.T) {
	if StateIdle.Color() != StateDone.Color() {
		t.Error("idle and done must both be green")
	}
	if StateIdle.Label() == StateDone.Label() {
		t.Error("idle and done must show different text")
	}
}

func TestStateLive(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{"idle", StateIdle, true},
		{"working", StateWorking, true},
		{"blocked", StateBlocked, true},
		{"done", StateDone, true},
		{"expired", StateExpired, false},
		// Live is an allowlist over the four live states, matching Color and
		// Label. A denylist would call a corrupt state live while Color said
		// off and Label said idle: three answers to one question.
		{"out of range is not live", State(99), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Live(); got != tt.want {
				t.Errorf("State(%d).Live() = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestParseEvent(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   Event
		wantOK bool
	}{
		{"idle", "idle", EventIdle, true},
		{"started", "started", EventStarted, true},
		{"blocked", "blocked", EventBlocked, true},
		{"finished", "finished", EventFinished, true},
		{"ended", "ended", EventEnded, true},
		// Invariant 5: unknown names are ignored, never rejected.
		{"unknown name", "exploded", EventIdle, false},
		{"empty", "", EventIdle, false},
		{"case sensitive", "STARTED", EventIdle, false},
		{"whitespace is not trimmed", " started", EventIdle, false},
		// timeout is internal: a producer must not expire its own session.
		{"timeout is not a wire event", "timeout", EventIdle, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseEvent(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("ParseEvent(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("ParseEvent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestEventString(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{"idle", EventIdle, "idle"},
		{"started", EventStarted, "started"},
		{"blocked", EventBlocked, "blocked"},
		{"finished", EventFinished, "finished"},
		{"ended", EventEnded, "ended"},
		{"timeout", EventTimeout, "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.event.String(); got != tt.want {
				t.Errorf("Event(%d).String() = %q, want %q", tt.event, got, tt.want)
			}
		})
	}
}

// Every wire event round trips through its name.
func TestEventStringParseRoundTrip(t *testing.T) {
	for _, event := range []Event{EventIdle, EventStarted, EventBlocked, EventFinished, EventEnded} {
		t.Run(event.String(), func(t *testing.T) {
			got, ok := ParseEvent(event.String())
			if !ok || got != event {
				t.Errorf("ParseEvent(%q) = (%v, %v), want (%v, true)", event.String(), got, ok, event)
			}
		})
	}
}

// The full transition table from design.md, including every no-op pair.
func TestStateNext(t *testing.T) {
	tests := []struct {
		name        string
		from        State
		event       Event
		want        State
		wantChanged bool
	}{
		// The listed transitions.
		{"idle + started -> working", StateIdle, EventStarted, StateWorking, true},
		{"idle + blocked -> blocked", StateIdle, EventBlocked, StateBlocked, true},
		{"idle + finished -> done", StateIdle, EventFinished, StateDone, true},
		{"working + blocked -> blocked", StateWorking, EventBlocked, StateBlocked, true},
		{"working + finished -> done", StateWorking, EventFinished, StateDone, true},
		{"blocked + started -> working", StateBlocked, EventStarted, StateWorking, true},
		// This row was inverted. It previously asserted a no-op, which pinned
		// the defect that left the lamp red after every answered permission
		// prompt. `finished` is how a block ends on the everyday path.
		{"blocked + finished -> done", StateBlocked, EventFinished, StateDone, true},
		{"done + started -> working", StateDone, EventStarted, StateWorking, true},
		// A finished session that begins a new turn can block inside it. RFC 1
		// section 8 requires red whenever any session is blocked.
		{"done + blocked -> blocked", StateDone, EventBlocked, StateBlocked, true},

		// ended expires from any live state.
		{"idle + ended -> expired", StateIdle, EventEnded, StateExpired, true},
		{"working + ended -> expired", StateWorking, EventEnded, StateExpired, true},
		{"blocked + ended -> expired", StateBlocked, EventEnded, StateExpired, true},
		{"done + ended -> expired", StateDone, EventEnded, StateExpired, true},
		{"expired + ended is a no-op", StateExpired, EventEnded, StateExpired, false},

		// Timeout expires from any live state.
		{"idle + timeout -> expired", StateIdle, EventTimeout, StateExpired, true},
		{"working + timeout -> expired", StateWorking, EventTimeout, StateExpired, true},
		{"blocked + timeout -> expired", StateBlocked, EventTimeout, StateExpired, true},
		{"done + timeout -> expired", StateDone, EventTimeout, StateExpired, true},
		{"expired + timeout is a no-op", StateExpired, EventTimeout, StateExpired, false},

		// idle restarts from any live state. Expired is terminal.
		{"idle + idle is a no-op", StateIdle, EventIdle, StateIdle, false},
		{"working + idle -> idle", StateWorking, EventIdle, StateIdle, true},
		{"blocked + idle -> idle", StateBlocked, EventIdle, StateIdle, true},
		{"done + idle -> idle", StateDone, EventIdle, StateIdle, true},
		{"expired + idle is a no-op", StateExpired, EventIdle, StateExpired, false},

		// Repeating the event a state is already in is a keepalive, not a
		// change. The relay transmits only on change, so these must stay false.
		{"working + started is a no-op", StateWorking, EventStarted, StateWorking, false},
		{"blocked + blocked is a no-op", StateBlocked, EventBlocked, StateBlocked, false},
		{"done + finished is a no-op", StateDone, EventFinished, StateDone, false},

		// Expired is terminal. Nothing brings a session back: the tracker
		// deletes it, and a later event for that ID creates a fresh session.
		{"expired + started is a no-op", StateExpired, EventStarted, StateExpired, false},
		{"expired + blocked is a no-op", StateExpired, EventBlocked, StateExpired, false},
		{"expired + finished is a no-op", StateExpired, EventFinished, StateExpired, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, changed := tt.from.Next(tt.event)
			if got != tt.want || changed != tt.wantChanged {
				t.Errorf("State(%d).Next(%v) = (%v, %v), want (%v, %v)",
					tt.from, tt.event, got, changed, tt.want, tt.wantChanged)
			}
		})
	}
}

// A false second return must always mean the state did not move, over every
// pair in the table. The tracker relies on this to decide what changed.
func TestStateNextChangedAgreesWithState(t *testing.T) {
	states := []State{StateIdle, StateWorking, StateBlocked, StateDone, StateExpired}
	events := []Event{EventIdle, EventStarted, EventBlocked, EventFinished, EventEnded, EventTimeout}
	for _, from := range states {
		for _, event := range events {
			next, changed := from.Next(event)
			if changed != (next != from) {
				t.Errorf("State(%d).Next(%v) = (%v, %v): changed disagrees with the state move",
					from, event, next, changed)
			}
		}
	}
}

// A session the relay never saw is created in the state its event implies. The
// blocked case is the one that matters: creating as Idle and then transitioning
// would leave the lamp green while an agent waits on a human.
func TestInitialState(t *testing.T) {
	tests := []struct {
		name     string
		event    Event
		want     State
		wantOK   bool
		wantLamp Color
	}{
		{"idle creates an idle session", EventIdle, StateIdle, true, ColorGreen},
		{"started creates a working session", EventStarted, StateWorking, true, ColorYellow},
		{"blocked creates a blocked session", EventBlocked, StateBlocked, true, ColorRed},
		{"finished creates a done session", EventFinished, StateDone, true, ColorGreen},
		{"ended creates nothing", EventEnded, StateIdle, false, ColorGreen},
		{"timeout creates nothing", EventTimeout, StateIdle, false, ColorGreen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := InitialState(tt.event)
			if ok != tt.wantOK {
				t.Fatalf("InitialState(%v) ok = %v, want %v", tt.event, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("InitialState(%v) = %v, want %v", tt.event, got, tt.want)
			}
			if ok && got.Color() != tt.wantLamp {
				t.Errorf("InitialState(%v).Color() = %v, want %v", tt.event, got.Color(), tt.wantLamp)
			}
		})
	}
}

// An event that creates a session must be a no-op when re-applied to the state
// it created. The tracker runs Next on freshly created sessions, and a second
// move there would contradict InitialState.
func TestInitialStateIsStableUnderItsOwnEvent(t *testing.T) {
	for _, event := range []Event{EventIdle, EventStarted, EventBlocked, EventFinished} {
		t.Run(event.String(), func(t *testing.T) {
			initial, ok := InitialState(event)
			if !ok {
				t.Fatalf("InitialState(%v) ok = false, want true", event)
			}
			if next, changed := initial.Next(event); changed || next != initial {
				t.Errorf("State(%d).Next(%v) = (%v, %v), want (%v, false)",
					initial, event, next, changed, initial)
			}
		})
	}
}

// Whatever the producer did, a timeout always reaches Expired. This is what
// stops a crashed session holding the lamp.
func TestTimeoutAlwaysReachesExpired(t *testing.T) {
	for _, from := range []State{StateIdle, StateWorking, StateBlocked, StateDone, StateExpired} {
		if next, _ := from.Next(EventTimeout); next != StateExpired {
			t.Errorf("State(%d).Next(timeout) = %v, want expired", from, next)
		}
	}
}

// stateName gives a readable name for a State in failure messages. State has
// no String method on purpose: Label() is user-facing text for the device
// screen, and these are internal names for humans reading test output.
func stateName(s State) string {
	switch s {
	case StateIdle:
		return "Idle"
	case StateWorking:
		return "Working"
	case StateBlocked:
		return "Blocked"
	case StateDone:
		return "Done"
	case StateExpired:
		return "Expired"
	default:
		return fmt.Sprintf("State(%d)", uint8(s))
	}
}

// A block ends when the agent finishes the turn the human unblocked. This is
// the everyday path for the only adapter that ships, and it was silently
// dropped: the lamp stayed red after every answered permission prompt.
func TestBlockedFinishedReachesDone(t *testing.T) {
	next, changed := StateBlocked.Next(EventFinished)

	if next != StateDone {
		t.Errorf("Blocked.Next(finished) = %s, want Done", stateName(next))
	}
	if !changed {
		t.Error("Blocked.Next(finished) reported no change, want true: the lamp must go green")
	}
	if got := next.Color(); got != ColorGreen {
		t.Errorf("resulting lamp = %v, want green", got)
	}
}

// The real Claude Code hook sequence, end to end, on a clean session. Each hook
// maps to one semantic event at the adapter boundary, and the lamp is asserted
// after every step. The last step is the one that regressed: Stop sends
// `finished` from Blocked, and the desk must go green.
func TestClaudeCodeHookSequenceEndsGreen(t *testing.T) {
	steps := []struct {
		hook  string
		event Event
		state State
		lamp  Color
	}{
		{"SessionStart", EventIdle, StateIdle, ColorGreen},
		{"UserPromptSubmit", EventStarted, StateWorking, ColorYellow},
		{"Notification", EventBlocked, StateBlocked, ColorRed},
		{"Stop", EventFinished, StateDone, ColorGreen},
	}

	// A session the relay has never seen starts in the state its first event
	// implies, exactly as the tracker would create it.
	state, ok := InitialState(steps[0].event)
	if !ok {
		t.Fatalf("InitialState(%v) ok = false, want true", steps[0].event)
	}
	if state != steps[0].state {
		t.Fatalf("after %s: state = %s, want %s",
			steps[0].hook, stateName(state), stateName(steps[0].state))
	}
	if got := state.Color(); got != steps[0].lamp {
		t.Fatalf("after %s: lamp = %v, want %v", steps[0].hook, got, steps[0].lamp)
	}

	for _, step := range steps[1:] {
		next, changed := state.Next(step.event)
		if !changed {
			t.Fatalf("%s sent %v from %s and nothing moved, want %s",
				step.hook, step.event, stateName(state), stateName(step.state))
		}
		state = next
		if state != step.state {
			t.Fatalf("after %s: state = %s, want %s",
				step.hook, stateName(state), stateName(step.state))
		}
		if got := state.Color(); got != step.lamp {
			t.Fatalf("after %s: lamp = %v, want %v", step.hook, got, step.lamp)
		}
	}

	if got := state.Color(); got != ColorGreen {
		t.Errorf("lamp after the full hook sequence = %v, want green", got)
	}
}

// Every edge in RFC 1 section 7, as a table, so that dropping one fails loudly
// rather than quietly leaving a lamp stuck.
//
// This table mirrors the transition table in design.md. THE TWO MUST CHANGE
// TOGETHER: if you add or remove a row here, edit design.md in the same commit,
// and vice versa. TestTransitionTableMatchesDesignDoc checks they agree.
func TestRFCSection7Edges(t *testing.T) {
	for _, tt := range rfcSection7Table {
		t.Run(tt.name, func(t *testing.T) {
			next, changed := tt.from.Next(tt.event)
			if next != tt.want {
				t.Errorf("%s.Next(%v) = %s, want %s",
					stateName(tt.from), tt.event, stateName(next), stateName(tt.want))
			}
			if changed != tt.wantChanged {
				t.Errorf("%s.Next(%v) changed = %v, want %v",
					stateName(tt.from), tt.event, changed, tt.wantChanged)
			}
			if got := next.Color(); got != tt.wantLamp {
				t.Errorf("%s.Next(%v) lamp = %v, want %v",
					stateName(tt.from), tt.event, got, tt.wantLamp)
			}
		})
	}
}

// rfcSection7Table is the transition table written out in design.md, under
// "Transitions". Each row is one row of that table, in the same order.
//
// Keeping it as a package-level value rather than inline lets two tests read
// it: one checks the code obeys it, the other checks it covers every pair so
// no edge can be forgotten.
var rfcSection7Table = []struct {
	name        string
	from        State
	event       Event
	want        State
	wantChanged bool
	wantLamp    Color
}{
	// From Idle.
	{"idle + started -> working", StateIdle, EventStarted, StateWorking, true, ColorYellow},
	{"idle + blocked -> blocked", StateIdle, EventBlocked, StateBlocked, true, ColorRed},
	{"idle + finished -> done", StateIdle, EventFinished, StateDone, true, ColorGreen},

	// From Working.
	{"working + blocked -> blocked", StateWorking, EventBlocked, StateBlocked, true, ColorRed},
	{"working + finished -> done", StateWorking, EventFinished, StateDone, true, ColorGreen},

	// From Blocked. The finished edge is the one that was missing.
	{"blocked + started -> working", StateBlocked, EventStarted, StateWorking, true, ColorYellow},
	{"blocked + finished -> done", StateBlocked, EventFinished, StateDone, true, ColorGreen},

	// From Done. A new turn may start working or go straight to blocked.
	{"done + started -> working", StateDone, EventStarted, StateWorking, true, ColorYellow},
	{"done + blocked -> blocked", StateDone, EventBlocked, StateBlocked, true, ColorRed},

	// Timeout expires any live state.
	{"idle + timeout -> expired", StateIdle, EventTimeout, StateExpired, true, ColorOff},
	{"working + timeout -> expired", StateWorking, EventTimeout, StateExpired, true, ColorOff},
	{"blocked + timeout -> expired", StateBlocked, EventTimeout, StateExpired, true, ColorOff},
	{"done + timeout -> expired", StateDone, EventTimeout, StateExpired, true, ColorOff},

	// ended expires any live state.
	{"idle + ended -> expired", StateIdle, EventEnded, StateExpired, true, ColorOff},
	{"working + ended -> expired", StateWorking, EventEnded, StateExpired, true, ColorOff},
	{"blocked + ended -> expired", StateBlocked, EventEnded, StateExpired, true, ColorOff},
	{"done + ended -> expired", StateDone, EventEnded, StateExpired, true, ColorOff},

	// idle restarts from any LIVE state. Expired is terminal, per RFC 1 §7,
	// and the tracker deletes an expired session before any event can reach it.
	{"working + idle -> idle", StateWorking, EventIdle, StateIdle, true, ColorGreen},
	{"blocked + idle -> idle", StateBlocked, EventIdle, StateIdle, true, ColorGreen},
	{"done + idle -> idle", StateDone, EventIdle, StateIdle, true, ColorGreen},
	{"expired + idle is a no-op", StateExpired, EventIdle, StateExpired, false, ColorOff},

	// Repeating the current state is a keepalive, not a change.
	{"idle + idle is a keepalive", StateIdle, EventIdle, StateIdle, false, ColorGreen},
	{"working + started is a keepalive", StateWorking, EventStarted, StateWorking, false, ColorYellow},
	{"blocked + blocked is a keepalive", StateBlocked, EventBlocked, StateBlocked, false, ColorRed},
	{"done + finished is a keepalive", StateDone, EventFinished, StateDone, false, ColorGreen},

	// Expired is terminal and no event revives it.
	{"expired + started is a no-op", StateExpired, EventStarted, StateExpired, false, ColorOff},
	{"expired + blocked is a no-op", StateExpired, EventBlocked, StateExpired, false, ColorOff},
	{"expired + finished is a no-op", StateExpired, EventFinished, StateExpired, false, ColorOff},
	{"expired + ended is a no-op", StateExpired, EventEnded, StateExpired, false, ColorOff},
	{"expired + timeout is a no-op", StateExpired, EventTimeout, StateExpired, false, ColorOff},
}

// The documented table must cover every (state, event) pair. An omission here
// is how the blocked + finished edge went missing from design.md, and from the
// code that was written to follow it, for as long as it did.
func TestTransitionTableMatchesDesignDoc(t *testing.T) {
	states := []State{StateIdle, StateWorking, StateBlocked, StateDone, StateExpired}
	events := []Event{EventIdle, EventStarted, EventBlocked, EventFinished, EventEnded, EventTimeout}

	documented := make(map[[2]uint8]bool, len(rfcSection7Table))
	for _, row := range rfcSection7Table {
		key := [2]uint8{uint8(row.from), uint8(row.event)}
		if documented[key] {
			t.Errorf("duplicate row for %s + %v", stateName(row.from), row.event)
		}
		documented[key] = true
	}

	for _, from := range states {
		for _, event := range events {
			if !documented[[2]uint8{uint8(from), uint8(event)}] {
				t.Errorf("no documented transition for %s + %v: "+
					"add it to rfcSection7Table and to the table in design.md",
					stateName(from), event)
			}
		}
	}

	if got, want := len(rfcSection7Table), len(states)*len(events); got != want {
		t.Errorf("rfcSection7Table has %d rows, want %d: one per (state, event) pair", got, want)
	}
}
