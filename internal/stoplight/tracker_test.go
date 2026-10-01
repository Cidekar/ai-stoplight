package stoplight

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const testTimeout = 30 * time.Minute

var base = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// report is a terse constructor for the common case.
func report(id, event string) Report {
	return Report{SessionID: id, Event: event}
}

// applyAll feeds a sequence of events at one-minute intervals and returns the
// tracker, so that Started ordering follows the arrival order.
func applyAll(t *testing.T, timeout time.Duration, reports ...Report) *Tracker {
	t.Helper()
	tracker := NewTracker(timeout)
	for i, r := range reports {
		tracker.Apply(r, base.Add(time.Duration(i)*time.Minute))
	}
	return tracker
}

func TestNewTrackerStartsEmpty(t *testing.T) {
	tracker := NewTracker(testTimeout)
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off", got)
	}
	if got := tracker.Sessions(); len(got) != 0 {
		t.Errorf("Sessions() = %v, want empty", got)
	}
	frame := tracker.Frame()
	if frame.Color != ColorOff || len(frame.Sessions) != 0 {
		t.Errorf("Frame() = %+v, want off with no sessions", frame)
	}
}

// Invariant 2: zero live sessions means off, never green. An empty desk is not
// a finished task.
func TestInvariantNoLiveSessionsMeansOff(t *testing.T) {
	tests := []struct {
		name    string
		reports []Report
	}{
		{"never had a session", nil},
		{"the only session ended", []Report{report("a1", "started"), report("a1", "ended")}},
		{"a finished session then ended", []Report{
			report("a1", "started"), report("a1", "finished"), report("a1", "ended"),
		}},
		{"every session ended", []Report{
			report("a1", "started"), report("b2", "started"),
			report("a1", "ended"), report("b2", "ended"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := applyAll(t, testTimeout, tt.reports...)
			if got := tracker.Aggregate(); got != ColorOff {
				t.Errorf("Aggregate() = %v, want off", got)
			}
			if got := tracker.Frame().Color; got != ColorOff {
				t.Errorf("Frame().Color = %v, want off", got)
			}
			if got := len(tracker.Sessions()); got != 0 {
				t.Errorf("len(Sessions()) = %d, want 0", got)
			}
		})
	}
}

// Invariant 1: the aggregate is the most urgent state across every live
// session.
func TestInvariantAggregateIsMostUrgent(t *testing.T) {
	tests := []struct {
		name    string
		reports []Report
		want    Color
	}{
		{"one idle session", []Report{report("a1", "idle")}, ColorGreen},
		{"one working session", []Report{report("a1", "started")}, ColorYellow},
		{"one blocked session", []Report{
			report("a1", "started"), report("a1", "blocked"),
		}, ColorRed},
		{"one finished session", []Report{
			report("a1", "started"), report("a1", "finished"),
		}, ColorGreen},
		{"blocked beats working", []Report{
			report("a1", "started"), report("a1", "blocked"), report("b2", "started"),
		}, ColorRed},
		{"blocked beats done", []Report{
			report("a1", "started"), report("a1", "blocked"),
			report("b2", "started"), report("b2", "finished"),
		}, ColorRed},
		{"working beats done", []Report{
			report("a1", "started"), report("a1", "finished"), report("b2", "started"),
		}, ColorYellow},
		{"working beats idle", []Report{
			report("a1", "idle"), report("b2", "started"),
		}, ColorYellow},
		{"all done is green", []Report{
			report("a1", "started"), report("a1", "finished"),
			report("b2", "started"), report("b2", "finished"),
		}, ColorGreen},
		{"a blocked session survives others ending", []Report{
			report("a1", "started"), report("a1", "blocked"),
			report("b2", "started"), report("b2", "ended"),
		}, ColorRed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := applyAll(t, testTimeout, tt.reports...)
			if got := tracker.Aggregate(); got != tt.want {
				t.Errorf("Aggregate() = %v, want %v", got, tt.want)
			}
			if got := tracker.Frame().Color; got != tt.want {
				t.Errorf("Frame().Color = %v, want %v", got, tt.want)
			}
		})
	}
}

// Invariant 1: an expired session contributes nothing, so it cannot hold the
// lamp red.
func TestInvariantExpiredSessionsExcludedFromAggregate(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("dead", "started"), base)
	tracker.Apply(report("dead", "blocked"), base)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Fatalf("Aggregate() = %v, want red before expiry", got)
	}

	tracker.Sweep(base.Add(testTimeout))

	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off once the blocked session expired", got)
	}
}

// Invariant 3: provider is never consulted. The same events under different
// providers give the same colour.
func TestInvariantAggregateIgnoresProvider(t *testing.T) {
	providers := []string{"", "claude-code", "deepseek", "ci", "some-agent-from-2030"}
	for _, provider := range providers {
		t.Run("provider="+provider, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			tracker.Apply(Report{SessionID: "a1", Event: "started", Provider: provider}, base)
			tracker.Apply(Report{SessionID: "a1", Event: "blocked", Provider: provider}, base)
			if got := tracker.Aggregate(); got != ColorRed {
				t.Errorf("Aggregate() = %v, want red", got)
			}
		})
	}

	// Mixed providers aggregate as one set.
	mixed := NewTracker(testTimeout)
	mixed.Apply(Report{SessionID: "a1", Event: "started", Provider: "claude-code"}, base)
	mixed.Apply(Report{SessionID: "a1", Event: "finished", Provider: "claude-code"}, base)
	mixed.Apply(Report{SessionID: "b2", Event: "started", Provider: "deepseek"}, base)
	mixed.Apply(Report{SessionID: "b2", Event: "blocked", Provider: "deepseek"}, base)
	if got := mixed.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red across providers", got)
	}
}

// Invariant 4: the same set of sessions yields the same colour whatever order
// the events arrived in. Permutes the interleaving of three sessions' streams.
func TestInvariantAggregateIsOrderIndependent(t *testing.T) {
	// Three independent sessions, each with its own ordered event stream. Any
	// interleaving that preserves per-session order must end the same way.
	streams := [][]Report{
		{report("a1", "started"), report("a1", "blocked")},
		{report("b2", "started"), report("b2", "finished")},
		{report("c3", "idle")},
	}

	var (
		orderings [][]Report
		walk      func(current []Report, cursors []int)
	)
	walk = func(current []Report, cursors []int) {
		done := true
		for i, stream := range streams {
			if cursors[i] < len(stream) {
				done = false
				next := append(append([]Report(nil), current...), stream[cursors[i]])
				advanced := append([]int(nil), cursors...)
				advanced[i]++
				walk(next, advanced)
			}
		}
		if done {
			orderings = append(orderings, current)
		}
	}
	walk(nil, make([]int, len(streams)))

	if len(orderings) != 30 {
		t.Fatalf("generated %d interleavings, want 30", len(orderings))
	}

	for _, ordering := range orderings {
		names := make([]string, 0, len(ordering))
		for _, r := range ordering {
			names = append(names, r.SessionID+":"+r.Event)
		}
		tracker := NewTracker(testTimeout)
		// One timestamp for every event, so Started order cannot silently
		// carry the result.
		for _, r := range ordering {
			tracker.Apply(r, base)
		}
		if got := tracker.Aggregate(); got != ColorRed {
			t.Errorf("Aggregate() = %v, want red for arrival order %v", got, names)
		}
		if got := len(tracker.Sessions()); got != 3 {
			t.Errorf("len(Sessions()) = %d, want 3 for arrival order %v", got, names)
		}
	}
}

