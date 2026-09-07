// U8g2lib.h - a host stub of the U8g2 surface display.cpp uses.
//
// Only the constructor and the handful of methods the display module calls
// are needed. Nothing renders: the stub records the last text drawn and at
// what offset, which is enough to assert that the marquee moved, that the
// counter reads "2/4", and that a clear left the screen empty.
//
// u8g2_uint_t is UNSIGNED here, exactly as it is in the library when
// U8G2_16BIT is set on a 32-bit target. That matters: display.cpp draws the
// scrolled label at a negative x cast to this type, relying on the wrap to a
// large positive value being clipped off-screen to the left. A signed stub
// would quietly compile a different program from the one that gets flashed.

#ifndef STOPLIGHT_TEST_U8G2LIB_H
#define STOPLIGHT_TEST_U8G2LIB_H

#include <stdint.h>
#include <string.h>

typedef uint16_t u8g2_uint_t;

#define U8G2_R0 0
#define U8X8_PIN_NONE 255

// Fonts are referenced by address only, so extern symbols are enough.
extern const uint8_t u8g2_font_5x7_tf[];
extern const uint8_t u8g2_font_10x20_tf[];
extern const uint8_t u8g2_font_7x13_tf[];
extern const uint8_t u8g2_font_9x18_tf[];

// What the stub recorded from the last frame the firmware drew. The display
// assertions read these instead of pixels.
struct SlTestScreen {
  char header[64];      // the text drawn on line 1 (the pin marker)
  char label[128];      // the text drawn on the label line
  char state[64];       // retained: the old third row, no longer drawn
  int32_t labelX;       // x the label was drawn at, sign restored
  bool cleared;         // clearBuffer() ran and nothing was drawn after it
  uint32_t sendCount;   // how many buffer transfers happened
  bool powerSave;       // panel powered down

  // beginCount counts calls to begin(). The periodic re-assert that fixes the
  // permanently blank screen is unconditional, so there is no error state to
  // observe: counting the init is the only way the harness can see it happen
  // at all. See "THE BLANK SCREEN BUG" in display.h.
  uint32_t beginCount;

  // The position dots that replaced the "2/4" counter. dots counts how many
  // were drawn, filledDot is the index of the solid one (-1 if none), and
  // dotX holds their centres so a test can assert they do not run off the
  // panel. Recording the shape of the row rather than pixels is enough for
  // "one dot per session, the current one filled".
  uint8_t dots;
  int16_t filledDot;
  uint16_t dotX[16];
};

extern SlTestScreen slTestScreen;

class U8G2_SSD1306_72X40_ER_F_HW_I2C {
 public:
  U8G2_SSD1306_72X40_ER_F_HW_I2C(uint8_t rotation, uint8_t reset) {
    (void)rotation;
    (void)reset;
  }

  void begin() { slTestScreen.beginCount++; }
  void setBusClock(uint32_t hz) { (void)hz; }
  void setFont(const uint8_t* f) { (void)f; }
  void setFontPosTop() {}
  void setPowerSave(uint8_t on) { slTestScreen.powerSave = (on != 0); }

  void clearBuffer() {
    slTestScreen.header[0] = '\0';
    slTestScreen.label[0] = '\0';
    slTestScreen.state[0] = '\0';
    slTestScreen.labelX = 0;
    slTestScreen.dots = 0;
    slTestScreen.filledDot = -1;
    slTestScreen.cleared = true;
  }

  void sendBuffer() { slTestScreen.sendCount++; }

  // The dots. A hollow circle is another session, a filled disc is the one on
  // screen, and both are recorded in draw order so the index of the filled one
  // is its position in the row.
  void drawCircle(u8g2_uint_t x, u8g2_uint_t y, u8g2_uint_t r) {
    (void)y;
    (void)r;
    recordDot(x);
  }

  void drawDisc(u8g2_uint_t x, u8g2_uint_t y, u8g2_uint_t r) {
    (void)y;
    (void)r;
    slTestScreen.filledDot = (int16_t)slTestScreen.dots;
    recordDot(x);
  }

  void setClipWindow(u8g2_uint_t x0, u8g2_uint_t y0, u8g2_uint_t x1,
                     u8g2_uint_t y1) {
    (void)x0;
    (void)y0;
    (void)x1;
    (void)y1;
  }
  void setMaxClipWindow() {}

  // drawStr routes on the header's y only. The layout is two rows now, so
  // "y == 0 is the header, anything below it is the label" needs no constant
  // from display.h and does not drift when the label row moves.
  u8g2_uint_t drawStr(u8g2_uint_t x, u8g2_uint_t y, const char* s) {
    if (s == nullptr) {
      return 0;
    }
    slTestScreen.cleared = false;

    if (y == 0) {
      copyInto(slTestScreen.header, sizeof(slTestScreen.header), s);
    } else {
      copyInto(slTestScreen.label, sizeof(slTestScreen.label), s);
      // Undo the deliberate unsigned wrap so a test can assert the offset.
      slTestScreen.labelX = (int16_t)x;
    }
    return (u8g2_uint_t)(strlen(s) * 10);
  }

 private:
  static void recordDot(u8g2_uint_t x) {
    slTestScreen.cleared = false;
    if (slTestScreen.dots <
        sizeof(slTestScreen.dotX) / sizeof(slTestScreen.dotX[0])) {
      slTestScreen.dotX[slTestScreen.dots] = x;
    }
    slTestScreen.dots++;
  }

  static void copyInto(char* dst, size_t cap, const char* src) {
    size_t n = 0;
    while (src[n] != '\0' && n + 1 < cap) {
      dst[n] = src[n];
      n++;
    }
    dst[n] = '\0';
  }
};

#endif  // STOPLIGHT_TEST_U8G2LIB_H
