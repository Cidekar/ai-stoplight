package stoplight

import (
	"testing"
	"time"
)

// syncOf builds a full-state sync for one provider, observed at base+offset.
func syncOf(provider string, offset time.Duration, entries ...Report) Sync {
	return Sync{
		Provider:   provider,
		ObservedAt: base.Add(offset),
		Sessions:   entries,
	}
}

// withProvider stamps a provider on a report, which ingest does from the body
// and a sync does from its envelope.
func withProvider(r Report, provider string) Report {
	r.Provider = provider
	return r
}

// The point of the endpoint: a session the sync does not mention is gone.
// Reporting one session at a time cannot express an absence, so before this
// existed a session that ended without a final event stayed lit forever.
func TestReconcileRemovesTheUndeclared(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(withProvider(report("a", "started"), "claude-code"), base)
	tracker.Apply(withProvider(report("b", "blocked"), "claude-code"), base)

	if got := len(tracker.Sessions()); got != 2 {
		t.Fatalf("len(Sessions()) = %d, want 2", got)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Fatalf("Aggregate() = %v, want red", got)
	}

	// Only "a" is still live. "b" is absent, so it ended.
	tracker.Reconcile(syncOf("claude-code", time.Minute, report("a", "started")), base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if sessions[0].ID != "a" {
		t.Errorf("surviving session = %q, want a", sessions[0].ID)
	}
	if got := tracker.Aggregate(); got != ColorYellow {
		t.Errorf("Aggregate() = %v, want yellow: the red session was declared gone", got)
	}
}

// An empty list is a declaration, not a malformed request: it says this
// producer has nothing live, which is the normal state of a quiet desk.
func TestReconcileEmptyListClearsTheProvider(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(withProvider(report("a", "blocked"), "claude-code"), base)

	changed := tracker.Reconcile(syncOf("claude-code", time.Minute), base.Add(time.Minute))

	if !changed {
		t.Error("Reconcile() = false, want true: the frame lost a session")
	}
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off", got)
	}
}

// Authority is per provider. Two producers polling on their own schedules
// would otherwise delete each other's sessions on every tick, and the lamp
// would oscillate for as long as both ran.
func TestReconcileLeavesOtherProvidersAlone(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(withProvider(report("mine", "started"), "claude-code"), base)
	tracker.Apply(withProvider(report("theirs", "blocked"), "deepseek"), base)

	// claude-code declares it has nothing.
	tracker.Reconcile(syncOf("claude-code", time.Minute), base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if sessions[0].ID != "theirs" {
		t.Errorf("surviving session = %q, want theirs", sessions[0].ID)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: another provider's red must survive", got)
	}
}

// An unattributed session IS reaped by a sync, and protecting it was a real
// leak. The Claude Code hooks sent no provider, so every session they created
// was unattributable and no sync could ever remove it: pre-warmed workers that
// fire one event and are never dispatched piled up, and one of them holding a
// red state kept the lamp red while every real session was fine.
//
// An unattributed session has no other claimant by definition, so the syncing
// producer is the best claim available. Removing it early costs one session
// that its next report recreates. The other way costs an unfixable red.
func TestReconcileReapsUnattributedSessions(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("anon", "blocked"), base) // no provider

	if got := tracker.Aggregate(); got != ColorRed {
		t.Fatalf("Aggregate() = %v, want red", got)
	}

	tracker.Reconcile(syncOf("claude-code", time.Minute), base.Add(time.Minute))

	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0: an unattributed session has no other claimant", got)
	}
	if got := tracker.Aggregate(); got != ColorOff {
		t.Errorf("Aggregate() = %v, want off: the stuck red must clear", got)
	}
}