// Invariant 5: an unknown event name is ignored. No state change, no error,
// and no new session.
func TestInvariantUnknownEventsIgnored(t *testing.T) {
	tests := []struct {
		name  string
		event string
	}{
		{"a future event name", "escalated"},
		{"empty", ""},
		{"wrong case", "BLOCKED"},
		{"internal timeout is not accepted from the wire", "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			tracker.Apply(report("a1", "started"), base)
			before := tracker.Frame()

			if changed := tracker.Apply(report("a1", tt.event), base.Add(time.Minute)); changed {
				t.Error("Apply() with an unknown event = true, want false")
			}
			if got := tracker.Frame(); !got.Equal(before) {
				t.Errorf("Frame() = %+v, want unchanged %+v", got, before)
			}
			if got := tracker.Aggregate(); got != ColorYellow {
				t.Errorf("Aggregate() = %v, want yellow", got)
			}
		})
	}
}

// An unknown event must not bring a session into existence either.
func TestUnknownEventDoesNotCreateSession(t *testing.T) {
	tracker := NewTracker(testTimeout)
	if changed := tracker.Apply(report("ghost", "escalated"), base); changed {
		t.Error("Apply() = true, want false")
	}
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off", got)
	}
}

// A report with no session ID cannot be tracked and is ignored rather than
// creating an anonymous session.
func TestApplyIgnoresEmptySessionID(t *testing.T) {
	tracker := NewTracker(testTimeout)
	if changed := tracker.Apply(report("", "blocked"), base); changed {
		t.Error("Apply() = true, want false")
	}
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
}

// Invariant 6: an unknown session ID creates the session, in the state its
// event implies. Losing a red light is worse than tracking a session whose
// start was missed, so an unseen `blocked` must land on red, not green.
func TestInvariantUnknownSessionIsCreated(t *testing.T) {
	tests := []struct {
		name      string
		event     string
		want      State
		aggregate Color
	}{
		{"blocked for an unseen session is red", "blocked", StateBlocked, ColorRed},
		{"started for an unseen session is yellow", "started", StateWorking, ColorYellow},
		{"finished for an unseen session is green", "finished", StateDone, ColorGreen},
		{"idle for an unseen session is green", "idle", StateIdle, ColorGreen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			tracker.Apply(report("orphan", tt.event), base)

			sessions := tracker.Sessions()
			if len(sessions) != 1 {
				t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
			}
			if sessions[0].ID != "orphan" {
				t.Errorf("ID = %q, want %q", sessions[0].ID, "orphan")
			}
			if sessions[0].State != tt.want {
				t.Errorf("State = %v, want %v", sessions[0].State, tt.want)
			}
			if got := tracker.Aggregate(); got != tt.aggregate {
				t.Errorf("Aggregate() = %v, want %v", got, tt.aggregate)
			}
		})
	}
}

// The defect this guards: a producer restarts mid-turn and reports `blocked`
// for a session the relay never saw. The lamp must go red. A relay that created
// the session as Idle would show green while a human is being waited on.
func TestBlockedForUnseenSessionIsRed(t *testing.T) {
	tracker := NewTracker(testTimeout)
	if changed := tracker.Apply(report("restarted", "blocked"), base); !changed {
		t.Fatal("Apply() = false, want true: a new red session moves the frame")
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Fatalf("Aggregate() = %v, want red", got)
	}
	if got := tracker.Frame().Color; got != ColorRed {
		t.Errorf("Frame().Color = %v, want red", got)
	}
}

// An unseen `blocked` must beat a healthy session, exactly as a seen one would.
func TestBlockedForUnseenSessionWinsTheAggregate(t *testing.T) {
	tracker := applyAll(t, testTimeout,
		report("known", "started"),
		report("orphan", "blocked"),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// `ended` for a session we never saw creates nothing at all. There is no
// session to expire, so the frame must not move.
func TestEndedForUnknownSessionLeavesNothing(t *testing.T) {
	tracker := NewTracker(testTimeout)
	if changed := tracker.Apply(report("orphan", "ended"), base); changed {
		t.Error("Apply() = true, want false: nothing existed to end")
	}
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off", got)
	}
	if got := tracker.Frame().Color; got != ColorOff {
		t.Errorf("Frame().Color = %v, want off", got)
	}
}

// An unseen `ended` must not disturb the sessions the relay does know about.
func TestEndedForUnknownSessionDoesNotDisturbOthers(t *testing.T) {
	tracker := applyAll(t, testTimeout, report("known", "blocked"))
	if changed := tracker.Apply(report("orphan", "ended"), base.Add(time.Minute)); changed {
		t.Error("Apply() = true, want false")
	}
	if got := len(tracker.Sessions()); got != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", got)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// An idle session that hits a permission prompt with no intervening `started`
// still turns the lamp red.
func TestBlockedFromIdleTurnsRed(t *testing.T) {
	tracker := applyAll(t, testTimeout,
		report("s1", "idle"),
		report("s1", "blocked"),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
	sessions := tracker.Sessions()
	if len(sessions) != 1 || sessions[0].State != StateBlocked {
		t.Errorf("Sessions() = %+v, want one blocked session", sessions)
	}
}

// The same for finishing straight out of idle: the screen must say done.
func TestFinishedFromIdleReachesDone(t *testing.T) {
	tracker := applyAll(t, testTimeout,
		report("s1", "idle"),
		report("s1", "finished"),
	)
	sessions := tracker.Sessions()
	if len(sessions) != 1 || sessions[0].State != StateDone {
		t.Fatalf("Sessions() = %+v, want one done session", sessions)
	}
	if got := tracker.Aggregate(); got != ColorGreen {
		t.Errorf("Aggregate() = %v, want green", got)
	}
}

// Invariant 7: sessions come back ordered by start time, which fixes the
// rotation order on the device.
func TestInvariantSessionsOrderedByStart(t *testing.T) {
	tracker := NewTracker(testTimeout)
	// Deliberately create them out of alphabetical order.
	tracker.Apply(report("zulu", "started"), base.Add(1*time.Minute))
	tracker.Apply(report("alpha", "started"), base.Add(2*time.Minute))
	tracker.Apply(report("mike", "started"), base.Add(3*time.Minute))

	want := []string{"zulu", "alpha", "mike"}
	sessions := tracker.Sessions()
	if len(sessions) != len(want) {
		t.Fatalf("len(Sessions()) = %d, want %d", len(sessions), len(want))
	}
	for i, id := range want {
		if sessions[i].ID != id {
			t.Errorf("Sessions()[%d].ID = %q, want %q", i, sessions[i].ID, id)
		}
	}

	// The frame carries the same order.
	frame := tracker.Frame()
	for i, id := range want {
		if frame.Sessions[i].ID != id {
			t.Errorf("Frame().Sessions[%d].ID = %q, want %q", i, frame.Sessions[i].ID, id)
		}
	}

	// Later events must not reorder the rotation.
	tracker.Apply(report("zulu", "blocked"), base.Add(9*time.Minute))
	sessions = tracker.Sessions()
	for i, id := range want {
		if sessions[i].ID != id {
			t.Errorf("after a later event Sessions()[%d].ID = %q, want %q", i, sessions[i].ID, id)
		}
	}
}

// Sessions created within the same clock tick still get a total order, so the
// rotation does not shuffle between frames.
func TestSessionsOrderIsStableForIdenticalStartTimes(t *testing.T) {
	tracker := NewTracker(testTimeout)
	for _, id := range []string{"c3", "a1", "b2"} {
		tracker.Apply(report(id, "started"), base)
	}
	want := []string{"a1", "b2", "c3"}
	for run := range 20 {
		sessions := tracker.Sessions()
		for i, id := range want {
			if sessions[i].ID != id {
				t.Fatalf("run %d: Sessions()[%d].ID = %q, want %q", run, i, sessions[i].ID, id)
			}
		}
	}
}

// Invariant 8: Apply reports change only when the frame actually moved.
func TestInvariantApplyReportsChangeOnlyOnFrameMove(t *testing.T) {
	tests := []struct {
		name    string
		setup   []Report
		apply   Report
		want    bool
		comment string
	}{
		{"first event creates a session", nil, report("a1", "started"), true, ""},
		{
			name:  "a repeated started is a no-op",
			setup: []Report{report("a1", "started")},
			apply: report("a1", "started"),
			want:  false,
		},
		{
			name:  "finished while done is a no-op",
			setup: []Report{report("a1", "started"), report("a1", "finished")},
			apply: report("a1", "finished"),
			want:  false,
		},
		{
			name:  "blocked from working moves the lamp",
			setup: []Report{report("a1", "started")},
			apply: report("a1", "blocked"),
			want:  true,
		},
		{
			name:  "blocked from idle moves the lamp",
			setup: []Report{report("a1", "idle")},
			apply: report("a1", "blocked"),
			want:  true,
		},
		{
			name:  "blocked while already blocked is a no-op",
			setup: []Report{report("a1", "blocked")},
			apply: report("a1", "blocked"),
			want:  false,
		},
		{
			name:  "ended for a session we never saw changes nothing",
			setup: []Report{report("a1", "started")},
			apply: report("ghost", "ended"),
			want:  false,
		},
		{
			name:  "ended removes the session",
			setup: []Report{report("a1", "started")},
			apply: report("a1", "ended"),
			want:  true,
		},
		{
			name:  "a new session joins the frame",
			setup: []Report{report("a1", "started")},
			apply: report("b2", "started"),
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := applyAll(t, testTimeout, tt.setup...)
			before := tracker.Frame()

			got := tracker.Apply(tt.apply, base.Add(time.Hour/2-time.Minute))

			if got != tt.want {
				t.Errorf("Apply() = %v, want %v", got, tt.want)
			}
			// The return value must agree with what the frame actually did.
			moved := !tracker.Frame().Equal(before)
			if got != moved {
				t.Errorf("Apply() = %v but the frame moved = %v", got, moved)
			}
		})
	}
}

// A label arriving later changes the screen, so the frame must be resent even
// though the colour held still.
func TestApplyReportsChangeWhenOnlyLabelMoves(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "first"}, base)

	changed := tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "second"}, base.Add(time.Minute))

	if !changed {
		t.Error("Apply() = false, want true when the label changed")
	}
	if got := tracker.Frame().Sessions[0].Label; got != "second" {
		t.Errorf("Label = %q, want %q", got, "second")
	}
}

