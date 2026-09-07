// button.cpp - debounce and press classification.
//
// The state machine is a plain millis() comparison with no delay(), because
// blocking here would stall serial reads and could lose a frame.

#include "button.h"

Button::Button()
    : stable_(false),
      lastRaw_(false),
      lastChange_(0),
      pressedAt_(0),
      longFired_(false) {}

void Button::begin() {
  // INPUT_PULLUP: the pin idles HIGH and the button pulls it to ground.
  pinMode(BUTTON_PIN, INPUT_PULLUP);
}

ButtonEvent Button::update(uint32_t now) {
  // Active low, so invert at the edge of the system and reason in terms of
  // "pressed" everywhere below.
  bool raw = (digitalRead(BUTTON_PIN) == LOW);

  if (raw != lastRaw_) {
    // The level moved. Restart the debounce window and believe nothing until
    // it has held still for BUTTON_DEBOUNCE_MS.
    lastRaw_ = raw;
    lastChange_ = now;
    return BUTTON_NONE;
  }

  // Unsigned subtraction, so this stays correct across the millis() rollover
  // at ~49 days. Never compare (now > lastChange_ + window) here: that form
  // breaks on wrap.
  if ((uint32_t)(now - lastChange_) < BUTTON_DEBOUNCE_MS) {
    return BUTTON_NONE;  // still bouncing
  }

  if (raw == stable_) {
    // No edge to report. One case still needs handling: the button is being
    // held and has just crossed the long press threshold.
    if (stable_ && !longFired_ &&
        (uint32_t)(now - pressedAt_) >= BUTTON_LONG_PRESS_MS) {
      longFired_ = true;
      return BUTTON_LONG_PRESS;
    }
    return BUTTON_NONE;
  }

  // The debounced level changed.
  stable_ = raw;

  if (stable_) {
    // Press began. Nothing is emitted yet: the gesture is not known until
    // it is either released or held past the threshold.
    pressedAt_ = now;
    longFired_ = false;
    return BUTTON_NONE;
  }

  // Release. If the long press already fired, this release is the tail of
  // that gesture and must not also count as a short press.
  if (longFired_) {
    longFired_ = false;
    return BUTTON_NONE;
  }

  if ((uint32_t)(now - pressedAt_) < BUTTON_LONG_PRESS_MS) {
    return BUTTON_SHORT_PRESS;
  }

  // Held past the threshold, but update() was not called while it was down
  // (a long blocking iteration elsewhere). Report the gesture the user
  // actually made rather than dropping it.
  return BUTTON_LONG_PRESS;
}
