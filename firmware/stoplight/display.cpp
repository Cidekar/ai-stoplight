// display.cpp - rotation, marquee scrolling, and the pin.
//
// The screen is two rows: a small header carrying the pin marker and the
// position counter, and a large label filling the rest of the panel. See
// display.h for why the state row is gone.
//
// The marquee is a four phase cycle driven entirely from elapsed time, with
// no per-frame accumulator:
//
//   hold at start (1s) -> scroll left 1px per 90ms -> hold at end -> reset
//
// It is a marquee and not a wrap-around loop on purpose. A loop never gives
// a stable moment to read the first characters, which is where the useful
// part of a branch name usually is.
//
// ROTATION WAITS FOR SCROLLING. The three second interval is a minimum, not
// a period. A slot advances when BOTH the minimum has elapsed AND the label
// has finished its scroll cycle. Rotation is therefore paced by content
// rather than by a clock, and runs slightly irregularly by design.

#include "display.h"

#include <string.h>

namespace {

// safeCopy copies a C string into a fixed buffer and always terminates it.
// strncpy does not terminate on truncation, which is the classic way this
// kind of code grows a buffer overrun.
void safeCopy(char* dst, const char* src, size_t cap) {
  size_t n = 0;
  while (src[n] != '\0' && n < cap) {
    dst[n] = src[n];
    n++;
  }
  dst[n] = '\0';
}

}  // namespace

Display::Display()
    // U8G2_R0: no rotation. The offset for the 72x40 window is applied by
    // this constructor, which is the whole reason it is the one to use.
    : u8g2_(U8G2_R0, U8X8_PIN_NONE),
      count_(0),
      index_(0),
      slotEnteredAt_(0),
      scrollStartAt_(0),
      pinned_(false),
      pinnedAt_(0),
      holding_(false),
      holdUntil_(0),
      asleep_(false),
      dirty_(true),
      lastReinitAt_(0),
      reinitCount_(0),
      lastDrawnOffset_(-1),
      lastDrawnIndex_(0xFF),
      lastDrawnCount_(0xFF),
      lastDrawnPinned_(false) {
  pinId_[0] = '\0';
  for (uint8_t i = 0; i < SL_MAX_SESSIONS; i++) {
    slots_[i].s.id[0] = '\0';
    slots_[i].s.label[0] = '\0';
    slots_[i].s.state[0] = '\0';
    slots_[i].s.color = COLOR_OFF;
    slots_[i].lastColor = COLOR_OFF;
  }
}

void Display::begin() {
  // Wire is started by the sketch with the correct SDA/SCL pins before this
  // runs, because the board's I2C is not on the core's default pins.
  u8g2_.begin();
  u8g2_.setBusClock(400000);  // fast mode; the panel handles it comfortably
  u8g2_.clearBuffer();
  u8g2_.sendBuffer();
}

// reassertPanel re-runs the init that begin() ran at boot, then redraws.
//
// This is the fix for the permanently blank screen. It is deliberately
// unconditional: see "THE BLANK SCREEN BUG" in display.h for the bench
// measurement showing that a blanked panel still ACKs its address, which is
// what rules out detecting the fault before repairing it.
//
// u8g2.begin() is idempotent on a healthy panel. It rewrites the same
// configuration registers with the same values and clears the internal
// buffer, so on the overwhelmingly common path where nothing is wrong this
// costs one short bus transaction and changes nothing on screen.
void Display::reassertPanel(uint32_t now) {
  lastReinitAt_ = now;
  reinitCount_++;

  u8g2_.begin();
  u8g2_.setBusClock(400000);

  // begin() leaves the controller powered on. If the screen is meant to be
  // blank, put it straight back to sleep rather than flashing the last frame
  // at a user who is not there. Nothing is drawn in that case.
  if (asleep_) {
    u8g2_.setPowerSave(1);
    return;
  }

  // begin() also cleared the panel's own buffer, so whatever was on screen is
  // gone and the "only redraw when the pixels differ" cache in tick() is now
  // wrong. Force a full redraw so the label comes straight back.
  dirty_ = true;
  render(now);
}