// A keepalive that moves nothing must not wake the radio.
func TestApplyKeepaliveDoesNotChangeFrame(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)
	tracker.Apply(report("a1", "blocked"), base.Add(time.Minute))

	for i := 2; i < 8; i++ {
		if changed := tracker.Apply(report("a1", "blocked"), base.Add(time.Duration(i)*time.Minute)); changed {
			t.Errorf("keepalive %d: Apply() = true, want false", i)
		}
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// Invariant 9: a session silent for longer than the timeout expires.
func TestInvariantSweepExpiresSilentSessions(t *testing.T) {
	tests := []struct {
		name        string
		elapsed     time.Duration
		wantChanged bool
		wantLive    int
	}{
		{"well inside the timeout", time.Minute, false, 1},
		{"just inside the timeout", testTimeout - time.Nanosecond, false, 1},
		{"exactly at the timeout", testTimeout, true, 0},
		{"past the timeout", testTimeout + time.Minute, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			tracker.Apply(report("a1", "started"), base)
			tracker.Apply(report("a1", "blocked"), base)

			changed := tracker.Sweep(base.Add(tt.elapsed))

			if changed != tt.wantChanged {
				t.Errorf("Sweep() = %v, want %v", changed, tt.wantChanged)
			}
			if got := len(tracker.Sessions()); got != tt.wantLive {
				t.Errorf("len(Sessions()) = %d, want %d", got, tt.wantLive)
			}
		})
	}
}

// Any event resets the countdown.
func TestSweepCountdownResetsOnEveryEvent(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)

	// A keepalive twenty minutes in pushes expiry out.
	tracker.Apply(report("a1", "started"), base.Add(20*time.Minute))

	if changed := tracker.Sweep(base.Add(40 * time.Minute)); changed {
		t.Error("Sweep() = true, want false: the keepalive reset the countdown")
	}
	if got := len(tracker.Sessions()); got != 1 {
		t.Errorf("len(Sessions()) = %d, want 1", got)
	}

	if changed := tracker.Sweep(base.Add(51 * time.Minute)); !changed {
		t.Error("Sweep() = false, want true once the reset countdown elapsed")
	}
}

// A sweep expires only the sessions that went quiet.
func TestSweepExpiresOnlySilentSessions(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("quiet", "started"), base)
	tracker.Apply(report("quiet", "blocked"), base)
	tracker.Apply(report("chatty", "started"), base.Add(25*time.Minute))

	if changed := tracker.Sweep(base.Add(35 * time.Minute)); !changed {
		t.Fatal("Sweep() = false, want true")
	}

	sessions := tracker.Sessions()
	if len(sessions) != 1 || sessions[0].ID != "chatty" {
		t.Fatalf("Sessions() = %+v, want only chatty", sessions)
	}
	// The dead blocked session must not still be holding the lamp red.
	if got := tracker.Aggregate(); got != ColorYellow {
		t.Errorf("Aggregate() = %v, want yellow", got)
	}
}

// Invariant 8, for Sweep: nothing expired means nothing to send.
func TestSweepReportsNoChangeWhenNothingExpires(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)

	for i := 1; i < 5; i++ {
		if changed := tracker.Sweep(base.Add(time.Duration(i) * time.Minute)); changed {
			t.Errorf("sweep %d: Sweep() = true, want false", i)
		}
	}
}

// An empty tracker sweeps quietly, however often the ticker fires.
func TestSweepOnEmptyTrackerIsQuiet(t *testing.T) {
	tracker := NewTracker(testTimeout)
	for i := range 5 {
		if changed := tracker.Sweep(base.Add(time.Duration(i) * time.Hour)); changed {
			t.Errorf("sweep %d: Sweep() = true, want false", i)
		}
	}
}

// A second sweep after everything expired reports no further change.
func TestSweepIsIdempotent(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)

	if changed := tracker.Sweep(base.Add(time.Hour)); !changed {
		t.Fatal("first Sweep() = false, want true")
	}
	if changed := tracker.Sweep(base.Add(2 * time.Hour)); changed {
		t.Error("second Sweep() = true, want false")
	}
}

