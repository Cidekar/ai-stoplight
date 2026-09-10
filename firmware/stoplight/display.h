// display.h - the screen: rotation, marquee scrolling, and the pin.
//
// THE PANEL, AND THE ONE THING THAT WILL WASTE YOUR EVENING:
//
// The 0.42 inch OLED is a 72x40 window driven by an SSD1306 that addresses a
// 128x64 buffer. The visible area sits at offset (13, 14) inside that buffer.
// Draw at 0,0 with a generic driver and the pixels land off-panel, and the
// screen looks dead rather than misconfigured. U8G2_SSD1306_72X40_ER_F_HW_I2C
// applies the offset, which is why that exact constructor is not negotiable.
// See firmware/readme.md.
//
// WHAT THIS MODULE OWNS, per design.md "What the firmware owns": rotation
// timing, scroll position, and which session is pinned. All of it survives a
// dropped link, because the relay sends state and the device decides how to
// show it.
//
// WHAT IT DOES NOT OWN: the lamps. There is no Lamps reference in this
// header or its implementation. The pin cannot reach them.
//
// THE BLANK SCREEN BUG, AND WHY THE FIX LOOKS BRUTE-FORCE:
//
// The panel used to go dark permanently in normal use. The lamps kept
// working, frames kept arriving, and an I2C scan still found the panel at
// 0x3C -- only a reflash brought the screen back.
//
// The cause: begin() ran once at boot and never again. An SSD1306 holds its
// entire configuration in volatile registers, so any disturbance that
// rewrites them -- a brownout while the wires are moved, a glitch on the bus
// corrupting a command byte -- leaves a panel that is powered and addressable
// but no longer configured to show anything. The firmware carried on pushing
// buffers into it forever. A reflash "fixed" it only because it re-ran
// begin().
//
// THE PART THAT DECIDES THE DESIGN: the panel still ACKs its address while
// blank. This was measured on the bench, not assumed. Both reproductions
// below reported ack=1 the entire time the screen was dark:
//
//   0xAE (display off) latched behind U8g2's back  -> dark, ACKs, never returns
//   0xA8 0x00 (multiplex ratio corrupted)          -> dark, ACKs, never returns
//
// So there is nothing to detect. Probing 0x3C with beginTransmission and
// endTransmission reports a healthy panel while the user is looking at a
// dead one, and U8g2's send-path return values say the bus write succeeded,
// because it did. A detect-then-recover design cannot see this fault.
//
// Hence: re-assert the initialisation unconditionally on a timer and never
// ask whether it was needed. Re-running the init on a working panel is a
// no-op that costs about a millisecond, so the cheap unconditional fix is
// strictly better than an expensive detector that does not work.

#ifndef STOPLIGHT_DISPLAY_H
#define STOPLIGHT_DISPLAY_H

#include <Arduino.h>
#include <U8g2lib.h>

#include "protocol.h"

// Timing, all from the readme.
#define DISP_ROTATE_MIN_MS 3000   // minimum slot time; scrolling may extend it
#define DISP_SCROLL_HOLD_START_MS 1000  // let the first characters be read
#define DISP_SCROLL_HOLD_END_MS 800     // pause at the end before resetting
// One pixel per step. Raised from 90ms to 68ms when the label went to the
// 10x20 font: wider glyphs mean more overhang for the same text, so the old
// rate made a long branch name take an uncomfortably long time to get through.
// 68ms is about a third faster without tipping into frantic.
#define DISP_SCROLL_STEP_MS 33
#define DISP_PIN_TIMEOUT_MS (15UL * 60UL * 1000UL)  // 15 minutes

// How often the panel's initialisation is re-asserted. See "THE BLANK SCREEN
// BUG" below. Ten seconds is chosen so that the worst case a user ever sees
// is a screen that is dark for ten seconds and then fixes itself, which reads
// as a flicker rather than as a failure. The cost is one init sequence, about
// thirty bytes over a 400kHz bus, roughly a millisecond, every ten seconds.
#define DISP_REINIT_INTERVAL_MS 10000UL