int Display::findById(const char* id) const {
  if (id == nullptr || id[0] == '\0') {
    return -1;
  }
  for (uint8_t i = 0; i < count_; i++) {
    if (strcmp(slots_[i].s.id, id) == 0) {
      return (int)i;
    }
  }
  return -1;
}

void Display::resetScroll(uint32_t now) {
  scrollStartAt_ = now;
  dirty_ = true;
}

void Display::clampIndex() {
  if (count_ == 0) {
    index_ = 0;
    return;
  }
  if (index_ >= count_) {
    index_ = 0;
  }
}

void Display::applyFrame(const Frame& f, uint32_t now) {
  // A frame with no "sessions" key leaves the screen alone entirely. This is
  // the "colour only" case: the lamps move, the screen does not.
  if (!f.hasSessions) {
    return;
  }

  // Remember what was on screen so the same session can keep the slot even
  // if the list reordered underneath it.
  char currentId[SL_MAX_ID + 1];
  currentId[0] = '\0';
  if (count_ > 0 && index_ < count_) {
    safeCopy(currentId, slots_[index_].s.id, SL_MAX_ID);
  }

  // Snapshot the previous colours before the rebuild overwrites slots_.
  // Only id and colour are needed, so this is ~200 bytes rather than a copy
  // of the whole Slot array.
  PriorColor previous[SL_MAX_SESSIONS];
  uint8_t previousCount = count_;
  for (uint8_t i = 0; i < previousCount; i++) {
    safeCopy(previous[i].id, slots_[i].s.id, SL_MAX_ID);
    previous[i].color = slots_[i].lastColor;
  }

  // Rebuild in the order the relay sent, which is start-time order. Keeping
  // the relay's order is what gives a session a stable place in the cycle.
  count_ = 0;
  bool currentScrollInvalidated = false;

  for (uint8_t i = 0; i < f.count && count_ < SL_MAX_SESSIONS; i++) {
    Slot& dst = slots_[count_];
    dst.s = f.sessions[i];

    // Find this session's previous colour, if we had it before.
    Color prevColor = dst.s.color;
    bool existed = false;
    for (uint8_t j = 0; j < previousCount; j++) {
      if (strcmp(previous[j].id, dst.s.id) == 0) {
        prevColor = previous[j].color;
        existed = true;
        break;
      }
    }
    dst.lastColor = dst.s.color;

    // Scrolling resets to the start whenever the session changes colour.
    // Only matters if this is the session currently on screen.
    if (existed && prevColor != dst.s.color &&
        currentId[0] != '\0' && strcmp(dst.s.id, currentId) == 0) {
      currentScrollInvalidated = true;
    }

    count_++;
  }

  // Put the screen back on the session it was showing, wherever it moved to.
  int keep = findById(currentId);
  if (keep >= 0) {
    if ((uint8_t)keep != index_) {
      index_ = (uint8_t)keep;
      // The slot did not change, only its position, so the scroll cycle and
      // the slot timer both continue rather than restarting.
      dirty_ = true;
    }
  } else {
    // The session on screen is gone. Start the replacement cleanly.
    clampIndex();
    slotEnteredAt_ = now;
    resetScroll(now);
  }

  clampIndex();

  if (currentScrollInvalidated) {
    resetScroll(now);
  }

  // A pin releases itself the moment its session disappears from the frame.
  if (pinned_ && findById(pinId_) < 0) {
    pinned_ = false;
    pinId_[0] = '\0';
    // Rotation resumes from here, so give the current slot a full interval
    // rather than advancing the instant the pin drops.
    slotEnteredAt_ = now;
    resetScroll(now);
  }

  // A session turning red takes the screen immediately and holds a full
  // interval. The moment something needs you is the moment you want to know
  // which project it is.
  //
  // This jump respects the pin: a pin is a deliberate instruction to watch
  // one session, and the lamp has already gone red regardless, so the alert
  // is not hidden. The lamps are the alert; the screen is the detail.
  if (!pinned_) {
    for (uint8_t i = 0; i < count_; i++) {
      bool wasRed = false;
      for (uint8_t j = 0; j < previousCount; j++) {
        if (strcmp(previous[j].id, slots_[i].s.id) == 0) {
          wasRed = (previous[j].color == COLOR_RED);
          break;
        }
      }
      // Only a transition into red jumps. A session that is already red and
      // stays red must not hold the screen hostage every frame.
      if (slots_[i].s.color == COLOR_RED && !wasRed) {
        index_ = i;
        slotEnteredAt_ = now;
        // holding_ carries "a hold is in force"; holdUntil_ carries only when
        // it ends. now + DISP_ROTATE_MIN_MS may be any value at all, 0
        // included, once millis() has wrapped.
        holding_ = true;
        holdUntil_ = now + DISP_ROTATE_MIN_MS;
        resetScroll(now);
        break;
      }
    }
  }

  dirty_ = true;

  // Any frame is activity, so bring the screen back if it had blanked.
  if (asleep_) {
    wake(now);
  }
}

