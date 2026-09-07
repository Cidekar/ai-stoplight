// lamps.cpp - PWM control of the three LEDs.
//
// The ESP32 Arduino core renamed the LEDC API in 3.0: 2.x uses channels
// (ledcSetup / ledcAttachPin / ledcWrite by channel), 3.x attaches a pin
// directly (ledcAttach / ledcWrite by pin). Both are in the wild, so this
// file supports either rather than pinning the reader to one core version.

#include "lamps.h"

#if ESP_ARDUINO_VERSION_MAJOR >= 3
#define LAMPS_LEDC_V3 1
#else
#define LAMPS_LEDC_V3 0
// Channels 0-2. The core has 6 on the C3, and nothing else here uses LEDC.
#define LAMP_CH_RED 0
#define LAMP_CH_YELLOW 1
#define LAMP_CH_GREEN 2
#endif

namespace {

#if !LAMPS_LEDC_V3
// channelFor maps a pin onto its LEDC channel on core 2.x, where ledcWrite
// addresses the channel rather than the pin.
uint8_t channelFor(uint8_t pin) {
  switch (pin) {
    case LAMP_PIN_RED: return LAMP_CH_RED;
    case LAMP_PIN_YELLOW: return LAMP_CH_YELLOW;
    case LAMP_PIN_GREEN: return LAMP_CH_GREEN;
    default: return LAMP_CH_RED;
  }
}
#endif

}  // namespace

Lamps::Lamps() : current_(COLOR_OFF), begun_(false) {}

void Lamps::begin() {
#if LAMPS_LEDC_V3
  ledcAttach(LAMP_PIN_RED, LAMP_PWM_FREQ, LAMP_PWM_BITS);
  ledcAttach(LAMP_PIN_YELLOW, LAMP_PWM_FREQ, LAMP_PWM_BITS);
  ledcAttach(LAMP_PIN_GREEN, LAMP_PWM_FREQ, LAMP_PWM_BITS);
#else
  ledcSetup(LAMP_CH_RED, LAMP_PWM_FREQ, LAMP_PWM_BITS);
  ledcSetup(LAMP_CH_YELLOW, LAMP_PWM_FREQ, LAMP_PWM_BITS);
  ledcSetup(LAMP_CH_GREEN, LAMP_PWM_FREQ, LAMP_PWM_BITS);
  ledcAttachPin(LAMP_PIN_RED, LAMP_CH_RED);
  ledcAttachPin(LAMP_PIN_YELLOW, LAMP_CH_YELLOW);
  ledcAttachPin(LAMP_PIN_GREEN, LAMP_CH_GREEN);
#endif

  begun_ = true;

  // Start dark. Off means nothing is running, which is the truthful state
  // before the first frame arrives.
  write(LAMP_PIN_RED, false);
  write(LAMP_PIN_YELLOW, false);
  write(LAMP_PIN_GREEN, false);
  current_ = COLOR_OFF;
}

void Lamps::write(uint8_t pin, bool on) {
  uint32_t duty = on ? LAMP_DUTY : 0;
#if LAMPS_LEDC_V3
  ledcWrite(pin, duty);
#else
  ledcWrite(channelFor(pin), duty);
#endif
}

void Lamps::set(Color c) {
  if (!begun_) {
    return;  // called before begin(); writing now would configure nothing
  }
  if (c == current_) {
    return;  // idempotent, so loop() can call this unconditionally
  }

  // Exactly one lit at a time. Writing all three every change means there is
  // no ordering in which two lamps are briefly lit together, and no path
  // where an old lamp is left on because a branch forgot to clear it.
  write(LAMP_PIN_RED, c == COLOR_RED);
  write(LAMP_PIN_YELLOW, c == COLOR_YELLOW);
  write(LAMP_PIN_GREEN, c == COLOR_GREEN);

  current_ = c;
}
