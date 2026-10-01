# Stoplight

**Stop alt-tabbing to see if your agent is done.**

```
        ╭───────────╮
        │           │
        │    ( ● )  │  ← RED     needs you
        │           │
        │    (   )  │    yellow  working
        │           │
        │    (   )  │    green   done
        │           │
        ├───────────┤
        │ ▪ 2/4     │
        │ auth-api  │  ← which job is waiting
        │ ● needs   │
        ╰───────────╯
```

A small traffic light that sits on your desk and tells you, without you looking for it, exactly what every AI coding session is doing.

You know the loop. You give an agent a task, switch to something else, and then spend the next ten minutes half-wondering. You tab back. Still thinking. You tab back again. It finished four minutes ago and has been waiting on a yes.

Stoplight ends that. The answer is on your desk, in your peripheral vision, in a colour you can read from across the room.

**It is not tied to any one AI tool.** Stoplight speaks [an open protocol](rfc.md): four events over one HTTP endpoint. Claude Code, Codex, Gemini CLI, Cursor, Aider, DeepSeek, Kimi, an agent you wrote this morning, or a build that takes twenty minutes — if it can make a web request, it can drive the light. Claude Code ships with an adapter because it was first, not because it is special.

## Why you want one

**It uses the eye you are not using.** A notification competes with your screen. A light on your desk does not. You catch red the way you catch a car's brake lights, without deciding to look.

**It knows which project.** Three sessions running, one goes red. The screen below the lamps tells you it is `auth-api`, not the refactor you actually care about. Names scroll, the screen rotates, and one button pins whichever one you are watching.

**It never lies to you.** The lamp always shows the most urgent state across every session. Pin the screen, walk away, come back — if anything needs you, it is still red.

**You build it in an evening.** One board the size of a stamp, three LEDs, a battery. The case prints in four parts. No soldering iron heroics required.

**It runs itself.** One install command, then it starts with your laptop and stays out of your way. No terminal window to babysit.

**It is not tied to one vendor.** Four events over HTTP is the whole integration. If your agent can make a web request, it can drive the light.

## How it works

Three pieces, and you only ever touch the first one.

```
   ANY PRODUCER               RELAY                     THE LIGHT
   ────────────               ─────                     ─────────
   any LLM CLI ──┐
   your agent  ──┤                        USB        ┌───────────┐
   a test suite──┼── HTTP ──▶  one Go   ─serial─▶    │  ( ● )    │
   a slow build──┤             binary                │  (   )    │
   a deploy    ──┘             tracks   ╌╌╌BLE╌╌▶    │  (   )    │
                               every     planned     │ auth-api  │
     "I am working"            session               └───────────┘
     "I need a human"          at once                on your desk
     "I am done"
```

USB serial is the link that works today. BLE is designed and is the next step on the [roadmap](roadmap.md), but it is not implemented, so the light is a wired device for now.

Nothing in that left column is privileged. They all send the same four events to the same endpoint, and the relay weighs them identically.

**Your agent reports what happened.** Four events: started, blocked, finished, ended. Nothing else. It never picks a colour, because it cannot know what else is running.

**One small Go binary decides what matters.** It tracks every session from every tool at once and resolves them to a single answer. Red beats yellow beats green, always. No web server to configure, no database, no config file to hand-write.

**The light does the telling.** Awake the moment something changes. It connects over USB serial today, and the board and the design are ready for the battery-powered BLE version that comes next.

Here is one moment, end to end:

```
   your agent hits a permission prompt
                  │
                  ▼
      POST {"event":"blocked",          ● RED
            "label":"auth-api"}   ──▶   + "auth-api" on screen
                  │
                  ▼
   you glance up, already know which job, and act
```

Unplug the light, kill the relay — your work never notices. Every client discards errors by design. The light is an accessory, never a dependency.

## Works with anything

Three ways in, depending on what your tool gives you.

| Your tool | What you do |
|---|---|
| Has a hook or plugin system | Use an adapter, or write one. `stoplight install` sets it up. |
| You can run a command around it | Wrap it in five lines of shell |
| You wrote it yourself | Post directly at the points where you know your state |