bool Display::labelOverflows(const Slot& sl, int16_t* overflowPx) const {
  int16_t w = (int16_t)(strlen(sl.s.label) * DISP_CHAR_W);
  if (w <= DISP_W) {
    *overflowPx = 0;
    return false;
  }
  *overflowPx = (int16_t)(w - DISP_W);
  return true;
}

// scrollOffset returns how far left the label should be drawn, in pixels,
// and reports whether the marquee has completed a full cycle.
//
// Phases, measured from scrollStartAt_:
//   [0, hold)                     offset 0, holding at the start
//   [hold, hold+travel)           moving left, 1px per DISP_SCROLL_STEP_MS
//   [hold+travel, +holdEnd)       offset max, holding at the end
//   beyond                        finished; reset to the start
int16_t Display::scrollOffset(uint32_t now, int16_t overflowPx,
                              bool* finished) const {
  if (overflowPx <= 0) {
    // A label that fits is drawn statically and never moves. It is also
    // never "unfinished", so it can never hold up rotation.
    *finished = true;
    return 0;
  }

  uint32_t elapsed = (uint32_t)(now - scrollStartAt_);
  uint32_t travelMs = (uint32_t)overflowPx * DISP_SCROLL_STEP_MS;

  if (elapsed < DISP_SCROLL_HOLD_START_MS) {
    *finished = false;
    return 0;
  }

  uint32_t moving = elapsed - DISP_SCROLL_HOLD_START_MS;
  if (moving < travelMs) {
    *finished = false;
    return (int16_t)(moving / DISP_SCROLL_STEP_MS);
  }

  if (moving < travelMs + DISP_SCROLL_HOLD_END_MS) {
    *finished = false;  // holding at the end, still part of the cycle
    return overflowPx;
  }

  // One full cycle done. Rotation may now advance. If it does not (one
  // session, or pinned) the caller resets scrollStartAt_ and the marquee
  // runs again from the start.
  *finished = true;
  return overflowPx;
}

void Display::expirePin(uint32_t now) {
  if (!pinned_) {
    return;
  }
  // A pin also clears after a timeout even if the session is still alive.
  // Without expiry you pin something on a Monday, forget, and quietly stop
  // seeing every other session for a week.
  if ((uint32_t)(now - pinnedAt_) >= DISP_PIN_TIMEOUT_MS) {
    pinned_ = false;
    pinId_[0] = '\0';
    slotEnteredAt_ = now;
    resetScroll(now);
    dirty_ = true;
  }
}

