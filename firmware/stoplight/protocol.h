// protocol.h - the wire format between the relay and this device.
//
// One JSON object per line, newline terminated, over serial (BLE later).
// The format is documented in design.md, "Relay to light".
//
//   {"color":"red","sessions":[{"id":"a1","label":"auth-api",
//     "state":"needs you","color":"red"}]}
//
// Two rules from the spec drive every decision in this file:
//
//   1. EVERY field is optional. A frame carrying only "color" moves the
//      lamps and leaves the screen alone. That is why Frame carries
//      hasColor / hasSessions flags rather than sentinel values: absent
//      and empty are different instructions.
//
//   2. UNKNOWN FIELDS ARE IGNORED, NEVER REJECTED. The relay and the
//      firmware ship independently, so a field added to the relay next
//      month must not brick a light flashed today.
//
// KEY ORDER IS NOT REQUIRED, BUT "color" FIRST IS CHEAPER. parseFrame is
// order independent, and so is the overflow salvage below: an oversized line
// surrenders its aggregate whether "color" came before the sessions array or
// after it. The two are recovered by different means, though, and the cheap
// one only works in one direction:
//
//   "color" FIRST   the prefix the reader kept contains the key, so a plain
//                   forward parse finds it. Exact, and it works no matter how
//                   far past the cap the line ran.
//   "color" LAST    the key is past the truncation point and is recovered
//                   from SL_TAIL_MAX bytes of retained tail instead. That
//                   works, but it needs the key and its value to fall inside
//                   the last SL_TAIL_MAX bytes, which is a weaker guarantee
//                   than the prefix path gives.
//
// The relay's Go encoder emits "color" first because the Frame struct
// declares it first, and internal/stoplight/frame_test.go asserts that,
// so reordering the struct fails a test rather than silently downgrading a
// light to the weaker path. Neither path may ever be the ONLY one: a lamp
// that depends on Go struct field order is a lamp that goes wrong quietly.

#ifndef STOPLIGHT_PROTOCOL_H
#define STOPLIGHT_PROTOCOL_H

#include <Arduino.h>

// Caps. Everything is a fixed-size buffer: no heap, no fragmentation over
// a multi-day uptime, and a malformed frame can never grow memory.
#define SL_MAX_SESSIONS 8   // more than fits comfortably in a rotation
#define SL_MAX_ID 40        // a 36 char UUID plus headroom; see below
#define SL_MAX_LABEL 48     // long enough to be worth scrolling
#define SL_MAX_STATE 16     // "needs you" is the longest the relay sends

// SL_MAX_ID must hold a whole session id, because ids are matched with
// strcmp and a truncated id is not a shorter id, it is a WRONG id. A Claude
// Code session_id is a 36 character UUID, so the previous cap of 24 turned
// two sessions sharing a UUID prefix into the same id. That is not cosmetic:
// Display::findById is the pin anchor and the key for preserving a rotation
// slot, so colliding ids make the pin follow the wrong session and make the
// did-this-turn-red check read another session's previous colour, which
// suppresses the jump to a session that just went red. 40 covers the UUID
// with room for a prefixed or suffixed id from a future producer.
//
// The cost is 16 bytes per stored id. Ids live in Session (8 in Frame, 8 in
// Display::slots_) and in Display::pinId_, which grows sizeof(Session) from
// 92 to 108 bytes, Frame from 740 to 868, and Display from 800 to 944.
// Measured on the host with the same layout rules the C3 uses.