// A non-positive timeout disables expiry by silence, leaving `ended` as the
// only way out.
func TestSweepWithoutTimeoutNeverExpires(t *testing.T) {
	tracker := NewTracker(0)
	tracker.Apply(report("a1", "started"), base)

	if changed := tracker.Sweep(base.Add(1000 * time.Hour)); changed {
		t.Error("Sweep() = true, want false when the timeout is disabled")
	}
	if got := len(tracker.Sessions()); got != 1 {
		t.Errorf("len(Sessions()) = %d, want 1", got)
	}
}

// A session that expired and then reports again is tracked afresh.
func TestSessionCanRestartAfterExpiry(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)
	tracker.Sweep(base.Add(time.Hour))
	if got := len(tracker.Sessions()); got != 0 {
		t.Fatalf("len(Sessions()) = %d, want 0 after expiry", got)
	}

	restart := base.Add(2 * time.Hour)
	tracker.Apply(report("a1", "started"), restart)

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if !sessions[0].Started.Equal(restart) {
		t.Errorf("Started = %v, want %v", sessions[0].Started, restart)
	}
	if sessions[0].State != StateWorking {
		t.Errorf("State = %v, want working", sessions[0].State)
	}
}

func TestFrameContents(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	tracker.Apply(Report{SessionID: "a1", Event: "blocked", Label: "auth-api"}, base)
	tracker.Apply(Report{SessionID: "b2", Event: "started", Label: "stoplight"}, base.Add(time.Minute))

	frame := tracker.Frame()

	want := Frame{
		Color: ColorRed,
		Sessions: []FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: ColorRed},
			{ID: "b2", Label: "stoplight", State: "working", Color: ColorYellow},
		},
	}
	if !frame.Equal(want) {
		t.Errorf("Frame() = %+v, want %+v", frame, want)
	}
}

// The frame shows the override, because that is what Display returns.
func TestFrameUsesDisplayLabel(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "derived"}, base)

	if got := tracker.Frame().Sessions[0].Label; got != "derived" {
		t.Errorf("Label = %q, want %q", got, "derived")
	}
}

// A producer that sends no label gets one derived from cwd, provider or ID.
func TestApplyDerivesLabelWhenAbsent(t *testing.T) {
	tests := []struct {
		name      string
		provider  string
		sessionID string
		want      string
	}{
		{"from provider", "claude-code", "a1", "claude-code"},
		{"from session id", "", "a1", "a1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			tracker.Apply(Report{SessionID: tt.sessionID, Event: "started", Provider: tt.provider}, base)

			sessions := tracker.Sessions()
			if len(sessions) != 1 {
				t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
			}
			if sessions[0].Label != tt.want {
				t.Errorf("Label = %q, want %q", sessions[0].Label, tt.want)
			}
		})
	}
}

// An explicit label always wins over derivation.
func TestApplyPrefersExplicitLabel(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{
		SessionID: "a1", Event: "started", Label: "explicit", Provider: "claude-code",
	}, base)

	if got := tracker.Sessions()[0].Label; got != "explicit" {
		t.Errorf("Label = %q, want %q", got, "explicit")
	}
}

// A later report with no label must not wipe the label already held.
func TestApplyKeepsLabelWhenReportOmitsIt(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	tracker.Apply(Report{SessionID: "a1", Event: "blocked"}, base.Add(time.Minute))

	if got := tracker.Sessions()[0].Label; got != "auth-api" {
		t.Errorf("Label = %q, want %q", got, "auth-api")
	}
}

// An override is what the screen shows, in place of the derived label.
func TestSetOverrideWinsOverLabel(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)

	if changed := tracker.SetOverride("a1", "nightly run", base.Add(time.Minute)); !changed {
		t.Error("SetOverride() = false, want true: the frame label moved")
	}

	session := tracker.Sessions()[0]
	if session.Override != "nightly run" {
		t.Errorf("Override = %q, want %q", session.Override, "nightly run")
	}
	if session.Label != "auth-api" {
		t.Errorf("Label = %q, want the derived label kept underneath", session.Label)
	}
	if got := tracker.Frame().Sessions[0].Label; got != "nightly run" {
		t.Errorf("Frame label = %q, want %q", got, "nightly run")
	}
}

// The whole point of an override: a later report carrying a derived label must
// not clobber it. Apply writes Label, and Display reads Override first.
func TestSetOverrideSurvivesLaterReport(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	tracker.SetOverride("a1", "nightly run", base.Add(time.Minute))

	tracker.Apply(Report{SessionID: "a1", Event: "blocked", Label: "auth-api"}, base.Add(2*time.Minute))
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "some-branch"}, base.Add(3*time.Minute))

	if got := tracker.Frame().Sessions[0].Label; got != "nightly run" {
		t.Errorf("Frame label = %q, want the override to survive", got)
	}
	if got := tracker.Sessions()[0].Label; got != "some-branch" {
		t.Errorf("Label = %q, want the derived label to keep tracking underneath", got)
	}
}

// An empty label clears the override, so the derived label shows again.
func TestSetOverrideEmptyClears(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	tracker.SetOverride("a1", "nightly run", base.Add(time.Minute))

	if changed := tracker.SetOverride("a1", "", base.Add(2*time.Minute)); !changed {
		t.Error("SetOverride(\"\") = false, want true: the frame label moved back")
	}
	if got := tracker.Sessions()[0].Override; got != "" {
		t.Errorf("Override = %q, want it cleared", got)
	}
	if got := tracker.Frame().Sessions[0].Label; got != "auth-api" {
		t.Errorf("Frame label = %q, want the derived label back", got)
	}
}

// An override for a session we never saw creates NO session. A label carries
// no state, so nothing about it reports work, and RFC 1 section 8 requires an
// empty desk to be off rather than green. The override is parked instead, and
// Apply consumes it when the session genuinely starts.
func TestSetOverrideCreatesUnknownSession(t *testing.T) {
	tracker := NewTracker(testTimeout)

	if changed := tracker.SetOverride("ghost", "mystery", base); changed {
		t.Error("SetOverride() = true for an unseen session, want false: " +
			"naming a task must not light the lamp")
	}

	if got := tracker.Sessions(); len(got) != 0 {
		t.Fatalf("len(Sessions()) = %d, want 0: a label creates no session", len(got))
	}
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off: no session reported any work", got)
	}
	frame := tracker.Frame()
	if frame.Color != ColorOff || len(frame.Sessions) != 0 {
		t.Errorf("Frame() = %+v, want off with no sessions", frame)
	}

	// The name is not lost. It applies the moment the session really starts.
	if changed := tracker.Apply(report("ghost", "started"), base.Add(time.Minute)); !changed {
		t.Error("Apply() = false, want true: the session started")
	}
	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1 once the session started", len(sessions))
	}
	if got := sessions[0].Override; got != "mystery" {
		t.Errorf("Override = %q, want mystery: the parked label must be consumed", got)
	}
	if got := tracker.Frame().Sessions[0].Label; got != "mystery" {
		t.Errorf("Frame label = %q, want mystery", got)
	}
}

// A parked override is consumed once and once only. A session that ends and
// starts again must not silently pick the old name back up.
func TestParkedOverrideIsConsumedOnce(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.SetOverride("ghost", "mystery", base)
	tracker.Apply(report("ghost", "started"), base.Add(time.Minute))
	tracker.Apply(report("ghost", "ended"), base.Add(2*time.Minute))

	tracker.Apply(report("ghost", "started"), base.Add(3*time.Minute))
	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if got := sessions[0].Override; got != "" {
		t.Errorf("Override = %q, want it empty: a parked label applies once", got)
	}
}