void Display::tick(uint32_t now) {
  expirePin(now);

  // Re-assert the panel's configuration on a timer. This is the recovery for
  // a controller that has lost its init, and it runs BEFORE the asleep_
  // return on purpose: a panel can be disturbed while the screen is blanked
  // just as easily as while it is showing something, and the repair must not
  // wait for the next frame to arrive. reassertPanel keeps a sleeping panel
  // asleep. Unsigned subtraction is correct across the millis() rollover.
  if ((uint32_t)(now - lastReinitAt_) >= DISP_REINIT_INTERVAL_MS) {
    reassertPanel(now);
  }

  if (asleep_) {
    return;  // nothing to advance while the panel is blank
  }

  if (count_ == 0) {
    if (dirty_) {
      render(now);
    }
    return;
  }

  clampIndex();

  int16_t overflowPx = 0;
  bool overflows = labelOverflows(slots_[index_], &overflowPx);
  bool scrollFinished = false;
  int16_t offset = scrollOffset(now, overflowPx, &scrollFinished);

  // A single session does not rotate: a cycle of one is a static screen
  // redrawing for no reason. Its label still scrolls, so the marquee is
  // restarted here rather than advancing to a next slot that does not exist.
  bool canRotate = (count_ > 1) && !pinned_;

  bool minElapsed = (uint32_t)(now - slotEnteredAt_) >= DISP_ROTATE_MIN_MS;

  bool holdOver = !holding_ || ((int32_t)(now - holdUntil_) >= 0);

  if (canRotate && minElapsed && scrollFinished && holdOver) {
    // Both conditions met: the minimum interval has passed AND the label has
    // finished scrolling. This is the whole of "rotation waits for
    // scrolling".
    index_ = (uint8_t)((index_ + 1) % count_);
    slotEnteredAt_ = now;
    holding_ = false;
    resetScroll(now);
    render(now);
    return;
  }

  if (!canRotate && overflows && scrollFinished) {
    // Nowhere to advance to, but the label still needs to be readable, so
    // run the marquee again. Scrolling continues while pinned, by design.
    resetScroll(now);
    render(now);
    return;
  }

  // Redraw only when the pixels would differ. Static labels therefore cost
  // one draw and then nothing.
  if (dirty_ || offset != lastDrawnOffset_ || index_ != lastDrawnIndex_ ||
      count_ != lastDrawnCount_ || pinned_ != lastDrawnPinned_) {
    render(now);
  }
}

void Display::render(uint32_t now) {
  u8g2_.clearBuffer();

  if (count_ == 0) {
    // No live sessions: the screen is cleared. Nothing is running, so there
    // is nothing to name.
    u8g2_.sendBuffer();
    dirty_ = false;
    lastDrawnOffset_ = 0;
    lastDrawnIndex_ = 0xFF;
    lastDrawnCount_ = 0;
    lastDrawnPinned_ = pinned_;
    return;
  }

  clampIndex();
  const Slot& sl = slots_[index_];

  // Line 1: the pin marker, then one dot per session with the current one
  // filled.
  //
  // The marker keeps the top-left corner and the dots always start at the
  // same x whether it is there or not. Dots that shifted sideways on every
  // pin would read as the position having changed, which is the one thing
  // this row exists to communicate.
  if (pinned_) {
    u8g2_.setFont(u8g2_font_5x7_tf);
    u8g2_.setFontPosTop();
    u8g2_.drawStr(DISP_PIN_X, 0, "*");
  }

  // One session draws no dots at all. A single dot says nothing that the
  // label above it does not already say, and this matches the existing rule
  // that a lone session does not rotate.
  if (count_ > 1) {
    for (uint8_t i = 0; i < count_; i++) {
      // Centres, not left edges: drawCircle and drawDisc both take a centre.
      uint16_t cx =
          (uint16_t)(DISP_DOTS_X + DISP_DOT_R + i * DISP_DOT_SPACING);

      // Never draw past the right edge. count_ is capped at SL_MAX_SESSIONS
      // (8), which fits by construction, but the guard costs nothing and
      // means raising that cap degrades by dropping dots rather than by
      // wrapping them onto the label.
      if (cx + DISP_DOT_R >= DISP_W) {
        break;
      }

      if (i == index_) {
        u8g2_.drawDisc(cx, DISP_DOT_CY, DISP_DOT_R);
      } else {
        u8g2_.drawCircle(cx, DISP_DOT_CY, DISP_DOT_R);
      }
    }
  }

  // Line 2: the label, in the large font, scrolled if it overflows.
  int16_t overflowPx = 0;
  bool overflows = labelOverflows(sl, &overflowPx);
  bool finished = false;
  int16_t offset = overflows ? scrollOffset(now, overflowPx, &finished) : 0;

  u8g2_.setFont(DISP_LABEL_FONT);

  // Clip the label to its own band so a scrolled glyph cannot bleed up into
  // the counter. The band runs to the bottom of the panel now that nothing
  // sits below it.
  u8g2_.setClipWindow(0, DISP_LABEL_BAND_TOP, DISP_W, DISP_H);

  // drawStr takes u8g2_uint_t, which is unsigned (uint16_t on a 32-bit
  // target, where U8G2_16BIT is set). A negative x therefore wraps to a
  // large positive value, and U8g2's clipping discards it as off-screen to
  // the left, which is exactly the intent. Casting explicitly rather than
  // relying on an implicit narrowing conversion keeps that deliberate and
  // silences the warning it would otherwise produce.
  u8g2_.drawStr((uint16_t)(int16_t)(-offset), DISP_LABEL_Y, sl.s.label);

  u8g2_.setMaxClipWindow();

  u8g2_.sendBuffer();

  dirty_ = false;
  lastDrawnOffset_ = offset;
  lastDrawnIndex_ = index_;
  lastDrawnCount_ = count_;
  lastDrawnPinned_ = pinned_;
}