// SL_LINE_MAX is one whole frame. It must exceed the largest frame the relay
// can emit, because a line over the cap is not merely truncated, it stops
// being parsed as a session list at all.
//
// Worst case, with the relay capping the list at SL_MAX_SESSIONS:
//
//   per entry  {"id":"<40>","label":"<48>","state":"needs you",
//               "color":"yellow"}                         = 145 bytes
//   8 entries  8 * 145                                    = 1160
//   separators 7 commas between entries                   =    7
//   envelope   {"color":"yellow","sessions":[]}           =   32
//                                                          ------
//                                                           1199
//
// Measured, not estimated: the figure above is what encoding/json produces
// for that frame. 1536 leaves 337 bytes of headroom, which absorbs a label
// override longer than SL_MAX_LABEL (the relay does not bound labels, and
// the firmware truncates them on the way in rather than refusing them) and
// any small key a future relay adds. The buffer is a fixed member of one
// static LineReader, so the extra 1024 bytes over the old cap is a one-off
// static cost, not per frame.
#define SL_LINE_MAX 1536

// SL_TAIL_MAX is how many bytes from the END of an oversized line the reader
// keeps, so that a "color" key placed after the sessions array is still
// recoverable. See the key order note at the top of this file.
//
// Sizing: the pair the tail scan looks for is
//
//   ,"color":"yellow"}                                    = 18 bytes
//
// plus whatever trailing keys a future relay adds after it. 96 bytes absorbs
// several such keys and still costs under 100 bytes of static RAM, which is
// the whole budget for making the salvage independent of key order. It is a
// ring buffer, so an arbitrarily long line costs no more than this.
#define SL_TAIL_MAX 96

// TOTAL COST OF BOTH CAPS, measured rather than estimated. The three objects
// that hold this data are all static: the reusable Frame in the sketch, the
// LineReader, and the Display.
//
//                    old (24/512)   new (40/1536)
//   Frame                    740             868
//   LineReader               520            1544
//   Display                  800             944
//                          -----           -----
//                           2060            3356    (+1296 bytes)
//
// The ESP32-C3 has 400KB of SRAM, so the whole of it is 0.8% and the increase
// is 0.3%. That is a sane price for a lamp that shows the right colour.

// Color is ordered by urgency so that a numeric max() over a set yields
// the aggregate. This mirrors the Go Color type in design.md.
enum Color : uint8_t {
  COLOR_OFF = 0,
  COLOR_GREEN = 1,
  COLOR_YELLOW = 2,
  COLOR_RED = 3
};

struct Session {
  char id[SL_MAX_ID + 1];
  char label[SL_MAX_LABEL + 1];
  char state[SL_MAX_STATE + 1];
  Color color;
};

struct Frame {
  Color color;        // the aggregate; only meaningful when hasColor
  bool hasColor;      // "color" was present in the JSON
  // "sessions" was present AND its array closed cleanly, even if it was
  // empty. A TRUNCATED array leaves this false on purpose. An unterminated
  // list is not an instruction to clear the screen: see the note beside
  // sessionsClosed in parseFrame for the blank-screen bug that caused.
  bool hasSessions;
  uint8_t count;      // number of entries filled in sessions[]
  Session sessions[SL_MAX_SESSIONS];
};

// parseColor maps a wire name onto a Color. Unknown names return false and
// the caller leaves the previous value alone, per the compatibility rule.
bool parseColor(const char* s, Color* out);

// parseFrame parses one complete line of JSON into out.
//
// Returns false when the line is unusable: empty, not a JSON object, or
// GARBLED. It never returns false for an unrecognised field: those are
// skipped. A truncated frame that yielded something usable still returns
// true, because showing most of a frame beats showing none of it.
//
// TRUNCATED and GARBLED are not the same. A frame cut short mid-token (the
// link dropped before the newline) is salvageable: its keys are real, so it
// returns true and the aggregate reaches the lamps. A frame GARBLED by two
// frames being glued at a lost newline, {"color":"yellow","sess{"color":
// "green",...}, is not: its structure breaks on a character the grammar
// forbids rather than on the end of the line, its keys cannot be trusted, and
// it returns false so the stale colour is not kept and the next clean frame is
// awaited. See the note beside corrupt in parseFrame.
bool parseFrame(const char* line, Frame* out);

