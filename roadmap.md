# Roadmap

## Status

**Working over USB serial and over Bluetooth. Not yet on a battery.**

Plug the light in and the whole chain runs: any producer posts to the relay, the relay tracks every session, and the lamps and screen follow. The Claude Code adapter installs itself, the relay installs as a service on all three platforms, and the enclosure generator produces printable STLs. The firmware's host harness passes 440 behavioural checks under the sanitizers.

The radio is closed. Both ends of BLE are written and both were driven against the real board: the relay finds the light by service UUID, and all three lamps were confirmed by eye over the radio with the cable supplying power only. `--ble` selects it, and with no flag the relay looks for a serial light first and a Bluetooth one second. One firmware image serves both links, so which one is used is a decision the relay makes at run time rather than one taken at flash time.

The gap now is power. Nothing has ever run off a battery, and the low-battery warning has no mechanism: the C3 carries no fuel gauge, so a percentage needs a divider on an ADC pin or a different board.

## Build order

Status reflects what is in the tree, not what is planned.

| # | Step | Needs hardware | Status |
|---|---|---|---|
| 1 | State machine, session tracking, aggregation, virtual light. | No | **Done** |
| 2 | RFC 1 ingest: the HTTP endpoint and the unix socket. Curl can drive the light. | No | **Done** |
| 3 | The Claude Code adapter, plus service install for macOS. | No | **Done** |
| 4 | ESP32 firmware over USB serial. Lamps first, then the screen. | Yes | **Done**, verified on real hardware |
| 5 | BLE transport on both ends. | Yes | **Done**, verified on real hardware |
| 6 | Battery, power tuning, and low-battery warning. | Yes | **Partial** |
| 7 | Enclosure fit and finish against real parts. | Yes | **Partial** |
| 8 | Service install for Linux and Windows. | No | **Done** |

**Step 1.** `internal/stoplight` holds the state machine, the tracker and label derivation, and `internal/light` renders frames to the terminal. The transition table in this project's [design](design.md#transitions) is mirrored in Go as `rfcSection7Table` and asserted row by row. Caps on session count and field size came later, after probes found unbounded growth.

