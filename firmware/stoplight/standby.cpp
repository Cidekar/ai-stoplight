// standby.cpp - the grace-timer state machine. See standby.h for why.

#include "standby.h"

Standby::Standby() : active_(false), lastAwakeAt_(0) {}

void Standby::begin(uint32_t now) {
  active_ = false;
  // Arm the timer at boot so a board that never connects still powers down
  // once the grace window passes. Without this a never-connected light would
  // measure its idle time from millis() zero, which is the same result only by
  // accident; stating it is the point.
  lastAwakeAt_ = now;
}

void Standby::noteActivity(uint32_t now) {
  lastAwakeAt_ = now;
  // Activity does NOT wake the light here. Waking is the caller's job, driven
  // by the real signal: a frame wakes the display and the lamp follows the
  // frame's aggregate, so this module leaving active_ set would fight that. The
  // next update() clears active_ because the timer is now fresh.
}

StandbyTransition Standby::update(uint32_t now, bool connected) {
  // A live BLE link is activity in its own right: a connected but quiet central
  // must not let the light blank between frames. Rearming here also means the
  // grace timer only ever counts from the moment the link actually went down.
  if (connected) {
    lastAwakeAt_ = now;
  }

  // Already awake and still being driven: nothing to do. Unsigned subtraction
  // keeps the elapsed-time test correct across the millis() rollover.
  bool graceExpired = (uint32_t)(now - lastAwakeAt_) >= STANDBY_GRACE_MS;

  if (active_) {
    // Powered down. Activity rearmed the timer, so a fresh frame or a reconnect
    // lifts standby. Report the RESUME edge once so the caller re-asserts the
    // lamp: the frame that rearmed the timer may carry no "color" key, and then
    // the lamp would stay dark-by-accident until some later frame happened to
    // carry a colour. The display wake is the frame's own job; only the lamp
    // needs this edge.
    if (!graceExpired) {
      active_ = false;
      return STANDBY_RESUMED;
    }
    return STANDBY_NONE;
  }

  // Awake. Enter standby only once, on the iteration the grace window closes.
  if (graceExpired) {
    active_ = true;
    return STANDBY_ENGAGED;  // the caller clears the lamps and sleeps the panel
  }

  return STANDBY_NONE;
}