// Parking an override for a session that never starts must not leak. The park
// expires on the same silence rule as a session.
func TestParkedOverrideExpires(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.SetOverride("ghost", "mystery", base)

	tracker.Sweep(base.Add(testTimeout + time.Minute))

	tracker.Apply(report("ghost", "started"), base.Add(2*testTimeout))
	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if got := sessions[0].Override; got != "" {
		t.Errorf("Override = %q, want it empty: the park went stale", got)
	}
}

// An override changes the screen, never the lamp. Aggregation reads state.
//
// The empty case is the one that matters most, and is the one this guard used
// to miss: with no session at all the lamp is off, and naming a task must
// leave it off rather than inventing a green session to hang the name on.
func TestSetOverrideDoesNotChangeAggregate(t *testing.T) {
	tests := []struct {
		name  string
		event string // "" means apply nothing: an empty tracker
		want  Color
	}{
		{"empty desk stays off", "", ColorOff},
		{"working stays yellow", "started", ColorYellow},
		{"blocked stays red", "blocked", ColorRed},
		{"done stays green", "finished", ColorGreen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := NewTracker(testTimeout)
			if tt.event != "" {
				tracker.Apply(Report{SessionID: "a1", Event: tt.event}, base)
			}
			before := tracker.Aggregate()

			tracker.SetOverride("a1", "renamed", base.Add(time.Minute))

			if got := tracker.Aggregate(); got != tt.want || got != before {
				t.Errorf("Aggregate() = %v, want %v unchanged", got, tt.want)
			}
			if got := tracker.Frame().Color; got != tt.want {
				t.Errorf("Frame().Color = %v, want %v", got, tt.want)
			}
			if tt.event == "" {
				if got := len(tracker.Sessions()); got != 0 {
					t.Errorf("len(Sessions()) = %d, want 0: a label creates no session", got)
				}
				return
			}
			if got := tracker.Sessions()[0].State.Color(); got != tt.want {
				t.Errorf("session colour = %v, want %v", got, tt.want)
			}
		})
	}
}

// Setting the same override twice changes no frame, so the radio stays quiet.
func TestSetOverrideReportsNoChangeWhenIdentical(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	tracker.SetOverride("a1", "nightly run", base.Add(time.Minute))

	if changed := tracker.SetOverride("a1", "nightly run", base.Add(2*time.Minute)); changed {
		t.Error("SetOverride() = true for an unchanged label, want false")
	}
	// Clearing an override that was never set is likewise quiet.
	tracker.Apply(Report{SessionID: "b2", Event: "started", Label: "b"}, base.Add(3*time.Minute))
	if changed := tracker.SetOverride("b2", "", base.Add(4*time.Minute)); changed {
		t.Error("SetOverride(\"\") = true with no override set, want false")
	}
}

// An empty session ID is ignored, matching Apply.
func TestSetOverrideIgnoresEmptySessionID(t *testing.T) {
	tracker := NewTracker(testTimeout)

	if changed := tracker.SetOverride("", "anything", base); changed {
		t.Error("SetOverride(\"\") = true, want false")
	}
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
}

// An override resets the silence countdown, because it is a sign of life.
func TestSetOverrideRefreshesLastSeen(t *testing.T) {
	const timeout = 30 * time.Minute
	tracker := NewTracker(timeout)
	tracker.Apply(Report{SessionID: "a1", Event: "blocked"}, base)

	tracker.SetOverride("a1", "renamed", base.Add(25*time.Minute))

	// Without the refresh this sweep would expire the session.
	tracker.Sweep(base.Add(45 * time.Minute))
	if got := len(tracker.Sessions()); got != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1: the override refreshed LastSeen", got)
	}

	tracker.Sweep(base.Add(60 * time.Minute))
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0 once genuinely silent", got)
	}
}

func TestApplyRecordsMetadata(t *testing.T) {
	tracker := NewTracker(testTimeout)
	seen := base.Add(time.Minute)
	tracker.Apply(Report{
		SessionID: "a1", Event: "started", Label: "auth", Provider: "deepseek", Cwd: "/tmp/auth",
	}, seen)

	session := tracker.Sessions()[0]
	if session.Provider != "deepseek" {
		t.Errorf("Provider = %q, want %q", session.Provider, "deepseek")
	}
	if session.Dir != "/tmp/auth" {
		t.Errorf("Dir = %q, want %q", session.Dir, "/tmp/auth")
	}
	if !session.Started.Equal(seen) {
		t.Errorf("Started = %v, want %v", session.Started, seen)
	}
	if !session.LastSeen.Equal(seen) {
		t.Errorf("LastSeen = %v, want %v", session.LastSeen, seen)
	}
}

// Started is fixed at creation; only LastSeen moves.
func TestApplyKeepsStartedFixed(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)
	later := base.Add(10 * time.Minute)
	tracker.Apply(report("a1", "blocked"), later)

	session := tracker.Sessions()[0]
	if !session.Started.Equal(base) {
		t.Errorf("Started = %v, want %v", session.Started, base)
	}
	if !session.LastSeen.Equal(later) {
		t.Errorf("LastSeen = %v, want %v", session.LastSeen, later)
	}
}

// Sessions returns copies, so a caller cannot reach into the tracker's state.
func TestSessionsReturnsCopies(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth"}, base)

	sessions := tracker.Sessions()
	sessions[0].Label = "tampered"
	sessions[0].State = StateBlocked

	if got := tracker.Sessions()[0].Label; got != "auth" {
		t.Errorf("Label = %q, want %q: the caller mutated tracker state", got, "auth")
	}
	if got := tracker.Aggregate(); got != ColorYellow {
		t.Errorf("Aggregate() = %v, want yellow: the caller mutated tracker state", got)
	}
}

// The whole lifecycle, one session, end to end.
func TestSessionLifecycle(t *testing.T) {
	tracker := NewTracker(testTimeout)

	steps := []struct {
		event       string
		wantState   State
		wantColor   Color
		wantChanged bool
	}{
		{"idle", StateIdle, ColorGreen, true},
		{"started", StateWorking, ColorYellow, true},
		{"blocked", StateBlocked, ColorRed, true},
		{"started", StateWorking, ColorYellow, true},
		{"finished", StateDone, ColorGreen, true},
		{"started", StateWorking, ColorYellow, true},
		{"finished", StateDone, ColorGreen, true},
		{"ended", StateExpired, ColorOff, true},
	}
	for i, step := range steps {
		changed := tracker.Apply(report("a1", step.event), base.Add(time.Duration(i)*time.Minute))
		if changed != step.wantChanged {
			t.Errorf("step %d (%s): Apply() = %v, want %v", i, step.event, changed, step.wantChanged)
		}
		if got := tracker.Aggregate(); got != step.wantColor {
			t.Errorf("step %d (%s): Aggregate() = %v, want %v", i, step.event, got, step.wantColor)
		}
		if step.wantState == StateExpired {
			if got := len(tracker.Sessions()); got != 0 {
				t.Errorf("step %d (%s): len(Sessions()) = %d, want 0", i, step.event, got)
			}
			continue
		}
		if got := tracker.Sessions()[0].State; got != step.wantState {
			t.Errorf("step %d (%s): State = %v, want %v", i, step.event, got, step.wantState)
		}
	}
}