// Layout. Two rows, not three. The panel is 72x40:
//
//   y=0    the header: a pin marker and one dot per session
//   y=14   the label, in DISP_LABEL_FONT, scrolled if it overflows
//
// The state line ("working", "needs you") was removed to give the label the
// bottom two thirds of the panel. The state is already on the lamps, in
// colour, from across the room -- spending a third of a 40px screen to spell
// out what the red LED is already saying was the worst-value row of the three.
//
// DISP_CHAR_W must match the advance of DISP_LABEL_FONT, because the overflow
// and scroll maths are done in characters times advance rather than by asking
// U8g2 for a string width. The two are changed together or the marquee scrolls
// the wrong distance. u8g2_font_10x20_tf advances 10px per glyph, so a 72px
// line holds 7 characters.
#define DISP_W 72
#define DISP_H 40
#define DISP_LABEL_FONT u8g2_font_9x18_tf
#define DISP_CHAR_W 9
#define DISP_LABEL_Y 26
// Top of the label band. The label BASELINE is DISP_LABEL_Y; glyphs sit
// above it, so clipping from the baseline would cut off every character.
#define DISP_LABEL_BAND_TOP 8
#define DISP_VISIBLE_CHARS (DISP_W / DISP_CHAR_W)

// Position dots. One per session across the header, the current one filled.
// This replaces the "2/4" counter: at a glance the shape of the row says both
// how many sessions there are and where you are in the cycle, without the eye
// having to read and compare two digits.
//
// Radius 2 makes a 5px circle, which is the same visual weight as the 5x7
// digits it replaces. At 7px spacing the widest case, SL_MAX_SESSIONS of 8,
// spans 7*7+5 = 54px and still leaves room for the pin marker in front of it.
#define DISP_DOT_R 2
#define DISP_DOT_SPACING 7
#define DISP_DOT_CY 3   // centre y: a 5px circle sits in rows 1..5
#define DISP_PIN_X 0    // the pin marker keeps the top-left corner
#define DISP_DOTS_X 8   // dots start clear of the marker, marker or not

class Display {
 public:
  Display();

  void begin();

  // applyFrame takes a parsed frame and updates the session list.
  //
  // Only the screen is touched. The caller passes the aggregate colour to
  // Lamps separately, which is what keeps the two independent.
  //
  // Sessions are matched by id across frames, so a session keeps its
  // rotation slot as others come and go, and a pin survives a list update.
  void applyFrame(const Frame& f, uint32_t now);

  // tick advances rotation and scrolling and redraws when something moved.
  // Non-blocking; call it every loop().
  void tick(uint32_t now);

  // Button actions.
  void togglePin(uint32_t now);  // short press
  void advance(uint32_t now);    // long press: next session, scroll reset

  // Screen blanking, to save power when nothing has arrived for a while.
  void sleep();
  void wake(uint32_t now);
  bool asleep() const { return asleep_; }

  // Observers. These exist so the rotation, scrolling and pin rules can be
  // exercised on a host compiler, where U8g2 is stubbed out and there is no
  // panel to read back. They are const and used by nothing on the device.
  const char* currentLabel() const;
  bool isPinned() const { return pinned_; }
  uint8_t sessionCount() const { return count_; }

  // holdingRed reports whether a red session's hold is still in force.
  //
  // This is observable on purpose. The hold's deadline coincides with the
  // rotation minimum in every path through tick(), so the two agree and the
  // hold cannot be seen from currentLabel() alone. That is precisely why the
  // zero-sentinel bug it replaced was invisible: at millis() == 0xFFFFF448
  // the deadline now + DISP_ROTATE_MIN_MS wraps to exactly 0, a live hold
  // read as no hold, and nothing in the rotation behaviour gave it away.
  // A separate flag cannot be aliased by any timestamp, and this observer is
  // what lets the harness assert that directly rather than by proxy.
  bool holdingRed() const { return holding_; }
  uint32_t holdDeadline() const { return holdUntil_; }

