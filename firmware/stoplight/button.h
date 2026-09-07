// button.h - one momentary button on GPIO 3, to ground.
//
// The pin uses the internal pull-up, so it reads HIGH at rest and LOW while
// pressed. No external resistor.
//
//   Short press, under 500ms   pin or unpin the session on screen
//   Long press, 500ms or more  advance to the next session
//
// Long press was chosen over a double tap because a double tap needs a
// timing window between presses, and pressing a button set into a small case
// rocks the case on the desk, which makes that window unreliable. A long
// press has no window, only a duration, and a missed long press reads as a
// short press rather than doing nothing visible.

#ifndef STOPLIGHT_BUTTON_H
#define STOPLIGHT_BUTTON_H

#include <Arduino.h>

#define BUTTON_PIN 3
#define BUTTON_DEBOUNCE_MS 50    // contact bounce settles well inside this
#define BUTTON_LONG_PRESS_MS 500 // the short/long boundary from the spec

enum ButtonEvent : uint8_t {
  BUTTON_NONE = 0,
  BUTTON_SHORT_PRESS,
  BUTTON_LONG_PRESS
};

class Button {
 public:
  Button();

  void begin();

  // update polls the pin and returns an event at most once per press.
  // Non-blocking: it never waits for the button to be released.
  //
  // A long press fires the moment the hold passes the threshold, while the
  // button is still down, rather than on release. Waiting for release would
  // make a deliberate long hold feel unresponsive, and the user has already
  // supplied all the information the gesture carries.
  ButtonEvent update(uint32_t now);

 private:
  bool stable_;        // debounced level: true when pressed
  bool lastRaw_;       // last level read from the pin
  uint32_t lastChange_;// when lastRaw_ last differed, for debouncing
  uint32_t pressedAt_; // when the debounced press began
  bool longFired_;     // long press already reported for this hold
};

#endif  // STOPLIGHT_BUTTON_H
