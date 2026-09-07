# Firmware

ESP32 firmware for the Stoplight device. Three LEDs show the aggregate state of every AI session, and a 0.42 inch OLED shows which session that state belongs to.

The firmware owns rotation, scrolling and the pin, so all three keep working when the link to the relay drops. See [design.md](../design.md), "What the firmware owns".

## Read this first: the screen offset

**This is the single thing that will cost you an evening.**

The 0.42 inch OLED is a 72×40 window driven by an SSD1306 that addresses a **128×64** buffer. The visible area sits at a **column offset of 28** inside that buffer. Draw at `0,0` with a generic 128×64 driver and the pixels land outside the window. The panel then looks completely dead: no flicker, no partial text, nothing. It is indistinguishable from a broken screen or a wiring fault, which is why people spend hours on it.

These are the values U8g2 applies for this panel. They are read from `u8x8_d_ssd1306_72x40.c` in the library source, and the rendering they produce is confirmed on hardware:

| Value | Measured |
|---|---|
| `default_x_offset` | 28 |
| `flipmode_x_offset` | 28 |
| `tile_width` | 9 tiles, so 72 pixels |
| `tile_height` | 5 tiles, so 40 pixels |
| Visible area | 72×40 |

The offset is horizontal only. There is no vertical offset to apply: the panel uses the first five tile rows of the buffer, so the y origin is 0.

Use this constructor and nothing else. It applies the offset for you.

```cpp
U8G2_SSD1306_72X40_ER_F_HW_I2C u8g2(U8G2_R0, U8X8_PIN_NONE);
```

| Part of the name | Meaning |
|---|---|
| `72X40_ER` | The correct panel geometry, including the offset |
| `F` | Full frame buffer. Needed for smooth scrolling. |
| `HW_I2C` | Hardware I²C, which is what this board wires up |

If you copy a constructor from a generic SSD1306 tutorial, it will be `128X64_NONAME` and your screen will stay dark. That is the bug.

## Flashing

```bash
cd firmware
make setup     # first run only
make flash
make monitor
```

That is the whole flow. `make` on its own lists every target.

| Target | Does |
|---|---|
| `help` | List the targets, and print the FQBN and the detected port. The default. |
| `setup` | Install the ESP32 core and U8g2 |
| `compile` | Compile the sketch |
| `upload` | Upload the last build |
| `flash` | `compile` then `upload` — the one you want |
| `monitor` | Open the serial console at 115200 |
| `test` | Run the host checks under ASan/UBSan, no board needed |
| `board-details` | Print the board's real menu options, to check the FQBN |
| `ports` | List the serial ports arduino-cli can see |
| `clean` | Remove the build directories |

**`make setup` downloads a few hundred MB** of Xtensa/RISC-V toolchain the first time, and takes a while on a slow link. It runs once per machine, not once per checkout. Everything after it is fast.

The Makefile needs `arduino-cli`, which it will not install for you. If it is missing, every target that needs it stops and prints `brew install arduino-cli` rather than failing with a bare "command not found". Installing a toolchain is your call, so `make setup` only runs the arduino-cli-internal steps once the CLI itself exists.

**The port is auto-detected** as the first `/dev/cu.usbmodem*`. Override it with `PORT=`:

```bash
make flash PORT=/dev/cu.usbmodem1101
```

If no port is found, the most likely cause by a wide margin is a **charge-only USB-C cable**. It enumerates nothing at all and is physically identical to a data cable. The error message says so, because this is the failure that wastes an afternoon.

### Why a Makefile rather than the IDE

The board settings that matter are not defaults, and one of them is silently wrong more often than it is right. The Makefile puts them in a single FQBN, defined once:

```
esp32:esp32:esp32c3:CDCOnBoot=cdc,FlashSize=4M,FlashMode=qio
```

`CDCOnBoot=cdc` is the FQBN form of **USB CDC On Boot: Enabled**. It is the setting people miss in the GUI, and the reason a command-line flow is better here. Left at its default the board still enumerates and still uploads, so nothing looks wrong — but `Serial` goes to the hardware UART pins instead of USB. The monitor stays empty, no frames ever arrive, and the device looks dead. In a Makefile the setting is reviewable and cannot be forgotten; in a Tools menu it is one dropdown among twelve.

Override the whole FQBN with `FQBN=` if your module differs, for example an 8MB flash part. `make board-details` prints the menu keys and values your installed core actually accepts.

### Fallback: the Arduino IDE

If you prefer a GUI, the same build is these settings under Tools. Flash as **ESP32C3 Dev Module**.

