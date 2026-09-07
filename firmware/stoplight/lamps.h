// lamps.h - the three LEDs.
//
// The lamps answer one question: does anything, anywhere, need a human. They
// are driven from the aggregate colour in the frame and from nothing else.
//
// THE INVARIANT: no display feature may change what the lamps show. Rotation,
// scrolling and pinning all live in display.{h,cpp} and this module has no
// reference to any of them. set() takes a Color and there is no other way in.
// A pinned green session while another session is red still shows RED, and
// the only way to break that would be to pass the wrong Color from the one
// call site in the sketch. See design.md, "Invariants".

#ifndef STOPLIGHT_LAMPS_H
#define STOPLIGHT_LAMPS_H

#include <Arduino.h>

#include "protocol.h"

// Pins. GPIO 5 and 6 carry I2C to the screen; GPIO 8 and 9 carry the onboard
// LED and boot strapping. Neither group is safe to reuse.
#define LAMP_PIN_RED 0
#define LAMP_PIN_YELLOW 1
#define LAMP_PIN_GREEN 2

// Duty cycle out of 255. The LEDs sit behind a printed diffuser, so a third
// of full current looks the same to the eye and draws a third of the power.
// The LEDs dominate the battery budget, so this is the single biggest lever
// on run time.
#define LAMP_DUTY 80

// PWM. The ESP32 Arduino core drives this through LEDC.
#define LAMP_PWM_FREQ 5000  // well above flicker perception
#define LAMP_PWM_BITS 8     // 0-255, matching LAMP_DUTY

class Lamps {
 public:
  Lamps();

  // begin configures the PWM channels and leaves every lamp dark.
  void begin();

  // set lights exactly one lamp, or none for COLOR_OFF. Idempotent: calling
  // it with the current colour touches no hardware, so it is safe to call
  // every loop.
  void set(Color c);

  Color current() const { return current_; }

 private:
  void write(uint8_t pin, bool on);

  Color current_;
  bool begun_;
};

#endif  // STOPLIGHT_LAMPS_H