**Step 2.** `internal/relay` serves `POST /v1/session` and the unix socket, and grew two endpoints the original plan did not name: `POST /v1/task` and `GET /v1/status`. Both are documented, the first in [RFC 1 §5.3](rfc.md#53-the-task-endpoint) and the second in [design.md](design.md#the-http-endpoints).

**Step 3.** `internal/adapter/claudecode` writes and removes the four hook entries without clobbering anything else in `settings.json`, and `stoplight install` wires up the adapter and the service in one command.

**Step 4.** `firmware/stoplight` drives the lamps over PWM, the 72×40 OLED, rotation, marquee scrolling and the button. It parses the frame format incrementally, so an oversized line still yields its aggregate colour. `firmware/test` runs the whole thing on a host: 440 checks, 0 failures, under AddressSanitizer and UndefinedBehaviorSanitizer. What the harness cannot check is the display offset, because there are no pixels on a host to inspect. See [firmware/readme.md](firmware/readme.md).

**Step 5.** Both ends. The host side is `internal/transport/ble`, which implements the `Transport` interface; `--ble` and `--ble-name` select it, and `selectTransport` auto-discovers serial first and BLE second. It matches the light by service UUID rather than by name, retries forever with backoff the way the serial transport does, and splits a frame into chunks that fit the negotiated ATT MTU. The `Transport` interface held: nothing above it changed.

The firmware side is `firmware/stoplight/blelink.{h,cpp}`. It publishes the service and characteristic, advertises the service UUID, and queues the bytes it receives. **It does not parse.** The contract is explicit that a BLE chunk is indistinguishable from a partial serial read, so chunks go into a byte queue and `loop()` drains that queue into the same `LineReader` the cable uses. Two readers would be two chances to disagree about the frame format, and the BLE one would be the one nobody watches.

The queue is also the thread handoff. The write callback runs on the Bluetooth stack's task rather than the Arduino loop task, and parsing there would touch `Display` and `Lamps` from a second thread when neither is synchronised.

The contract both ends implement is `ble.ChunkContract` in `internal/transport/ble/contract.go`, a Go constant rather than a comment so neither side can change it silently.

Verified on hardware on 2026-09-11: the relay found the light by UUID alone, and red, yellow and green were each confirmed by eye over the radio with the USB cable supplying power only. No send errors occurred across the session. The radio costs flash rather than correctness: the image went from 26% to 51% of program storage, and RAM from 5% to 7%.

This is also where the project's first third-party dependency arrived, `tinygo.org/x/bluetooth`. Three unrelated platform Bluetooth stacks is not a thing to hand-roll. See CONTRIBUTING.md for the rule that replaced "standard library only".

**Step 6.** The two firmware levers are in: the lamps run on PWM rather than at full current, and the screen powers the panel down after a period with no frames. The battery itself is not, and neither is any low-battery warning. The C3 has no fuel gauge, so a warning needs either a divider on an ADC pin or a different board.

This step is no longer blocked, only unstarted. Step 5 closing means there is finally something to measure: a light on a battery has to hold a BLE connection, and the radio is the load that decides run time. The arithmetic could not be done against a wired device.

**Step 7.** The models are parametric, every dimension lives in `enclosure/params.json`, and `gen_stl.py` produces the four STLs. `test_params.py` proves the `.scad`, the STL generator and the readme tables never drift from the JSON. None of it has been fitted against printed parts, which is the whole point of the step.

**Step 8.** `internal/service` has `launchd.go`, `systemd.go` and `schtasks.go` behind build tags, with `unsupported.go` for everything else. All three compile on every platform via `make cross`, and `make test-linux` runs the systemd tests for real in a container. That target exists because a data-loss bug in `systemd.Uninstall` once sat in the tree behind a green suite on macOS.

## Why this order

**Steps 1 to 3 need no parts**, which is the point. The virtual light renders lamp colour and screen text to the terminal, so session tracking, the priority rules, rotation, scrolling and the button logic are all testable and provable before any hardware exists. That is most of the project, and it is why most of the project is finished.

**Ingest comes before any adapter.** Step 2 makes curl a first-class client, which means the protocol is exercised by something that knows nothing about Claude Code before the Claude Code adapter is written. Build the general case first and the specific case is a translation table. Build it the other way and the general case inherits one tool's assumptions.

**Steps 4 and 5 are split deliberately.** Debugging a protocol over a wire is far easier than debugging it over a radio. Get the firmware talking over USB serial, prove the message format, then change only the transport. If BLE then misbehaves, the fault is in the radio layer and nowhere else. This is the split the project is sitting in right now, and it is working as intended: the frame format is proven over a cable, so BLE has one job.

**Step 7 comes after the electronics work.** The enclosure should follow parts you are holding, not numbers from a product listing. The models are parametric for exactly this reason: measure the real button and LEDs, change a variable, reprint.

**Step 8 is last** because the service is the one component that differs per platform. One working platform proves the design before it is repeated three times. In the event it ran early, because installing the relay as a service was the only way to use it daily, and the platform differences were smaller than expected.

## Open questions

**Battery life.** Still open, and no longer blocked: the radio exists, so there is something to measure. The two levers are already built, in that the firmware dims the lamps with PWM and powers the panel down when frames stop arriving. What is missing is a battery and a meter. The radio is the interesting load, because a light on a battery has to hold a connection rather than merely receive on one.

**Low-battery warning.** Newly sharpened by the build. The C3 has no fuel gauge, so a percentage needs a divider on an ADC pin, or a different board. Nothing is implemented, and it may end up as a lamp pattern rather than a number, because the screen holds ten characters and a percentage is a poor use of them.

**Enclosure fit.** Still open. The `fit = 0.20` clearance is a first guess, `test_params.py` proves only that the number is used consistently everywhere, and no part has been printed against real hardware.

**Button dimensions.** Still open. The models assume a 6 × 6mm tactile switch. A panel-mount button is often 12mm across and needs 10mm of depth, which changes the pocket, the top thickness and the overall height.

### Answered by the build

**Rotation timing.** Three seconds per session, with scrolling as a minimum rather than a fixed period, so a slot holds until its label finishes scrolling. Red jumps the queue immediately and holds for a full interval. The host harness proves the waiting and the jump; whether three seconds is comfortable is a judgement no test settles.

**How many sessions a frame can carry.** Unasked at design time and answered the hard way. A frame is one line into a fixed firmware buffer, so both ends need caps: the tracker holds at most 256 live sessions and truncates oversized fields, and the firmware parses incrementally so an oversized line still surrenders its aggregate colour rather than the whole frame.

**Whether a label needs its own endpoint.** Yes. Relabelling through ingest meant sending an event, and the only event that fit drove the session to idle and turned a blocked session green at the exact moment the human was naming what blocked them. `/v1/task` carries no event for that reason.

**Whether `stoplight status` can just check the port.** No. A closed port and a running relay with no light attached are different problems with different fixes, and a bare connect cannot tell them apart. That is what `GET /v1/status` is for.

## Decisions already made

Recorded so they are not relitigated.

| Decision | Why |
|---|---|
| One binary, not a web app | A hook lives for milliseconds. A web framework needs a long-running server. |
| Push, not polling | Producers report their own state. Inferring "is it waiting" from outside is unreliable, and a light that is sometimes wrong is worse than none. |
| A protocol, not a Claude integration | The state machine was never Claude-specific. Naming events semantically costs nothing and lets any agent connect. See [RFC 1](rfc.md). |
| Free-text `provider`, not an enum | An enum needs updating for every new agent. This should outlive the current crop. |
| One aggregate across all providers | Per-provider lamps would be a different device. Provider identity belongs on the screen. |
| Aggregate lamp, rotating screen | The lamp answers "does anything need me". The screen answers "what is running". |
| Pin never affects the lamps | A pinned green session must not be able to hide a red one. |
| Rotation and pin live in firmware | They keep working when the link drops, which will matter more over a radio than it does over a cable. |
| Long press, not double tap | A double tap needs a timing window, and pressing a small case rocks it. |
| Button on top | Pressing down is braced by the desk. A side press slides the case. |
| Line-delimited JSON | Readable in a serial monitor, which pays for the extra bytes the first time you debug. |
