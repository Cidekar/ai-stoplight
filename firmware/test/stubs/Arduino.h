// Arduino.h - a host stub of the small slice of the Arduino API the firmware
// uses.
//
// The firmware cannot be compiled for the ESP32 in CI, and a board is not
// always to hand, but the parser, the rotation state machine, the lamps and
// the button are all plain C++ that never touches a register directly. They
// only need millis(), the GPIO calls and the LEDC calls to exist. Stubbing
// those here lets every rule in design.md's invariants section be executed by
// a host compiler under a sanitiser, which is where a truncated line or an
// off-by-one in a fixed buffer actually shows up.
//
// The stub is deliberately observable: time is set by the test rather than
// read from a clock, and the pin and PWM writes are recorded so the lamp
// assertions can read back what the firmware wrote.

#ifndef STOPLIGHT_TEST_ARDUINO_H
#define STOPLIGHT_TEST_ARDUINO_H

#include <stddef.h>
#include <stdint.h>
#include <string.h>

// The ESP32 core exposes its version this way, and lamps.cpp switches on it
// to pick between the 2.x and 3.x LEDC APIs. Claiming 3 exercises the branch
// the current core actually takes.
#define ESP_ARDUINO_VERSION_MAJOR 3

#define HIGH 1
#define LOW 0
#define INPUT 0
#define OUTPUT 1
#define INPUT_PULLUP 2

// ---------------------------------------------------------------------------
// Time. The host has no millis(), and a test that waited on a real clock
// would be slow and flaky. Tests set the clock directly, which also makes the
// rollover at ~49 days reachable: slTestSetMillis(0xFFFFFF00) and step past
// the wrap, which is the only practical way to prove the unsigned arithmetic.
// ---------------------------------------------------------------------------
extern uint32_t slTestMillis;

inline void slTestSetMillis(uint32_t ms) { slTestMillis = ms; }
inline uint32_t millis() { return slTestMillis; }
inline uint32_t micros() { return slTestMillis * 1000u; }

// delay() exists so a stray call still links. loop() must never call it, and
// the harness asserts on that separately by grepping the source.
inline void delay(uint32_t ms) { slTestMillis += ms; }

// ---------------------------------------------------------------------------
// GPIO. Reads come from a table the test writes, so a button press is
// simulated by setting the pin low.
// ---------------------------------------------------------------------------
extern uint8_t slTestPinLevel[64];
extern uint8_t slTestPinMode[64];

inline void pinMode(uint8_t pin, uint8_t mode) {
  if (pin < 64) {
    slTestPinMode[pin] = mode;
    // INPUT_PULLUP idles HIGH, which is what the button sees at rest.
    if (mode == INPUT_PULLUP) {
      slTestPinLevel[pin] = HIGH;
    }
  }
}

inline int digitalRead(uint8_t pin) {
  return (pin < 64) ? slTestPinLevel[pin] : HIGH;
}

inline void digitalWrite(uint8_t pin, uint8_t value) {
  if (pin < 64) {
    slTestPinLevel[pin] = value;
  }
}

// ---------------------------------------------------------------------------
// LEDC. lamps.cpp drives the LEDs through PWM, so the duty written per pin is
// what the lamp assertions read back.
// ---------------------------------------------------------------------------
extern uint32_t slTestLedcDuty[64];
extern uint8_t slTestLedcAttached[64];

inline bool ledcAttach(uint8_t pin, uint32_t freq, uint8_t bits) {
  (void)freq;
  (void)bits;
  if (pin < 64) {
    slTestLedcAttached[pin] = 1;
    slTestLedcDuty[pin] = 0;
  }
  return true;
}

inline bool ledcWrite(uint8_t pin, uint32_t duty) {
  if (pin < 64) {
    slTestLedcDuty[pin] = duty;
  }
  return true;
}

// The 2.x channel API, present so the other arm of the #if in lamps.cpp still
// compiles if ESP_ARDUINO_VERSION_MAJOR is lowered.
inline uint32_t ledcSetup(uint8_t ch, uint32_t freq, uint8_t bits) {
  (void)ch;
  (void)bits;
  return freq;
}
inline void ledcAttachPin(uint8_t pin, uint8_t ch) {
  (void)pin;
  (void)ch;
}

// ---------------------------------------------------------------------------
// Serial. Only what the sketch uses. Not exercised directly by these tests,
// which drive LineReader::feed() a byte at a time instead, because that is
// the boundary where the defect lived.
// ---------------------------------------------------------------------------
class SerialStub {
 public:
  void begin(unsigned long baud) { (void)baud; }

  // Present so the sketch compiles on a host. It records nothing, because
  // there is no buffer here to size: the stub hands the reader every byte a
  // test gives it. That is exactly why the undersized real buffer could not
  // be caught by any host check -- see the note in stoplight.ino.
  void setRxBufferSize(size_t n) { (void)n; }
  int available() const { return 0; }
  int read() { return -1; }
  size_t print(const char* s) { return s ? strlen(s) : 0; }
  size_t println(const char* s) { return print(s) + 1; }
};

extern SerialStub Serial;

// Wire, for the sketch's I2C setup. The display stub needs no transport.
class WireStub {
 public:
  void begin(int sda, int scl) {
    (void)sda;
    (void)scl;
  }
  void beginTransmission(uint8_t a) { (void)a; }
  uint8_t endTransmission() { return 0; }
};

extern WireStub Wire;

#endif  // STOPLIGHT_TEST_ARDUINO_H
