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

#include "button.h"
#include "display.h"
#include "lamps.h"
#include "protocol.h"

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

  lastFrameAt = millis();
}

void loop() {
  const uint32_t now = millis();

  // 1. Drain whatever serial has for us. Bounded per iteration so that a
  // fast writer cannot starve the button and the display: the reader keeps
  // its partial line between iterations, so stopping early is free.
  uint16_t budget = 256;
  while (Serial.available() > 0 && budget-- > 0) {
    int c = Serial.read();
    if (c < 0) {
      break;
    }
    if (!reader.feed((char)c)) {
      // An oversized line yields no frame, but it may still have given up
      // its aggregate. Move the lamps on it and leave the screen alone: the
      // session list was unusable, and a stale label beats a wrong one.
      // Losing the labels is acceptable; losing the lamp is not.
      Color salvaged;
      if (reader.overflowColor(&salvaged)) {
        lamps.set(salvaged);
        lastFrameAt = now;
      }
      continue;
    }

    // A complete line. Parse it, and ignore it if it was not usable.
    if (!parseFrame(reader.line(), &frame)) {
      continue;
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

    lastFrameAt = now;
  }

  // 2. The button.
  switch (button.update(now)) {
    case BUTTON_SHORT_PRESS:
      display.togglePin(now);
      break;
    case BUTTON_LONG_PRESS:
      display.advance(now);
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
}