**With an adapter.** Claude Code ships with one, so `stoplight install` wires it up and you are done. Adapters for other tools are the most welcome contribution to this project.

**By wrapping.** Works for any command, including ones with no hooks at all. Yellow while it runs, green if it succeeds, red if it does not:

```bash
stoplight_run() {
  local id="wrap:$$" label="${PWD##*/}"
  _sl() { curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/session \
    -H 'Content-Type: application/json' \
    -d "{\"session_id\":\"$id\",\"event\":\"$1\",\"label\":\"$label\"}" \
    >/dev/null 2>&1 || true; }
  _sl started; "$@"; local rc=$?
  [ $rc -eq 0 ] && _sl finished || _sl blocked
  return $rc
}

stoplight_run my-agent --do-the-thing
stoplight_run make integration-test
```

**By posting directly.** One request, no library, no dependency:

```bash
curl -s -m 0.25 -X POST http://127.0.0.1:7373/v1/session \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"job-1","event":"blocked","label":"auth-api"}' || true
```

That is the entire integration surface. The `|| true` is deliberate and it is the rule for every client: if the light is unreachable, your work carries on as though nothing happened.

Full specification in [RFC 1](rfc.md), with reference clients in shell, Python and Go.

## Setup

```
stoplight install     # adapters for what you have, plus a service at login
```

One command. It detects which agents are installed, wires up an adapter for each, installs a background service, and starts it. `stoplight uninstall` reverses all of it. Both are idempotent, so running them twice changes nothing.

For Claude Code that means four hook entries in `~/.claude/settings.json`. For anything else, point it at the endpoint and there is nothing to install at all.

After that the light works without you thinking about it. `stoplight` on its own runs the relay in the foreground, which is what you want while developing or debugging, but the service means you never have to.

## Running as a service

A light that needs a terminal window open is a light you will forget to start. The relay therefore runs as a background service that survives logout, reboot and crashes.

Each platform has one obvious mechanism, and Stoplight uses it rather than shipping its own daemon manager.

| Platform | Mechanism | Location |
|---|---|---|
| macOS | launchd user agent | `~/Library/LaunchAgents/com.stoplight.relay.plist` |
| Linux | systemd user unit | `~/.config/systemd/user/stoplight.service` |
| Windows | Scheduled task, at logon | Task Scheduler |

All three are user-level, not system-level. The relay needs no privileges, it needs your session's access to the USB device, and a user service starts when you log in rather than at boot. Installing to the system would ask for a password and gain nothing.

`RunAtLoad` and `KeepAlive` are both set on macOS, with the equivalents elsewhere. If the relay crashes, the service manager restarts it. If the light is unplugged, the relay stays up and keeps retrying rather than exiting, because exiting would look identical to a crash.

Logs go to `~/.local/state/stoplight/stoplight.log`, or the platform equivalent. Every event is recorded on arrival, so one file answers whether a producer reported, whether the relay saw it, and whether the light acknowledged it. The service appends to that one file and nothing rotates it yet, so truncate it yourself if it grows. Rotation is planned.

### Managing it

| Command | What it does |
|---|---|
| `stoplight status` | Service state, connection, current sessions |
| `stoplight restart` | Bounce the service |
| `stoplight logs` | Tail the log |
| `stoplight service stop` | Stop until next login |
| `stoplight service disable` | Stop and do not start at login |

These wrap the platform tool rather than replacing it. `launchctl` and `systemctl` still work directly if you prefer them, and `stoplight status` prints the underlying command it used so the wrapper is never a black box.

### Producers and the relay are independent

Producers post and forget. If the relay is not running, the post fails and the producer carries on. `stoplight notify` exits 0 on every path, and the reference clients in RFC 1 discard errors deliberately.

That independence matters during upgrades and crashes. Your agent keeps working whether or not the light does, and the relay can be restarted underneath a running session without losing anything except the moments it was down.

