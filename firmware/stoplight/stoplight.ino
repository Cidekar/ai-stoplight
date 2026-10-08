// stoplight.ino - a physical traffic light showing AI agent status.
//
// Board: ESP32-C3 SuperMini with onboard 0.42 inch OLED.
// Flash as "ESP32C3 Dev Module", USB CDC On Boot ENABLED, 4MB, QIO.
// See firmware/readme.md, especially the note about the screen offset.
//
// The relay sends the whole session list as one line of JSON per change.
// This firmware owns rotation, scrolling and the pin, so all three keep
// working when the link drops. See design.md, "What the firmware owns".
//
// THE INVARIANT THAT MATTERS, from design.md:
//
//   A pinned or rotating screen can never hide a red lamp.
//
// It is enforced structurally rather than by care. The aggregate colour goes
// from the parsed frame to Lamps::set() in one statement below, and Display
// is never consulted. Display has no reference to Lamps and no way to obtain
// one. To break the guarantee you would have to add a Lamps call inside the
// display module, which is a conspicuous change rather than an easy mistake.
//
// Nothing here blocks. There is no delay() anywhere in loop(), because a
// blocking wait would stall serial reads and make the button feel dead.

#include <Arduino.h>
#include <Wire.h>

#include "blelink.h"
#include "button.h"
#include "display.h"
#include "lamps.h"
#include "protocol.h"
#include "standby.h"

// I2C for the onboard panel. These are not the core's default pins, so Wire
// must be told about them explicitly before the display starts.
#define PIN_SDA 5
#define PIN_SCL 6
#define OLED_ADDR 0x3C  // if the screen stays blank, try 0x3D. See the readme.

// Blank the screen after this long with no frame, and wake on the next one.
// The lamps stay lit: the colour is still true, and it is the reason the
// device exists. Only the screen sleeps.
#define SCREEN_IDLE_MS (5UL * 60UL * 1000UL)

Lamps lamps;
Display display;
Button button;
BleLink ble;
Standby standby;

// ONE reader for BOTH transports, which is the point.
//
// A BLE chunk is indistinguishable from a partial serial read: both are a
// slice of bytes at an arbitrary offset, and the '\n' in the payload is the
// only frame delimiter either one has. Feeding both into the same LineReader
// is what makes that true in code rather than only in the contract. Two
// readers would be two chances to disagree about the frame format, and the
// BLE one would be the one nobody watches.
//
// It also means a frame may arrive half over USB and half over the radio.
// That is nonsense in practice, but it is harmless: the bytes concatenate,
// the line either parses or it does not, and a corrupt line costs one update.
LineReader reader;

// One reusable Frame. Parsing into a static buffer avoids putting a ~1KB
// struct on the stack on every line.
Frame frame;

uint32_t lastFrameAt = 0;

void setup() {
  // The CDC receive buffer must hold a whole frame, and the default does not.
  //
  // The default is 256 bytes. A frame carrying four sessions with real UUID
  // ids runs to about 500, so the tail of every large frame was dropped on
  // the floor before loop() could read it. The loss is SILENT: available()
  // reports what arrived, never what was discarded, so the reader sees a
  // short line, parses it happily, and the screen shows fewer sessions than
  // were sent. Nothing anywhere reports an error.
  //
  // No host test can catch this. The stubs have no transport and no buffer,
  // so the parser reads every byte the test hands it and all eight sessions
  // arrive. It reproduces only on hardware, and only above 256 bytes.
  //
  // Sized to SL_LINE_MAX so the buffer can hold the largest line the reader
  // will accept. Must be called BEFORE begin(), which is where the buffer is
  // allocated; afterwards it is ignored and the default stands.
  Serial.setRxBufferSize(SL_LINE_MAX);

  // USB CDC. Nothing waits on the port: the device must run standalone on a
  // battery with no host attached, so a "wait for serial" loop would hang it.
  Serial.begin(115200);

  Wire.begin(PIN_SDA, PIN_SCL);

  lamps.begin();
  display.begin();
  button.begin();

  // The radio starts unconditionally, alongside serial rather than instead
  // of it. Which link the relay uses is the relay's choice, and a light that
  // answered only one of them would need a build flag to switch, which is a
  // decision made at flash time about something discovered at run time.
  ble.begin();

  const uint32_t now = millis();
  lastFrameAt = now;

  // Arm the standby grace timer at boot. A board powered on with no central
  // ever present powers down once the grace window passes, rather than holding
  // a dark-but-live light forever. See standby.h, issue #39.
  standby.begin(now);
}

