// stubs.cpp - storage for the host stubs.
//
// The stub headers declare this state extern so both the firmware modules and
// the test file see one copy of the simulated clock, pins and screen.

#include "Arduino.h"
#include "U8g2lib.h"

uint32_t slTestMillis = 0;

uint8_t slTestPinLevel[64];
uint8_t slTestPinMode[64];

uint32_t slTestLedcDuty[64];
uint8_t slTestLedcAttached[64];

SerialStub Serial;
WireStub Wire;

// Referenced by address only; the stub never dereferences them.
const uint8_t u8g2_font_5x7_tf[1] = {0};
const uint8_t u8g2_font_10x20_tf[1] = {0};
const uint8_t u8g2_font_7x13_tf[1] = {0};
const uint8_t u8g2_font_9x18_tf[1] = {0};

SlTestScreen slTestScreen;