// Concurrent use must not race and must not lose sessions. Run under -race.
func TestTrackerConcurrentAccess(t *testing.T) {
	tracker := NewTracker(testTimeout)

	const (
		workers = 8
		rounds  = 50
	)
	events := []string{"idle", "started", "blocked", "finished", "unknown-event"}

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			id := string(rune('a' + worker))
			for round := range rounds {
				now := base.Add(time.Duration(round) * time.Second)
				tracker.Apply(Report{
					SessionID: id,
					Event:     events[round%len(events)],
					Provider:  "worker",
				}, now)
			}
		}(w)
	}

	// Readers and a sweeper run alongside the writers.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				tracker.Aggregate()
				tracker.Frame()
				tracker.Sessions()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := range rounds {
			tracker.Sweep(base.Add(time.Duration(round) * time.Second))
		}
	}()

	wg.Wait()

	if got := len(tracker.Sessions()); got != workers {
		t.Errorf("len(Sessions()) = %d, want %d", got, workers)
	}
}

// Concurrent writers must not lose a red light.
func TestTrackerConcurrentBlockedIsNeverLost(t *testing.T) {
	tracker := NewTracker(testTimeout)

	var wg sync.WaitGroup
	for w := range 16 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			id := string(rune('a' + worker))
			tracker.Apply(report(id, "started"), base)
			// One worker blocks; the rest finish.
			if worker == 0 {
				tracker.Apply(report(id, "blocked"), base)
			} else {
				tracker.Apply(report(id, "finished"), base)
			}
		}(w)
	}
	wg.Wait()

	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: a blocked session was lost", got)
	}
}

// The real Claude Code hook sequence through the tracker, on a clean relay,
// asserting the aggregate after every hook.
//
// This is the everyday path for the only adapter that ships. It regressed:
// Stop sends `finished` from Blocked, that transition was dropped, and the lamp
// stayed red after every answered permission prompt until the session was
// restarted or the 30-minute timeout fired.
func TestClaudeCodeHookSequenceLeavesTheDeskGreen(t *testing.T) {
	steps := []struct {
		hook  string
		event string
		state State
		want  Color
	}{
		{"SessionStart", "idle", StateIdle, ColorGreen},
		{"UserPromptSubmit", "started", StateWorking, ColorYellow},
		{"Notification", "blocked", StateBlocked, ColorRed},
		{"Stop", "finished", StateDone, ColorGreen},
	}

	tracker := NewTracker(testTimeout)
	for i, step := range steps {
		at := base.Add(time.Duration(i) * time.Minute)

		if changed := tracker.Apply(report("cc-1", step.event), at); !changed {
			t.Fatalf("%s: Apply(%s) = false, want true: the frame must move", step.hook, step.event)
		}
		if got := tracker.Aggregate(); got != step.want {
			t.Fatalf("after %s: Aggregate() = %v, want %v", step.hook, got, step.want)
		}
		if got := tracker.Frame().Color; got != step.want {
			t.Fatalf("after %s: Frame().Color = %v, want %v", step.hook, got, step.want)
		}

		sessions := tracker.Sessions()
		if len(sessions) != 1 {
			t.Fatalf("after %s: len(Sessions()) = %d, want 1", step.hook, len(sessions))
		}
		if sessions[0].State != step.state {
			t.Fatalf("after %s: state = %s, want %s",
				step.hook, sessions[0].State.Label(), step.state.Label())
		}
	}

	if got := tracker.Aggregate(); got != ColorGreen {
		t.Errorf("Aggregate() after the full hook sequence = %v, want green: "+
			"an answered permission prompt must not leave the lamp red", got)
	}
}

// A finished session that begins a new turn can hit a permission prompt inside
// it. RFC 1 section 8 requires red whenever any session is blocked, so `blocked`
// from Done must move the lamp rather than being swallowed.
func TestBlockedFromDoneTurnsTheLampRed(t *testing.T) {
	tracker := applyAll(t, testTimeout, report("a1", "started"), report("a1", "finished"))
	if got := tracker.Aggregate(); got != ColorGreen {
		t.Fatalf("Aggregate() = %v, want green before the new turn", got)
	}

	if changed := tracker.Apply(report("a1", "blocked"), base.Add(5*time.Minute)); !changed {
		t.Error("Apply(blocked) from done = false, want true")
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: a blocked session was swallowed", got)
	}
}