| Setting | Value |
|---|---|
| Board | ESP32C3 Dev Module |
| USB CDC On Boot | **Enabled** |
| Flash Size | 4MB (32Mb) |
| Flash Mode | QIO |
| Partition Scheme | Default 4MB with spiffs |
| Upload Speed | 921600 |

`USB CDC On Boot` must be **Enabled**, for the reason above. This is the second most common way to lose an hour on this board, after the screen offset.

Install the library by hand too: Tools -> Manage Libraries, search `U8g2`, install the one by *oliver*.

## Libraries

One dependency, which `make setup` installs:

| Library | Install |
|---|---|
| U8g2 by oliver | Library Manager, search "U8g2" |

There is no JSON library in that list, which is deliberate. The frame parser is hand written, for the reasons in [Parsing](#parsing) below.

## The fastest smoke test

This needs **no wiring at all** — the screen is soldered to the board, so a bare board and a data cable are the whole rig. It is the quickest way to prove a flash worked.

```bash
cd firmware
make flash
make monitor
```

Paste this one line and press enter:

```json
{"color":"red","sessions":[{"id":"a","label":"auth-api","state":"needs you","color":"red"}]}
```

The screen shows `auth-api` and `needs you`. That is a working flash: USB CDC is on, I²C is up, the constructor offset is right and the parser runs. If you have wired the LEDs, the red one lights as well.

Nothing on screen? Go to [The screen is completely blank](#the-screen-is-completely-blank). An empty monitor instead means USB CDC On Boot is not enabled — reflash with `make flash`, which sets it.

## Wiring

The screen needs no wiring. It is soldered to the board and driven over I²C on GPIO 5 and 6. Three LEDs and one button are the whole job.

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

| Part | GPIO | Notes |
|---|---|---|
| Red LED | 0 | anode -> 220Ω -> GPIO |
| Yellow LED | 1 | anode -> 220Ω -> GPIO |
| Green LED | 2 | anode -> 220Ω -> GPIO |
| Button | 3 | to ground, internal pull-up, no resistor |
| OLED SDA | 5 | on board, do not reuse |
| OLED SCL | 6 | on board, do not reuse |

All three LED cathodes share a common ground rail. I²C address is `0x3C`.

**Avoid GPIO 5 and 6** (the screen) and **GPIO 8 and 9** (the onboard LED and boot strapping). Pulling GPIO 9 low at boot puts the chip into download mode.

**Polarity matters.** The long leg of an LED is the anode and goes toward the resistor and the GPIO pin. The short leg is the cathode and goes to ground. Backwards, the LED simply stays dark.

**Breadboard it first.** Build the circuit on a breadboard before you solder anything. The enclosure dimensions have never been checked against real parts, so the fit is unproven — see [Still unverified](#still-unverified-everything-that-needs-parts-not-yet-attached). A breadboard also makes a reversed LED a five-second fix rather than a desoldering job.

### Prove each LED after wiring

Run the relay against the board, then force each colour in turn. A miswired or reversed LED shows up at once, because you already know which lamp is meant to be lit.

```bash
go build -o /tmp/sl .
/tmp/sl --serial /dev/cu.usbmodem2101 --addr 127.0.0.1:7413
```

Leave that running and open a second terminal. These commands light red, then yellow, then green:

```bash
post() {
  curl -s -m 2 -X POST http://127.0.0.1:7413/v1/session \
    -H 'Content-Type: application/json' -d "$1"
}

post '{"session_id":"w","event":"blocked","label":"wiring"}'   # RED
post '{"session_id":"w","event":"started","label":"wiring"}'   # YELLOW
post '{"session_id":"w","event":"finished","label":"wiring"}'  # GREEN
post '{"session_id":"w","event":"ended"}'                      # all dark
```

Exactly one lamp is lit at each of the first three steps. Anything else is a wiring fault, and the step that fails names the pin:

| Symptom | Cause |
|---|---|
| One colour never lights, the other two do | That LED is reversed, or its resistor leg is not seated |
| The wrong colour lights | Two GPIO wires are swapped |
| No LED ever lights | The ground rail does not reach the board's `GND` pin |
| All three look dim | Correct, not a fault. `LAMP_DUTY` is 80/255 by design — see [Lamps](#lamps). |

Change `/dev/cu.usbmodem2101` to match your board; `make ports` lists what is there. Port 7413 is used so this cannot disturb a relay already running on the default. This is the shape of [Five minutes to a working light](../CONTRIBUTING.md#five-minutes-to-a-working-light) in CONTRIBUTING.md, pointed at real hardware instead of `--virtual`.

## Host tests

The parser, the rotation state machine, the lamps and the button are plain C++ that never touches a register directly, so they run on a host compiler with `Arduino.h` and `U8g2lib.h` stubbed out. That is the whole test suite, and it needs no board.

```bash
cd firmware
make test
```

That builds and runs them. `make clean` removes the build directories. `cd firmware/test && make` does the same thing directly; `test/Makefile` owns the sanitiser flags and the stub include order, and `make test` delegates to it rather than repeating them. The only requirement is a C++17 compiler with AddressSanitizer, which both clang and gcc have. No `arduino-cli` and no board.

Everything is compiled with `-Wall -Wextra -Wconversion -Wshadow` and run under `-fsanitize=address,undefined`. The sanitisers are not optional: every buffer in the firmware is fixed size and the parser is the one component that reads untrusted input, so an off-by-one is the failure mode most worth catching, and it is exactly what ASan finds and a passing assertion does not.

| Path | Holds |
|---|---|
| `test/Makefile` | The build, the flags and the sanitiser options |
| `test/test_main.cpp` | Every check |
| `test/stubs/Arduino.h` | `millis()`, GPIO and LEDC, all observable from a test |
| `test/stubs/U8g2lib.h` | The constructor and the handful of methods `display.cpp` calls |
| `test/stubs/stubs.cpp` | Storage for the simulated clock, pins and screen |

Time is set by the test rather than read from a clock, which keeps the run instant and makes the `millis()` rollover at about 49 days reachable — otherwise it is untestable in practice.

What the checks cover:

| Group | Pins |
|---|---|
| Frame sizes | 1, 4, 5 and 8 sessions all fit, and the 8-session worst case at maximum id and label length still fits `SL_LINE_MAX` |
| Oversized lines | The aggregate is recovered from a frame too long to parse **whether `color` comes before or after the sessions array**, every colour survives both orders, a per-session colour is never mistaken for the aggregate, and the reader recovers afterwards |
| UUID ids | Two UUIDs sharing a 24-character prefix stay distinct, the pin follows its own session across a reorder, and the jump to red is not suppressed |
| Duplicate ids | Two sessions sharing an id do not both take a rotation slot, the first is kept, and every id in a parsed frame is unique |
| Unknown keys | Unknown fields at any depth are skipped, an unknown colour name leaves the lamps alone, and a value too deeply nested to skip costs that one key rather than every key after it |
| Truncated input | An unterminated line never completes, and a frame with no `color` leaves the lamps exactly where they were |
| The reported failure | Five sessions with one blocked turns the lamp red, end to end through the reader, the parser, the lamps and the display |
| The invariant | Pinning, advancing and ticking for a simulated minute never move a lamp |
| Rollover | Short and long presses stay correct across the `millis()` wrap, rotation measurably keeps cycling through it, and a red hold whose deadline lands on exactly 0 is still honoured |
| Display | Rotation waits for scrolling, one session never rotates, a session keeps its slot, and a pin expires both ways |
| Strings | Over-long values truncate, escapes decode, non-ASCII collapses to a single `?`, and a `\uXXXX` escape cut off by the end of the line reads nothing past the terminator |

The suite is a regression net for real defects and it fails loudly if any is reintroduced. Reverting `SL_LINE_MAX` to 512 and `SL_MAX_ID` to 24 turns 0 failures into 28. Each later fix is pinned the same way, measured by reverting it:

| Revert | Failures |
|---|---|
| Drop the tail salvage, so only a leading `color` is recoverable | 11 |
| Restore `holdUntil_ == 0` as the "no hold" sentinel | 4 |
| Make an unskippable value abandon the rest of the frame | 5 |
| Allow duplicate ids to take a slot each | 8 |

The `\uXXXX` bounds guard is checked differently, because with it removed the code still returns the right answer — it just reads four bytes past the terminator to get there. The check parses a truncated escape out of a heap buffer sized exactly to the input, so AddressSanitizer's redzone sits immediately after the NUL and the overread is a crash rather than a silent success. On the stack or in a string literal the same overread lands in other valid memory and nothing notices, which is exactly why it survived review for so long.

## Testing without the relay

The device reads newline-delimited JSON on USB serial, so a serial monitor is a complete test harness. `make monitor` opens one at the right baud rate and sends a newline on enter. In the Arduino IDE instead, open the monitor at **115200 baud** and set the line ending to **Newline** — the parser acts on a frame when it sees the newline, so with "No line ending" selected nothing will ever happen.

For the shortest possible check see [The fastest smoke test](#the-fastest-smoke-test). Paste any of these and press enter.

Two sessions, one of them blocked. The lamp goes red and the screen rotates:

```json
{"color":"red","sessions":[{"id":"a1","label":"auth-api","state":"needs you","color":"red"},{"id":"b2","label":"stoplight","state":"working","color":"yellow"}]}
```

A long label, to watch the marquee. It holds at the start, scrolls left, holds at the end, then resets:

```json
{"color":"yellow","sessions":[{"id":"a1","label":"feature/a-very-long-branch-name","state":"working","color":"yellow"}]}
```

Lamps only. The screen is left exactly as it was:

```json
{"color":"green"}
```

Everything off. Lamps dark, screen cleared:

```json
{"color":"off","sessions":[]}
```

Forward compatibility. Unknown fields are ignored rather than rejected, so this behaves the same as the first example:

```json
{"color":"red","version":2,"meta":{"nested":[1,2]},"sessions":[{"id":"a1","label":"auth-api","state":"needs you","color":"red","future":true}]}
```

Things worth trying by hand:

- Send four sessions and watch the row of position dots follow the rotation: four dots, and the filled one moves along as the screen cycles.
- **Short press** the button to pin. A `*` appears and rotation stops, but scrolling carries on.
- **Long press** (hold half a second) to advance to the next session and restart its scroll.
- Send a frame where one session turns red. The screen jumps to it at once.
- **Pin a green session while another is red.** The lamp must stay red. That is the invariant the whole device rests on.

## Behaviour

### Layout

The 72×40 panel carries two rows, not three:

| Row | y | Content |
|---|---|---|
| Header | 0 | The pin marker `*`, then one position dot per session, in the small 5×7 font |
| Label | 14 | The session label in `u8g2_font_10x20_tf`, scrolled if it overflows |

The position dots replace the older `n/N` counter. A hollow circle is another session and a filled disc is the one on screen, so the shape of the row says both how many sessions there are and where you are in the cycle without the eye having to read and compare two digits. **One session draws no dots at all**, matching the rule that a lone session does not rotate. Eight dots, the `SL_MAX_SESSIONS` cap, span 54px and still leave the corner free for the pin marker. The dots do not shift when the marker appears, because a row that moved sideways on a pin would read as the position having changed.

The state row (`working`, `needs you`) was removed to give the label the bottom two thirds of the panel. The state is already on the lamps, in colour, from across the room — spelling it out in text was the worst-value row of the three.

### Rotation

More than one session cycles the screen, **at least** three seconds each, in the order the relay sent (which is start time order). A session keeps its slot as others come and go, because slots are matched by `id` rather than by position.

One session does not rotate. A cycle of one is a static screen redrawing for nothing.

**Rotation waits for scrolling.** Three seconds is a minimum, not a period. A slot advances only when the interval has passed *and* the label has finished its scroll cycle, so rotation is paced by content and runs slightly irregularly on purpose. A label long enough to take seventeen seconds to scroll holds the screen for seventeen seconds.

When a session turns red the screen jumps to it immediately and holds a full interval before resuming.

### Scrolling

The 10×20 label font puts 7 characters on a line. Labels that fit are drawn statically and never move. Longer labels run a marquee: hold at the start for a second, scroll left at one pixel per 68ms, hold at the end, reset.

It is a marquee and not a wrap-around loop because a loop never gives a stable moment to read the first characters, which is where the useful part of a branch name usually is.

Scroll position resets whenever the session changes colour.

### Button

| Press | Duration | Action |
|---|---|---|
| Short | under 500ms | Pin or unpin the session on screen |
| Long | 500ms or more | Advance to the next session, reset scroll |

Debounced over 50ms. A long press fires while the button is still held rather than on release, so a deliberate hold feels responsive.

A pin expires two ways: when its session disappears from a frame, and after **15 minutes** regardless. Without expiry you pin something, forget, and quietly stop seeing every other session.

**A pin never affects the lamps.** See below.

### Lamps

Exactly one lamp is lit at a time, matching the aggregate `color`. `off` means all three dark, which is what "nothing is running" looks like — green means finished.

The LEDs are driven by PWM at a duty of 80/255 rather than at full current. Behind a printed diffuser this looks the same to the eye, and the LEDs dominate the battery budget, so it is the single biggest lever on run time. Change `LAMP_DUTY` in `lamps.h` if your diffuser is thicker.

#### The LEDC API split is unverified — read this before you flash

`lamps.cpp` contains two implementations behind `#if ESP_ARDUINO_VERSION_MAJOR >= 3`, because the ESP32 Arduino core renamed the LEDC API in 3.0.

| Core | API | What `lamps.cpp` calls |
|---|---|---|
| 3.x | pin-based | `ledcAttach(pin, freq, bits)`, then `ledcWrite(pin, duty)` |
| 2.x | channel-based | `ledcSetup(ch, freq, bits)` and `ledcAttachPin(pin, ch)`, then `ledcWrite(ch, duty)` |

**Targeted core: 3.x.** `make setup` installs the current `esp32:esp32`, which is 3.x, and that is the branch this firmware was written against.

**Neither branch has been compiled for an ESP32, and only one of them ever will be on any given machine.** The host stub declares `ESP_ARDUINO_VERSION_MAJOR 3`, so the host build exercises the 3.x branch only, and the stub's `ledcAttach` and `ledcWrite` do nothing but record a duty into an array. That means:

- The host tests prove the lamp *logic* — one LED lit at a time, `off` clearing all three, `set()` being idempotent. They prove nothing about the LEDC calls themselves.
- The 2.x branch is compiled by nothing, anywhere. It is unreachable on a 3.x core and untested on a 2.x one. Treat it as a best-effort fallback that has never been run.
- Signatures, argument order and return types on both branches were written from the core's documented API, not verified against a compiler.

If the LEDs stay dark on a board that is otherwise working, this split is the first place to look. Check which core version is installed with `arduino-cli core list` (Arduino IDE: Tools -> Board -> Boards Manager, search `esp32`), and confirm the matching branch is the one being compiled.

#### The lamps and the display stay apart

`lamps.h` and `lamps.cpp` contain no reference to `Display`, and `display.h` and `display.cpp` contain no reference to `Lamps`. That separation is what makes "a pinned or rotating screen can never hide a red lamp" a structural property rather than a promise. See [How the pin is kept away from the lamps](#how-the-pin-is-kept-away-from-the-lamps).

## How the pin is kept away from the lamps

The guarantee, from [design.md](../design.md): *a pinned or rotating screen can never hide a red lamp*. The device's whole value is that a green glance is trustworthy, so this is enforced by structure rather than by care.

- `lamps.h` has no reference to `Display`, and `display.h` has no reference to `Lamps`. Neither module can call the other.
- The only way to change a lamp is `Lamps::set(Color)`, and the only caller is one statement in `loop()`, which passes the aggregate straight from the parsed frame.
- Rotation index, scroll position and pin state live entirely inside `Display` and are never read when setting the lamps.

To break the invariant you would have to add a `Lamps` include and a call inside the display module. That is a conspicuous change in review, not an easy mistake.

## Parsing

The parser is hand written. **This is deliberate, and the alternative was ArduinoJson.**

The frame is a flat object with two keys, one of which is an array of flat objects with four string keys. There is no deep nesting, no numbers to convert, and nothing to re-serialise. A scanner for that shape is about 200 lines, and against a dependency it buys:

- **No allocator.** Every buffer is fixed size and lives in the object, so there is no heap fragmentation over a multi-day uptime and no `JsonDocument` capacity to guess at. An oversized label truncates instead of failing.
- **Explicit forward compatibility.** Unknown keys are skipped at any depth by a `skipValue` that steps over whole objects and arrays. Adding a field to the relay cannot break a light flashed today.
- **Partial frames still count.** A truncated line still applies whatever parsed before the break. Dropping a frame because a field the firmware never reads was malformed could mean dropping a red lamp.
- **Auditable.** The failure modes of the one component that touches untrusted input can be read end to end.

Guards, all exercised by the host tests: labels over 48 characters truncate; nesting deeper than 16 is refused; sessions past the eighth are skipped while the rest of the frame still parses; a session with no `id` is dropped, since it cannot be tracked across frames or pinned; a session whose `id` is already present is dropped for the same reason; non-ASCII collapses to a single `?` because the font has no glyph for it.

**A refused value costs one key, not the frame.** When `skipValue` gives up — nesting past the guard, or a malformed value — the parser scans forward to the next top-level comma and carries on, rather than abandoning every later key. It used to abandon them, which meant a frame like `{"x":[[[ …1000 deep… ]]],"color":"red"}` dropped the aggregate and the red lamp with it, over a field the firmware does not even read. The forward scan is string-aware, so a comma or brace inside a string literal is not mistaken for structure.

**Duplicate ids are dropped, keeping the first.** Ids are the key for the pin and for preserving a rotation slot, and both take the first match, so a second session sharing an id got a slot that nothing could ever address: a pin on it silently anchored to the first instead, and the did-this-turn-red check read the first's colour. This is the same failure `SL_MAX_ID` fixed, arriving from the relay rather than from truncation.

### Key order: `color` first, and why it is asserted on both sides

The parser itself is order-independent, and so is the oversized-line salvage. But the two salvage paths are not equally strong, and which one applies is decided purely by where `color` sits in the frame.

| `color` position | Recovered from | Strength |
|---|---|---|
| First | The prefix the reader kept, by a plain forward parse | Exact, however far past the cap the line ran |
| Last | The last `SL_TAIL_MAX` (96) bytes, retained in a ring buffer | Works, but the key and its value must fall inside that window |

Both light the right lamp, which is the point: **a truncated frame must never lose its aggregate because of where a Go struct happened to declare a field.** The salvage used to be prefix-only, so `{"sessions":[…huge…],"color":"red"}` recovered nothing and the lamps held a stale green — a red light silently disappearing because of field ordering in code on another machine.

The relay emits `color` first because `Frame` in `internal/stoplight/frame.go` declares it first, and `TestFrameSerialisesColorFirst` in that package asserts it on the wire bytes. Reordering the struct now fails a Go test with an explanatory message instead of quietly downgrading every oversized frame to the weaker path. The contract is written out in `firmware/stoplight/protocol.h` under "KEY ORDER".

The tail scan only accepts a `"color":"<name>"` pair followed by the frame's closing brace and nothing else. A `color` still inside the sessions array belongs to one session and is refused, so a per-session colour can never reach the lamps by this route.

### Sizing, and why a dropped line was a wrong colour

`SL_LINE_MAX` is 1536 bytes. It has to exceed the largest frame the relay can send, because a line over the cap is not merely truncated — it stops being usable as a session list at all.

The worst case is the eight-session cap at the maximum id and label length, which measures 1199 bytes: 8 entries of 145 bytes, 7 separating commas, and a 32-byte envelope. 1536 leaves 337 bytes of headroom for a label override longer than `SL_MAX_LABEL` and for any small key a future relay adds.

The earlier value of 512 was too small for real use, and the way it failed was the worst kind. Five sessions with 36-character UUIDs produce a 543-byte frame, so the line was discarded whole — the aggregate colour with it. Because frames are sent only on change, every later frame was oversized too, and the lamps held their last colour indefinitely. A session going red never showed.

Two changes make that unreachable, and a third contains the damage if it somehow happens anyway:

- The relay caps the list at eight entries (`MaxFrameSessions` in `internal/stoplight/tracker.go`) while still computing the aggregate over **every** live session. A ninth session going red still turns the lamp red even though it cannot be named on screen.
- `SL_LINE_MAX` is sized against the real worst case rather than a guess.
- **An oversized line now degrades instead of vanishing.** The reader scans the prefix it did keep for the top-level `color` key, and if that fails it scans a retained tail, so the aggregate reaches the lamps whichever end of the frame the key sits at. The screen is left untouched either way. Losing the labels is acceptable; losing the lamp is not. A `color` inside the sessions array is never mistaken for the aggregate, and when the aggregate genuinely cannot be recovered the lamps are left alone rather than guessed at. See [Key order](#key-order-color-first-and-why-it-is-asserted-on-both-sides).

`SL_MAX_ID` is 40, not 24. A Claude Code `session_id` is a 36-character UUID, and ids are compared with `strcmp`, so a truncated id is not a shorter id — it is a **wrong** id. At 24 characters two sessions sharing a UUID prefix collapsed into one, which made the pin follow the wrong session and made the did-this-turn-red check read another session's previous colour, suppressing the jump to red. Both caps together cost 1296 bytes of static RAM (2060 to 3356 across `Frame`, `LineReader` and `Display`), which is 0.8% of the C3's 400KB.

## Modules

| File | Owns |
|---|---|
| `stoplight/stoplight.ino` | Setup, the non-blocking main loop, wiring the modules together |
| `stoplight/protocol.{h,cpp}` | Frame struct, JSON parser, serial line reader |
| `stoplight/display.{h,cpp}` | Screen, rotation, marquee scrolling, pin state |
| `stoplight/lamps.{h,cpp}` | Three LEDs, PWM dimming |
| `stoplight/button.{h,cpp}` | Debounce, short and long press |

Nothing blocks. There is no `delay()` in `loop()`; every timer is a `millis()` comparison, because a blocking wait would stall serial reads and make the button feel dead. All elapsed-time maths uses unsigned subtraction, so it stays correct across the `millis()` rollover at about 49 days.

Serial reads are capped at 256 bytes per iteration so a fast writer cannot starve the button and the display. The reader keeps its partial line between iterations, so stopping early costs nothing.

Power: the screen blanks after five minutes with no frame and wakes on the next one. **The lamps stay lit** — the colour is still true, and it is the reason the device exists.

## Troubleshooting

### The screen is completely blank

**No host test can catch this, and the symptom is a screen that looks BROKEN rather than misconfigured.** That combination is why it is worth reading the list below before reaching for a multimeter.

The 72×40 offset lives entirely inside the U8g2 constructor. The host stub replaces that constructor with one that records nothing about geometry, and there are no pixels on a host to inspect, so the whole test suite passes identically with the right constructor and the wrong one. The 402 checks say nothing at all about this. It is the single largest gap between "the tests pass" and "the device works".

And when it is wrong there is no partial output to hint at it: no flicker, no clipped text, no garbled row. Every pixel lands outside the visible window, so the panel is **indistinguishable from a dead screen, an unpowered screen, a wrong I²C address, or a broken solder joint**. People replace the board over this. Rule the constructor out first, because it costs one line to check and it is the most likely cause by a wide margin.

In order of likelihood:

1. **Wrong constructor.** By far the most common cause, and the one no test can find for you. It must be `U8G2_SSD1306_72X40_ER_F_HW_I2C`. A generic `128X64_NONAME` driver renders outside the visible window and the panel looks dead — not wrong, *dead*. See the top of this file.
2. **Wrong I²C address.** This board is normally `0x3C`, but some units ship as `0x3D`. Change `OLED_ADDR` in `stoplight.ino`, or scan for it:

   ```cpp
   #include <Wire.h>
   void setup() {
     Serial.begin(115200);
     Wire.begin(5, 6);            // this board's SDA, SCL
     for (uint8_t a = 1; a < 127; a++) {
       Wire.beginTransmission(a);
       if (Wire.endTransmission() == 0) Serial.printf("found 0x%02X\n", a);
     }
   }
   void loop() {}
   ```

   No address at all means a hardware fault rather than a firmware one.
3. **Wrong I²C pins.** `Wire.begin(5, 6)` must run before `u8g2.begin()`. The core's defaults are not this board's pins.
4. **The panel is asleep.** After five minutes with no frame the screen blanks by design. Send any frame to wake it.

### Nothing happens when I paste JSON

- Set the serial monitor line ending to **Newline**. The parser acts on the newline, so without one the frame is never complete. `make monitor` already does this.
- Confirm the baud rate is **115200**.
- Confirm **USB CDC On Boot** is Enabled. Without it `Serial` goes to the UART pins, not USB. `make flash` sets it; a GUI flash may not have.
- Confirm the JSON is on one line. A pretty-printed frame spanning several lines parses as several broken frames.

### The board does not appear as a serial port

- Use a **data** USB-C cable. Charge-only cables enumerate nothing, and they look identical. This is the usual cause.
- Force download mode: hold `BOOT`, tap `RESET`, release `BOOT`, then upload.
- On macOS the port is `/dev/cu.usbmodem*`. `make ports` lists what arduino-cli can see.

### An LED never lights

- Check polarity. The long leg (anode) goes to the resistor and the GPIO pin; the short leg to ground. Backwards, it stays dark.
- Confirm the 220Ω resistor is in the anode line and all cathodes share the ground rail.
- The LEDs run dimmed at `LAMP_DUTY` 80/255, which is noticeably less than full brightness and is not a fault. Raise it in `lamps.h` to rule dimming in or out.

### The button does nothing, or triggers on its own

- It goes between GPIO 3 and **ground**, with no external resistor. The firmware enables the internal pull-up.
- A floating pin reads as random presses, which means the ground leg is not actually connected.

### Rotation looks irregular

That is correct. Rotation waits for scrolling, so slots with long labels hold the screen for longer than three seconds. A pinned session does not rotate at all — look for the `*` marker.

## Verified on hardware

**2026-09-06. The firmware compiles for the ESP32-C3 and runs on the real board.** Everything in this section was observed on the glass, not inferred from a passing test.

| Proven | Detail |
|---|---|
| The build | 346434 bytes of flash (26% of 1310720) and 19372 bytes of RAM (5% of 327680), at the FQBN in the Makefile |
| I²C | The panel answers at `0x3C` on SDA=GPIO5 and SCL=GPIO6, found by a scan |
| The constructor | `U8G2_SSD1306_72X40_ER_F_HW_I2C` is right, and the measured `x_offset` is 28. Text lands inside the visible window. |
| Frame delivery | A plain write to the device node is enough. **No DTR or RTS handshake is needed** — `printf '{…}\n' > /dev/cu.usbmodem*` renders a frame. |
| The relay | The Go relay drives the panel end to end over `--serial`, from an HTTP POST to pixels |
| Rotation | Several sessions cycle on screen, with the filled position dot tracking the label |
| Scrolling | A label longer than the line marquees: hold, scroll left, hold, reset |
| Rotation pacing | A long label holds the screen visibly longer than a short one, so rotation really is paced by content rather than by a clock |
| The off state | Ending every session clears the screen rather than freezing the last frame |

**2026-09-10. The lamps work, and the invariant holds on real hardware.** The LEDs are wired to GPIO 0, 1 and 2 and were driven from the relay over serial.

| Proven | Detail |
|---|---|
| The three LEDs light | Yellow and red both observed. The LEDs are wired and the lamp path runs end to end. |
| **The LEDC API split** | The 3.x branch of `lamps.cpp` works on real silicon. It had never been compiled for an ESP32 by anything before this — the host stub only records a duty into an array. The 2.x branch remains compiled by nothing. |
| PWM dimming at `LAMP_DUTY` | 80/255 is visible and readable on a bare LED, no diffuser |
| **The lamp stays red while the screen shows a green session** | The invariant the whole device rests on. A frame with aggregate `red` and three sessions coloured red, yellow and green held the lamp red through all three labels, including while the screen showed the green one. The lamp follows the aggregate and never the session on screen. |
| Rotation on hardware | Three sessions cycle every three seconds with the filled position dot tracking the label |

The no-handshake result is worth keeping. It is the reason `internal/transport/serial` can open the port with a plain `os.OpenFile` and stay on the standard library, and it was confirmed by writing a frame from a bare shell.

### Still unverified: everything that needs parts not yet attached

These are not doubts about the code. Nothing is wired to them yet, so nobody has looked.

| Unverified | Needs |
|---|---|
| The button: short press pins, long press advances | A button on GPIO 3 |
| BLE transport | Nothing wired, but untested against this board |
| Battery life | A battery |
| Enclosure fit | A printed enclosure |

### Host tests

What was verified on a host compiler with U8g2 and the Arduino API stubbed out. Run it yourself with `cd firmware && make test`; see [Host tests](#host-tests).

- All four modules and the sketch compile clean under `-Wall -Wextra -Wconversion -Wshadow`, with no warnings.
- **402 behavioural checks pass** under AddressSanitizer and UndefinedBehaviorSanitizer.
- Those checks include: frames at 1, 4, 5 and 8 sessions all fitting the line buffer, an oversized frame surrendering its aggregate colour with `color` at either end of the frame, 36-character UUIDs staying distinct, duplicate ids never taking two slots, a value too deeply nested to skip costing one key rather than the aggregate behind it, rotation genuinely waiting for a long label to finish scrolling, the immediate jump on red, a red hold whose deadline lands on exactly 0 still being honoured, the pin stopping rotation and expiring both ways, a session keeping its slot as others leave, truncated and deeply nested input, a `\uXXXX` escape cut off at the end of a heap-exact buffer reading nothing past the terminator, and short/long press classification across the `millis()` rollover.
- The U8g2 constructor, method signatures and font name were checked against the library source rather than recalled.

### What the host tests cannot reach

The stubs are a model of the Arduino API, not the API itself. They cannot catch a linker error against the real core, a timing problem, an I²C fault, or anything about actual pixels. Three gaps are worth naming, because in each one a passing test suite means nothing at all:

| Gap | Why no test reaches it | What a failure looks like |
|---|---|---|
| **The 72×40 panel offset** | It lives inside the U8g2 constructor, which the stub replaces. There are no pixels on a host to inspect, so every check passes identically with the wrong constructor. | A screen that looks **dead**, not misconfigured — no flicker, no clipped text. Indistinguishable from a hardware fault. See [The screen is completely blank](#the-screen-is-completely-blank). *Closed on 2026-09-06: the panel renders.* |
| **The LEDC API split in `lamps.cpp`** | The stub declares core 3.x, so only that branch is compiled, and its `ledcAttach`/`ledcWrite` do nothing but record a duty. The 2.x branch is compiled by nothing, anywhere. | LEDs that never light on an otherwise working board. See [the LEDC note](#the-ledc-api-split-is-unverified--read-this-before-you-flash). *Still open: no LEDs are wired.* |
| **Anything about real I²C, timing or the USB CDC port** | The stubs have no transport and no clock; the test sets time directly. | A blank screen, an empty serial monitor, or a marquee at the wrong speed. *Closed on 2026-09-06 for I²C, the CDC port and the marquee.* |

None of that substitutes for the panel lighting up. Check first that the screen renders at all, then that the marquee is readable at `DISP_SCROLL_STEP_MS`, and then that the LED brightness suits your diffuser.