// feedByte hands one byte to the reader and acts on a completed line. It is
// the whole of frame handling, and it is transport-agnostic on purpose: the
// serial drain and the BLE drain both call it, so neither link can develop
// its own idea of what a frame is.
//
// Returns true when the byte produced something worth counting as activity,
// which is a complete frame or a salvaged aggregate.
static bool feedByte(char c, uint32_t now) {
  if (!reader.feed(c)) {
    // An oversized line yields no frame, but it may still have given up its
    // aggregate. Move the lamps on it and leave the screen alone: the
    // session list was unusable, and a stale label beats a wrong one.
    // Losing the labels is acceptable; losing the lamp is not.
    Color salvaged;
    if (reader.overflowColor(&salvaged)) {
      lamps.set(salvaged);
      return true;
    }
    return false;
  }

  // A complete line. Parse it, and ignore it if it was not usable.
  if (!parseFrame(reader.line(), &frame)) {
    return false;
  }

  // THE LAMPS. Straight from the frame's aggregate, with no reference to
  // the display, the rotation index, or the pin.
  //
  // A frame with no "color" key leaves the lamps where they are, because
  // absent means "no instruction", not "off".
  if (frame.hasColor) {
    lamps.set(frame.color);
  }

  // THE SCREEN. Separately, and it cannot reach back to the lamps.
  display.applyFrame(frame, now);

  return true;
}

void loop() {
  const uint32_t now = millis();

  // 1. Drain both transports. Bounded per iteration so that a fast writer
  // cannot starve the button and the display: the reader keeps its partial
  // line between iterations, so stopping early is free.
  //
  // The two drains share a budget rather than having one each, so a busy
  // radio and a busy cable together still cannot hold the loop for longer
  // than one link could alone.
  uint16_t budget = 256;

  while (Serial.available() > 0 && budget-- > 0) {
    int c = Serial.read();
    if (c < 0) {
      break;
    }
    if (feedByte((char)c, now)) {
      lastFrameAt = now;
      // A frame over either transport is what keeps the light out of standby,
      // so a serial-driven board stays awake even though BLE never connects.
      standby.noteActivity(now);
    }
  }

  while (ble.available() > 0 && budget-- > 0) {
    int c = ble.read();
    if (c < 0) {
      break;
    }
    if (feedByte((char)c, now)) {
      lastFrameAt = now;
      standby.noteActivity(now);
    }
  }

  // 2. The button.
  //
  // A press is activity, exactly like a frame. Without this, step 4 below would
  // blank the screen SCREEN_IDLE_MS after the last frame no matter how recently
  // the button was pressed, so the press that togglePin/advance uses to wake a
  // sleeping screen is undone on this same pass and the button cannot bring the
  // screen back. Treating the press as activity keeps the woken screen up for a
  // fresh idle interval, which is the whole point of pressing it.
  switch (button.update(now)) {
    case BUTTON_SHORT_PRESS:
      display.togglePin(now);
      lastFrameAt = now;
      standby.noteActivity(now);
      break;
    case BUTTON_LONG_PRESS:
      display.advance(now);
      lastFrameAt = now;
      standby.noteActivity(now);
      break;
    default:
      break;
  }

  // 3. Rotation and scrolling.
  display.tick(now);

  // 4. Blank the screen after a quiet spell. Unsigned subtraction keeps this
  // correct across the millis() rollover at about 49 days.
  if (!display.asleep() && (uint32_t)(now - lastFrameAt) >= SCREEN_IDLE_MS) {
    display.sleep();
  }

  // 5. Standby. When the BLE link is down AND no frame has arrived for the
  // grace window, the light is no longer being told what to show, so power
  // down the lamps and the panel rather than holding a stale colour forever.
  // See standby.h, issue #39.
  //
  // The lamps are cleared to COLOR_OFF, not left on their last colour: a lamp
  // that is no longer true is worse than a dark one, and it is the LED current
  // and life this exists to stop. The panel is slept too, if the idle blank
  // above has not already done it. Reconnecting needs nothing here: the next
  // frame wakes the display and drives the lamp from its aggregate, and
  // standby.update() lifts itself the moment activity rearms the timer.
  if (standby.update(now, ble.connected())) {
    lamps.set(COLOR_OFF);
    if (!display.asleep()) {
      display.sleep();
    }
  }
}
