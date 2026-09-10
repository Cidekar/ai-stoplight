# Contributing to Stoplight

## Table of Contents

- [Quick Start](#quick-start)
- [What lives where](#what-lives-where)
- [Development Workflow](#development-workflow)
- [Testing](#testing)
  - [Five minutes to a working light](#five-minutes-to-a-working-light)
  - [Do not run `stoplight install`](#do-not-run-stoplight-install-to-test-it)
- [Hardware and Firmware](#hardware-and-firmware)
- [The Enclosure](#the-enclosure)
- [Code Standards](#code-standards)
- [Release Process](#release-process)
- [Getting Help](#getting-help)

## Quick Start

### Prerequisites

- Go 1.25 or later. This was 1.24 until the BLE transport landed: `tinygo.org/x/bluetooth` declares `go 1.25.0`, so the module does too. Staying on 1.24 was possible only by pinning the library back to v0.11.0, a release from two years earlier, which is a worse trade than asking for a current toolchain.
- Git
- The Arduino IDE or `arduino-cli`, only if you are working on firmware. This is a free toolchain you install on your computer to compile and flash code onto the ESP32. You do not need an Arduino board; the ESP32 is the only microcontroller in this project.
- A 3D printer and OpenSCAD, only if you are working on the enclosure

You do **not** need the hardware to contribute to most of this project. The Go app ships with a virtual light that renders lamp colour and screen text to your terminal, so session tracking, the priority rules, rotation, scrolling and button logic are all testable on a laptop with nothing plugged in.

### Setup

```bash
git clone https://github.com/cidekar/stoplight.git
cd stoplight

go mod download
make check

# Run against the virtual light
go run . --virtual
```

`make check` is the full pre-pull-request run: lint, cross-platform compile, tests, and the Linux container tests. `make` on its own lists every target.

## What lives where

| Path | What it is | Who it needs |
|---|---|---|
| `rfc.md` | The protocol. The public contract. | Everyone |
| `design.md` | State machine, types, internals | Go developers |
| `main.go`, `internal/` | The Go relay and CLI | Go developers |
| `internal/adapter/` | One package per agent | Anyone adding a tool |
| `internal/light/` | Virtual light for development | Go developers |
| `internal/transport/` | Serial and BLE, behind one interface | Go developers |
| `firmware/` | ESP32 firmware | An ESP32 board, and the Arduino toolchain to flash it |
| `enclosure/` | OpenSCAD source, STL generator, STLs | A printer |
| `roadmap.md` | Build order and open questions | Everyone |

Contributions to any one of these are welcome on their own. A better diffuser profile is as useful as a bug fix in the state machine.

## Adding support for another agent

The most useful contribution right now. Two routes.

**No code needed.** If your agent can run a command or make an HTTP request at the right moments, it already works. Post to `/v1/session` per [RFC 1](rfc.md) and you are done. If you write a wrapper script worth sharing, send it and we will link it.

**An adapter.** If your agent has a hook or plugin system, an adapter can install itself and make setup one command for everyone else.

```go
type Adapter interface {
    Name() string
    Install(binPath string) error
    Uninstall() error
    Installed() (bool, error)
}
```

Add a package under `internal/adapter/<name>`, map that agent's events onto the four in RFC 1, and register it. No other package changes. `internal/adapter/claudecode` is the worked example.

Two rules for adapters. Install must be idempotent, and uninstall must leave the host's config exactly as it was found, including entries other tools put there. People will run these against config files they care about.

## Development Workflow

Stoplight uses a fork-and-pull workflow. All work targets the `development` branch.

### External Contributors

**Target branch:** `development`

1. **Fork the repository** and clone your fork locally.

2. **Create a feature branch**

   ```bash
   git checkout -b feature/your-feature-name
   ```

3. **Make changes**

   - Write code following the conventions below
   - Add tests for new behaviour
   - Update documentation in the same PR

4. **Submit a pull request**

   - Push to your fork
   - Run `make check` yourself first. **There is no CI.** No `.github/` workflow exists, so nothing runs your tests when you open a PR. `make check` is the substitute, and it is on you.
   - Open a PR targeting `development`
   - Say in the PR that `make check` passed, and on which platform

5. **Review and merge**

   - Maintainer review
   - Incorporate feedback
   - Merge to `development`

### Internal Contributors

Same flow, with direct branch access rather than a fork.

```bash
git checkout development
git pull origin development
git checkout -b feature/your-feature-name
```

## Testing

### Five minutes to a working light

Do this first. It is the whole product running on your laptop with nothing plugged in, and it takes about five minutes. Everything below it assumes you have seen the light change colour at least once.

Build to a scratch path and run the relay on a port that collides with nothing. The default is 7373; 7411 is used here so this walkthrough cannot disturb a relay you already have running.

```bash
go build -o /tmp/sl .
/tmp/sl --virtual --addr 127.0.0.1:7411
```

Leave that running and open a second terminal. These two helpers keep the rest of the walkthrough short:

```bash
post() {
  curl -s -m 2 -X POST http://127.0.0.1:7411/v1/session \
    -H 'Content-Type: application/json' -d "$1"
}
agg() { curl -s -m 2 http://127.0.0.1:7411/v1/status; echo; }
```

Now drive two sessions through a full lifecycle. Watch the first terminal after every command: the virtual light redraws the lamps and the screen on each frame.

```bash
post '{"session_id":"a","event":"started","label":"refactor the tracker"}'
post '{"session_id":"b","event":"blocked","label":"needs review"}'
post '{"session_id":"b","event":"finished"}'
post '{"session_id":"a","event":"finished"}'
post '{"session_id":"a","event":"ended"}'
post '{"session_id":"b","event":"ended"}'
```

The aggregate colour after each step:

| Step | Event | Aggregate | Why |
|---|---|---|---|
| 1 | `a` started | **yellow** | one session working |
| 2 | `b` blocked | **RED** | most urgent wins across sessions, whatever the screen shows |
| 3 | `b` finished | **yellow** | `b` is green, but `a` is still working and yellow beats green |
| 4 | `a` finished | **green** | both done |
| 5 | both ended | **off** | nothing is running |

**Green and off are not the same thing, and this is the transition people get wrong.** Green means finished: work happened and it completed, and the light says so until you clear it. Off means nothing is running at all. An `ended` event removes the session, so the lamp goes dark rather than green. If you expect `ended` to leave a green light you have the model backwards, and a device that stayed green after every session ended would be green almost all the time and worth nothing at a glance.

### Reading the relay state

`GET /v1/status` is the fastest way to see what the relay thinks, and it is the check to reach for when the light is not doing what you expected.

```bash
curl -s http://127.0.0.1:7411/v1/status
```

After step 2 above, trimmed:

```json
{
  "transport": "virtual",
  "connected": true,
  "aggregate": "red",
  "uptime_seconds": 27,
  "sessions": [
    {"id": "a", "label": "refactor the tracker", "state": "working",   "color": "yellow"},
    {"id": "b", "label": "needs review",         "state": "needs you", "color": "red"}
  ]
}
```

`aggregate` is what the lamp shows. Each session also carries its own `color`, which may be less urgent than the aggregate — that is the aggregation rule visible on the wire. Sessions are never `null`: an empty desk marshals as `[]`. Full fields in `internal/relay/status.go`.

### An invariant you can check by hand

`stoplight task` renames a session on the screen. It must not be able to change the lamp, and that is worth ten seconds of your time because it was a real defect: the command used to post an `idle` event to ingest, which drove the session green. Naming the task you are blocked on is exactly the moment the light must stay red.

```bash
post '{"session_id":"c","event":"blocked","label":"waiting on you"}'
agg   # aggregate: red

/tmp/sl task --addr 127.0.0.1:7411 --session-id c "merge conflict in cmd.go"
agg   # label changed, aggregate STILL red

/tmp/sl task --addr 127.0.0.1:7411 --session-id c --clear
agg   # label back to "waiting on you", aggregate STILL red

post '{"session_id":"c","event":"ended"}'
```

The label moves and the lamp does not. `--clear` removes the override so the producer's label shows again; it is the same request with an empty label, not a second verb. `task` posts to `/v1/task`, which carries no event and so runs no state transition. Unlike `notify`, it reports errors rather than swallowing them, because a human typed it and is owed an answer.

When you are finished, stop the relay with Ctrl-C in the first terminal.

### The firmware and enclosure harnesses

Neither needs hardware, and both are quick.

```bash
cd firmware/test && make          # 303 checks under ASan and UBSan
cd enclosure && python3 test_params.py
```

The firmware harness compiles the firmware logic against host stubs with `-fsanitize=address,undefined` and `halt_on_error=1`, so a memory bug fails the run rather than producing a strange number. It ends with `303 checks, 0 failures`. `test_params.py` runs 41 checks and ends with `all checks passed`; it is described under [The Enclosure](#the-enclosure).

### What you cannot test without hardware

Be honest about this in a pull request. Three things do not have a host-side check, and two of them fail in ways that look like something else.

**BLE needs a board advertising.** The chunking, reassembly and every failure path are covered by tests against a fake link, but nothing on a laptop proves the relay talks to a real radio. That needs an ESP32 with the BLE firmware flashed and switched on.

**The 72×40 display offset cannot be verified on a host.** The offset lives inside the U8g2 constructor, and the host harness replaces that constructor with a stub that records nothing about geometry. There are no pixels to inspect. All 303 checks pass identically with the right constructor and the wrong one. Get it wrong and the panel renders outside the visible window, so it looks **dead** — no flicker, no clipped text, nothing to distinguish it from a broken screen or a wiring fault. It must be `U8G2_SSD1306_72X40_ER_F_HW_I2C`. See [The display offset](#the-display-offset).

**Battery life is measured, not calculated.** The readme's day-or-two figure is arithmetic from an idle radio and one lamp lit at a time. Real numbers need a charged cell, a running radio and a clock.

### Do not run `stoplight install` to test it

**`install` changes your real machine, and redirecting `HOME` does not sandbox it.** Read this before you go looking for a quick way to exercise the adapter or service code.

`stoplight install` does two things to the computer you are sitting at:

- It writes hook entries into your **real** `~/.claude/settings.json`.
- It registers a **real** background service: a launchd agent on macOS, a systemd user unit on Linux, a Task Scheduler job on Windows.

Setting `HOME` to a temp directory looks like it should contain this. It does not. The plist path follows `HOME`, but the launchd *domain* is `gui/<uid>` taken from `os.Getuid()`, so `launchctl bootstrap` still registers the job in your live login session. systemd and `schtasks` resolve their own paths the same way. The service manager was never reading `HOME` for the part that matters. An agent working on this project learned that the hard way and left a stale launchd job behind.

If you have already run it, clean up:

```bash
stoplight uninstall                                    # the supported route, all platforms

launchctl bootout gui/$(id -u)/com.stoplight.relay     # macOS, if uninstall did not
rm -f ~/Library/LaunchAgents/com.stoplight.relay.plist

systemctl --user disable --now stoplight.service       # Linux
rm -f ~/.config/systemd/user/stoplight.service && systemctl --user daemon-reload

schtasks /Delete /TN \Stoplight\Relay /F               # Windows
```

Then check your `~/.claude/settings.json` for a leftover hook entry pointing at a binary you have since deleted.

**Test this code the way the existing tests already do:** against temp fixtures and injected runners. Every adapter test writes to a `t.TempDir()` settings file, and every service manager takes its `run` function as a field so the tests substitute a fake and no real `launchctl`, `systemctl` or `schtasks` is ever invoked. That is why `make check` is safe to run and `install` is not. If you are adding a platform or an adapter, follow that pattern rather than reaching for the real thing.

### Before you open a pull request

One command runs everything:

```bash
make check
```

It is `make lint`, `make cross`, `make test` and `make test-linux` in order. Run it before every pull request.

**This project has no CI.** There is no `.github/` directory and no workflow, so nothing checks a pull request automatically. `make check` is what CI would run if it existed, which is why it covers cross-compilation and the Linux container as well as the tests for your own platform. Until CI exists, running it is the contributor's job and a reviewer has no green tick to rely on.

| Target | What it does |
|---|---|
| `make test` | The tests for your platform, with the race detector |
| `make lint` | `go vet` for every platform, and a `gofmt` check that fails on any unformatted file |
| `make build` | Build the binary |
| `make cross` | Build **and** compile the tests for Linux, Windows and macOS |
| `make test-linux` | Run the Linux tests for real, in a container |
| `make check` | All of the above |

### Why `go test ./...` is not enough

The `internal/service` package has one manager per platform behind a build tag: `systemd.go` is `//go:build linux`, `schtasks.go` is `//go:build windows`. On macOS the Go tool never compiles either one, so `go test ./...` skips roughly a third of the package and reports success. Code that is never compiled is never tested, and a *failing* test in a tagged file looks exactly like a passing one — that is how a data-loss bug in `systemd.Uninstall` once sat in the tree behind a green suite.

`make cross` closes the compile gap on any machine. `make test-linux` closes the execution gap: it cross-compiles the test binaries and runs them in Alpine, so the systemd tests actually execute. It needs Docker; without it the target prints a skip and passes, so `make check` still works on a laptop with no container runtime. If you touch `internal/service`, run it with Docker up.

### Unit tests

```bash
go test ./...
```

### Against the virtual light

The virtual light implements the same transport interface as USB serial, so the whole system runs without hardware. That is what [Five minutes to a working light](#five-minutes-to-a-working-light) walks through, and it is the fastest way to exercise session tracking, the priority rules, rotation and the button with nothing plugged in.

```bash
go run . --virtual
```

### Against real hardware

If you have a light built, plug it in over USB, or switch it on and let the relay find it over Bluetooth.

```bash
go run .                              # auto-discover: serial first, then BLE
go run . --serial /dev/cu.usbmodem*   # a specific USB device
go run . --ble                        # the nearest Bluetooth light
go run . --ble-name StoplightA4       # one named Bluetooth light
```

With no light found on either transport, the relay says so and falls back to the virtual light rather than failing.

**Auto-discovery tries serial before BLE.** A plugged-in cable is an unambiguous statement of intent and a radio in range is not, because a light on somebody else's desk can advertise into the room. Serial discovery is also a filesystem glob that answers in microseconds, where a BLE scan costs seconds of radio time before the relay can start listening. And serial is the debuggable link, so somebody with both attached is usually mid-debug and wants the wire. Pass `--ble` to say otherwise.

`--ble` and `--ble-name` are a decision rather than a hint: neither falls back to serial. The transport retries forever, so a light switched on later is still picked up.

### Bluetooth

The BLE transport is [roadmap](roadmap.md) step 5, and it lives in `internal/transport/ble` behind the same `Transport` interface as serial. Nothing above the transport layer knows which one is in use.

**The light is matched by service UUID, never by name.** Names collide, and users rename things. The service and characteristic UUIDs are exported constants in `internal/transport/ble`, and the firmware is written against the same two values:

| | UUID |
|---|---|
| Service, advertised | `6e5d0001-b5a3-f393-e0a9-e50e24dcca9e` |
| Frame characteristic, write | `6e5d0002-b5a3-f393-e0a9-e50e24dcca9e` |

**Frames are chunked.** A BLE write cannot exceed the negotiated ATT MTU, which is 20 bytes on a link that never upgraded and up to 244 on one that did, while a frame runs to about 1200 bytes. The transport splits a frame into MTU-sized pieces and the firmware reassembles them by scanning for the newline delimiter, exactly as the serial reader already does. The full contract is `ble.ChunkContract` in `internal/transport/ble/contract.go`. It is a constant rather than a comment so both sides quote one source, and changing it breaks the firmware.

You need a board with the BLE firmware flashed to test this for real. Everything else about the transport, including the chunking and every failure path, is covered by tests with a fake link and needs no hardware.

On macOS you get a one-time Bluetooth permission prompt. Central-role scanning works from an unsigned CLI binary, so no entitlement or app bundle is needed. If you decline the prompt, the relay says so and stops rather than retrying a decision only you can reverse; auto-discovery just skips Bluetooth and moves on.

### Test requirements

- New behaviour needs a test
- State machine changes need a test covering multiple concurrent sessions
- Anything touching the aggregate rule needs a test proving a pinned or rotated screen cannot hide a red lamp
- Anything touching ingest needs a test proving unknown events and unknown fields are ignored rather than rejected, per [RFC 1 §11](rfc.md#11-compatibility)

That last one matters more than it sounds. The device's whole value is that a green glance is trustworthy. A bug that lets a red session go unreported is the one bug this project cannot ship.

## Hardware and Firmware

### Flashing

Board settings for the ESP32-C3 SuperMini:

- Board: `ESP32C3 Dev Module`
- USB CDC On Boot: **Enabled**
- Flash size: 4MB
- Flash mode: QIO

### The display offset

The 0.42 inch panel is 72×40 but its SSD1306 controller addresses a 128×64 buffer, so the visible window sits at a column offset of 28. Use the `U8G2_SSD1306_72X40_ER_F_HW_I2C` constructor, which applies the offset for you. A generic 0.42 inch driver renders off-screen and the panel looks dead.

I²C is on GPIO 5 for `SDA` and GPIO 6 for `SCL`, at address `0x3C`.

### Pin budget

| Pin | Use |
|---|---|
| 0, 1, 2 | Red, yellow, green LEDs |
| 3 | Button, internal pull-up |
| 5, 6 | Screen I²C, do not reuse |
| 8, 9 | Onboard LED and boot strapping, avoid |

### Protocol changes

The app and firmware talk in line-delimited JSON, and both sides ignore unknown fields. That is deliberate: it lets either side be updated without breaking the other. If you add a field, keep it optional and make the old behaviour the default when it is absent.

## The Enclosure

Models are parametric. Change a number, do not redraw geometry.

**Every dimension lives in `enclosure/params.json`, and nowhere else.** The OpenSCAD parameter block, the STL generator and the tables in `enclosure/readme.md` are all generated from it.

```bash
cd enclosure
# edit params.json
python3 params.py       # rewrites the parameter block in stoplight.scad
python3 gen_stl.py      # rewrites stl/*.stl
python3 gen_docs.py     # rewrites the tables in readme.md
python3 test_params.py  # proves nothing drifted
```

`test_params.py` is the guard. It compares every value in the `.scad` and the readme against the JSON and fails if they disagree, and it checks geometry sanity: bores clearing the button pocket, lamps not overlapping, sections summing to the stated height. Run it before opening a PR that touches the enclosure.

Do not hand-edit the block between the generated markers in `stoplight.scad`. It will be overwritten and the test will catch you first.

Useful contributions here:

- Fit tolerances for printers other than the one this was developed on
- A better diffuser profile for the lens inserts
- Mounts for other boards or battery sizes
- Photographs of finished builds

If you print one, please open an issue with your printer, filament, and the `fit` value that worked. Press fits vary enough between machines that a table of known-good values is worth more than any single default.

## Code Standards

### Go conventions

- Standard formatting, `go fmt`
- Meaningful names over short ones
- Handle errors, do not discard them
- Document exported functions and types

### Dependencies

**The standard library is the default. A third-party dependency is allowed when writing the thing ourselves would be absurd, and it has to be argued for in the pull request.**

This used to read "standard library only", and the whole relay was written that way. The serial transport is the reason it worked for so long: a USB CDC-ACM device ignores baud rate and framing, so opening the device as an ordinary file does everything a serial library would do, and the rule cost nothing. Most of what a project like this reaches for is like that. HTTP, JSON, filesystem work and process management are all in the standard library already, and a dependency added for one of those buys convenience at the price of a supply chain.

Bluetooth is not like that. `internal/transport/ble` depends on `tinygo.org/x/bluetooth`, and it is the first third-party dependency in the tree.

The reason is that there is no such thing as "the Bluetooth API". macOS speaks CoreBluetooth through Objective-C, Linux speaks BlueZ over D-Bus, and Windows speaks WinRT. Three unrelated stacks, three unrelated object models, and on macOS a delegate-callback design that has to be bridged out of Objective-C before Go can see it. Writing that ourselves is not a transport, it is a second project, and it is one where every bug looks like broken hardware. `tinygo.org/x/bluetooth` wraps all three behind one API, cross-compiles cleanly to all three targets, and is the library the TinyGo project maintains for exactly this.

The bar stays high. Before adding a dependency:

- Say what it would take to write it ourselves. If the answer is "a few hundred lines of standard library", write it.
- Prefer one that cross-compiles cleanly. `make cross` builds for Linux, Windows and macOS, and a dependency that breaks a platform build breaks the release.
- Prefer one with no dependencies of its own, or few. Check what it drags in with `go mod graph`.
- Keep it behind a package boundary. The BLE library is reachable only from `internal/transport/ble`, so the rest of the tree still compiles against the `Transport` interface and nothing else. A dependency confined to one package can be replaced; one that has spread through the tree cannot.
- Do not add one to `internal/stoplight`. The state machine holds the invariants and imports nothing, not even from this project. That stays true.

### The prime directive

A producer must never break its caller. `stoplight notify` runs inside someone's hook or wrapper script, so it exits 0 whatever happens: relay down, Bluetooth off, light unplugged, socket missing, malformed input. If you add a code path to `notify`, it exits 0.

The same rule binds every client in [RFC 1 §13](rfc.md#13-reference-implementations). If you contribute one in a new language, it swallows its errors.

### Commit messages

```
feat: add session pinning to the firmware
fix: stop a pinned green session masking a red lamp
docs: record the SSD1306 offset gotcha
test: cover three concurrent sessions
firmware: dim lamps with PWM to extend battery life
enclosure: widen the LED hole for flanged 5mm parts
```

## Release Process

### Branches

- **`main`** — production ready, protected
- **`development`** — integration branch for all work
- **`feature/*`** — individual features

### Versioning

Semantic versioning, `vMAJOR.MINOR.PATCH`.

Firmware and app versions move together. The protocol tolerates a mismatch by design, but a matched pair is what gets tested and what gets released.

### Release steps

1. Work merges to `development`
2. A release PR opens from `development` to `main`
3. `make check` on the release branch, then review
4. Merge, tag, and publish binaries plus a firmware image

Enclosure STLs are attached to releases so people can print without running a generator.

## Getting Help

### Issues and questions

- **Bug reports** — GitHub Issues, with your OS, Go version and board
- **Feature requests** — GitHub Issues
- **Build help and photos** — GitHub Discussions
- **Security concerns** — email the maintainers directly, do not open a public issue

### Good bug reports

For the Go app, include the log at `~/.local/state/stoplight/stoplight.log`. It records whether a hook fired, whether the relay saw it, and whether the light acknowledged it, which usually isolates the fault to one of the three parts immediately.

For firmware, include the board, the Arduino core version, and serial output.

For the enclosure, include your printer, slicer, filament and the parameters you changed.

### Contributing guidelines

- Start small
- Ask early
- Be patient with review

---

**Questions?** Open a GitHub Discussion or reach out to the maintainers.