// TestFrameCapsSessionsButNotTheAggregate is the relay half of the dropped
// frame defect. The device can only show SL_MAX_SESSIONS entries, and an
// uncapped list produced a line long enough for the firmware to discard
// whole, aggregate included. The list is therefore capped here, and the
// aggregate must still be computed over every live session.
func TestFrameCapsSessionsButNotTheAggregate(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// Twelve working sessions, comfortably over the cap.
	for i := 0; i < 12; i++ {
		id := "s-" + string(rune('a'+i))
		tracker.Apply(report(id, "started"), base.Add(time.Duration(i)*time.Minute))
	}

	frame := tracker.Frame()
	if len(frame.Sessions) != MaxFrameSessions {
		t.Errorf("frame carries %d sessions, want the cap of %d",
			len(frame.Sessions), MaxFrameSessions)
	}
	if frame.Color != ColorYellow {
		t.Errorf("Color = %v, want yellow", frame.Color)
	}

	// The thirteenth session goes blocked. It cannot be listed, because the
	// list is already full of older sessions, but the lamp MUST go red: the
	// aggregate covers every live session, not just the ones on screen.
	tracker.Apply(report("s-late", "blocked"), base.Add(20*time.Minute))

	frame = tracker.Frame()
	if len(frame.Sessions) != MaxFrameSessions {
		t.Errorf("frame carries %d sessions, want the cap of %d",
			len(frame.Sessions), MaxFrameSessions)
	}
	if frame.Color != ColorRed {
		t.Errorf("Color = %v, want red: a blocked session past the cap must "+
			"still reach the lamp", frame.Color)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// TestFrameKeepsTheMostUrgentSessionsWhenCapped pins the choice of which
// sessions survive truncation. If only some can be named, the ones that need
// a human are the ones worth naming.
func TestFrameKeepsTheMostUrgentSessionsWhenCapped(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// Ten sessions started one minute apart, all working. The IDs run BACKWARDS
	// against start order on purpose: "s-9" starts first and "s-0" starts last.
	// Start-time order and lexicographic ID order are therefore opposites here,
	// so an assertion that only compares adjacent IDs cannot pass by accident.
	ids := make([]string, 10)
	for i := 0; i < 10; i++ {
		ids[i] = "s-" + string(rune('9'-i))
		tracker.Apply(report(ids[i], "started"), base.Add(time.Duration(i)*time.Minute))
	}
	// The last two to start, which start-time order alone would drop, go
	// blocked. Urgency must keep them.
	tracker.Apply(report(ids[8], "blocked"), base.Add(30*time.Minute))
	tracker.Apply(report(ids[9], "blocked"), base.Add(31*time.Minute))

	frame := tracker.Frame()
	if len(frame.Sessions) != MaxFrameSessions {
		t.Fatalf("frame carries %d sessions, want %d", len(frame.Sessions),
			MaxFrameSessions)
	}

	found := map[string]Color{}
	for _, s := range frame.Sessions {
		found[s.ID] = s.Color
	}
	for _, id := range []string{ids[8], ids[9]} {
		if c, ok := found[id]; !ok {
			t.Errorf("blocked session %s was dropped from the frame; the most "+
				"urgent sessions must survive truncation", id)
		} else if c != ColorRed {
			t.Errorf("session %s colour = %v, want red", id, c)
		}
	}

	// The exact surviving sequence, in start-time order. The two blocked
	// sessions started last, so they win the two places freed by dropping the
	// two oldest working sessions; the survivors then sort by START TIME, which
	// for these IDs is descending lexicographically.
	//
	// Asserted as a fixed sequence derived from known start times, NOT as a
	// comparison of adjacent IDs: the latter tests the fixture's naming, not
	// the ordering rule.
	want := []string{ids[2], ids[3], ids[4], ids[5], ids[6], ids[7], ids[8], ids[9]}
	got := make([]string, len(frame.Sessions))
	for i, s := range frame.Sessions {
		got[i] = s.ID
	}
	if len(got) != len(want) {
		t.Fatalf("frame sessions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame sessions = %v, want %v (start-time order)", got, want)
			break
		}
	}
}

// TestFrameUnderTheCapIsUnchanged guards against the cap disturbing the
// ordinary case, which is the one that runs on every desk.
func TestFrameUnderTheCapIsUnchanged(t *testing.T) {
	tracker := applyAll(t, testTimeout,
		report("s-1", "started"),
		report("s-2", "blocked"),
		report("s-3", "finished"),
	)

	frame := tracker.Frame()
	if len(frame.Sessions) != 3 {
		t.Fatalf("frame carries %d sessions, want 3", len(frame.Sessions))
	}
	want := []string{"s-1", "s-2", "s-3"}
	for i, id := range want {
		if frame.Sessions[i].ID != id {
			t.Errorf("session %d = %s, want %s", i, frame.Sessions[i].ID, id)
		}
	}
	if frame.Color != ColorRed {
		t.Errorf("Color = %v, want red", frame.Color)
	}
}

// RFC 1 section 7: a session silent for the timeout MUST be expired. Apply
// enforces that itself rather than waiting for a Sweep, because between two
// sweeps a dead session would otherwise hold the lamp red.
func TestApplyExpiresSilentSessionsWithoutASweep(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("dead", "blocked"), base)

	// Ten hours later a different session starts. No Sweep ran in between.
	tracker.Apply(report("live", "started"), base.Add(10*time.Hour))

	if got := tracker.Aggregate(); got != ColorYellow {
		t.Errorf("Aggregate() = %v, want yellow: the blocked session went "+
			"silent for ten hours and must not hold the lamp red", got)
	}
	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1: the silent session must be "+
			"expired on the Apply path", len(sessions))
	}
	if sessions[0].ID != "live" {
		t.Errorf("surviving session = %q, want live", sessions[0].ID)
	}
}

// The silence check on Apply must not expire the session doing the reporting,
// however long it was quiet: the report it just sent IS the keepalive.
func TestApplyDoesNotExpireTheReportingSession(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "blocked"), base)

	tracker.Apply(report("a1", "blocked"), base.Add(10*time.Hour))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1: the report is a keepalive", len(sessions))
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// A non-positive timeout disables expiry by silence, on Apply as on Sweep.
func TestApplyDoesNotExpireWhenTimeoutDisabled(t *testing.T) {
	tracker := NewTracker(0)
	tracker.Apply(report("dead", "blocked"), base)
	tracker.Apply(report("live", "started"), base.Add(10*time.Hour))

	if got := len(tracker.Sessions()); got != 2 {
		t.Errorf("len(Sessions()) = %d, want 2 with expiry disabled", got)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red with expiry disabled", got)
	}
}

// RFC 1 Appendix A caps the field lengths. Oversized fields are TRUNCATED, not
// rejected: section 11 requires leniency. The cap protects the firmware's
// fixed line buffer, which MaxFrameSessions alone cannot do if one field is
// five thousand characters long.
func TestApplyTruncatesOversizedFields(t *testing.T) {
	tracker := NewTracker(testTimeout)
	longID := strings.Repeat("i", 5000)
	longLabel := strings.Repeat("l", 500)
	longProvider := strings.Repeat("p", 200)

	changed := tracker.Apply(Report{
		SessionID: longID,
		Event:     "blocked",
		Label:     longLabel,
		Provider:  longProvider,
	}, base)
	if !changed {
		t.Fatal("Apply() = false, want true: an oversized report is truncated, not rejected")
	}

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if got := len(sessions[0].ID); got != MaxSessionIDLen {
		t.Errorf("len(ID) = %d, want %d", got, MaxSessionIDLen)
	}
	if got := len(sessions[0].Label); got != MaxLabelLen {
		t.Errorf("len(Label) = %d, want %d", got, MaxLabelLen)
	}
	if got := len(sessions[0].Provider); got != MaxProviderLen {
		t.Errorf("len(Provider) = %d, want %d", got, MaxProviderLen)
	}

	// The WIRE form is capped more tightly than storage: a frame field is
	// bounded by the firmware's per-entry budget, not the relay's storage cap,
	// so eight entries cannot overflow SL_LINE_MAX. See MaxFrameIDLen /
	// MaxFrameLabelLen.
	frame := tracker.Frame()
	if got := len(frame.Sessions[0].ID); got != MaxFrameIDLen {
		t.Errorf("frame ID length = %d, want %d: the wire form uses the firmware cap",
			got, MaxFrameIDLen)
	}
	if got := len(frame.Sessions[0].Label); got != MaxFrameLabelLen {
		t.Errorf("frame label length = %d, want %d", got, MaxFrameLabelLen)
	}
	if frame.Color != ColorRed {
		t.Errorf("Color = %v, want red: truncation must not lose the light", frame.Color)
	}
}

// slLineMax mirrors SL_LINE_MAX in firmware/stoplight/protocol.h, the device's
// fixed line buffer. A frame whose encoded line exceeds it is dropped whole, so
// this is the budget every frame the relay emits must stay under.
const slLineMax = 1536

// A frame at the worst case, MaxFrameSessions entries each with an id and label
// at their storage caps, must fit the firmware line buffer. Before the wire
// caps this frame ran to ~3.5KB and overflowed SL_LINE_MAX, so the device
// discarded the session list and the screen went stale with no error either
// side. The per-field wire caps keep the composed frame under the budget.
func TestWorstCaseFrameFitsFirmwareLineBudget(t *testing.T) {
	tracker := NewTracker(testTimeout)
	for i := 0; i < MaxFrameSessions; i++ {
		tracker.Apply(Report{
			SessionID: strings.Repeat("i", MaxSessionIDLen-1) + strconv.Itoa(i),
			Event:     "blocked",
			Label:     strings.Repeat("L", MaxLabelLen),
		}, base.Add(time.Duration(i)*time.Second))
	}

	frame := tracker.Frame()
	if len(frame.Sessions) != MaxFrameSessions {
		t.Fatalf("len(Sessions) = %d, want %d", len(frame.Sessions), MaxFrameSessions)
	}

	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The transport appends one '\n' as the frame delimiter, so the wire line
	// is one byte longer than the marshalled object.
	wire := len(data) + 1
	if wire > slLineMax {
		t.Errorf("worst-case frame = %d bytes, exceeds SL_LINE_MAX %d", wire, slLineMax)
	}
}

