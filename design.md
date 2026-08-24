# Design

How Stoplight is built. Four layers, each derived from the one above it: the state machine defines behaviour, the types encode it, the packages arrange it, and the wire protocol carries it.

The public contract is [RFC 1](rfc.md), which any agent can implement. This document is the implementation behind it. Where the two overlap, the RFC wins.

Read the [readme](readme.md) first for what the device does and why.

## Contents

- [State machine](#state-machine)
- [Go types](#go-types)
- [Package layout](#package-layout)
- [Wire protocol](#wire-protocol)
- [Invariants](#invariants)

---

## State machine

### Session states

A session is one unit of work a human is waiting on: a conversation with an agent, a build, a test run. Every session is in exactly one state.

| State | Colour | Meaning |
|---|---|---|
| `Idle` | green | Started, nothing happening |
| `Working` | yellow | The producer is doing work on its own |
| `Blocked` | red | The producer needs a human before it can continue |
| `Done` | green | Finished the last turn |
| `Expired` | — | Went quiet, dropped from the set |

`Idle` and `Done` are both green but stay distinct, because the screen shows different text and only `Done` implies work completed.

### Events

Five arrive from producers, as defined in [RFC 1 §3](rfc.md#3-events). One is internal.

| Event | Source |
|---|---|
| `idle` | producer |
| `started` | producer |
| `blocked` | producer |
| `finished` | producer |
| `ended` | producer |
| `Timeout` | relay, after silence |

These names are deliberately semantic rather than borrowed from any agent's vocabulary. Claude Code's hook names are translated at the adapter boundary and appear nowhere below it.

### Transitions

```
                      idle
                       │
                       ▼
                   ┌───────┐
                   │ Idle  │◀────────────┐
                   └───┬───┘             │
                       │ started         │
                       ▼                 │
                   ┌─────────┐           │
             ┌────▶│ Working │────┐      │ idle
             │     └────┬────┘    │      │ (restart)
             │          │         │      │
             │ started  │ blocked │ finished
             │          ▼         ▼      │
             │     ┌─────────┐ ┌──────┐  │
             └─────┤ Blocked │ │ Done ├──┘
                   └────┬────┘ └───┬──┘
                        │          │
               Timeout  │          │ Timeout
               or ended ▼          ▼ or ended
                   ┌──────────────────┐
                   │     Expired      │
                   └──────────────────┘
                     terminal, and
                     dropped from set

    Four edges are left out to keep the diagram readable. Idle
    takes blocked to Blocked and finished to Done. Blocked takes
    finished to Done, and Done takes blocked to Blocked. The
    table below is complete; the diagram is not.

    Expired has no edge out. The `idle` restart applies from any
    LIVE state, not from Expired: a session that expired is
    deleted, and a later report for that ID starts a new one.
```

The two edges between `Blocked` and `Done` are the ones most easily missed, and both are ordinary. `finished` goes from `Blocked` to `Done`: the human answered and the agent completed the turn. `blocked` goes from `Done` to `Blocked`: a finished session began a new turn and hit a prompt inside it.

Written out, because the diagram compresses:

| From | Event | To |
|---|---|---|
| `Idle` | `started` | `Working` |
| `Idle` | `blocked` | `Blocked` |
| `Idle` | `finished` | `Done` |
| `Working` | `blocked` | `Blocked` |
| `Working` | `finished` | `Done` |
| `Blocked` | `started` | `Working` |
| `Blocked` | `finished` | `Done` |
| `Done` | `started` | `Working` |
| `Done` | `blocked` | `Blocked` |
| any live | `Timeout` | `Expired` |
| any live | `ended` | `Expired` |
| any live | `idle` | `Idle` (restart) |
| `Expired` | anything | `Expired` (terminal) |

`started`, `blocked` and `finished` all apply from every live state. The event says what the agent is doing now, and that is true whatever it was doing before. Restricting any of them by source state only drops a light the producer asked for.

The remaining no-ops are the pairs where the session is already in the state the event names, and every pair out of `Expired`. Repeating an event is a keepalive: a `finished` while already `Done` changes nothing, and that is normal rather than an error.

`Expired` is terminal, matching [RFC 1 §7](rfc.md#7-session-lifecycle), where the diagram ends there with no arrow out. Nothing revives an expired session in place. `idle` used to, on the reasoning that a restarting producer should get its session back, but that row was unreachable: the tracker DELETES an expired session the moment it is marked, so no event ever arrives at one. A restarting producer already takes the path the RFC intends. Its session is gone, and its next report creates a fresh one through `InitialState` with a new `Started`, which puts it at the end of the rotation where a just-started session belongs. Reviving in place would have kept the original `Started` and dropped a restarted session into the middle of the cycle.

`blocked` and `finished` apply from `Idle` as well as from `Working`. An agent can go from sitting idle straight to needing a human, with no turn in between: a scheduled job that wakes and immediately asks for a password, or a producer whose `started` was dropped. Requiring an intervening `started` would swallow the red light in exactly the case the light exists for.

`finished` applies from `Blocked` for the same reason, and it is the everyday path rather than an edge case. A permission prompt ends when the human answers and the agent completes the turn: the Claude Code adapter sends `blocked` from its `Notification` hook and `finished` from `Stop`, with no `started` in between. Dropping that edge leaves the lamp red after every answered prompt, which breaks the [RFC 1 §8](rfc.md#8-aggregation) guarantee that a glance is trustworthy.

`blocked` applies from `Done` because a finished session that begins a new turn can block inside it. [RFC 1 §8](rfc.md#8-aggregation) requires the lamp to be red whenever any session is blocked, so this must not be swallowed either.

This table is mirrored in Go as `rfcSection7Table` in `internal/stoplight/color_test.go`, which asserts the code obeys every row and that the rows cover every state and event pair. **The two must change together.**

#### Sessions the relay has never seen

A producer that restarts mid-session, or a relay that restarts under a running producer, sends an event for a session ID nobody is tracking. The relay creates the session rather than rejecting the event, because losing a red light is worse than tracking a session whose start was missed.

The created session takes the state the event **itself** implies. It is not created as `Idle` and then transitioned, because `Idle` is green: a `blocked` for an unseen session would show a green lamp while a human is being waited on.

| Event for an unseen session | Resulting state | Lamp |
|---|---|---|
| `idle` | `Idle` | green |
| `started` | `Working` | yellow |
| `blocked` | `Blocked` | red |
| `finished` | `Done` | green |
| `ended` | not created | off |
| `Timeout` | not created | off |

`ended` and `Timeout` only end a session. There is nothing to expire for a session that was never tracked, so neither creates one and neither moves the frame.

This is `InitialState` in `color.go`. It is a separate function from `Next`, because "which state does this event start a session in" and "which state does this event move a session to" are different questions that happen to agree on four events out of six.

#### A label is not an event

The rule above is about EVENTS, and it does not extend to `stoplight task`. An event carries state: a `blocked` from a producer whose start was missed is a red light that genuinely exists, and creating the session is how the relay avoids losing it. A label carries no state at all.

So `SetOverride` on a session the relay has never seen creates **nothing**. Creating one meant choosing a state to create it in, and the only honest choice for "a human typed a name" was `Idle`, which is green. Naming a task against a relay with no live sessions therefore turned the lamp from off to green and held it there for the full timeout: an empty desk reporting finished work, against the [§8](rfc.md#8-aggregation) rule two sections below and against `SetOverride`'s own promise that naming a task cannot move the lamp. There is no light to lose here, so there is nothing to weigh against inventing one.

The name is not thrown away either. It is **parked** against the session ID and consumed by `Apply` the first time that session genuinely reports, which is what makes naming a task before it starts still work. A parked name applies once, and ages out on the same 30-minute silence rule as a session, so a name for work that never arrives does not sit in memory.

#### Field caps

[RFC 1 Appendix A](rfc.md#appendix-a-json-schema) caps `session_id` at 128 bytes, `label` at 256, `provider` at 64 and `cwd` at 4096. `Apply` **truncates** to these before storing. It does not reject: [§11](rfc.md#11-compatibility) requires the relay to be lenient, so an oversized field must not cost a producer its light.

Truncation happens before the ID is used as a map key, so repeat reports from the same oversized ID find the same session rather than making a new one each time. The cut lands on a rune boundary: the caps are in bytes, because the reason they exist is the firmware's fixed line buffer, but cutting mid-rune would put invalid UTF-8 on the wire.

These caps and `MaxFrameSessions` solve two halves of one problem and neither is sufficient alone. `MaxFrameSessions` bounds how MANY sessions a frame carries; these bound how LARGE each one is. A single 5,000-character `session_id` produced an oversized line with only one session in it, and because frames are sent only on change, one oversized frame is not one lost update: every later frame is oversized too, and the lamp holds its last colour indefinitely.

#### The session cap

Nothing in the protocol bounds how many distinct session IDs a producer may report, and a session lives for the full timeout after its last event. A loop of unique IDs therefore grew the map without limit for 30 minutes, and because every `Apply` sorted the whole list, the cost was quadratic: a 50,000-session probe did not finish in two minutes.

`MaxLiveSessions` caps the map at 256, far past any real desk. On overflow the least recently seen session is evicted, with one exception: **a blocked session is never evicted while a non-blocked one could go instead**. Dropping the session that needs a human in order to keep one that does not is exactly backwards for a device whose purpose is to show that a human is needed. Only when every session is blocked does the least recently seen blocked session go.

#### Change detection

Building a frame costs two sorts of the session list, under the write lock. [RFC 1 §7](rfc.md#7-session-lifecycle) asks every long-running agent to re-send its state every few minutes as a keepalive, so the common report is one that changes nothing and those sorts ran only to conclude so.

The tracker therefore keeps a dirty flag. Every write path sets it when it moves something a frame can show: a session's state, its displayed label, or the session set itself. Nothing else can move a frame, so a clean tracker provably renders the frame it rendered last and `commitLocked` returns false without building anything.

### Timeouts

Two, and they exist for different reasons.

**Session timeout, 30 minutes of silence.** A session that crashes never sends `Stop`, and without this one dead session holds the lamp red forever. Any event resets the countdown.

The check runs on **both** paths, `Sweep` and `Apply`, and this matters. [RFC 1 §7](rfc.md#7-session-lifecycle) makes expiry a MUST, and leaving it to `Sweep` alone made that MUST conditional on a ticker: between two sweeps a dead session still held the lamp red, so a slow, stalled or absent ticker meant a stale red for as long as the gap lasted. `Apply` therefore expires the silent before it folds in the report it was given. The reporting session is exempt from its own check, because the report it just sent IS its keepalive. `Sweep` is still needed for the quiet case, where no reports arrive at all and nothing else would notice the silence.

**Pin timeout, 15 minutes.** A pin stops the screen rotating. Without expiry you pin something, forget, and stop seeing every other session. A pin also clears the moment its session expires.

### Aggregation

The lamp shows the most urgent state across every live session.

```
Blocked  >  Working  >  Idle | Done
  red        yellow         green
```

No live sessions at all means lamps off, not green. An empty desk is not a finished task.

---

## Go types

```go
package stoplight

// Color is what a lamp shows. Ordered by urgency, so the zero value is
// the least urgent and Max() over a set gives the aggregate.
type Color uint8

const (
    ColorOff Color = iota
    ColorGreen
    ColorYellow
    ColorRed
)

// State is where a session is in its lifecycle.
type State uint8

const (
    StateIdle State = iota
    StateWorking
    StateBlocked
    StateDone
    StateExpired
)

// Color maps a state onto a lamp colour.
func (s State) Color() Color

// Label is the short word shown under the session name.
func (s State) Label() string   // "idle" "working" "needs you" "done"

// Event is something a producer reported, or a timeout the relay raised.
// Wire names are the lowercase forms in RFC 1: idle, started, blocked,
// finished, ended.
type Event uint8

const (
    EventIdle Event = iota
    EventStarted
    EventBlocked
    EventFinished
    EventEnded
    EventTimeout   // internal, never arrives over the wire
)

// ParseEvent maps a wire name onto an Event. Unknown names return
// ok == false and MUST be ignored rather than rejected, so a producer
// written against a later spec does not break an older relay.
func ParseEvent(s string) (Event, bool)

func (e Event) String() string

// Next returns the state this event moves to, and whether it changed
// anything. A false second return means the event was a no-op.
func (s State) Next(e Event) (State, bool)

// InitialState returns the state a session takes when the relay sees it
// for the first time, and whether the session should be created at all.
// A fresh session is created directly in the state the event implies,
// never as Idle-then-transitioned. ok is false for ended and Timeout,
// which must not bring a session into existence.
func InitialState(e Event) (State, bool)
```

### Session

```go
// Session is one unit of work the relay is tracking. It may come from
// any producer: an LLM CLI, a build, a script.
type Session struct {
    ID       string    // opaque, stable for the life of the session
    Label    string    // derived from cwd, or sent by the producer
    Override string    // set by `stoplight task`, wins over Label
    Provider string    // free text: claude-code, deepseek, ci, ...
    State    State
    Started  time.Time // fixes the rotation order
    LastSeen time.Time // drives the timeout
    Dir      string    // working directory, for label derivation

    // labelSent records that Label came from the producer rather
    // than from DeriveLabel. Unexported: tracker bookkeeping.
    labelSent bool
}

// Display is the label actually shown, override first.
func (s *Session) Display() string
```

A **derived** label is recomputed whenever the inputs derivation reads move, meaning `Dir` or `Provider`. A **sent** label never is: the producer said what it wanted the work called, and that wins permanently.

The distinction is what `labelSent` is for, and without it the fallback stuck. [RFC 1 §9](rfc.md#9-labels) derives a label from the git branch of `cwd`, but a producer's first report often carries no `cwd`, so derivation fell all the way through to the session ID. Once `Label` was non-empty nothing recomputed it, so a later report that finally supplied `cwd` updated `Dir` and left the screen showing an opaque session ID for the life of the session.

### Report

What a producer sends to the relay. The wire form is specified in
[RFC 1 §5](rfc.md#5-message-format); this is its Go shape.

```go
// Report is one event from one producer. Identical over HTTP and the
// unix socket.
type Report struct {
    SessionID string `json:"session_id"`
    Event     string `json:"event"`              // parsed via ParseEvent
    Label     string `json:"label,omitempty"`
    Provider  string `json:"provider,omitempty"`
    Detail    string `json:"detail,omitempty"`   // reserved
    Cwd       string `json:"cwd,omitempty"`
}

// Validate checks the required fields. Unknown event names are not an
// error here: the caller ignores them per RFC 1 §11.
func (r *Report) Validate() error
```

### Frame

What the relay sends to the light. One per change, carrying the whole set.

```go
// Frame is the complete display state at a moment.
type Frame struct {
    Color    Color          `json:"color"`   // aggregate, drives the lamps
    Sessions []FrameSession `json:"sessions"`
}

// FrameSession is one entry in the rotation.
type FrameSession struct {
    ID    string `json:"id"`
    Label string `json:"label"`
    State string `json:"state"`
    Color Color  `json:"color"`
}
```

### Tracker

The state machine, and the only place session state lives.

```go
// Tracker holds every live session and computes the aggregate.
// Safe for concurrent use.
type Tracker struct{ /* ... */ }

func NewTracker(sessionTimeout time.Duration) *Tracker

// Apply feeds in a hook notification. Returns true if the resulting
// Frame differs from the last one, which is the signal to transmit.
// Truncates oversized fields, and expires silent sessions first.
func (t *Tracker) Apply(r Report, now time.Time) (changed bool)

// SetOverride names a session. An empty label clears the override.
// An unknown session is NOT created: the name is parked and applied
// when that session first reports. Never moves the aggregate.
func (t *Tracker) SetOverride(sessionID, label string, now time.Time) (changed bool)

// Sweep expires silent sessions. Call on a ticker. Returns true if
// anything expired. Apply runs the same check, so a stalled ticker
// no longer means a stale red.
func (t *Tracker) Sweep(now time.Time) (changed bool)

// Frame renders the current state for transmission.
func (t *Tracker) Frame() Frame

// Aggregate is the most urgent colour across live sessions,
// ColorOff when there are none.
func (t *Tracker) Aggregate() Color

// Sessions returns live sessions ordered by start time.
func (t *Tracker) Sessions() []Session
```

### Transport

The seam that makes hardware optional.

```go
// Transport carries frames to a light. Serial and the virtual light
// implement it today, and BLE will, so nothing above this cares which
// is in use.
type Transport interface {
    // Connect blocks until connected or ctx is cancelled. It retries
    // internally; a returned error means give up, not try again.
    Connect(ctx context.Context) error

    // Send transmits one frame. An error means this frame was lost,
    // not that the transport is dead.
    Send(f Frame) error

    // Connected reports whether a light is currently reachable.
    Connected() bool

    // Close releases the port or link.
    Close() error

    // Name identifies the transport in logs and `stoplight status`.
    Name() string
}
```

Two implementations exist, and a third is designed:

| Type | Package | Status | Notes |
|---|---|---|---|
| `*serial.Transport` | `internal/transport/serial` | **Shipping** | USB serial. `serial.Discover` picks a device, `--serial` overrides it. |
| `*virtual.Transport` | `internal/light` | **Shipping** | Renders frames to the terminal. The fallback when no light is found. |
| `*ble.Transport` | `internal/transport/ble` | **Shipping** | Bluetooth Low Energy. Scans by service UUID, `--ble` and `--ble-name` select it. |

All three arrived without changing anything above the interface, which is what it was for. BLE was added last and `selectTransport` in `cmd.go` grew two flags; the relay, the tracker and the state machine were untouched.

`selectTransport` auto-discovers serial before BLE. A plugged-in cable states intent unambiguously where a radio in range does not, since a light on a neighbouring desk can advertise into the room; serial discovery is a filesystem glob costing microseconds where a scan costs seconds of radio time before the relay can listen; and serial is the debuggable link, so somebody with both attached is usually mid-debug. `--ble` overrides the order and does not fall back.

BLE is the one transport that cannot write a frame in a single call. A GATT write is capped by the negotiated ATT MTU, 20 bytes at worst against a frame of roughly 1200, so `ble.Transport.Send` splits a frame into chunks and the firmware reassembles on the newline delimiter. The contract both sides implement is `ble.ChunkContract`.

### Relay

The long-running process that ties it together.

```go
// Relay owns the tracker, the transport, and both ingest listeners.
type Relay struct{ /* ... */ }

type Config struct {
    Transport      Transport
    SessionTimeout time.Duration // default 30m
    SweepInterval  time.Duration // default 30s
    ListenAddr     string        // default 127.0.0.1:7373, loopback only
    SocketPath     string        // default ~/.local/state/stoplight/sock
    LogPath        string
}

func NewRelay(cfg Config) (*Relay, error)

// Run blocks until ctx is cancelled. It never returns on transport
// failure: a light that is off, asleep or out of range is an expected
// condition, and exiting would look like a crash to the service manager.
func (r *Relay) Run(ctx context.Context) error

// Status is what `stoplight status` prints.
func (r *Relay) Status() Status

type Status struct {
    Transport string
    Connected bool
    Sessions  []Session
    Aggregate Color
    Uptime    time.Duration
}
```

---

## Package layout

```
main.go                     command dispatch only, no logic
design.md                   this file

internal/
  stoplight/                core types and the state machine
    color.go                Color, State, Event, transitions
    session.go              Session, label derivation
    tracker.go              Tracker
    frame.go                Frame, FrameSession
    report.go               Report, the RFC 1 message

  relay/                    the long-running process
    relay.go                Relay, Run loop, sender, reconnect
    http.go                 the mux and all three handlers
    socket.go               unix socket, ingest only
    status.go               Status, StatusResponse, TaskRequest

  notify/                   the one-shot client
    notify.go               Send and SendTask, discard errors, exit 0 always

  adapter/                  one package per agent
    adapter.go              the Adapter interface, registry
    claudecode/             settings.json hooks, event mapping
      claudecode.go         the Adapter implementation
      object.go             ordered JSON objects, preserving key order
      settings.go           read, merge, write without clobbering

  service/                  keeping the relay alive
    service.go              Install, Uninstall, Start, Stop interface
    launchd.go              macOS
    systemd.go              Linux
    schtasks.go             Windows
    unsupported.go          every other GOOS

  transport/
    transport.go            the Transport interface
    serial/serial.go        USB serial
    ble/ble.go              BLE: retry, chunking, connection state
    ble/radio.go            everything touching tinygo.org/x/bluetooth
    ble/contract.go         the chunking contract the firmware implements

  light/
    virtual.go              terminal renderer, implements Transport

firmware/                   ESP32 firmware, built with the Arduino toolchain
enclosure/                  OpenSCAD and STLs
```

Adding support for another agent means adding a package under `adapter/` and registering it. No other package changes, and agents that need no adapter simply post to the HTTP endpoint.

### Dependency direction

```
   main
    │
    ├──▶ relay ──▶ stoplight ◀── notify
    │      │
    │      └──▶ transport ◀── light
    │
    ├──▶ adapter ──▶ adapter/claudecode
    └──▶ service
```

`internal/stoplight` imports nothing from the project. Everything else may import it. Nothing imports `relay` except `main`. That keeps the state machine testable in isolation, which matters because it holds the invariants.

An adapter imports `internal/adapter` for the interface and the registry, and nothing else from this project. `claudecode` does not import `stoplight` at all: it writes event names into a config file as strings, so it never needs the Go constants. Either way an adapter cannot reach the tracker, so it cannot invent state.

### Package APIs

```go
// internal/adapter
type Adapter interface {
    Name() string                      // "claude-code"
    Install(binPath string) error      // idempotent
    Uninstall() error                  // restores what it found
    Installed() (bool, error)
}

func Register(a Adapter)
func All() []Adapter
func Get(name string) (Adapter, bool)

// internal/service
type Manager interface {
    Install(binPath string) error
    Uninstall() error
    Start() error
    Stop() error
    Disable() error
    Running() (bool, error)
    Command() string   // the underlying launchctl/systemctl invocation
}
func New() (Manager, error)   // picks by GOOS

// internal/notify
func Send(addr string, r Report) error
// Posts to /v1/session. `notify` discards the error and exits 0. It is
// returned for tests, and for SendTask's caller.

func SendTask(addr string, t Task) error
// Posts to /v1/task. Unlike Send, this error IS reported: `stoplight
// task` is typed by a human who is owed an answer.

// internal/light
func NewVirtual(w io.Writer) *Transport
```

---

## Wire protocol

Two hops. The first is public and specified by [RFC 1](rfc.md); the second is private between this relay and this firmware.

```
  any producer ──HTTP or socket──▶ relay ──USB serial──▶ light
       │                                                   │
   RFC 1 §5                                         frame format below
   public contract                                  private, may change
```

### Producer to relay

Specified in full by [RFC 1 §4](rfc.md#4-transport) and [§5](rfc.md#5-message-format). Summarised here:

```
POST http://127.0.0.1:7373/v1/session
{"session_id":"a1","event":"blocked","label":"auth-api","provider":"deepseek"}
```

The same JSON is accepted on a unix socket at `~/.local/state/stoplight/sock`, for producers that spawn a process per event.

The relay MUST bind HTTP to loopback only. If nothing is listening, producers discard the error and continue. `stoplight notify` exits 0 unconditionally.

### The HTTP endpoints

The relay serves three paths, and the mux 404s anything else. Only the first is public: [RFC 1](rfc.md) specifies `/v1/session` and `/v1/task`, and `/v1/status` is a private management endpoint this implementation adds.

| Path | Method | Success | Errors | Purpose |
|---|---|---|---|---|
| `/v1/session` | `POST` | `204`, no body | `400`, `405`, `413` | Ingest. One report, one state transition. |
| `/v1/task` | `POST` | `204`, no body | `400`, `405`, `413` | Set or clear a session's screen label. No event, so it never moves the lamp. |
| `/v1/status` | `GET` | `200`, a `StatusResponse` | `405`, `500` | Read-only snapshot. What `stoplight status` prints. |

`400` is malformed JSON or a missing required field. `405` carries an `Allow` header naming the one method the path accepts. `413` is a body over `MaxBodyBytes`, 8KB.

The unix socket carries **ingest only**. There is no socket equivalent of task or status, because both are typed by a human through the CLI rather than fired from a hook, so neither has the per-event process-spawn cost the socket exists to avoid.

`/v1/status` is deliberately outside RFC 1. The RFC specifies what a producer sends, and nothing about `StatusResponse` is a producer concern: it exposes this relay's internal session bookkeeping, its transport name, and its uptime. Freezing that shape in a public spec would fix the relay's internals as a contract for third parties, which is exactly what the RFC's two-hop split exists to avoid. `stoplight status` and this document are its only consumers, and it may change with the implementation.

`/v1/task` is in the RFC for the opposite reason. It has a public client in `internal/notify`, its request body is two documented fields with no internal detail, and relabelling a session is a thing a third-party producer plausibly wants to do without shipping an adapter.

### Relay to light

Newline-delimited JSON over a serial port. The format is transport-agnostic, so a BLE characteristic will carry the same lines when that transport exists and the firmware will still parse one format.

This hop is **not** part of RFC 1. It is an implementation detail between the relay and the reference firmware, and it may change without a protocol version bump. Producers never see it.

```json
{"color":"red","sessions":[{"id":"a1","label":"auth-api","state":"needs you","color":"red"},{"id":"b2","label":"stoplight","state":"working","color":"yellow"}]}
```

| Field | Type | Notes |
|---|---|---|
| `color` | enum | Aggregate. `off`, `green`, `yellow`, `red`. Drives the lamps. |
| `sessions` | array | Ordered by start time. May be empty. |
| `sessions[].id` | string | Lets a pin survive a list update |
| `sessions[].label` | string | Top line. Firmware scrolls if it overflows. |
| `sessions[].state` | string | Bottom line |
| `sessions[].color` | enum | That session's own colour |

Every field is optional. A frame carrying only `color` moves the lamps and leaves the screen alone. Unknown fields are ignored, so either side can be updated without breaking the other.

Empty `sessions` with `color: "off"` means no live sessions: lamps dark, screen cleared.

### JSON Schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "Stoplight frame",
  "type": "object",
  "properties": {
    "color": { "enum": ["off", "green", "yellow", "red"] },
    "sessions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id":    { "type": "string", "minLength": 1 },
          "label": { "type": "string" },
          "state": { "type": "string" },
          "color": { "enum": ["green", "yellow", "red"] }
        },
        "required": ["id"],
        "additionalProperties": true
      }
    }
  },
  "additionalProperties": true
}
```

`additionalProperties` is true deliberately. Forward compatibility beats strictness here, because the firmware and the app are updated separately and a stricter schema would turn a new field into a broken light.

### What the firmware owns

The relay sends state. The firmware decides how to show it. Rotation timing, scroll position, and which session is pinned all live on the device, so they keep working when the link drops.

---

## Invariants

Properties that must hold. Each one is a test.

**A pinned or rotating screen can never hide a red lamp.** The lamp is computed from every live session, independent of what the screen displays. This is the one that matters: the device's whole value is that a green glance is trustworthy.

**Aggregation ignores `provider`.** A red session is red whether it came from Claude Code, a shell script, or an agent that does not exist yet. One light, one aggregate.

**A producer can never break its caller.** `notify` exits 0 on every path: relay down, socket missing, malformed input, disk full. The reference clients in RFC 1 §13 discard errors for the same reason.

**The relay never exits on transport failure.** No light, cable unplugged, device gone: stay up and retry. Exiting looks like a crash to the service manager, which restarts it, which loops. The run loop polls `Transport.Connected` every second and re-connects, then re-sends the current frame so the light catches up.

**Unknown events and fields are ignored, not rejected.** A producer written against a later version of RFC 1 must not break an older relay.

**An unknown session is created, not rejected, in the state its event implies.** A producer that restarts mid-session may send `blocked` for a session the relay never saw. That session is created `Blocked`, so the lamp goes red. Creating it `Idle` and then applying the event would show green while a human is being waited on, which is the one failure the light exists to prevent. `ended` is the exception: there is nothing to expire, so no session is created.

**No live state refuses a work event.** `started`, `blocked` and `finished` are accepted from `Idle`, `Working`, `Blocked` and `Done` alike. An event says what the agent is doing now, so no earlier state can make it wrong. Every gate on the source state that has been tried here has lost a light: `finished` from `Blocked` held the lamp red after every answered permission prompt, and `blocked` from `Done` swallowed a red light on a session's second turn.

**Aggregation is order-independent.** The same set of sessions yields the same colour regardless of the order events arrived in.

**No live sessions means lamps off, not green.** Green means finished. Off means nothing running.

**A crashed session cannot hold the lamp.** The 30-minute sweep guarantees any state eventually reaches `Expired`, whatever the producer did.

**Adapter install and uninstall are idempotent.** Running twice changes nothing. Uninstall leaves the host's config as it was found, including other tools' entries.

**Frames are only sent on change.** `Apply` and `Sweep` return whether anything changed; the run loop transmits only then. A quiet desk is a quiet radio.
