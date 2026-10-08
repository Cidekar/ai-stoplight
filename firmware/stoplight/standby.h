// standby.h - power-down when nothing is driving the light.
//
// THE DEFECT THIS FIXES (issue #39). A device left powered on with no central
// connected held its last lamp colour forever: a yellow lamp was found still
// lit ten hours after the relay went away. The colour was no longer true, the
// LED was burning current and life for nothing, and the OLED's charge pump ran
// alongside it. The light must go dark when it is no longer being told what to
// show, and come straight back when it is.
//
// WHAT "NOTHING IS DRIVING IT" MEANS. The board serves BLE and USB serial at
// once, and the relay chooses the link at run time (see firmware/readme.md,
// "Bluetooth"). So the trigger cannot be the BLE link alone: a serial-only
// light is never BLE-connected yet is being driven perfectly well. The true
// signal is the ABSENCE OF BOTH: the BLE link is down AND no frame has arrived
// for a while. A frame over either transport is what keeps the light awake, and
// a live BLE link is treated as activity in its own right so a connected-but-
// quiet central does not blank the light between frames.
//
// THE GRACE PERIOD, AND WHY IT EXISTS. A BLE link drops and reconnects for
// ordinary reasons: a supervision-timeout blip, the central moving between
// scans. Blanking the instant the link reports down would flicker the lamps off
// and on across every such blip. STANDBY_GRACE_MS debounces that: the light
// only powers down after the link has been down AND silent for the whole grace
// window. It is short next to the ten-hour failure it prevents, and long enough
// to ride out a reconnect.
//
// NEVER-CONNECTED-AT-STARTUP is the same path. The grace timer starts at boot,
// so a board powered on with no central ever present powers down once the grace
// window passes with no frame, rather than sitting lit on COLOR_OFF forever.
//
// OUT OF SCOPE, DELIBERATELY (issue #39). This does NOT touch the ESP32-C3's
// sleep modes or the BLE advertising interval. The radio keeps advertising at
// its normal cadence so the relay can still find the light and wake it; only
// the lamps and the panel are powered down. Deep/light-sleep and advertising-
// interval tuning are a worthwhile follow-up but a separate, riskier change: a
// light that sleeps its CPU or slows its advertising is harder to reconnect to,
// and getting that wrong reintroduces the very "invisible light" failure the
// BLE work already fought once.

#ifndef STOPLIGHT_STANDBY_H
#define STOPLIGHT_STANDBY_H

#include <stdbool.h>
#include <stdint.h>

// STANDBY_GRACE_MS is how long the link must be down AND silent before the
// light powers down. Five seconds: a BLE supervision-timeout reconnect settles
// well inside it, so an ordinary radio blip never reaches the lamps, while the
// ten-hour stuck-lamp failure it guards against is stopped within seconds.
#define STANDBY_GRACE_MS (5UL * 1000UL)

// Standby tracks the grace timer and the engaged/clear transition. It owns no
// hardware: the sketch calls the lamp and display power-down itself when
// update() reports a transition, which keeps this module testable on a host
// with no panel and no radio, and keeps the lamp/display independence the rest
// of the firmware is built around.
class Standby {
 public:
  Standby();

  // begin arms the grace timer at boot. Passing the boot time means a board
  // that never connects powers down STANDBY_GRACE_MS after startup, which is
  // the never-connected-at-startup case from issue #39.
  void begin(uint32_t now);

  // noteActivity records that the light is being driven right now: a frame
  // arrived over either transport, or a button was pressed. It rearms the grace
  // timer, so activity over serial keeps a BLE-disconnected light awake.
  void noteActivity(uint32_t now);

  // update advances the state machine for one loop() iteration and returns
  // true on the iteration that ENTERS standby, so the caller clears the lamps
  // and sleeps the panel exactly once rather than every loop. `connected` is
  // the BLE link state; a live link counts as activity. Non-blocking, and the
  // elapsed-time maths is unsigned so it is correct across the millis() wrap.
  bool update(uint32_t now, bool connected);

  // active reports whether the light is currently powered down.
  bool active() const { return active_; }

 private:
  bool active_;         // powered down right now
  uint32_t lastAwakeAt_;  // last time the light was driven or the link was up
};

#endif  // STOPLIGHT_STANDBY_H
