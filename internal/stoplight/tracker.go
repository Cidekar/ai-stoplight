package stoplight

import (
	"sort"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxLiveSessions caps how many sessions the tracker will hold at once.
//
// Nothing in the protocol bounds the number of distinct session IDs a producer
// may report, and a session lives for the full timeout after its last event.
// A loop of unique IDs therefore grows the map without limit for 30 minutes.
//
// A few hundred is far past any real desk: the screen shows MaxFrameSessions
// at a time, and a human running more than a handful of agents at once is
// already past what the device can convey. The cap exists to bound work, not
// to express a policy about how many agents to run.
//
// When the cap is reached the least recently seen session is evicted, EXCEPT
// that a blocked session is never evicted while any non-blocked session could
// go instead. Dropping the session that needs a human to keep one that does
// not is exactly backwards.
const MaxLiveSessions = 256

// Tracker holds every live session and computes the aggregate. It is the only
// place session state lives, and it is safe for concurrent use.
type Tracker struct {
	mu             sync.RWMutex
	sessions       map[string]*Session
	sessionTimeout time.Duration
	last           Frame // the frame last reported as current, to detect change

	// parked holds labels named by `stoplight task` for sessions that have not
	// reported yet. See SetOverride: a label carries no state, so it must not
	// create a session, but it must not be thrown away either.
	parked map[string]parkedOverride

	// dirty marks the session set as possibly moved since `last` was computed.
	// A keepalive that changes nothing leaves it false and skips the two sorts
	// that building a frame costs. See commitLocked.
	dirty bool
}

// parkedOverride is a label waiting for its session to start.
type parkedOverride struct {
	label string
	setAt time.Time // parks expire on the same silence rule as sessions
}

// NewTracker returns a tracker that expires sessions after sessionTimeout of
// silence. A non-positive timeout disables expiry by silence, leaving `ended`
// and an explicit Sweep as the only ways out.
func NewTracker(sessionTimeout time.Duration) *Tracker {
	return &Tracker{
		sessions:       make(map[string]*Session),
		parked:         make(map[string]parkedOverride),
		sessionTimeout: sessionTimeout,
		last:           Frame{Color: ColorOff, Sessions: []FrameSession{}},
	}
}

// Apply feeds in one report. Returns true if the resulting Frame differs from
// the last one, which is the signal to transmit.
//
// An unknown event name is ignored rather than rejected, and an unknown
// session ID is created rather than rejected: losing a red light is worse than
// tracking a session whose start was missed. The new session starts in the
// state the event implies, per InitialState. An `ended` for an unknown session
// creates nothing.
//
// Fields over the RFC 1 Appendix A caps are truncated, never rejected.
//
// Apply also enforces the silence timeout before it computes the frame. RFC 1
// section 7 makes expiry a MUST, and leaving it to Sweep alone meant a dead
// session held the lamp red for the whole gap between two sweeps. A relay whose
// ticker is slow, stalled or absent must still not show a stale red.
func (t *Tracker) Apply(r Report, now time.Time) (changed bool) {
	// Truncate before anything reads these, so the truncated ID is the map key
	// and a repeat report from the same oversized ID finds the same session.
	if truncate(r.SessionID, MaxSessionIDLen) == "" {
		return false
	}
	if _, ok := ParseEvent(r.Event); !ok {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Expire the silent before this report is folded in. The reporting session
	// is exempt: the report it just sent is its keepalive, and its LastSeen is
	// set below.
	t.expireSilentLocked(now, truncate(r.SessionID, MaxSessionIDLen))

	if !t.applyLocked(r, now, time.Time{}) {
		return t.commitLocked()
	}

	t.dropExpiredLocked()
	return t.commitLocked()
}

// applyLocked folds one report into the session set. It reports whether the
// session exists afterwards, which is false for an `ended` naming a session
// the relay never tracked.
//
// observedAt is when the producer read this state, or the zero time when it
// did not say. Split out of Apply so that a full-state sync folds each of its
// entries through exactly the same label derivation, truncation, parked-label
// and transition logic as a single report: two code paths that agreed only by
// inspection would drift, and the sync path is the one nobody watches.
//
// The caller holds the lock, has already expired the silent, and calls
// commitLocked afterwards.
func (t *Tracker) applyLocked(r Report, now, observedAt time.Time) (exists bool) {
	event, ok := ParseEvent(r.Event)
	if !ok {
		return false
	}

	id := truncate(r.SessionID, MaxSessionIDLen)
	if id == "" {
		return false
	}

	session, found := t.sessions[id]
	if !found {
		// A producer that restarts may report an event for a session we never
		// saw. Create it directly in the state the event implies, so that a
		// `blocked` we did not see start still shows red.
		initial, create := InitialState(event)
		if !create {
			// `ended` for a session we never tracked. Nothing to expire.
			return false
		}
		session = &Session{
			ID:      id,
			State:   initial,
			Started: now,
		}
		// This is the session genuinely coming into existence, which is the
		// moment a parked label is for. Consume it: a park applies once.
		if park, ok := t.parked[id]; ok {
			delete(t.parked, id)
			if t.sessionTimeout <= 0 || now.Sub(park.setAt) < t.sessionTimeout {
				session.Override = park.label
			}
		}
		t.sessions[id] = session
		t.dirty = true
		t.evictLocked()
	}

	session.LastSeen = now

	// Derivation inputs. Track whether the label was sent by the producer, so
	// that a derived label can be recomputed later and a sent one never is.
	derivedInputsMoved := false
	if provider := truncate(r.Provider, MaxProviderLen); provider != "" && provider != session.Provider {
		session.Provider = provider
		derivedInputsMoved = true
	}
	if cwd := truncate(r.Cwd, MaxCwdLen); cwd != "" && cwd != session.Dir {
		session.Dir = cwd
		derivedInputsMoved = true
	}

	switch label := truncate(r.Label, MaxLabelLen); {
	case label != "":
		// The producer named it. A sent label always wins and is never
		// recomputed from cwd afterwards.
		if label != session.Label {
			session.Label = label
			t.dirty = true
		}
		session.labelSent = true
	case session.Label == "" || (!session.labelSent && derivedInputsMoved):
		// Either nothing has named it yet, or the inputs derivation reads have
		// moved. RFC 1 section 9 derives from cwd, so a first report with no
		// cwd falling back to the session ID must not freeze that fallback in
		// place when a later report finally supplies one.
		if next := DeriveLabel(session.Dir, session.Provider, session.ID); next != session.Label {
			session.Label = next
			t.dirty = true
		}
	}

	if next, moved := session.State.Next(event); moved {
		session.State = next
		t.dirty = true
	}

	// Record the observation last, so an entry that was folded in is the one
	// whose timestamp is kept. A zero observedAt leaves any existing stamp
	// alone: a hook that says nothing about when it read the world must not
	// erase a poll's claim to have read it later.
	if !observedAt.IsZero() && observedAt.After(session.observedAt) {
		session.observedAt = observedAt
	}

	return true
}

// Sync is a producer's complete set of live sessions, per RFC 1 section 5.4.
type Sync struct {
	Provider   string    `json:"provider"`
	ObservedAt time.Time `json:"observed_at"`
	Sessions   []Report  `json:"sessions"`
}

// Reconcile applies a producer's complete set of live sessions, per RFC 1
// section 5.4. Returns true if the resulting Frame differs from the last one.
//
// This is the only entry point where ABSENCE means something. Apply cannot
// express "and nothing else": a producer reporting one session at a time has
// no way to say a session it never mentioned has gone, so a session that ended
// without a final event stays lit forever. A declared whole makes the gap
// expressible, and closing it is the entire reason this method exists.
//
// # Authority is scoped to one provider
//
// A sync speaks only for the producer that sent it. Sessions belonging to any
// other provider are left untouched, because two producers polling on their
// own schedules would otherwise delete each other's work on every tick: one
// declares its two sessions, the relay drops the other's three, and the next
// tick reverses it. The lamp would oscillate for as long as both ran.
//
// A session the relay holds with NO provider is also left alone. It arrived
// before any producer identified itself, so no sync can honestly claim it.
//
// # Ordering is by observation, not arrival
//
// Each entry is ignored if the session already holds a strictly newer
// observation. A full-state poll is slower to produce and slower to send than
// a single hook, so a sync can land after a delta that already superseded it.
// Applying it anyway would reverse the fresher reading, which in the case that
// matters means a session that just went red going back to green.
//
// The same test governs the removals: a session observed more recently than
// this sync is not removed by it. Otherwise a session that started after the
// poll was taken would be deleted for the crime of not existing yet.
func (t *Tracker) Reconcile(sync Sync, now time.Time) (changed bool) {
	provider := truncate(sync.Provider, MaxProviderLen)
	if provider == "" {
		return false
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.expireSilentLocked(now, "")

	// Fold in everything declared. Reuse the per-report path so that label
	// derivation, truncation, parked labels and state transitions behave
	// identically however a report arrived.
	declared := make(map[string]bool, len(sync.Sessions))
	for _, entry := range sync.Sessions {
		id := truncate(entry.SessionID, MaxSessionIDLen)
		if id == "" {
			continue
		}
		declared[id] = true

		if existing, found := t.sessions[id]; found && sync.ObservedAt.Before(existing.orderingTime()) {
			// A newer observation already landed for this session. Keep it,
			// but still count the session as declared so it survives the
			// removal pass below. A hook-created session has no observedAt, so
			// its arrival time (LastSeen) stands in: without it a stale sync
			// would reverse the fresher hook reading.
			continue
		}
		entry.Provider = provider
		t.applyLocked(entry, now, sync.ObservedAt)
	}

	// Remove what this provider no longer claims. RFC 1 section 5.4: absent
	// means ended.
	for id, session := range t.sessions {
		switch {
		case declared[id]:
		case session.Provider != "" && session.Provider != provider:
			// Another producer's session. Only a named, DIFFERENT provider is
			// protected: a sync speaks for itself and must not delete work it
			// knows nothing about.
			//
			// An UNATTRIBUTED session is not protected, and used to be. The
			// reasoning was that a session which never named a producer could
			// not honestly be claimed by one, which sounds careful and leaks
			// without bound: the Claude Code hooks did not send a provider, so
			// every session they created was unattributable and no sync could
			// ever reap it. Pre-warmed workers that fire one event and are
			// never dispatched accumulated forever, and one of them holding a
			// red state kept the lamp red while every real session was fine.
			//
			// An unattributed session has no other claimant by definition, so
			// the producer that is syncing is the best claim available. Being
			// wrong costs one session removed early, and it will be recreated
			// by its next report. Being wrong the other way costs a lamp that
			// is permanently, unfixably red.
		case session.orderingTime().After(sync.ObservedAt):
			// Observed more recently than this sync was taken, so this sync
			// cannot speak to whether it exists. A hook-created session has no
			// observedAt, so its arrival time (LastSeen) stands in: without it
			// a stale sync would delete a session the hook just created.
		default:
			delete(t.sessions, id)
			t.dirty = true
		}
	}

	t.dropExpiredLocked()
	return t.commitLocked()
}

// SetOverride sets an explicit label that wins over the derived label.
// An empty label clears the override. Returns true if the resulting
// Frame differs from the last one, which is the signal to transmit.
//
// The override touches the screen only. State is untouched, so the aggregate
// colour cannot move: naming a task must never change what the lamp says.
//
// # An unknown session is NOT created
//
// This is the whole point of the method's contract, and creating one broke it.
// A session created here had to start in some state, and the only honest
// choice for "a human typed a name" was Idle, which is GREEN. So naming a task
// against a relay with no live sessions turned the lamp from off to green and
// held it there for the full timeout: an empty desk reporting finished work.
// That contradicts RFC 1 section 8, which requires no live sessions to mean
// lamps off, and it contradicts the promise three paragraphs above that the
// aggregate cannot move.
//
// Apply creates unknown sessions for a good reason that does not apply here.
// An event carries STATE: a `blocked` from a producer whose start was missed
// is a red light that genuinely exists and would otherwise be lost. A label
// carries no state at all. There is no light to lose, so there is nothing to
// weigh against inventing one.
//
// The name is not discarded either. It is PARKED against the session ID and
// consumed by Apply when that session first genuinely starts, which preserves
// naming a task before it starts. That is what the CLI actually needs:
// `stoplight task` is a human typing a name, and cmd.go already routes it away
// from ingest precisely so that it carries no event and runs no transition.
// A park expires on the same silence rule as a session, so a name for work
// that never arrives does not sit in memory forever.
func (t *Tracker) SetOverride(sessionID, label string, now time.Time) (changed bool) {
	id := truncate(sessionID, MaxSessionIDLen)
	if id == "" {
		return false
	}
	label = truncate(label, MaxLabelLen)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.expireSilentLocked(now, "")

	session, found := t.sessions[id]
	if !found {
		// Park it. An empty label parks nothing and clears any existing park,
		// matching the "empty clears" rule for a live session.
		if label == "" {
			delete(t.parked, id)
		} else {
			t.parked[id] = parkedOverride{label: label, setAt: now}
		}
		// No session moved, so no frame moved. The lamp stays exactly as it was,
		// which for an empty desk means off.
		return t.commitLocked()
	}

	session.LastSeen = now
	if session.Override != label {
		session.Override = label
		t.dirty = true
	}

	t.dropExpiredLocked()
	return t.commitLocked()
}

// Sweep expires sessions that have gone silent for longer than the timeout.
// Call it on a ticker: a crashed session never sends `ended`, and without this
// one dead session holds the lamp red forever.
//
// Apply runs the same check, so a stalled ticker no longer means a stale red.
// Sweep is still needed for the quiet case: with no reports arriving at all,
// nothing else would notice the silence.
func (t *Tracker) Sweep(now time.Time) (changed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.expireSilentLocked(now, "")
	t.dropExpiredLocked()
	return t.commitLocked()
}

// expireSilentLocked marks every session silent for longer than the timeout as
// expired. exempt names a session to skip, which is the one currently
// reporting: its report is its keepalive, so it must not be expired by the
// silence that preceded it.
//
// A non-positive timeout disables expiry by silence, leaving `ended` as the
// only way out. Parked overrides age out on the same rule, so a name for work
// that never starts does not accumulate.
func (t *Tracker) expireSilentLocked(now time.Time, exempt string) {
	if t.sessionTimeout <= 0 {
		return
	}
	for id, session := range t.sessions {
		if id == exempt {
			continue
		}
		if now.Sub(session.LastSeen) >= t.sessionTimeout {
			session.State = StateExpired
			t.dirty = true
		}
	}
	for id, park := range t.parked {
		if now.Sub(park.setAt) >= t.sessionTimeout {
			delete(t.parked, id)
		}
	}
}

// evictLocked brings the session count back inside MaxLiveSessions.
//
// The victim is the least recently seen session, with one exception that
// matters more than the rule: a blocked session is never evicted while a
// non-blocked one could go instead. The device exists to show that a human is
// needed, so dropping the session that needs one in order to keep a session
// that does not would defeat it. Only when EVERY session is blocked does the
// least recently seen blocked session go, and by then something is very wrong
// with the producers rather than with this cap.
func (t *Tracker) evictLocked() {
	for len(t.sessions) > MaxLiveSessions {
		var victimID string
		var victim *Session
		for id, session := range t.sessions {
			if victim == nil {
				victimID, victim = id, session
				continue
			}
			if betterVictim(session, victim) {
				victimID, victim = id, session
			}
		}
		if victim == nil {
			return
		}
		delete(t.sessions, victimID)
		t.dirty = true
	}
}

// betterVictim reports whether a should be evicted in preference to b. Not
// blocked beats blocked; among equals, least recently seen goes first, with ID
// breaking the tie so that eviction is deterministic rather than depending on
// map iteration order.
func betterVictim(a, b *Session) bool {
	aBlocked := a.State == StateBlocked
	bBlocked := b.State == StateBlocked
	if aBlocked != bBlocked {
		return bBlocked
	}
	if !a.LastSeen.Equal(b.LastSeen) {
		return a.LastSeen.Before(b.LastSeen)
	}
	return a.ID < b.ID
}

// truncate cuts s to at most max BYTES, on a rune boundary. RFC 1 Appendix A
// states the caps as JSON Schema maxLength, and the reason they exist here is
// the firmware's fixed line buffer, which counts bytes. Cutting mid-rune would
// put invalid UTF-8 on the wire, so the cut backs up to the last boundary.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Frame renders the current state for transmission.
func (t *Tracker) Frame() Frame {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.frameLocked()
}

// Aggregate is the most urgent colour across live sessions, ColorOff when
// there are none. An empty desk is not a finished task.
func (t *Tracker) Aggregate() Color {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.aggregateLocked()
}

// Sessions returns the live sessions ordered by start time, which fixes the
// rotation order on the device.
func (t *Tracker) Sessions() []Session {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.liveLocked()
}

// dropExpiredLocked removes expired sessions from the set. They contribute no
// colour and no screen entry, so nothing is gained by keeping them.
func (t *Tracker) dropExpiredLocked() {
	for id, session := range t.sessions {
		if !session.State.Live() {
			delete(t.sessions, id)
			t.dirty = true
		}
	}
}

// commitLocked records the current frame and reports whether it moved. Frames
// are only sent on change, so a quiet desk is a quiet radio.
//
// The dirty flag is the fast path. Building a frame costs two sorts of the
// whole session list, and RFC 1 section 7 asks every long-running agent to
// re-send its state every few minutes as a keepalive. Those keepalives are the
// common case and they conclude nothing changed, so paying for two sorts to
// find that out scaled badly: with a couple of thousand sessions it was most
// of a millisecond per report, under the write lock, to produce no output.
//
// Every write path sets dirty when it moves something a frame can show: state,
// the displayed label, or the session set itself. Nothing else can move a
// frame, so a clean tracker provably renders the frame it rendered last.
func (t *Tracker) commitLocked() bool {
	if !t.dirty {
		return false
	}
	t.dirty = false
	next := t.frameLocked()
	if next.Equal(t.last) {
		return false
	}
	t.last = next
	return true
}

// liveLocked returns the live sessions sorted by start time, breaking ties on
// ID so that the order is total: sessions created within the same clock tick
// must not rotate differently between frames.
func (t *Tracker) liveLocked() []Session {
	live := make([]Session, 0, len(t.sessions))
	for _, session := range t.sessions {
		if session.State.Live() {
			live = append(live, *session)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].Started.Equal(live[j].Started) {
			return live[i].ID < live[j].ID
		}
		return live[i].Started.Before(live[j].Started)
	})
	return live
}

// aggregateLocked takes the most urgent colour across live sessions. Provider
// is deliberately not consulted: one light, one aggregate.
func (t *Tracker) aggregateLocked() Color {
	aggregate := ColorOff
	for _, session := range t.sessions {
		if !session.State.Live() {
			continue
		}
		if c := session.State.Color(); c > aggregate {
			aggregate = c
		}
	}
	return aggregate
}

// MaxFrameSessions is the most session entries a frame may carry. It matches
// SL_MAX_SESSIONS in firmware/stoplight/protocol.h.
//
// The device shows one session at a time and rotates, so entries past the
// eighth were never reachable on screen: the firmware already discarded them
// while parsing. Capping here instead means the frame stays inside the
// firmware's line buffer, which is what actually matters. An uncapped list
// of many sessions produced a line long enough to be dropped whole, and
// because frames are sent only on change, every later frame was oversized
// too and the lamp held its last colour indefinitely.
//
// THE AGGREGATE IS NOT CAPPED. It is computed over every live session, so a
// ninth session going red still turns the lamp red even though it cannot be
// listed. The lamp must reflect every session even when the screen cannot
// name them all.
const MaxFrameSessions = 8

// frameLocked builds the frame for the current session set.
func (t *Tracker) frameLocked() Frame {
	live := t.liveLocked()

	// The aggregate comes from aggregateLocked, which walks the whole session
	// map. Truncating the list below therefore cannot move the lamp.
	frame := Frame{
		Color:    t.aggregateLocked(),
		Sessions: make([]FrameSession, 0, min(len(live), MaxFrameSessions)),
	}

	shown := live
	if len(shown) > MaxFrameSessions {
		// Over the cap, so something has to be dropped. Keep the most urgent
		// sessions: if only eight of nine can be named, the one that needs a
		// human is the one worth naming. Ties keep start-time order, so the
		// rotation stays stable frame to frame rather than reshuffling
		// whenever two sessions share a colour.
		byUrgency := make([]Session, len(live))
		copy(byUrgency, live)
		sort.SliceStable(byUrgency, func(i, j int) bool {
			return byUrgency[i].State.Color() > byUrgency[j].State.Color()
		})
		shown = byUrgency[:MaxFrameSessions]

		// Restore start-time order among the survivors. The device treats the
		// order it receives as the rotation order, and start-time order is
		// what gives a session a stable place in the cycle.
		sort.SliceStable(shown, func(i, j int) bool {
			if shown[i].Started.Equal(shown[j].Started) {
				return shown[i].ID < shown[j].ID
			}
			return shown[i].Started.Before(shown[j].Started)
		})
	}

	for i := range shown {
		frame.Sessions = append(frame.Sessions, FrameSession{
			ID:    shown[i].ID,
			Label: shown[i].Display(),
			State: shown[i].State.Label(),
			Color: shown[i].State.Color(),
		})
	}
	return frame
}
