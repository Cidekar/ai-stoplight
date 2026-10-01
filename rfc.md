# RFC 1: The Stoplight Agent Status Protocol

**Version** 1.0 · **Status** Draft · **Updated** September 2026

## Abstract

This document specifies how any agent reports its status to a Stoplight device. The protocol is a single HTTP endpoint carrying four semantic events. It names no vendor, no model, and no framework, and it requires no library.

The protocol is deliberately small enough to implement in ten lines of any language. That is the whole design goal: a tool released next year should be able to drive a light built this year, without either knowing about the other.

Claude Code is one implementation, mapped in through an adapter like any other.

## Contents

- [1. Terminology](#1-terminology)
- [2. Model](#2-model)
- [3. Events](#3-events)
- [4. Transport](#4-transport)
- [5. Message format](#5-message-format)
  - [5.3 The task endpoint](#53-the-task-endpoint)
  - [5.4 The sync endpoint](#54-the-sync-endpoint)
- [6. Responses](#6-responses)
- [7. Session lifecycle](#7-session-lifecycle)
- [8. Aggregation](#8-aggregation)
- [9. Labels](#9-labels)
- [10. Adapters](#10-adapters)
  - [10.1 Discovery mechanisms](#101-discovery-mechanisms)
- [11. Compatibility](#11-compatibility)
- [12. Security](#12-security)
- [13. Reference implementations](#13-reference-implementations)

---

## 1. Terminology

The key words MUST, MUST NOT, SHOULD, SHOULD NOT and MAY are to be interpreted as described in RFC 2119.

**Agent** — any program doing work a human waits on. An LLM CLI, an IDE assistant, a build, a test suite.

**Session** — one unit of work with a beginning and an end. A conversation, a job, a run.

**Producer** — anything that sends events. An adapter, a script, a curl command.

**Relay** — the Stoplight process receiving events and driving the light.

**Device** — the physical light. Out of scope here; see [design.md](design.md).

---

## 2. Model

A producer reports **what happened**. The relay decides **what the light does**. Producers never send colours.

```
   ANY PRODUCER             RELAY                  DEVICE
   ────────────             ─────                  ──────
   an LLM CLI  ──┐
   an IDE agent──┤          tracks
   your script ──┼── HTTP ─▶ every    ── link ──▶  lamps
   a test run  ──┤          session                screen
   a CI job    ──┘          at once
```

Only the left hop is specified here. How a relay reaches its device is an implementation matter: the reference relay uses USB serial today and BLE is planned, and a producer sees no difference either way.

The left column is open-ended by design. This document describes what a producer sends, never what a producer is.

This split is the point. A producer that sent colours would need to know about every other session to choose correctly. By sending events instead, a producer needs to know only about itself, and the relay resolves conflicts.

---

## 3. Events

Four events. They are semantic, not vendor terms.

| Event | Meaning | Lamp |
|---|---|---|
| `started` | The agent began working | yellow |
| `blocked` | The agent needs a human | red |
| `finished` | The agent completed its work | green |
| `ended` | The session is over, stop tracking it | — |

A fifth is optional:

| Event | Meaning | Lamp |
|---|---|---|
| `idle` | Session exists, nothing happening | green |

Producers MUST send `started` before `blocked` or `finished` for a given session. A relay receiving `blocked` for an unknown session MUST create it rather than reject the event, because producers restart and relays restart and neither should lose a light.

A relay creating a session it has never seen MUST place it in the state the received event implies, and MUST NOT create it in a default state and then apply the event to that default. The two differ exactly where it matters: a session created idle and then sent `blocked` would show green.

| Event received for an unknown session | Relay MUST create the session as | Lamp |
|---|---|---|
| `idle` | idle | green |
| `started` | working | yellow |
| `blocked` | blocked | red |
| `finished` | done | green |
| `ended` | *not created* | — |

A relay MUST NOT create a session on `ended`, or on its own silence timeout. Both only end a session, and a session that was never tracked has nothing to expire.

`ended` is a courtesy. A relay MUST expire silent sessions on its own, because agents crash without saying goodbye.

### Choosing the right event

The distinction that carries the product is `blocked` versus `started`.

Send `blocked` when a human must act before work continues: a permission prompt, a clarifying question, a confirmation, a failed step needing a decision. Send `started` when the agent is thinking, generating, or running tools without needing anyone.

If you cannot tell the difference from inside your agent, send `started` and never `blocked`. A light that shows yellow honestly is worth more than one that shows red on a guess.

---

## 4. Transport

### 4.1 HTTP

The primary interface. The relay listens on loopback.

```
POST http://127.0.0.1:7373/v1/session
Content-Type: application/json
```

Port 7373 is the default and MUST be configurable. The relay MUST bind to loopback only unless explicitly configured otherwise.

This specification defines two paths under `/v1/`:

| Path | Method | Purpose | Section |
|---|---|---|---|
| `/v1/session` | `POST` | Report one event. The core of the protocol. | [§5](#5-message-format) |
| `/v1/task` | `POST` | Set or clear a session's screen label. Optional. | [§5.3](#53-the-task-endpoint) |

A relay MUST serve `/v1/session`. A relay SHOULD serve `/v1/task`. A relay MAY serve other paths under `/v1/` for its own management; those are outside this specification and a producer MUST NOT depend on them. The reference relay serves `GET /v1/status` on exactly that basis.

### 4.2 Unix socket

The relay MUST also accept the same JSON on a unix domain socket, default `~/.local/state/stoplight/sock`. This exists for producers that fire once per event and cannot afford TCP setup, such as shell hooks.

The message format is identical. One JSON object per connection.

The socket carries **session events only**. A relay MAY expose `/v1/task` over the socket and is not required to: relabelling is a human-driven action and does not have the once-per-event process-spawn cost the socket exists to avoid. The reference relay does not.

### 4.3 Choosing

| Use | When |
|---|---|
| HTTP | Almost always. Any language, any tool, trivially testable with curl. |
| Unix socket | High-frequency local hooks where process spawn cost matters |

---

## 5. Message format

```json
{
  "session_id": "a1b2c3",
  "event": "blocked",
  "label": "auth-api",
  "provider": "deepseek",
  "detail": "waiting for approval to write files"
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `session_id` | string | yes | Stable for the session's life. Opaque to the relay. |
| `event` | enum | yes | One of `started`, `blocked`, `finished`, `ended`, `idle` |
| `label` | string | no | Shown on screen. See [§9](#9-labels). |
| `provider` | string | no | Free text: `claude-code`, `deepseek`, `kimi`, `ci`. Identifies the producer. |
| `detail` | string | no | Longer text. Reserved: the current device is too small to show it. |
| `cwd` | string | no | Working directory. Lets the relay derive a label if none is given. |

Unknown fields MUST be ignored, not rejected.

### 5.1 session_id

Any string, stable for the session. A UUID, a PID, a conversation ID, a branch name.

Two producers using the same `session_id` will be treated as one session. Producers SHOULD namespace it if collision is plausible, for example `deepseek:4417`.

### 5.2 provider

Free text, not an enum, so a new agent needs no change to this spec. The relay MAY show it on screen and MUST NOT use it for aggregation. One light shows one aggregate across every provider. A red DeepSeek session and a red Claude session are both just red.

### 5.3 The task endpoint

`POST /v1/task` sets the label shown on the screen for one session, overriding whatever the producer sent and whatever the relay derived.

```
POST http://127.0.0.1:7373/v1/task
Content-Type: application/json

{"session_id":"a1b2c3","label":"nightly integration run"}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `session_id` | string | yes | Which session to relabel. Same identifier as [§5.1](#51-session_id). |
| `label` | string | yes, may be empty | The text to show. An EMPTY label CLEARS the override. |

Unknown fields MUST be ignored, as everywhere else in this protocol.

The responses match [§6](#6-responses), with one difference: a missing or empty `session_id` is a `400`. An unknown event is ignored because the request still names a session, but a request naming no session names nothing at all.

| Code | Meaning |
|---|---|
| `204` | Accepted. No body. |
| `400` | Malformed JSON, or a missing `session_id` |
| `405` | Any method other than `POST` |
| `413` | Body over 8KB |

Three rules govern it, and each one exists because the obvious alternative loses a light.

**It carries no event.** A relabel MUST NOT run a state transition. Naming the task you are blocked on is exactly the moment the lamp must stay red, so a relay MUST NOT treat a task request as an `idle` report or as any other event.

**It MUST NOT move the aggregate.** Labels reach the screen only. [§8](#8-aggregation) is computed from states, and no relabel can change one.

**An unknown session MUST NOT be created.** A label carries no state, so there is no light to lose by declining. Creating one would mean choosing a state, and every choice is wrong: green invents finished work on an empty desk. A relay SHOULD instead park the label against that ID and apply it when the session first genuinely reports, so naming work before it starts still works. A parked label SHOULD age out on the same silence timeout as a session.

The request is still accepted with a `204` in that case. The producer asked for something reasonable and the relay honoured it as far as it honestly could.

### 5.4 The sync endpoint

`POST /v1/sessions` declares a producer's **complete** set of live sessions in one request. Every other message in this protocol is a delta about one session; this is the only one that describes a whole world.

```
POST http://127.0.0.1:7373/v1/sessions
Content-Type: application/json

{"provider":"claude-code","observed_at":"2026-09-10T16:47:30Z","sessions":[
  {"session_id":"a1b2c3","event":"started","label":"auth-api"},
  {"session_id":"d4e5f6","event":"blocked","label":"stoplight"}
]}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `provider` | string | yes | Which producer is declaring. Scopes the reconciliation. Free text, as [§5.2](#52-provider). |
| `observed_at` | RFC 3339 timestamp | yes | When the producer read this state, NOT when it sent the request. |
| `sessions` | array | yes, may be empty | Every live session this producer knows about. |

Each entry takes the same fields as [§5](#5-message-format), minus `provider`, which the envelope carries.

**A sync is authoritative for its own provider, and for nothing else.** This is the rule the endpoint exists for:

- A session in `sessions` is created or updated, exactly as if it had arrived at `/v1/session`.
- A session the relay holds for this `provider` that is **absent** from `sessions` MUST be treated as `ended` and removed.
- A session belonging to any **other** provider MUST NOT be touched. Two producers syncing independently must not delete each other's work.

**An empty `sessions` array is meaningful, not malformed.** It declares that this producer has nothing live, and the relay MUST remove everything it holds for that provider. A producer with no sessions is the normal state of a quiet desk.

**`observed_at` is the ordering key, not arrival time.** A relay MUST ignore any session entry, and any implied removal, that is older than what it already holds for that session. Polling and event-driven reporting have different latencies, so a slow sync can arrive after a fast delta that supersedes it. Without this rule a stale read silently reverses a fresh one, and a session that just went red goes green again.

Responses match [§6](#6-responses), with the additions below. A body over 64KB is rejected rather than 8KB, because a sync carries many sessions where every other request carries one.

| Code | Meaning |
|---|---|
| `204` | Accepted. No body. |
| `400` | Malformed JSON, a missing `provider`, or a missing or unparseable `observed_at` |
| `405` | Any method other than `POST` |
| `413` | Body over 64KB |

**Why this is not just repeated calls to `/v1/session`.** Removal is the whole point. A producer can already report every session it knows about one at a time, but it has no way to say "and nothing else" — so a session that vanished without an `ended` stays lit forever. Absence is only expressible against a declared whole.

**Producers SHOULD sync on start.** A relay holds state in memory and loses it on restart, and [§7](#7-session-lifecycle) sends events only on change, so after a restart a genuinely idle session is invisible until it happens to move. A sync on producer start, and again on any reconnection, closes that window. This is the one case where the relay's own restart is recoverable without waiting for the world to change.

**A producer MUST NOT sync in place of reporting.** Sync is a floor, not a substitute. Polling at any interval a desk would tolerate is far slower than a state change, and the device exists to be immediate: the whole value is a lamp that goes red while you are still looking away. Producers that can report on change MUST do so, and MAY sync alongside it as a correction.

---

## 6. Responses

These apply to `POST /v1/session`. [§5.3](#53-the-task-endpoint) and [§5.4](#54-the-sync-endpoint) note where `/v1/task` and `/v1/sessions` differ.

| Code | Meaning |
|---|---|
| `204` | Accepted. No body. |
| `400` | Malformed JSON, or a missing or unknown required field |
| `405` | Any method other than `POST`. The response carries an `Allow` header. |
| `413` | Body over 8KB |

The relay MUST NOT return a body on success. Producers SHOULD ignore the response entirely.

**Producers MUST NOT fail when the relay is unreachable.** This is the most important rule in this document. A status light is an accessory. If the relay is down, the port is closed, or the device is unplugged, the producer continues its real work as though nothing happened. Connection errors are discarded, not logged as failures, not retried in a way that blocks, and never surfaced to the user.

Producers SHOULD use a timeout of 250ms or less.

---

## 7. Session lifecycle

```
    idle ──▶ IDLE ──── started ──▶ WORKING ──── blocked ──▶ BLOCKED
               │                      │                        │
               │ blocked              │ finished               │ started
               │ or finished          ▼                        │
               └───────────────────▶ DONE ◀────────────────────┘
                                      │
                                      │ ended, or silence
                                      ▼
                                   EXPIRED
```

A relay MUST accept `blocked` and `finished` from the idle state, not only from working. An agent can need a human without an intervening turn: a scheduled job that wakes and asks for a password, or a producer whose `started` was lost. A relay that ignored `blocked` from idle would drop the red light.

A relay MUST create an unknown session in the state its event implies, as specified in §3.

A session that sends no event for **30 minutes** MUST be expired by the relay. Producers cannot be trusted to clean up after themselves.

Any event resets that timer. A long-running agent SHOULD re-send its current state every few minutes as a keepalive.

Sending `started` for an existing session is legal and common: it means a new turn.

---

## 8. Aggregation

The lamp shows the most urgent state across all live sessions, from all providers.

```
   blocked  >  started  >  finished | idle
     red        yellow          green
```

No live sessions means the lamps are **off**, not green. Green means work completed. Off means nothing is running.

A relay MUST compute this over every session regardless of provider, and MUST NOT let any display feature — rotation, pinning, filtering — change what the lamps show. If any session is blocked, the lamp is red. This is the guarantee that makes a glance trustworthy.

---

## 9. Labels

The `label` is what appears on the device screen, so a human can tell which session needs them.

Producers SHOULD send something short and distinguishing. The reference device shows about **ten characters per line**, so a label is a name, not a sentence.

Good: `auth-api`, `payments`, `nightly-e2e`
Poor: `Refactoring the authentication module to use refresh tokens`

If no `label` is sent, the relay derives one: git branch of `cwd`, else the directory name, else the `provider`, else the `session_id`.

Relays SHOULD strip noise prefixes such as `feature/`, `bugfix/` and `worktree-` before display.

---

## 10. Adapters

An adapter connects a specific agent to this protocol. It knows two things: how that agent signals its state, and how to install itself.

```go
type Adapter interface {
    Name() string
    Install(binPath string) error
    Uninstall() error
    Installed() (bool, error)
}
```

Adapters live in `internal/adapter/<name>`. Adding one is a new package, never a change to the core.

An adapter MAY learn an agent's state by any means the agent offers: hooks, a log file, a query command, an API, or several at once. **What it emits is always the events in [§3](#3-events) and nothing else.** How it found out is its own business, and no part of the relay may learn the difference. This is the same boundary that stops the word "hook" from appearing below the adapter, applied to every other mechanism.

An adapter that uses more than one mechanism MUST reconcile them **before** emitting, and SHOULD do so on observation time, preferring the most recent reading of a session. The relay receives one ordered stream per session and MUST NOT be asked to arbitrate between an adapter's own sources.

### 10.1 Discovery mechanisms

Two shapes, and most agents afford only the first or only the second.

**Reporting on change.** The agent tells the adapter when something happens: a hook, a callback, a tailed log. Fast, and the only way to get a lamp that moves while you are still looking away. But it reports only what it chooses to report, so what it omits is invisible: a session that ends without a final event stays lit, and a session the agent never announced is never known.

**Polling.** The adapter asks the agent for its whole current state on an interval. Slower by the length of the interval, and it MUST NOT be the only mechanism where reporting is available. What it buys is **completeness**, and completeness is what fixes the failures reporting cannot:

- A session that disappeared without a final event is absent from the next poll, so it can be ended.
- A session the agent never announced appears in the poll, so it can be named.
- A session the agent announced but never drove — an idle worker, a warm spare, an internal process — can be told apart from a real one, because a poll says what the agent itself considers a session.
- After a relay restart, one poll restores the whole picture. See [§5.4](#54-the-sync-endpoint).

**A poll is a whole truth and SHOULD be sent as one.** An adapter that polls SHOULD post the result to `/v1/sessions` rather than translating it into a burst of per-session events, because a burst cannot express the absences, which is most of what a poll knows.

**Polling MUST NOT sit on the reporting path.** A hook runs while the agent waits for it, and [§6](#6-responses) already holds producers to 250ms. Querying an agent for its full state is far slower than that and belongs on its own schedule, never inside the handler for an event.

The relay does not poll, and this section does not make it a poller. A poller is an adapter, adapters are producers, and producers push. The distinction matters because inferring "is this waiting for a human" from outside the agent is exactly what this protocol refuses to do: the poll asks the agent what it thinks, which is a report, not an inference.

### 10.2 A worked example: Claude Code

Claude Code emits hooks on session events, so its adapter is a table and an installer. Any agent with a similar mechanism follows the same shape.

| Claude Code hook | Matcher | Protocol event |
|---|---|---|
| `SessionStart` | `startup` | `idle` |
| `UserPromptSubmit` | | `started` |
| `PreToolUse` | | `started` |
| `Notification` | `permission_prompt` | `blocked` |
| `Stop` | | `finished` |
| `SessionEnd` | | `ended` |

The matcher narrows a hook to the one sub-event that means the mapped protocol event; a blank matcher means every occurrence. Two hooks need one. `SessionStart` fires on `startup`, `resume`, `clear`, `fork` and `compact`, but only `startup` is a session sitting idle: an auto-compaction mid-turn would otherwise drop a working session to idle until the next `Stop`. `Notification` fires on every notification type, and only `permission_prompt` means a human is needed: `idle_prompt` fires about a minute after a turn ends, and matching it would turn a green session red.

`PreToolUse` carries no colour of its own; it reports `started` to clear `blocked`. When a human approves a permission prompt the agent runs the tool, and `PreToolUse` fires long before `Stop`. Without it nothing between the `Notification` and the `Stop` sends an event, so an approved prompt stays red for the rest of the turn. `started` from `blocked` returns the session to working, which is where it is. The poll table agrees: a running session reports `working`, which is also `started`.

`SessionEnd` is what makes a session leave. Without it the only removal paths are an explicit `ended` from some other producer and the silence timeout, and a session that keeps emitting events is never silent: a background agent that reports `blocked` every few minutes refreshes its own keepalive and holds the lamp red indefinitely. `SessionEnd` fires on `exit`, on Ctrl-D and on abnormal termination, so the session that a human has closed is the session that leaves the light.

Install writes these entries into that tool's config, each invoking `stoplight notify`, which posts to the relay and exits 0 unconditionally.

Claude Code also answers `claude agents --json`, so this adapter is a poller as well, per [§10.1](#101-discovery-mechanisms). The second table is the state it reports at rest, against the event that state implies:

| `claude agents --json` state | Protocol event |
|---|---|
| `working` | `started` |
| `blocked` | `blocked` |
| `done` | `finished` |
| `failed` | `finished` |
| `stopped` | `finished` |

`failed` and `stopped` are both green, not red. Both are over, and red means a human is needed: a session that failed unattended must not hold the desk's attention until someone clears it. The screen cannot say why a session ended, so the lamp must not imply the ending needs anything.

An unrecognised state reports as `started` rather than being dropped. Dropping it would leave the session absent from the declaration, and absence means ended, so an agent state added in a later release would silently extinguish the light for a session that is plainly alive.

The two tables must agree about what a colour means. The hook table translates events as they happen and the state table translates a state observed at rest, so a disagreement would have the poll fighting the hooks it exists to correct.

The mapping table is where a vendor's vocabulary stops. Nothing below the adapter knows the word "hook", or which tool sent the event. That boundary is what keeps this protocol from accreting one tool's concepts.

### 10.3 Writing your own

Most agents have no hook system, which is why HTTP is the primary interface. Three approaches, in order of preference:

**Emit from the agent.** If you control the code, post directly at the points where you know your state. Most accurate.

**Wrap the agent.** A shell function that posts `started`, runs the real binary, then posts `finished`. Catches the common case in five lines.

**Wrap the tool call.** If your framework has middleware or a tool-call hook, post from there.

What this spec deliberately does **not** describe is inferring state by watching processes or tailing logs. Guessing "is it waiting for me" from outside is unreliable, and a status light that is sometimes wrong is worse than none. Push, do not poll.

---

## 11. Compatibility

This protocol versions through the path prefix, `/v1/`, and every endpoint in it moves together. A v2 relay would serve `/v2/session` and `/v2/task`, and MAY serve the v1 pair alongside them.

Within a major version:

- New optional fields MAY be added. Receivers ignore unknown fields.
- New event types MAY be added. Receivers MUST ignore unknown events rather than erroring.
- New endpoints MAY be added under the same prefix. A producer that does not know one simply does not call it.
- Existing fields MUST NOT change meaning.
- Existing events MUST NOT change meaning.
- Existing endpoints MUST NOT change meaning.

A producer written against v1 today MUST keep working against any v1 relay. A producer MUST NOT assume that every path under `/v1/` is specified here: a relay MAY serve its own management endpoints in that space, as [§4.1](#41-http) allows.

---

## 12. Security

The relay binds to loopback by default. Any local process can therefore set the light. That is the intended trust model: this is a status indicator on your own desk, and the data it carries is a colour and a short label.

Relays MUST NOT bind to a non-loopback interface without explicit configuration. Relays SHOULD cap request bodies at 8KB.

Producers SHOULD NOT put secrets in `label` or `detail`. It is rendered on a screen and written to a log.

---

## 13. Reference implementations

### curl

```bash
curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/session \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","event":"blocked","label":"my-job"}' \
  || true
```

The `|| true` is the protocol's whole philosophy in three characters.

Naming a session, per [§5.3](#53-the-task-endpoint), and then clearing that name:

```bash
curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/task \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","label":"nightly integration run"}' \
  || true

curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/task \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"demo","label":""}' \
  || true
```

### Shell wrapper

Wraps any command. Yellow while it runs, green on success, red on failure.

```bash
stoplight_run() {
  local id="wrap:$$" label="${PWD##*/}"
  _sl() {
    curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/session \
      -H 'Content-Type: application/json' \
      -d "{\"session_id\":\"$id\",\"event\":\"$1\",\"label\":\"$label\"}" \
      >/dev/null 2>&1 || true
  }
  _sl started
  "$@"; local rc=$?
  [ $rc -eq 0 ] && _sl finished || _sl blocked
  return $rc
}
```

### Python

```python
import json, urllib.request

def stoplight(session_id, event, label=None, provider=None):
    body = {"session_id": session_id, "event": event}
    if label:    body["label"] = label
    if provider: body["provider"] = provider
    req = urllib.request.Request(
        "http://127.0.0.1:7373/v1/session",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"},
    )
    try:
        urllib.request.urlopen(req, timeout=0.25)
    except Exception:
        pass   # a status light must never break the caller
```

### Go

```go
func Stoplight(id, event, label string) {
    b, _ := json.Marshal(map[string]string{
        "session_id": id, "event": event, "label": label,
    })
    c := &http.Client{Timeout: 250 * time.Millisecond}
    resp, err := c.Post("http://127.0.0.1:7373/v1/session",
        "application/json", bytes.NewReader(b))
    if err == nil {
        resp.Body.Close()
    }
    // errors intentionally discarded
}
```

---

## Appendix A: JSON Schema

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://stoplight.dev/schema/v1/session.json",
  "title": "Stoplight session event",
  "type": "object",
  "properties": {
    "session_id": { "type": "string", "minLength": 1, "maxLength": 128 },
    "event": {
      "enum": ["started", "blocked", "finished", "ended", "idle"]
    },
    "label":    { "type": "string", "maxLength": 256 },
    "provider": { "type": "string", "maxLength": 64 },
    "detail":   { "type": "string", "maxLength": 1024 },
    "cwd":      { "type": "string", "maxLength": 4096 }
  },
  "required": ["session_id", "event"],
  "additionalProperties": true
}
```

`additionalProperties` is true deliberately. A relay that rejected unknown fields would break every producer written against a later version of this spec.

The task request from [§5.3](#53-the-task-endpoint):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://stoplight.dev/schema/v1/task.json",
  "title": "Stoplight task label",
  "type": "object",
  "properties": {
    "session_id": { "type": "string", "minLength": 1, "maxLength": 128 },
    "label":      { "type": "string", "maxLength": 256 }
  },
  "required": ["session_id", "label"],
  "additionalProperties": true
}
```

`label` is required but may be the empty string, which is how a caller clears an override.

The sync request from [§5.4](#54-the-sync-endpoint):

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://stoplight.dev/schema/v1/sessions.json",
  "title": "Stoplight session sync",
  "type": "object",
  "properties": {
    "provider":    { "type": "string", "minLength": 1, "maxLength": 64 },
    "observed_at": { "type": "string", "format": "date-time" },
    "sessions": {
      "type": "array",
      "maxItems": 256,
      "items": {
        "type": "object",
        "properties": {
          "session_id": { "type": "string", "minLength": 1, "maxLength": 128 },
          "event":      { "enum": ["idle", "started", "blocked", "finished"] },
          "label":      { "type": "string", "maxLength": 256 }
        },
        "required": ["session_id", "event"],
        "additionalProperties": true
      }
    }
  },
  "required": ["provider", "observed_at", "sessions"],
  "additionalProperties": true
}
```

`sessions` may be empty, which declares that this producer has nothing live. `ended` is absent from the entry `enum` deliberately: a sync expresses an ending by omission, and an entry saying a session has ended while also listing it as live is a contradiction rather than a shorthand.

## Appendix B: Design notes

**Why no colours from producers.** A producer knows only its own state. Choosing a colour requires knowing every session, which only the relay does.

**Why four events.** Three would drop the distinction between "working" and "needs you", which is the distinction the device exists for. Five or more starts encoding agent-specific concepts that do not generalise.

**Why free-text `provider`.** An enum would need updating for every new agent, and this spec should outlive the current crop of them.

**Why `blocked` rather than `waiting` or `input_required`.** It reads correctly for non-LLM producers too: a blocked build, a blocked deploy.

**Why loopback only.** A status light for a machine you are sitting at. Remote reporting is a different product with different security requirements.

**Why `/v1/task` is here and the reference relay's `/v1/status` is not.** A task label is a producer concern: its body is two fields, both already defined by this spec, and a third-party tool may reasonably want to name its own work without writing an adapter. A status snapshot is the opposite. It exposes one relay's session bookkeeping, its transport name and its uptime, none of which a producer needs and all of which are implementation detail. Specifying it would freeze an implementation's internals as a public contract, which is the exact coupling the two-hop split in [§2](#2-model) exists to prevent. It is documented in [design.md](design.md) instead.