// Truncation is keyed consistently: the truncated ID is the map key, so a
// second report from the same oversized ID updates the same session rather
// than making a new one every time.
func TestTruncatedSessionIDIsStable(t *testing.T) {
	tracker := NewTracker(testTimeout)
	longID := strings.Repeat("i", 5000)

	tracker.Apply(Report{SessionID: longID, Event: "started"}, base)
	tracker.Apply(Report{SessionID: longID, Event: "blocked"}, base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1: the truncated ID must be the key", len(sessions))
	}
	if sessions[0].State != StateBlocked {
		t.Errorf("State = %v, want blocked", sessions[0].State)
	}
}

// A field already inside its cap is untouched, including a multi-byte one.
// Truncation must cut on a rune boundary rather than splitting a character.
func TestApplyLeavesFieldsWithinTheCapAlone(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)
	if got := tracker.Sessions()[0].Label; got != "auth-api" {
		t.Errorf("Label = %q, want it untouched", got)
	}

	// 300 three-byte runes: over the 256-byte label cap, so it must be cut,
	// and cut without leaving half a rune behind.
	wide := strings.Repeat("あ", 300)
	tracker.Apply(Report{SessionID: "b2", Event: "started", Label: wide}, base.Add(time.Minute))
	label := tracker.Sessions()[0].Label
	if len(label) > MaxLabelLen {
		t.Errorf("len(Label) = %d, want at most %d", len(label), MaxLabelLen)
	}
	if !utf8.ValidString(label) {
		t.Errorf("Label = %q is not valid UTF-8: truncation split a rune", label)
	}
}

// The session map is capped. An unbounded map lets a loop of unique IDs grow
// it for the whole timeout, and the sort on every Apply makes that quadratic.
func TestTrackerCapsLiveSessions(t *testing.T) {
	tracker := NewTracker(testTimeout)
	for i := 0; i < MaxLiveSessions*3; i++ {
		tracker.Apply(Report{
			SessionID: "s-" + strconv.Itoa(i),
			Event:     "started",
		}, base.Add(time.Duration(i)*time.Second))
	}

	if got := len(tracker.Sessions()); got > MaxLiveSessions {
		t.Errorf("len(Sessions()) = %d, want at most %d", got, MaxLiveSessions)
	}
}

// Eviction must never drop a blocked session to make room for a green one.
// The red light is the whole point of the device.
func TestEvictionPrefersGreenOverBlocked(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// One blocked session, reported first and never heard from again, so it is
	// the least recently seen and would be the first victim of plain LRU.
	tracker.Apply(report("blocked-one", "blocked"), base)

	// Fill and overfill with green sessions.
	for i := 0; i < MaxLiveSessions*2; i++ {
		tracker.Apply(Report{
			SessionID: "green-" + strconv.Itoa(i),
			Event:     "finished",
		}, base.Add(time.Duration(i+1)*time.Second))
	}

	if got := len(tracker.Sessions()); got > MaxLiveSessions {
		t.Fatalf("len(Sessions()) = %d, want at most %d", got, MaxLiveSessions)
	}
	found := false
	for _, s := range tracker.Sessions() {
		if s.ID == "blocked-one" {
			found = true
		}
	}
	if !found {
		t.Error("the blocked session was evicted in preference to a green one; " +
			"the lamp must keep the session that needs a human")
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// RFC 1 section 9 derives the label from cwd. A first report without a cwd
// falls back to the session ID; a later report that supplies one must
// re-derive, or the fallback sticks forever.
func TestLaterCwdRederivesTheLabel(t *testing.T) {
	dir := t.TempDir()
	tracker := NewTracker(testTimeout)

	// No cwd, so the label falls back to the session ID.
	tracker.Apply(report("a1", "started"), base)
	if got := tracker.Sessions()[0].Label; got != "a1" {
		t.Fatalf("Label = %q, want the session ID as the fallback", got)
	}

	// A later report supplies the cwd. The label must be derived from it.
	tracker.Apply(Report{SessionID: "a1", Event: "started", Cwd: dir}, base.Add(time.Minute))
	if got := tracker.Sessions()[0].Label; got != filepath.Base(dir) {
		t.Errorf("Label = %q, want %q derived from the new cwd", got, filepath.Base(dir))
	}
}

// A label the producer sent is never overwritten by derivation. Only a derived
// label is re-derived.
func TestProducerLabelSurvivesALaterCwd(t *testing.T) {
	dir := t.TempDir()
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "auth-api"}, base)

	tracker.Apply(Report{SessionID: "a1", Event: "started", Cwd: dir}, base.Add(time.Minute))

	if got := tracker.Sessions()[0].Label; got != "auth-api" {
		t.Errorf("Label = %q, want the producer's label kept", got)
	}
}

// A later provider re-derives too, when there is no cwd and no sent label.
func TestLaterProviderRederivesTheLabel(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)
	if got := tracker.Sessions()[0].Label; got != "a1" {
		t.Fatalf("Label = %q, want the session ID as the fallback", got)
	}

	tracker.Apply(Report{SessionID: "a1", Event: "started", Provider: "claude-code"}, base.Add(time.Minute))

	if got := tracker.Sessions()[0].Label; got != "claude-code" {
		t.Errorf("Label = %q, want it re-derived from the provider", got)
	}
}

// A keepalive that moves nothing must not rebuild the frame. This is the hot
// path: RFC 1 section 7 asks producers to re-send their state every few
// minutes, and every one of those used to pay for two full sorts.
func TestKeepaliveReportsNoChange(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a1", "started"), base)

	for i := 1; i <= 5; i++ {
		if changed := tracker.Apply(report("a1", "started"), base.Add(time.Duration(i)*time.Minute)); changed {
			t.Errorf("keepalive %d: Apply() = true, want false", i)
		}
	}
	if got := tracker.Aggregate(); got != ColorYellow {
		t.Errorf("Aggregate() = %v, want yellow", got)
	}
	if got := len(tracker.Sessions()); got != 1 {
		t.Errorf("len(Sessions()) = %d, want 1", got)
	}
}

// The change-detection shortcut must still notice a label move, which changes
// the frame without changing any colour.
func TestLabelChangeStillReportsChange(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "one"}, base)

	if changed := tracker.Apply(Report{SessionID: "a1", Event: "started", Label: "two"}, base.Add(time.Minute)); !changed {
		t.Error("Apply() = false, want true: the frame label moved")
	}
	if got := tracker.Frame().Sessions[0].Label; got != "two" {
		t.Errorf("Frame label = %q, want two", got)
	}
}

// A large session set must stay responsive. This is the shape of the probe
// that did not finish: many unique IDs, each one an Apply.
func TestManySessionsStayResponsive(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	tracker := NewTracker(testTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50000; i++ {
			tracker.Apply(Report{
				SessionID: "s-" + strconv.Itoa(i),
				Event:     "started",
			}, base.Add(time.Duration(i)*time.Millisecond))
		}
	}()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("50,000 applies did not finish in 60s")
	}
	if got := len(tracker.Sessions()); got > MaxLiveSessions {
		t.Errorf("len(Sessions()) = %d, want at most %d", got, MaxLiveSessions)
	}
}