void Display::togglePin(uint32_t now) {
  if (asleep_) {
    wake(now);
    return;  // the first press wakes the screen rather than acting blind
  }
  if (count_ == 0) {
    return;  // nothing to pin
  }
  clampIndex();

  if (pinned_) {
    pinned_ = false;
    pinId_[0] = '\0';
    // Give the newly unpinned slot a full interval before rotation resumes,
    // so the screen does not jump the instant the button is released.
    slotEnteredAt_ = now;
    resetScroll(now);
  } else {
    pinned_ = true;
    safeCopy(pinId_, slots_[index_].s.id, SL_MAX_ID);
    pinnedAt_ = now;
    // Scrolling continues while pinned: a pinned label still needs to be
    // readable. The scroll cycle is deliberately not reset here.
  }

  dirty_ = true;
}

void Display::advance(uint32_t now) {
  if (asleep_) {
    wake(now);
    return;
  }
  if (count_ == 0) {
    return;
  }

  index_ = (uint8_t)((index_ + 1) % count_);
  slotEnteredAt_ = now;
  holding_ = false;  // a manual move overrides a red hold: the user is driving
  resetScroll(now);

  // Advancing past a pinned session moves the pin with the screen. The
  // alternative, silently unpinning, would resume rotation without the user
  // asking for it.
  if (pinned_) {
    safeCopy(pinId_, slots_[index_].s.id, SL_MAX_ID);
    pinnedAt_ = now;  // an explicit interaction restarts the 15 minute clock
  }

  dirty_ = true;
  render(now);
}

const char* Display::currentLabel() const {
  if (count_ == 0 || index_ >= count_) {
    return "";
  }
  return slots_[index_].s.label;
}

void Display::sleep() {
  if (asleep_) {
    return;
  }
  asleep_ = true;
  // Power down the panel rather than drawing a blank buffer: an OLED with
  // every pixel off still runs its charge pump.
  u8g2_.setPowerSave(1);
}

void Display::wake(uint32_t now) {
  if (!asleep_) {
    return;
  }
  asleep_ = false;
  u8g2_.setPowerSave(0);
  // Give the restored slot a clean interval and scroll cycle, so the first
  // thing seen after waking is the start of the label.
  slotEnteredAt_ = now;
  resetScroll(now);
  dirty_ = true;
  render(now);
}