Recovery runs one way only: from the relay down to the light. The relay holds the session set in memory and re-sends the current frame the moment the light comes back, so the lamps catch up without anyone asking the producers for anything. The relay never calls back to a producer, because it holds no address for one. That is also the limit of the recovery: restart the relay itself and the session set is gone, and each session reappears only when its producer next reports. [RFC 1 §7](rfc.md#7-session-lifecycle) asks long-running agents to re-send their state every few minutes for exactly this reason.

## Events

A producer reports what happened. It never picks a colour, because it cannot know what else is running. Four events cover everything:

| Event | Colour | Meaning |
|---|---|---|
| `started` | Yellow | Working |
| `blocked` | Red | Needs a human |
| `finished` | Green | Done |
| `ended` | — | Stop tracking this session |

The distinction that carries the whole product is `blocked` versus `started`. Send `blocked` when a human has to act before anything continues. Send `started` when the agent is thinking or running tools on its own.

### Claude Code

The adapter maps Claude Code's hooks onto those events. This is the only place a vendor's vocabulary appears:

| Claude Code hook | Matcher | Event |
|---|---|---|
| `SessionStart` | `startup` | `idle` |
| `UserPromptSubmit` | | `started` |
| `PreToolUse` | | `started` |
| `Notification` | `permission_prompt` | `blocked` |
| `Stop` | | `finished` |

The matcher keeps a hook to the sub-event that means the event. `SessionStart` fires on more than a fresh start, so it is pinned to `startup`, or an auto-compaction mid-turn would read as idle. `Notification` fires on more than a permission prompt, so it is pinned to `permission_prompt`, or a routine idle reminder would turn the light red. `PreToolUse` carries the `started` that clears `blocked`: after you approve a prompt the agent runs a tool, and this is what returns the session to working before the turn ends.

Adding another agent is a new adapter, or no adapter at all if it can post directly. See [RFC 1 §10](rfc.md#10-adapters).

It also appends a line to a log file on every call. When the light does not respond, that log is what tells you whether the event arrived at all.

#### Sessions already open are invisible until they next do something

Every hook is edge-triggered: it fires on a transition, never on a state. Nothing enumerates sessions, and the relay holds no address for a producer, so it cannot ask what is already running. A session that was open before the relay started has no pending transition to announce itself with, and the light does not know it exists.

**Nothing needs restarting.** Type anything in that window and `UserPromptSubmit` fires, the session registers, and it behaves normally from then on. The rule in practice: an idle old window is invisible until you touch it. A window you never touch again stays dark, which is the right answer anyway, because it is not telling you anything.

This is the direct cost of [push, not polling](roadmap.md#decisions-already-made), and it is the correct trade. Inferring "is this waiting for a human" from outside the agent is unreliable, and a light that is sometimes wrong is worse than no light.

#### A killed window leaves a session behind

There is no hook for "this process was killed". Closing a terminal, `kill -9`, or a crash all skip `Stop`, so no `finished` or `ended` event is ever sent and the session stays in the relay at whatever colour it held.

That matters when the colour was red: the lamp stays red for a session that no longer exists, and no amount of waiting clears it. Sessions carry a `last_seen` timestamp, visible in `GET /v1/status`, so a stale entry is easy to spot — it is the one whose `last_seen` stopped moving.

Clear it by hand with the id from the status endpoint:

```bash
curl -s -X POST http://127.0.0.1:7373/v1/session \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"<the stale id>","event":"ended"}'
```

Restarting the relay also clears everything, since the session set lives in memory.

## State model

All sessions share one light, whatever produced them, and the most urgent state wins.

```
red  >  yellow  >  green
```

If any session needs a human, the light is red. Otherwise, if any session is working, the light is yellow. Otherwise it is green. No live sessions at all means the lamps are off, because green means finished and off means nothing is running.

The relay tracks each session separately and computes the aggregate. A Claude Code session and a shell script are weighted identically; the light does not care which tool asked. Sessions that end are dropped, and a session that goes quiet expires after 30 minutes so one crash cannot hold the light red forever.

## Task display

The lamps tell you the state. The screen tells you which job that state belongs to. This matters as soon as you run more than one session, because a red light with no context does not say which project is waiting on you.

A producer can send a label. If it does not, the relay derives one.

```
auth-api
● working
```

The derived label is the git branch of the session's working directory, falling back to the directory name, then the provider name, then the session ID. Branches are usually named after the work, so this is free and it is usually right. A detached HEAD has no branch name, so it falls through to the directory name like any other case.

A producer can send its own instead, and you can override any session by hand.

```
stoplight task --session-id <id> "nightly integration run"
stoplight task --session-id <id> --clear
```

That overrides the label until the session ends or you clear it. `--clear` removes the override, so the derived label shows again.

The session ID is not optional. There is no "current session" concept yet, because nothing tells the CLI which of your sessions the terminal you are typing in belongs to, so the command asks you which one you mean. `stoplight status` lists the live IDs.

### Rotation

With more than one session running, the screen cycles through them. Each session gets the screen for at least three seconds, then the display advances.

Order is by session start time, so a session keeps its place in the cycle as others come and go. A single session does not rotate, because a cycle of one is a static screen that redraws for no reason. A counter in the corner shows position in the set.

```
▪ 2/4
stoplight
● working
```

Two rules keep rotation from hiding things.

The lamps never rotate. They always show the aggregate of every session, so a red lamp means something needs you even while the screen displays a green session. The lamp answers whether anything wants attention and the screen answers what is running. Those are different questions and the device answers both at once.

When a session turns red, the screen jumps to it immediately and holds for a full interval before resuming the cycle. The moment something needs you is the moment you want to know which project it is.

### Scrolling

The screen holds about ten characters per line, which is narrower than many branch names. Labels that do not fit scroll.

Scrolling is a marquee, not a loop. The label holds at the start for a second, scrolls left at a readable rate, holds at the end, then resets. A continuous wrap-around never gives a stable moment to read the first characters, which is where the useful part of a branch name usually is.

Three limits keep it calm. Labels that already fit are drawn statically and never move. The state line never scrolls, because `working` and `needs you` are short and are what you read at a glance. Scrolling resets to the start whenever the session changes colour.

Rotation waits for scrolling. The three second interval is a minimum, not a fixed period, so a slot holds until its label has finished scrolling and then advances. Rotation therefore runs slightly irregularly, paced by content rather than a clock.

Before scrolling, the app strips common prefixes from the derived label. `feature/`, `bugfix/`, `hotfix/` and `worktree-` carry no information when every branch has one. `worktree-stoplight-readme` becomes `stoplight-readme`, which is shorter and more distinct.

### Button

One button on the case, wired to a spare GPIO and ground, using the internal pull-up. Firmware debounces it over about 50ms.

| Press | Action |
|---|---|
| Short, under 500ms | Pin or unpin the session on screen |
| Long, 500ms or more | Advance to the next session |

Pinning stops the rotation on the current session. Scrolling continues, because a pinned label still needs to be readable. A marker in the corner shows the pin.

A pin releases itself. It clears when that session ends, and it clears after a timeout even if the session is still alive. Without expiry you could pin something on a Monday, forget, and quietly stop seeing every other session for a week.

Pinning changes the screen and never the lamps. A pinned green session with a red session running still gives a red lamp. The pin must not be able to hide an alert.

The pin lives in the firmware, so it survives a dropped link and reconnection. It is a property of the device rather than of the session list, which is what will let it survive a dropped BLE link too.

Long press was chosen over a double tap. A double tap needs a timing window between presses, and pressing a button set into a small case rocks the case on the desk, which makes that window unreliable. A long press has no window, only a duration, and a missed long press reads as a short press instead of doing nothing visible.

## Protocol

Two hops. Producers talk to the relay over HTTP, specified in [RFC 1](rfc.md). The relay then talks to the light, described here.

```
  any producer ──HTTP──▶ relay ──USB serial──▶ light
                 RFC 1            frame format below
                 public           private, may change
```

Only the first hop is a public contract. The frame format below is between this relay and this firmware, and can change without breaking a single producer.

One JSON object per line, newline-terminated, over a USB serial port. The format is transport-agnostic on purpose: when BLE lands it carries the same lines over a characteristic, and the firmware parses one format either way.

The relay sends the whole session list on every change, not one line of text.

```json
{"color":"red","sessions":[
  {"id":"a1","label":"stoplight-readme","state":"needs you","color":"red"},
  {"id":"b2","label":"adele-api","state":"working","color":"yellow"}
]}
```

| Field | Values | Notes |
|---|---|---|
| `color` | `red`, `yellow`, `green`, `off` | Aggregate. Drives the lamps. |
| `sessions` | array | One entry per active session, ordered by start time |
| `sessions[].id` | any string | Stable for the life of the session |
| `sessions[].label` | any string | Top line. Scrolled by the firmware if it overflows. |
| `sessions[].state` | any string | Bottom line |
| `sessions[].color` | `red`, `yellow`, `green` | This session's own state |

Every field is optional. A message carrying only `color` moves the lamps and leaves the screen alone. Unknown fields are ignored, so the app and firmware can be updated independently.

Sending the full list rather than rotation frames keeps the link quiet and puts rotation, scrolling and pinning in the firmware, where they keep working if the link drops. That matters little over a wire and matters a great deal over a radio, which is why the format was chosen this way before BLE exists. The `id` field is what lets a pin survive an update to the list.

The screen is monochrome and holds about ten characters per line. Colour lives in the lamps, not the display.

JSON costs a few bytes over a single-character protocol and pays that back the first time you open a serial monitor and can read what is happening.

## Hardware

The light is an ESP32 with BLE hardware, three LEDs, a screen, and a LiPo battery. The firmware talks over USB serial today. The radio is on the board and unused, waiting for the BLE transport.

### Board

The board is an **ESP32-C3 SuperMini with 0.42 inch OLED**.

It is 25 × 20.5mm, roughly a postage stamp, with a 72×40 OLED soldered on and a ceramic BLE antenna. The screen is part of the board, so there is no separate display module and no I²C wiring. Thirteen GPIOs are broken out, which is far more than the three the lamps need.

The screen is small. The lit area is 9.2 × 5.2mm, and it holds about ten characters per line across five lines. That is enough for a branch name and a state word, which is all the label needs to be. It is a status light, not a terminal.

The board has no charging circuit, so a TP4057 module supplies one for three wires.

The panel is a 72×40 display driven by an SSD1306 controller that addresses a 128×64 buffer, so the visible window sits at a column offset of 28. Use the `U8G2_SSD1306_72X40_ER_F_HW_I2C` constructor in U8g2, which applies the offset for you. A generic 0.42 inch driver renders off-screen and looks like a dead panel. I²C is on GPIO 5 for `SDA` and GPIO 6 for `SCL`, at address `0x3C`.

Flash it as `ESP32C3 Dev Module` with `USB CDC On Boot` enabled, 4MB flash, QIO mode.

### Bill of materials

Nine parts, one order, and you probably own three of them already.

| Item | Qty | ASIN | Notes |
|---|---|---|---|
| ESP32-C3 SuperMini with 0.42 inch OLED | 1 | `B0H2GWGP9Q` | 5 pack. BLE, USB-C, screen built in. |
| TP4057 charging module, USB-C | 1 | `B0CDWZ9MDC` | 5 pack. 1A, protection built in. |
| LiPo battery, 500mAh, 503035 | 1 | `B0BJPG4B72` | 35 × 30 × 5mm, protection board built in |
| 220Ω resistors, ¼W | 3 | `B08QRFCMC9` | 100 pack, metal film, ±1% |
| 5mm LEDs, red, yellow, green | 3 | | Common enough to have already |
| Momentary button | 1 | | Pin and advance. A 6 × 6mm tactile switch is typical. |
| USB-C data cable | 1 | | Must carry data for flashing. Charge-only cables enumerate nothing. |
| Slide switch, SPDT | 1 | | Optional. Cuts battery drain when the light is put away. |
| Perfboard and 30AWG wire | 1 | | For the permanent build. Thin wire: the cavity is small. |

The boards, charging modules and resistors only sell in multipacks, so one order builds about five lights. Treat the spares as insurance, because these parts are cheap and easy to kill with a soldering iron.

Add a half-size breadboard and jumper wires if you want to test before soldering. Worth it.

Check the ASIN and the size selector at checkout. Suppliers list many values, pack sizes and connector types under near-identical titles, and the resistor pack in particular chooses its resistance through a size option.

The battery is the only part with no substitute on hand, and cell listings often ship slowly. Order it first, or accept that it arrives last. Nothing early in the build needs it, because the board runs from USB while you write firmware.

### Wiring

The screen needs no wiring. It is soldered to the board and the firmware drives it over I²C on GPIO 5 and 6, which are committed to the display.

That leaves the LEDs. Each anode connects through a 220Ω resistor to a GPIO pin. All cathodes share a common ground rail.

```
   ESP32-C3 SuperMini
   ┌───────────────────┐
   │ ▉▉ 0.42" OLED  ▉▉ │      220Ω goes BEFORE the LED anode
   │                   │      ──────────────────────────────
   │ Soldered on. I²C  │      long leg  = anode  = resistor side
   │ on GPIO 5 and 6.  │      short leg = cathode = ground side
   │ No wiring at all. │
   │                   │                      long       short
   │                   │                        ╷          ╷
   │                   │                    ╭───┴──────────┴─────╮
   │           GPIO 0  ├──────▉▉▉▉──────────┤   ● RED            ├────┐
   │                   │                    ╰────────────────────╯    │
   │                   │                    ╭───┴──────────┴─────╮
   │           GPIO 1  ├──────▉▉▉▉──────────┤   ● YELLOW         ├────┤
   │                   │                    ╰────────────────────╯    │
   │                   │                    ╭───┴──────────┴─────╮
   │           GPIO 2  ├──────▉▉▉▉──────────┤   ● GREEN          ├────┤
   │                   │                    ╰────────────────────╯    │
   │                   │                                              │
   │           GPIO 3  ├───────o   o────────────────────┐             │
   │                   │       button, no resistor      │             │
   │                   │                                │             │
   │           GND     ├────────────────────────────────┴─────────────┘
   └─────┬─────────────┘                one common ground rail
         ╧
       USB-C
```

| LED | GPIO |
|---|---|
| Red | 0 |
| Yellow | 1 |
| Green | 2 |

The button goes to GPIO 3 and ground. No resistor: the firmware enables the internal pull-up, so the pin reads high until the button pulls it low.

Avoid GPIO 5 and 6, which drive the screen, and GPIO 8 and 9, which carry the onboard LED and the boot strapping. Confirm the numbers against your board pinout before soldering.

Polarity matters. The long leg of an LED is the anode and goes toward the resistor and GPIO pin. The short leg is the cathode and goes toward ground. Backwards, the LED stays dark.

Breadboard the circuit before you solder it. The enclosure dimensions have never been checked against real parts, and a reversed LED on a breadboard costs seconds. [firmware/readme.md](firmware/readme.md#prove-each-led-after-wiring) gives a three-command sequence that lights red, then yellow, then green, so a miswired LED names itself.

Power runs through the charging module. The battery connects to the `B+` and `B-` pads, and the module's `OUT+` and `OUT-` feed the board's `5V` and `GND` pins. Put the optional slide switch in the `OUT+` line. Charge over the module's own USB-C port, and use the board's USB-C port for flashing.

Check the silkscreen before soldering. Pad names vary between the TP4056 and TP4057 and between suppliers, and some boards label the load side `+` and `-` only. Getting the battery backwards destroys the module.

A 1A module charges a 500mAh cell at about 2C, which is faster than the cell prefers. The protection circuit makes this safe rather than ideal, and the module runs warm while charging. Lower the programming resistor if your board has one, or accept a fast charge and a warm module.

### Power

BLE is what will let this run on a battery, and until it exists the USB cable powers the light as well as carrying its frames. The rest of this section describes the battery build the BLE step unlocks, and the parts list already covers it.

The radio idles at a few milliamps, so the LEDs dominate the draw. A 500mAh cell should give a day or two of use, because only one lamp is lit at a time and the light is idle for most of a working day. That is arithmetic, not measurement: real numbers come after the BLE transport does.

Two things extend it. The firmware dims the LEDs with PWM rather than driving them at full current, which costs nothing visually behind a diffuser. It also blanks the screen after a period with no updates and wakes it on the next message.

The C3 has no fuel gauge, so the firmware cannot report a true battery percentage. If that matters more than size, the TFT Feather has a MAX17048 and reports real numbers. A larger cell also fits: a 603443 at 1000mAh is 43 × 34 × 6mm and still clears the bay with a 2mm deeper body.

## Enclosure

Four printed parts: a body, three lens inserts, a back cover, and a button plunger. About **36 × 78 × 18mm**, roughly a matchbox standing upright.

```
     ┌───────────────┐
     │      ( )      │   recessed button, found by touch
     ├───────────────┤
     │   ╲ ╭─────╮   │
     │     │  ●  │ R │   visors hood each lamp
     │   ╲ ╰─────╯   │
     │     ╭─────╮   │   battery hides upright
     │   ╲ │  ●  │ Y │   behind the lamp column
     │     ╰─────╯   │
     │   ╲ ╭─────╮   │
     │     │  ●  │ G │
     │     ╰─────╯   │
     ├───────────────┤
     │  ┌─────────┐  │
     │  │ auth-api│  │   board clips into printed rails
     │  │ ● needs │  │   no screws
     │  └─────────┘  │
     └───────────────┘
       ├── 36mm ──┤
```

The body takes a few hours to print. A lens takes two minutes, which is why you print one first to check your printer's tolerances before committing to the rest.

Everything else — dimensions, print settings, the parametric source, and the STLs — is in **[`enclosure/`](enclosure/readme.md)**.

## Layout

```
main.go               entry point and command dispatch
internal/stoplight/   state machine, session tracking, aggregation
internal/relay/       the long-running process, HTTP and socket ingest
internal/adapter/     one package per agent, claudecode is the first
internal/service/     launchd, systemd and scheduled task install
internal/transport/   USB serial today, BLE planned, behind one interface
internal/light/       virtual light for development
internal/notify/      the one-shot client used by hooks
firmware/             ESP32 firmware
```

Transports sit behind one interface, so nothing above it cares which is connected. `internal/transport/serial` is the only hardware transport that exists; BLE will be a second package implementing the same interface, and nothing above it changes when it lands. The virtual light implements that interface too and prints to the terminal, so the whole system is testable without hardware.

Adding another agent means adding a package under `internal/adapter/`. Nothing else changes, and agents that can post directly need no adapter at all.

Full detail in [design.md](design.md).

## Commands

| Command | What it does |
|---|---|
| `stoplight install` | Hook entries, service, and start it. The only setup step. |
| `stoplight uninstall` | Remove the hooks and the service |
| `stoplight` | Run the relay in the foreground. For development. |
| `stoplight status` | Service state, connection, current sessions |
| `stoplight restart` | Bounce the service |
| `stoplight logs` | Tail the log |
| `stoplight service stop` | Stop until next login |
| `stoplight service disable` | Stop and do not start at login |
| `stoplight task --session-id <id> <text>` | Override the screen label for that session |
| `stoplight task --session-id <id> --clear` | Remove the override and show the derived label again |
| `stoplight notify <event>` | One-shot. Called by hooks, not by you. |
| `stoplight help` | The same command list, printed |

Flags on the bare relay command:

| Flag | What it does |
|---|---|
| `--virtual` | Use the virtual light instead of hardware |
| `--serial <path>` | Use a specific serial device instead of auto-discovering one |
| `--addr <host:port>` | HTTP listen address, default `127.0.0.1:7373` |
| `--timeout <duration>` | Session silence timeout, default `30m` |

`--virtual` and `--serial` are mutually exclusive. With neither, the relay looks for a serial light and falls back to the virtual one, saying so, because a silent fallback looks exactly like broken hardware. `status` and `task` take `--addr` too, for a relay on a non-default port.

## Build it

| | |
|---|---|
| [`rfc.md`](rfc.md) | **RFC 1** — the protocol. Read this to connect your own agent. |
| [`design.md`](design.md) | State machine, Go types, package layout, internals |
| [`enclosure/`](enclosure/readme.md) | Printable case: OpenSCAD source, STL generator, and the STLs |
| [`roadmap.md`](roadmap.md) | Project status, build order, and the decisions behind it |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | How to work on the app, an adapter, the firmware, or the case |

You do not need the hardware to start. The Go app ships with a virtual light that renders to your terminal, so most of the project is testable on a laptop with nothing plugged in.

## License

[Apache License 2.0](LICENSE). Copyright 2026 Cidekar, LLC.

Build it, sell it, put it on your desk at work. The licence asks only that you keep the notice and state what you changed.