// But a session belonging to a NAMED, different provider is still protected.
// Two producers syncing on their own schedules must not delete each other.
func TestReconcileStillProtectsANamedOtherProvider(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(withProvider(report("theirs", "blocked"), "deepseek"), base)
	tracker.Apply(report("anon", "started"), base) // unattributed

	tracker.Reconcile(syncOf("claude-code", time.Minute), base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if sessions[0].ID != "theirs" {
		t.Errorf("surviving session = %q, want theirs", sessions[0].ID)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: another provider's red must survive", got)
	}
}

// The ordering rule. A poll is slower to produce and slower to send than a
// hook, so a sync can land after a delta that already superseded it. Applying
// it anyway turns a session that just went red back to green.
func TestReconcileDoesNotReverseANewerObservation(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// A sync taken at T+1 declares the session working.
	tracker.Reconcile(
		syncOf("claude-code", time.Minute, report("a", "started")),
		base.Add(time.Minute),
	)

	// A later sync, but taken EARLIER, still says working. It must not undo
	// anything: there is nothing newer to undo yet.
	tracker.Reconcile(
		syncOf("claude-code", 30*time.Second, report("a", "started")),
		base.Add(2*time.Minute),
	)

	// Now a fresh sync at T+3 says blocked.
	tracker.Reconcile(
		syncOf("claude-code", 3*time.Minute, report("a", "blocked")),
		base.Add(3*time.Minute),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Fatalf("Aggregate() = %v, want red", got)
	}

	// A stale sync taken at T+2 arrives late and claims working. The red
	// observation at T+3 is newer, so the lamp must stay red.
	tracker.Reconcile(
		syncOf("claude-code", 2*time.Minute, report("a", "started")),
		base.Add(4*time.Minute),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: a stale sync reversed a fresher reading", got)
	}
}

// The same test governs removals, or a session that started after the poll was
// taken would be deleted for not existing yet.
func TestReconcileDoesNotRemoveASessionNewerThanTheSync(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// A sync at T+1 establishes one session.
	tracker.Reconcile(
		syncOf("claude-code", time.Minute, report("old", "started")),
		base.Add(time.Minute),
	)

	// A session appears at T+5, reported by a sync of its own.
	tracker.Reconcile(
		syncOf("claude-code", 5*time.Minute, report("new", "blocked")),
		base.Add(5*time.Minute),
	)

	// A sync taken at T+3 arrives late. It knows about neither session as of
	// its own reading, but it cannot speak to "new", observed at T+5.
	tracker.Reconcile(
		syncOf("claude-code", 3*time.Minute),
		base.Add(6*time.Minute),
	)

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if sessions[0].ID != "new" {
		t.Errorf("surviving session = %q, want new: the sync removed a session it was too old to know about", sessions[0].ID)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// The case RFC 1 section 5.4 calls out and the ordering guards existed for, but
// did not cover: a hook establishes a session, then a poll taken BEFORE the
// hook fired arrives late and tries to reverse it. A report to /v1/session
// carries no observedAt, so the guard has to fall back to arrival time or the
// hook has no protection at all. Blocked landing by hook, then a stale poll
// calling it working, must leave the lamp red — a human is being waited on.
func TestReconcileDoesNotReverseANewerHook(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// A hook starts the session, then a later hook blocks it.
	tracker.Apply(withProvider(report("a", "started"), "claude-code"), base)
	tracker.Apply(withProvider(report("a", "blocked"), "claude-code"), base.Add(11*time.Second))

	// A poll taken at T+10s — before the block — arrives at T+11.5s and lists
	// the session as working. It is older than the hook, so it must not win.
	tracker.Reconcile(
		syncOf("claude-code", 10*time.Second, report("a", "started")),
		base.Add(11500*time.Millisecond),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: a stale poll reversed a hook-delivered red", got)
	}
}

// The removal side of the same gap: a hook creates a session AFTER the poll
// snapshot was taken, so the poll cannot know about it. Without an arrival-time
// fallback the new session has a zero observedAt, loses the ordering guard, and
// is deleted for not existing yet.
func TestReconcileDoesNotRemoveANewerHook(t *testing.T) {
	tracker := NewTracker(testTimeout)

	// A hook creates a blocked session at T+1s.
	tracker.Apply(withProvider(report("b", "blocked"), "claude-code"), base.Add(time.Second))

	// An empty poll taken at T — before the hook — arrives late. It cannot
	// speak to a session observed after its snapshot, so the session stays.
	tracker.Reconcile(syncOf("claude-code", 0), base.Add(1500*time.Millisecond))

	sessions := tracker.Sessions()
	if len(sessions) != 1 || sessions[0].ID != "b" {
		t.Fatalf("Sessions() = %v, want the hook session b to survive", sessions)
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red: a stale poll removed a newer hook session", got)
	}
}

// A sync creates what it declares, in the state each entry implies. This is
// how a relay that restarted recovers a session that is merely sitting idle
// and would otherwise be invisible until it next moved.
func TestReconcileCreatesDeclaredSessions(t *testing.T) {
	tracker := NewTracker(testTimeout)

	tracker.Reconcile(syncOf("claude-code", time.Minute,
		Report{SessionID: "a", Event: "started", Label: "auth-api"},
		Report{SessionID: "b", Event: "blocked", Label: "stoplight"},
		Report{SessionID: "c", Event: "finished", Label: "done-thing"},
	), base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 3 {
		t.Fatalf("len(Sessions()) = %d, want 3", len(sessions))
	}
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}

	states := map[string]State{}
	for _, s := range sessions {
		states[s.ID] = s.State
	}
	for id, want := range map[string]State{"a": StateWorking, "b": StateBlocked, "c": StateDone} {
		if states[id] != want {
			t.Errorf("session %s state = %v, want %v", id, states[id], want)
		}
	}
}

// A sync stamps the provider from its envelope, so entries need not repeat it
// and the removal pass can tell whose sessions are whose.
func TestReconcileStampsTheProviderFromTheEnvelope(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Reconcile(syncOf("claude-code", time.Minute, report("a", "started")), base.Add(time.Minute))

	sessions := tracker.Sessions()
	if len(sessions) != 1 {
		t.Fatalf("len(Sessions()) = %d, want 1", len(sessions))
	}
	if sessions[0].Provider != "claude-code" {
		t.Errorf("Provider = %q, want claude-code", sessions[0].Provider)
	}

	// And the stamp is what makes a second sync able to remove it.
	tracker.Reconcile(syncOf("claude-code", 2*time.Minute), base.Add(2*time.Minute))
	if got := len(tracker.Sessions()); got != 0 {
		t.Errorf("len(Sessions()) = %d, want 0", got)
	}
}

// A sync with no provider names nobody, so it cannot be authoritative for
// anybody. Silently reconciling it would remove every providerless session.
func TestReconcileRejectsAnEmptyProvider(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Apply(report("a", "blocked"), base)

	if changed := tracker.Reconcile(syncOf("", time.Minute), base.Add(time.Minute)); changed {
		t.Error("Reconcile() = true, want false")
	}
	if got := len(tracker.Sessions()); got != 1 {
		t.Errorf("len(Sessions()) = %d, want 1", got)
	}
}

// The invariant the device rests on, through the sync path: the aggregate is
// computed over every live session, so a sync that declares one red session
// among many greens still turns the lamp red.
func TestReconcileHoldsTheAggregateInvariant(t *testing.T) {
	tracker := NewTracker(testTimeout)

	entries := []Report{{SessionID: "r", Event: "blocked"}}
	for i := range 7 {
		entries = append(entries, Report{
			SessionID: "g" + string(rune('0'+i)),
			Event:     "finished",
		})
	}
	tracker.Reconcile(syncOf("claude-code", time.Minute, entries...), base.Add(time.Minute))

	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}

// A hook that says nothing about when it read the world must not erase a
// poll's claim to have read it later, or every delta would reopen the door to
// a stale sync.
func TestApplyDoesNotClearAnObservationStamp(t *testing.T) {
	tracker := NewTracker(testTimeout)
	tracker.Reconcile(
		syncOf("claude-code", 5*time.Minute, report("a", "blocked")),
		base.Add(5*time.Minute),
	)

	// A hook arrives with no observation time.
	tracker.Apply(withProvider(report("a", "blocked"), "claude-code"), base.Add(6*time.Minute))

	// A sync taken at T+2 is still too old to reverse the T+5 reading.
	tracker.Reconcile(
		syncOf("claude-code", 2*time.Minute, report("a", "started")),
		base.Add(7*time.Minute),
	)
	if got := tracker.Aggregate(); got != ColorRed {
		t.Errorf("Aggregate() = %v, want red", got)
	}
}