  // reinitCount reports how many times the panel's init has been re-asserted
  // since boot. Observable so the harness can assert the timer's cadence,
  // that a sleeping panel is not woken by it, and that it survives the
  // millis() rollover. Nothing on the device reads it.
  uint32_t reinitCount() const { return reinitCount_; }

 private:
  // Per-session display state, kept beside the session data so that it
  // follows the session across frames rather than across slot indices.
  struct Slot {
    Session s;
    Color lastColor;  // to detect a colour change and reset the scroll
  };

  // PriorColor remembers one session's colour from the previous frame. Only
  // the id and the colour are needed to decide "did this change colour" and
  // "did this turn red", so applyFrame snapshots these rather than copying
  // the whole Slot array, which would put ~700 bytes on the stack.
  struct PriorColor {
    char id[SL_MAX_ID + 1];
    Color color;
  };

  void render(uint32_t now);
  // reassertPanel re-runs the controller's init and redraws. It is the whole
  // recovery: it makes no attempt to decide whether recovery was needed,
  // because a blanked panel is indistinguishable from a healthy one.
  void reassertPanel(uint32_t now);
  void resetScroll(uint32_t now);
  void clampIndex();
  // isRed, countRed and nextIndex confine rotation to the red sessions while
  // any session is red, so a session that needs a human cannot scroll away
  // before it is dealt with. See the comment at the canRotate test in tick().
  bool isRed(uint8_t i) const;
  uint8_t countRed() const;
  uint8_t nextIndex(uint8_t redCount) const;
  int findById(const char* id) const;
  int16_t scrollOffset(uint32_t now, int16_t overflowPx, bool* finished) const;
  bool labelOverflows(const Slot& sl, int16_t* overflowPx) const;
  void expirePin(uint32_t now);

  U8G2_SSD1306_72X40_ER_F_HW_I2C u8g2_;

  Slot slots_[SL_MAX_SESSIONS];
  uint8_t count_;

  uint8_t index_;         // which slot is on screen
  uint32_t slotEnteredAt_;// when the current slot took the screen
  uint32_t scrollStartAt_;// when the current label's scroll cycle began

  // Pinning. pinId_ holds the session id rather than an index, so the pin
  // follows the session when the list reorders and releases itself when the
  // session disappears.
  bool pinned_;
  char pinId_[SL_MAX_ID + 1];
  uint32_t pinnedAt_;

  // A red session takes the screen immediately and holds a full interval.
  // holdUntil_ is that guarantee, so the jump is not undone by the next tick.
  //
  // holding_ says whether holdUntil_ means anything. It is a separate flag
  // rather than a zero sentinel because 0 is a REACHABLE deadline: millis()
  // wraps at 2^32, so at now == 0xFFFFF448 the deadline now + 3000 is exactly
  // 0, and a zero-means-no-hold test would read a live hold as absent and let
  // the next tick rotate straight off the session that just went red. That is
  // the one moment the hold exists to prevent.
  bool holding_;
  uint32_t holdUntil_;

  bool asleep_;
  bool dirty_;            // something changed; redraw on the next tick

  // When the panel init was last re-asserted, and how many times. See "THE
  // BLANK SCREEN BUG" above.
  uint32_t lastReinitAt_;
  uint32_t reinitCount_;

  // Last rendered state, so tick() only pushes pixels when they differ.
  // A full 72x40 buffer transfer over I2C is not free, and redrawing an
  // unchanged static label is the exact waste the "no rotation for one
  // session" rule exists to avoid.
  int16_t lastDrawnOffset_;
  uint8_t lastDrawnIndex_;
  uint8_t lastDrawnCount_;
  bool lastDrawnPinned_;
};

#endif  // STOPLIGHT_DISPLAY_H