// findAggregateColor scans a possibly incomplete frame for the top level
// "color" key and returns its value. It exists for the overflow path, where
// the session list is unusable but the aggregate must still reach the lamps.
//
// Only the top level is considered: a "color" inside the sessions array
// belongs to one session and is not the aggregate. Returns false when no
// usable top level colour is present, and the caller then leaves the lamps
// alone.
//
// The scan is a forward parse, so it recovers the aggregate only when "color"
// appears before the truncation point. findTailAggregateColor covers the
// other order; LineReader uses both.
bool findAggregateColor(const char* prefix, Color* out);

// findTailAggregateColor scans the LAST bytes of an oversized frame for a
// top level "color" key, for the case where the relay put "color" after the
// sessions array and the prefix scan therefore never reached it.
//
// tail points at the final characters of the line, in order, and is NUL
// terminated. It begins mid-frame, so the nesting depth at its start is
// unknown. Recovery is therefore restricted to the one shape that is
// unambiguous: a "color":"<name>" pair that is followed, before the end of
// the line, only by the frame's closing brace and whitespace. Anything still
// open at that point means the pair was inside the sessions array rather than
// at the top level, and it is refused.
//
// Returns false when no such pair is present, and the caller then leaves the
// lamps alone rather than guessing.
bool findTailAggregateColor(const char* tail, Color* out);

// LineReader accumulates serial bytes until it sees a newline. It is
// non-blocking by construction: feed() takes one byte and returns whether
// a full line is ready, so the caller never waits on the port.
//
// OVERSIZED LINES DEGRADE, THEY DO NOT VANISH. A line longer than
// SL_LINE_MAX still cannot be parsed as a session list, because the tail is
// gone and a truncated list looks like sessions ending. But the aggregate
// colour is the one field that must never be lost: it is the whole reason
// the device exists. So an overflowing line is reported through
// overflowColor() instead of through line(), and the caller moves the lamps
// while leaving the screen untouched. Losing the labels is acceptable.
// Losing the lamp is not.
//
// The salvage does not depend on where "color" sits in the frame. The reader
// keeps both the prefix up to SL_LINE_MAX and the last SL_TAIL_MAX bytes, and
// tries the prefix first and the tail second, so either key order yields the
// aggregate. See the key order note at the top of this file.
class LineReader {
 public:
  LineReader();

  // feed returns true when c completed a line. The line is then available
  // from line() until the next feed() call.
  //
  // feed returns false for an oversized line, because there is no line to
  // hand back. Check overflowColor() as well as feed(): the two are
  // exclusive, and an oversized line signals only through the former.
  bool feed(char c);

  const char* line() const { return buf_; }

  // overflowColor reports the aggregate recovered from the line that just
  // overflowed, and is true only on the feed() call that consumed that
  // line's terminator. It is cleared by the next feed().
  bool overflowColor(Color* out) const {
    if (!haveOverflowColor_) {
      return false;
    }
    *out = overflowColor_;
    return true;
  }

  void reset();

 private:
  // tailAppend records one byte of an overflowing line in the ring buffer,
  // and tailLinearise copies the ring out in order into out, which holds
  // SL_TAIL_MAX characters plus a terminator.
  void tailAppend(char c);
  void tailLinearise(char* out) const;

  char buf_[SL_LINE_MAX + 1];
  uint16_t len_;
  bool overflow_;  // line was too long; discard through to the next newline

  // The last SL_TAIL_MAX bytes of the current overflowing line, as a ring so
  // that a line of any length costs a fixed amount. Only written while
  // overflow_ is set: a line that fits is recovered from buf_ instead.
  char tail_[SL_TAIL_MAX];
  uint8_t tailLen_;    // bytes held, up to SL_TAIL_MAX
  uint8_t tailHead_;   // where the next byte goes

  // The aggregate salvaged from an overflowing line, valid for exactly one
  // feed() call. Kept here rather than returned so that feed() keeps its
  // one-byte-in, one-bool-out shape.
  bool haveOverflowColor_;
  Color overflowColor_;
};

#endif  // STOPLIGHT_PROTOCOL_H
