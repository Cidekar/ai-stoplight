// test_main.cpp - host-compiled checks for the Stoplight firmware.
//
// design.md's invariants section opens with "Each one is a test". This is that
// file. The firmware modules are compiled here against the stubs in stubs/,
// so every rule below runs on a host compiler under AddressSanitizer and
// UndefinedBehaviorSanitizer rather than only on a board nobody can attach to
// CI.
//
// Two defects motivated the harness, and both are pinned here:
//
//   1. A frame over SL_LINE_MAX was dropped whole, aggregate included. With
//      five real sessions the frame exceeded 512 bytes, so a red lamp never
//      arrived and the light held green. Covered by the frame size checks and
//      the overflow degradation checks.
//
//   2. SL_MAX_ID truncated a 36 character UUID to 24, so two sessions sharing
//      a prefix became one id. That misdirects the pin and suppresses the
//      jump to red. Covered by the UUID checks.

#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include <string>

#include "Arduino.h"
#include "U8g2lib.h"
#include "blelink.h"
#include "button.h"
#include "display.h"
#include "lamps.h"
#include "protocol.h"
#include "standby.h"

namespace {

int gChecks = 0;
int gFailures = 0;

void check(bool ok, const char* what) {
  gChecks++;
  if (!ok) {
    gFailures++;
    printf("  FAIL  %s\n", what);
  }
}

void checkStr(const char* got, const char* want, const char* what) {
  gChecks++;
  if (strcmp(got, want) != 0) {
    gFailures++;
    printf("  FAIL  %s: got \"%s\", want \"%s\"\n", what, got, want);
  }
}

void checkInt(long got, long want, const char* what) {
  gChecks++;
  if (got != want) {
    gFailures++;
    printf("  FAIL  %s: got %ld, want %ld\n", what, got, want);
  }
}

void section(const char* name) { printf("%s\n", name); }

// ---------------------------------------------------------------------------
// Frame builders. These mirror what internal/stoplight/tracker.go emits, so a
// size measured here is the size that actually goes over the wire.
// ---------------------------------------------------------------------------

// buildFrame produces one frame with n sessions, using an id and label of the
// given lengths. The aggregate is whatever `color` says.
std::string buildFrame(int n, const char* color, size_t idLen, size_t labelLen,
                       const char* state, const char* sessionColor) {
  std::string s = "{\"color\":\"";
  s += color;
  s += "\",\"sessions\":[";
  for (int i = 0; i < n; i++) {
    if (i > 0) {
      s += ",";
    }
    // A realistic UUID shape, made unique in its LAST characters so that a
    // truncating parser collapses them together. That is exactly the defect.
    std::string id = "3f2a1b7c-9d4e-4a10-b8c3-0000000000";
    while (id.size() < idLen) {
      id += "x";
    }
    id.resize(idLen);
    char suffix[8];
    snprintf(suffix, sizeof(suffix), "%02d", i);
    id[idLen - 2] = suffix[0];
    id[idLen - 1] = suffix[1];

    std::string label(labelLen, 'y');

    s += "{\"id\":\"" + id + "\",\"label\":\"" + label + "\",\"state\":\"" +
         state + "\",\"color\":\"" + sessionColor + "\"}";
  }
  s += "]}";
  return s;
}

// feedLine pushes a whole line plus its terminator through a LineReader and
// reports what came out: whether a frame completed, and whether an aggregate
// was salvaged from an overflow.
struct FeedResult {
  bool completed;
  bool salvaged;
  Color salvagedColor;
  std::string line;
};

FeedResult feedLine(LineReader* r, const std::string& text) {
  FeedResult out;
  out.completed = false;
  out.salvaged = false;
  out.salvagedColor = COLOR_OFF;

  for (size_t i = 0; i < text.size(); i++) {
    if (r->feed(text[i])) {
      out.completed = true;
      out.line = r->line();
    }
  }
  if (r->feed('\n')) {
    out.completed = true;
    out.line = r->line();
  }
  Color c;
  if (r->overflowColor(&c)) {
    out.salvaged = true;
    out.salvagedColor = c;
  }
  return out;
}

// ---------------------------------------------------------------------------
// Frame sizes at 1, 4, 5 and 8 sessions.
//
// The original bug in one table: with 36 char UUIDs and a 12 char label, five
// sessions already exceeded the old 512 byte cap, and eight were nowhere near
// fitting. Every one of these must now be accepted whole.
// ---------------------------------------------------------------------------
void testFrameSizes() {
  section("frame sizes");

  struct Case {
    int sessions;
    size_t wasOverOldCap;  // did this exceed the old 512 byte limit
  };
  const Case cases[] = {{1, 0}, {4, 0}, {5, 1}, {8, 1}};

  for (size_t i = 0; i < sizeof(cases) / sizeof(cases[0]); i++) {
    const int n = cases[i].sessions;
    // 36 char UUID and a 12 char label: the measured real-world shape.
    std::string frame = buildFrame(n, "red", 36, 12, "needs you", "red");

    char what[128];
    snprintf(what, sizeof(what), "%d sessions (%zu bytes) fits SL_LINE_MAX", n,
             frame.size());
    check(frame.size() <= SL_LINE_MAX, what);

    // Confirm the premise: 5 and 8 really were over the old cap, so these
    // cases are guarding the actual regression rather than a hypothetical.
    snprintf(what, sizeof(what), "%d sessions vs the old 512 byte cap", n);
    check((frame.size() > 512) == (cases[i].wasOverOldCap != 0), what);

    // And the frame survives the reader and the parser intact.
    LineReader reader;
    FeedResult fed = feedLine(&reader, frame);
    snprintf(what, sizeof(what), "%d sessions completes a line", n);
    check(fed.completed, what);

    Frame f;
    snprintf(what, sizeof(what), "%d sessions parses", n);
    check(parseFrame(fed.line.c_str(), &f), what);

    snprintf(what, sizeof(what), "%d sessions keeps the aggregate", n);
    check(f.hasColor && f.color == COLOR_RED, what);

    snprintf(what, sizeof(what), "%d sessions yields %d entries", n, n);
    checkInt(f.count, n, what);
  }

  // The true worst case the relay can now emit: SL_MAX_SESSIONS entries at
  // the maximum id and label the firmware stores. This is the number
  // SL_LINE_MAX was sized against, so if it ever stops fitting the constant
  // is wrong.
  std::string worst =
      buildFrame(SL_MAX_SESSIONS, "yellow", SL_MAX_ID, SL_MAX_LABEL,
                 "needs you", "yellow");
  char what[128];
  snprintf(what, sizeof(what), "worst case %zu bytes fits SL_LINE_MAX (%d)",
           worst.size(), SL_LINE_MAX);
  check(worst.size() <= SL_LINE_MAX, what);

  LineReader reader;
  FeedResult fed = feedLine(&reader, worst);
  check(fed.completed, "worst case completes a line");
  Frame f;
  check(parseFrame(fed.line.c_str(), &f), "worst case parses");
  checkInt(f.count, SL_MAX_SESSIONS, "worst case yields 8 entries");
  check(f.hasColor && f.color == COLOR_YELLOW, "worst case keeps aggregate");
}

// ---------------------------------------------------------------------------
// Oversized line degradation.
//
// A line past SL_LINE_MAX still cannot be trusted as a session list, because
// the tail is missing and a short list looks like sessions ending. But the
// aggregate must survive: losing the labels is acceptable, losing the lamp is
// not. This is the heart of defect 1.
// ---------------------------------------------------------------------------
void testOverflowDegradation() {
  section("oversized line degradation");

  // A frame far past the cap, carrying red.
  std::string huge = buildFrame(60, "red", 40, 48, "needs you", "red");
  check(huge.size() > SL_LINE_MAX, "the oversized frame really is oversized");

  LineReader reader;
  FeedResult fed = feedLine(&reader, huge);

  check(!fed.completed, "an oversized line yields no frame");
  check(fed.salvaged, "an oversized line still yields its aggregate");
  check(fed.salvagedColor == COLOR_RED, "the salvaged aggregate is red");

  // overflowColor is a const observer, so reading it twice is stable. What
  // must not happen is the colour surviving into the NEXT line: the following
  // feed() clears it, which is checked just below with a normal frame.
  Color again;
  check(reader.overflowColor(&again) && again == COLOR_RED,
        "reading the salvaged colour is idempotent");

  // One more byte, and the stale colour is gone. Otherwise an oversized red
  // frame would keep re-applying red to every later line.
  reader.feed('{');
  Color leftover;
  check(!reader.overflowColor(&leftover),
        "the salvaged colour does not survive into the next line");
  reader.reset();

  // A normal frame after an oversized one is unaffected: the reader recovers
  // rather than staying wedged. This is what makes the degraded path safe to
  // hit repeatedly, which is what happens when every frame is oversized.
  std::string normal = buildFrame(2, "green", 36, 10, "working", "green");
  FeedResult after = feedLine(&reader, normal);
  check(after.completed, "the reader recovers after an oversized line");
  check(!after.salvaged, "a normal line reports no salvaged colour");
  Frame f;
  check(parseFrame(after.line.c_str(), &f), "the following frame parses");
  check(f.hasColor && f.color == COLOR_GREEN, "the following frame is green");
  checkInt(f.count, 2, "the following frame has both sessions");

  // Every aggregate colour survives the overflow path, not just red.
  const char* names[] = {"off", "green", "yellow", "red"};
  const Color want[] = {COLOR_OFF, COLOR_GREEN, COLOR_YELLOW, COLOR_RED};
  for (int i = 0; i < 4; i++) {
    LineReader r;
    std::string big = buildFrame(60, names[i], 40, 48, "working", "green");
    FeedResult res = feedLine(&r, big);
    char what[96];
    snprintf(what, sizeof(what), "aggregate \"%s\" survives an oversized frame",
             names[i]);
    check(res.salvaged && res.salvagedColor == want[i], what);
  }

  // A per-session colour must never be mistaken for the aggregate. Here the
  // top level says green while the sessions say red; if the scan wandered
  // into the array it would report red and the lamp would lie.
  {
    LineReader r;
    std::string big = buildFrame(60, "green", 40, 48, "needs you", "red");
    FeedResult res = feedLine(&r, big);
    check(res.salvaged, "aggregate is found when sessions carry a colour too");
    check(res.salvagedColor == COLOR_GREEN,
          "a per-session colour is not mistaken for the aggregate");
  }

  // An oversized line whose "color" key sits AFTER the sessions array. The
  // key is far past the truncation point, so the prefix scan cannot see it
  // and the aggregate is recovered from the retained tail instead.
  //
  // This used to lose the lamp outright: the salvage was a prefix-only
  // forward parse, so it worked when the relay put "color" first and silently
  // did nothing when it did not. A red frame then left the lamps on a stale
  // green, and whether that happened depended on the field order of a Go
  // struct. That is exactly the coupling the degradation path exists to
  // remove, so both orders must now light the right lamp.
  {
    LineReader r;
    std::string s = "{\"sessions\":[";
    for (int i = 0; i < 60; i++) {
      if (i > 0) s += ",";
      s += "{\"id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"label\":"
           "\"yyyyyyyyyyyy\",\"state\":\"working\",\"color\":\"yellow\"}";
    }
    s += "],\"color\":\"red\"}";
    FeedResult res = feedLine(&r, s);
    check(!res.completed, "trailing-colour oversized line yields no frame");
    check(res.salvaged,
          "a trailing aggregate is salvaged from the tail, not lost");
    check(res.salvagedColor == COLOR_RED,
          "the trailing aggregate is red, not the sessions' yellow");
  }

  // Both key orders must produce the same lamp, for every colour. This is the
  // property that makes the salvage independent of the Go struct's field
  // order rather than merely correct for today's encoder.
  {
    const char* orderNames[] = {"off", "green", "yellow", "red"};
    const Color orderWant[] = {COLOR_OFF, COLOR_GREEN, COLOR_YELLOW, COLOR_RED};
    for (int i = 0; i < 4; i++) {
      // The sessions all carry a DIFFERENT colour from the aggregate, so a
      // scan that wandered into the array would be caught here.
      std::string sessions = "\"sessions\":[";
      for (int j = 0; j < 60; j++) {
        if (j > 0) sessions += ",";
        sessions +=
            "{\"id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"label\":"
            "\"yyyyyyyyyyyy\",\"state\":\"working\",\"color\":\"green\"}";
      }
      sessions += "]";
      std::string colorKey = std::string("\"color\":\"") + orderNames[i] + "\"";

      LineReader first;
      FeedResult a = feedLine(&first, "{" + colorKey + "," + sessions + "}");
      LineReader last;
      FeedResult b = feedLine(&last, "{" + sessions + "," + colorKey + "}");

      char what[128];
      snprintf(what, sizeof(what),
               "\"%s\" survives an oversized frame with color FIRST",
               orderNames[i]);
      check(a.salvaged && a.salvagedColor == orderWant[i], what);

      snprintf(what, sizeof(what),
               "\"%s\" survives an oversized frame with color LAST",
               orderNames[i]);
      check(b.salvaged && b.salvagedColor == orderWant[i], what);

      snprintf(what, sizeof(what),
               "key order does not change the salvaged lamp for \"%s\"",
               orderNames[i]);
      check(a.salvaged == b.salvaged && a.salvagedColor == b.salvagedColor,
            what);
    }
  }

  // A trailing key after the aggregate must not hide it: the tail scan looks
  // backwards for the LAST top level "color", so a later unknown key simply
  // has to fit in the retained tail alongside it.
  {
    LineReader r;
    std::string s = "{\"sessions\":[";
    for (int i = 0; i < 60; i++) {
      if (i > 0) s += ",";
      s += "{\"id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"label\":"
           "\"yyyyyyyyyyyy\",\"state\":\"working\",\"color\":\"green\"}";
    }
    s += "],\"color\":\"red\"}";
    FeedResult res = feedLine(&r, s);
    check(res.salvaged && res.salvagedColor == COLOR_RED,
          "a trailing aggregate after a green session list is still red");
  }

  // findTailAggregateColor directly, where the top-level test is what matters.
  {
    Color c = COLOR_OFF;
    check(findTailAggregateColor("\"state\":\"working\"}],\"color\":\"red\"}",
                                 &c),
          "a tail ending in a top level colour yields it");
    check(c == COLOR_RED, "the tail aggregate is red");
  }
  {
    // The tail ends INSIDE the sessions array: the "color" here belongs to a
    // session, and the array and the frame are both still open after it. It
    // must be refused rather than driving the lamps.
    Color c = COLOR_OFF;
    check(!findTailAggregateColor(
              "{\"id\":\"a1\",\"color\":\"red\"},{\"id\":\"b2\"}]}", &c),
          "a per-session colour in the tail is not the aggregate");
  }
  {
    Color c = COLOR_OFF;
    check(!findTailAggregateColor("", &c), "an empty tail yields nothing");
    check(!findTailAggregateColor("no colour here at all}", &c),
          "a tail with no colour key yields nothing");
    check(!findTailAggregateColor("],\"color\":\"chartreuse\"}", &c),
          "an unknown name in the tail yields nothing");
    check(!findTailAggregateColor("],\"color\":\"re", &c),
          "a tail cut inside the value yields nothing");
    check(!findTailAggregateColor("],\"colorway\":\"red\"}", &c),
          "a key merely starting with color is not the aggregate");
    check(!findTailAggregateColor("],\"x\":\"a \\\"color\\\":\\\"red\\\"\"}",
                                  &c),
          "the text color inside a string value is not a key");
  }

  // findAggregateColor directly: a prefix cut mid-way through the session
  // list still gives up a leading aggregate.
  {
    Color c = COLOR_OFF;
    const char* prefix =
        "{\"color\":\"yellow\",\"sessions\":[{\"id\":\"a1\",\"label\":\"part";
    check(findAggregateColor(prefix, &c), "a cut prefix yields its aggregate");
    check(c == COLOR_YELLOW, "the cut prefix aggregate is yellow");
  }
  {
    Color c = COLOR_RED;
    check(!findAggregateColor("{\"sessions\":[{\"id\":", &c),
          "a prefix with no aggregate yields nothing");
    check(!findAggregateColor("", &c), "an empty prefix yields nothing");
    check(!findAggregateColor("not json", &c), "a non-object yields nothing");
    check(!findAggregateColor("{\"color\":\"chartreuse\"}", &c),
          "an unknown aggregate name yields nothing");
  }
}

// ---------------------------------------------------------------------------
// UUID ids must not collide.
//
// This is defect 2. Real session ids are 36 character UUIDs, and the pin and
// the slot-preservation logic both key on the id with strcmp. Truncation does
// not make an id shorter, it makes it WRONG.
// ---------------------------------------------------------------------------
void testUuidIdsDoNotCollide() {
  section("UUID ids");

  check(SL_MAX_ID >= 36, "SL_MAX_ID holds a whole 36 character UUID");

  // Two UUIDs differing only after the 24th character: the exact shape that
  // used to collide.
  const char* idA = "3f2a1b7c-9d4e-4a10-b8c3-aaaaaaaaaaaa";
  const char* idB = "3f2a1b7c-9d4e-4a10-b8c3-bbbbbbbbbbbb";
  checkInt((long)strlen(idA), 36, "the test UUID is 36 characters");

  std::string frame = std::string("{\"color\":\"red\",\"sessions\":[") +
                      "{\"id\":\"" + idA +
                      "\",\"label\":\"auth-api\",\"state\":\"needs "
                      "you\",\"color\":\"red\"}," +
                      "{\"id\":\"" + idB +
                      "\",\"label\":\"indexer\",\"state\":\"working\","
                      "\"color\":\"yellow\"}]}";

  Frame f;
  check(parseFrame(frame.c_str(), &f), "the two-UUID frame parses");
  checkInt(f.count, 2, "both UUID sessions are kept");
  checkStr(f.sessions[0].id, idA, "the first UUID is stored whole");
  checkStr(f.sessions[1].id, idB, "the second UUID is stored whole");
  check(strcmp(f.sessions[0].id, f.sessions[1].id) != 0,
        "two UUIDs sharing a 24 character prefix do not collide");

  // The consequence that actually bit: with colliding ids the pin follows the
  // wrong session. Pin the first, then confirm it stays on the first.
  Display d;
  slTestSetMillis(1000);
  d.applyFrame(f, 1000);
  checkInt(d.sessionCount(), 2, "the display took both sessions");
  checkStr(d.currentLabel(), "auth-api", "the display starts on the first");

  d.togglePin(1000);
  check(d.isPinned(), "the first session is pinned");

  // Reorder the list. The pin is anchored by id, so it must follow the
  // session, not the slot. A truncated id would have matched the wrong one.
  std::string reordered = std::string("{\"color\":\"red\",\"sessions\":[") +
                          "{\"id\":\"" + idB +
                          "\",\"label\":\"indexer\",\"state\":\"working\","
                          "\"color\":\"yellow\"}," +
                          "{\"id\":\"" + idA +
                          "\",\"label\":\"auth-api\",\"state\":\"needs "
                          "you\",\"color\":\"red\"}]}";
  Frame g;
  check(parseFrame(reordered.c_str(), &g), "the reordered frame parses");
  d.applyFrame(g, 2000);
  check(d.isPinned(), "the pin survives a reorder");
  checkStr(d.currentLabel(), "auth-api",
           "the pin followed its own session, not the slot");

  // The did-this-turn-red check also keys on the id. With colliding ids it
  // reads another session's previous colour and suppresses the jump. Here the
  // second session turns red and the screen must jump to it.
  Display d2;
  Frame start;
  std::string both = std::string("{\"color\":\"yellow\",\"sessions\":[") +
                     "{\"id\":\"" + idA +
                     "\",\"label\":\"auth-api\",\"state\":\"working\","
                     "\"color\":\"yellow\"}," +
                     "{\"id\":\"" + idB +
                     "\",\"label\":\"indexer\",\"state\":\"working\","
                     "\"color\":\"yellow\"}]}";
  check(parseFrame(both.c_str(), &start), "the all-yellow frame parses");
  d2.applyFrame(start, 1000);
  checkStr(d2.currentLabel(), "auth-api", "the screen starts on the first");

  Frame turned;
  std::string second = std::string("{\"color\":\"red\",\"sessions\":[") +
                       "{\"id\":\"" + idA +
                       "\",\"label\":\"auth-api\",\"state\":\"working\","
                       "\"color\":\"yellow\"}," +
                       "{\"id\":\"" + idB +
                       "\",\"label\":\"indexer\",\"state\":\"needs "
                       "you\",\"color\":\"red\"}]}";
  check(parseFrame(second.c_str(), &turned), "the turned-red frame parses");
  d2.applyFrame(turned, 2000);
  checkStr(d2.currentLabel(), "indexer",
           "the screen jumps to the session that turned red");

  // An id longer than SL_MAX_ID still truncates rather than overflowing, and
  // the frame still parses. Truncation is the documented behaviour at the
  // boundary; the point of raising the cap is that real ids never reach it.
  {
    std::string longId(SL_MAX_ID + 20, 'z');
    std::string s = "{\"sessions\":[{\"id\":\"" + longId +
                    "\",\"label\":\"x\",\"state\":\"working\"}]}";
    Frame h;
    check(parseFrame(s.c_str(), &h), "an over-long id still parses");
    checkInt(h.count, 1, "an over-long id still yields a session");
    checkInt((long)strlen(h.sessions[0].id), SL_MAX_ID,
             "an over-long id truncates to SL_MAX_ID without overflowing");
  }
}

// ---------------------------------------------------------------------------
// Unknown keys are ignored, never rejected. The relay and the firmware ship
// independently, so a field added next month must not brick a light flashed
// today.
// ---------------------------------------------------------------------------
void testUnknownKeysSkipped() {
  section("unknown keys");

  const char* line =
      "{\"color\":\"red\",\"version\":2,\"meta\":{\"nested\":[1,2,{\"deep\":"
      "true}]},\"flags\":[true,false,null],\"sessions\":[{\"id\":\"a1\","
      "\"label\":\"auth-api\",\"state\":\"needs you\",\"color\":\"red\","
      "\"future\":{\"x\":[1,2]}}],\"trailing\":\"ignored\"}";

  Frame f;
  check(parseFrame(line, &f), "a frame full of unknown keys parses");
  check(f.hasColor && f.color == COLOR_RED, "the aggregate still arrives");
  check(f.hasSessions, "the sessions key is still seen");
  checkInt(f.count, 1, "the known session is still read");
  checkStr(f.sessions[0].id, "a1", "the id is read past unknown keys");
  checkStr(f.sessions[0].label, "auth-api", "the label is read");
  checkStr(f.sessions[0].state, "needs you", "the state is read");
  check(f.sessions[0].color == COLOR_RED, "the session colour is read");

  // An unknown key BEFORE the aggregate must not hide it, at either level.
  Frame g;
  check(parseFrame("{\"zzz\":{\"a\":[1,{\"b\":2}]},\"color\":\"yellow\"}", &g),
        "an unknown key before the aggregate parses");
  check(g.hasColor && g.color == COLOR_YELLOW,
        "the aggregate is found after an unknown object");

  // An unknown aggregate name leaves hasColor false, so the caller keeps the
  // lamps where they are rather than guessing.
  Frame h;
  check(parseFrame("{\"color\":\"chartreuse\"}", &h),
        "an unknown colour name still parses");
  check(!h.hasColor, "an unknown colour name leaves the lamps alone");

  // Nesting deeper than the guard is refused rather than blowing the stack,
  // and the frame before it still counts.
  {
    std::string deep = "{\"color\":\"red\",\"deep\":";
    for (int i = 0; i < 40; i++) deep += "[";
    for (int i = 0; i < 40; i++) deep += "]";
    deep += "}";
    Frame k;
    check(parseFrame(deep.c_str(), &k), "a deeply nested frame still parses");
    check(k.hasColor && k.color == COLOR_RED,
          "the aggregate survives a nesting that is refused");
  }

  // The same nesting BEFORE the aggregate. This used to drop the red lamp:
  // skipValue refused the over-nested value, the key loop broke out, and
  // every later key was abandoned including "color". A malformed value under
  // a key the firmware does not even read must cost that key and nothing
  // else, so the aggregate and the sessions after it must both survive.
  {
    std::string deep = "{\"x\":";
    for (int i = 0; i < 1000; i++) deep += "[";
    for (int i = 0; i < 1000; i++) deep += "]";
    deep +=
        ",\"color\":\"red\",\"sessions\":[{\"id\":\"a1\",\"label\":\"auth\","
        "\"state\":\"needs you\",\"color\":\"red\"}]}";
    Frame k;
    check(parseFrame(deep.c_str(), &k),
          "an over-nested value before the aggregate still parses");
    check(k.hasColor && k.color == COLOR_RED,
          "the aggregate survives an over-nested value BEFORE it");
    check(k.hasSessions, "the sessions key after the refused value is reached");
    checkInt(k.count, 1, "the session after the refused value is read");
    checkStr(k.sessions[0].label, "auth", "and it is read correctly");
  }

  // The recovery must not be fooled by structure inside a string. A refused
  // value containing a quoted comma or brace must still hand back at the
  // real top level separator, not at one inside a literal.
  {
    std::string deep = "{\"x\":[";
    for (int i = 0; i < 40; i++) deep += "[";
    deep += "\"a,b}c]d\"";
    for (int i = 0; i < 40; i++) deep += "]";
    deep += "],\"color\":\"yellow\"}";
    Frame k;
    check(parseFrame(deep.c_str(), &k), "a refused value with a tricky string parses");
    check(k.hasColor && k.color == COLOR_YELLOW,
          "commas and braces inside a string do not misplace the recovery");
  }

  // A refused value with nothing after it must end the frame cleanly rather
  // than looping or reading past the terminator.
  {
    std::string deep = "{\"color\":\"green\",\"x\":";
    for (int i = 0; i < 200; i++) deep += "{\"a\":";
    deep += "1";
    for (int i = 0; i < 200; i++) deep += "}";
    deep += "}";
    Frame k;
    check(parseFrame(deep.c_str(), &k), "a trailing refused value parses");
    check(k.hasColor && k.color == COLOR_GREEN,
          "a trailing refused value costs nothing already read");
  }

  // The overflow salvage has the same recovery, so an over-nested value in
  // front of the aggregate must not lose the lamp there either.
  {
    Color c = COLOR_OFF;
    std::string prefix = "{\"x\":";
    for (int i = 0; i < 100; i++) prefix += "[";
    for (int i = 0; i < 100; i++) prefix += "]";
    prefix += ",\"color\":\"red\",\"sessions\":[{\"id\":\"a1\",\"lab";
    check(findAggregateColor(prefix.c_str(), &c),
          "the salvage reaches past an over-nested value");
    check(c == COLOR_RED, "and recovers the right aggregate");
  }

  // A "sessions" key of the wrong type is skipped without losing the rest of
  // the frame, including when it is unskippable.
  {
    Frame k;
    check(parseFrame("{\"sessions\":\"not an array\",\"color\":\"red\"}", &k),
          "a wrong-typed sessions key parses");
    check(k.hasColor && k.color == COLOR_RED,
          "the aggregate survives a wrong-typed sessions key");
    check(!k.hasSessions, "a wrong-typed sessions key is not a session list");
  }
  {
    // An OBJECT, not an array, so the wrong-type branch is the one taken, and
    // nested past the guard so that skipValue refuses it there.
    std::string s = "{\"sessions\":";
    for (int i = 0; i < 40; i++) s += "{\"a\":";
    s += "1";
    for (int i = 0; i < 40; i++) s += "}";
    s += ",\"color\":\"red\"}";
    Frame k;
    check(parseFrame(s.c_str(), &k), "an over-nested sessions value parses");
    check(k.hasColor && k.color == COLOR_RED,
          "the aggregate survives an unskippable sessions value");
    check(!k.hasSessions, "an unskippable sessions value is not a list");
  }

  // Duplicate ids in one frame. Two entries sharing an id both used to get a
  // slot, and findById returns the FIRST, so the second could never be
  // pinned or tracked: a pin on it anchored to the first instead. Same class
  // as the SL_MAX_ID collision, sourced from the relay rather than from
  // truncation. Keep the first and drop the rest.
  {
    Frame k;
    check(parseFrame("{\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"first\",\"state\":\"working\","
                     "\"color\":\"green\"},"
                     "{\"id\":\"a1\",\"label\":\"duplicate\",\"state\":\"needs "
                     "you\",\"color\":\"red\"},"
                     "{\"id\":\"b2\",\"label\":\"second\",\"state\":\"working\","
                     "\"color\":\"green\"}]}",
                     &k),
          "a frame with a duplicate id parses");
    checkInt(k.count, 2, "the duplicate id is dropped, the others are kept");
    checkStr(k.sessions[0].label, "first", "the FIRST of the pair is kept");
    checkStr(k.sessions[1].label, "second",
             "the entry after the duplicate is still read");
  }

  // Every id in a parsed frame is unique, which is what the display's
  // findById needs in order to be unambiguous.
  {
    std::string s = "{\"sessions\":[";
    for (int i = 0; i < 6; i++) {
      if (i > 0) s += ",";
      s += "{\"id\":\"same\",\"label\":\"x\",\"state\":\"working\"}";
    }
    s += "]}";
    Frame k;
    check(parseFrame(s.c_str(), &k), "an all-duplicate frame parses");
    checkInt(k.count, 1, "six copies of one id yield one session");
  }
  {
    // And the general property, over a frame that mixes duplicates in.
    std::string s =
        "{\"sessions\":[{\"id\":\"a\",\"label\":\"1\"},{\"id\":\"b\",\"label\":"
        "\"2\"},{\"id\":\"a\",\"label\":\"3\"},{\"id\":\"c\",\"label\":\"4\"},"
        "{\"id\":\"b\",\"label\":\"5\"}]}";
    Frame k;
    check(parseFrame(s.c_str(), &k), "the mixed-duplicate frame parses");
    checkInt(k.count, 3, "only the distinct ids are kept");
    bool allUnique = true;
    for (uint8_t i = 0; i < k.count; i++) {
      for (uint8_t j = (uint8_t)(i + 1); j < k.count; j++) {
        if (strcmp(k.sessions[i].id, k.sessions[j].id) == 0) allUnique = false;
      }
    }
    check(allUnique, "no two sessions in a parsed frame share an id");
  }

  // The consequence, on the display: a pin must anchor to a session that
  // findById can actually reach. With a duplicate present the second copy had
  // a slot no pin could ever address.
  {
    Frame k;
    check(parseFrame("{\"sessions\":["
                     "{\"id\":\"dup\",\"label\":\"kept\",\"state\":\"working\"},"
                     "{\"id\":\"dup\",\"label\":\"shadowed\",\"state\":\"needs "
                     "you\"},"
                     "{\"id\":\"other\",\"label\":\"other\",\"state\":"
                     "\"working\"}]}",
                     &k),
          "the duplicate-id frame parses");
    Display d;
    d.applyFrame(k, 1000);
    checkInt(d.sessionCount(), 2, "the display never sees the duplicate");
    checkStr(d.currentLabel(), "kept", "the display shows the first of the pair");

    // Advance to the next slot: it must be the OTHER session, not a second
    // copy of the same id that the pin could not distinguish.
    d.advance(1000);
    checkStr(d.currentLabel(), "other",
             "the next slot is a different session, not a shadowed duplicate");
    d.togglePin(1000);
    check(d.isPinned(), "the second slot can be pinned");

    // Reorder, and the pin must still land on its own session.
    Frame m;
    check(parseFrame("{\"sessions\":["
                     "{\"id\":\"other\",\"label\":\"other\",\"state\":"
                     "\"working\"},"
                     "{\"id\":\"dup\",\"label\":\"kept\",\"state\":\"working\"},"
                     "{\"id\":\"dup\",\"label\":\"shadowed\",\"state\":\"needs "
                     "you\"}]}",
                     &m),
          "the reordered duplicate-id frame parses");
    d.applyFrame(m, 2000);
    check(d.isPinned(), "the pin survived the reorder");
    checkStr(d.currentLabel(), "other",
             "the pin followed its own session past the duplicate");
  }

  // Sessions past the cap are skipped while the rest of the frame parses.
  {
    std::string many = buildFrame(SL_MAX_SESSIONS + 4, "red", 36, 8, "working",
                                  "yellow");
    Frame k;
    check(parseFrame(many.c_str(), &k), "an over-long session list parses");
    checkInt(k.count, SL_MAX_SESSIONS, "the list is capped at SL_MAX_SESSIONS");
    check(k.hasColor && k.color == COLOR_RED,
          "the aggregate survives an over-long session list");
  }

  // A session with no id is dropped: it cannot be pinned or tracked.
  {
    Frame k;
    check(parseFrame("{\"sessions\":[{\"label\":\"nameless\"},{\"id\":\"a1\","
                     "\"label\":\"real\"}]}",
                     &k),
          "a frame with an id-less session parses");
    checkInt(k.count, 1, "the id-less session is dropped");
    checkStr(k.sessions[0].label, "real", "the session with an id is kept");
  }
}

// ---------------------------------------------------------------------------
// Truncated input must leave the lamps alone.
//
// A frame that never completes is not an instruction. The reader must not
// hand a partial line to the parser, and a parsed frame with no aggregate
// must leave hasColor false so the sketch does not call Lamps::set().
// ---------------------------------------------------------------------------
void testTruncatedInputLeavesLampsAlone() {
  section("truncated input");

  // A line with no terminator never completes.
  {
    LineReader r;
    const char* partial = "{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\"";
    bool completed = false;
    for (const char* p = partial; *p; p++) {
      if (r.feed(*p)) completed = true;
    }
    check(!completed, "an unterminated line never completes");
    Color c;
    check(!r.overflowColor(&c),
          "an unterminated line reports no salvaged colour");
  }

  // Lamps follow the frame and nothing else. A frame with no "color" key
  // leaves them exactly where they were: absent means no instruction.
  {
    Lamps lamps;
    lamps.begin();
    check(lamps.current() == COLOR_OFF, "lamps start dark");

    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[]}", &f),
          "the red frame parses");
    check(f.hasColor, "the red frame carries an aggregate");
    lamps.set(f.color);
    check(lamps.current() == COLOR_RED, "the lamp went red");
    checkInt((long)slTestLedcDuty[LAMP_PIN_RED], LAMP_DUTY, "the red LED is lit");
    checkInt((long)slTestLedcDuty[LAMP_PIN_GREEN], 0, "the green LED is dark");

    // A truncated frame that yields no aggregate must not move the lamp.
    Frame g;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"lab", &g),
          "a truncated frame still parses what it can");
    check(!g.hasColor, "a truncated frame carries no aggregate");
    if (g.hasColor) {
      lamps.set(g.color);
    }
    check(lamps.current() == COLOR_RED,
          "a truncated frame left the lamp where it was");

    // Only one lamp is ever lit at a time.
    lamps.set(COLOR_YELLOW);
    checkInt((long)slTestLedcDuty[LAMP_PIN_RED], 0, "red is cleared");
    checkInt((long)slTestLedcDuty[LAMP_PIN_YELLOW], LAMP_DUTY, "yellow is lit");
    checkInt((long)slTestLedcDuty[LAMP_PIN_GREEN], 0, "green stays dark");

    lamps.set(COLOR_OFF);
    checkInt((long)slTestLedcDuty[LAMP_PIN_RED], 0, "off clears red");
    checkInt((long)slTestLedcDuty[LAMP_PIN_YELLOW], 0, "off clears yellow");
    checkInt((long)slTestLedcDuty[LAMP_PIN_GREEN], 0, "off clears green");
  }

  // A frame with no "sessions" key leaves the screen alone entirely: the
  // colour-only case, where the lamps move and the screen does not.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"yellow\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"auth-api\",\"state\":\"working\"}]}",
                     &f),
          "the seeding frame parses");
    d.applyFrame(f, 1000);
    checkInt(d.sessionCount(), 1, "the screen took the session");

    Frame colorOnly;
    check(parseFrame("{\"color\":\"red\"}", &colorOnly),
          "a colour-only frame parses");
    check(!colorOnly.hasSessions, "a colour-only frame has no sessions key");
    d.applyFrame(colorOnly, 2000);
    checkInt(d.sessionCount(), 1, "a colour-only frame left the screen alone");
    checkStr(d.currentLabel(), "auth-api", "the label is unchanged");
  }

  // THE PERMANENTLY BLANK SCREEN. A frame truncated inside the sessions array
  // must not clear the panel.
  //
  // This is the regression test for the reported bug. hasSessions used to be
  // set on the opening bracket, so a cut line arrived as "sessions present,
  // zero of them", applyFrame read that as every session having ended, and
  // the screen went empty and stayed empty. The lamps were unaffected, which
  // is why the failure looked like dead hardware rather than a bad parse.
  //
  // The existing truncation checks above all assert about the LAMPS, which is
  // exactly why this got through: nothing fed a truncated list to a seeded
  // Display and looked at the label.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"realwork\",\"state\":\"working\"}]}",
                     &f),
          "the seeding frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkStr(d.currentLabel(), "realwork", "the screen shows the session");

    // Cut mid-array, after a complete entry and its comma.
    Frame cut;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"b2\","
                     "\"label\":\"other\",\"state\":\"working\"},",
                     &cut),
          "a frame cut mid-array still parses for the lamps");
    check(cut.hasColor, "the cut frame still salvaged its aggregate");
    check(!cut.hasSessions,
          "a cut sessions array does NOT count as a session list");
    d.applyFrame(cut, 2000);
    d.tick(2000);
    checkInt(d.sessionCount(), 1, "the cut frame did not empty the screen");
    checkStr(d.currentLabel(), "realwork",
             "the cut frame left the label alone");

    // Cut immediately after the opening bracket: the exact shape that set
    // hasSessions with nothing behind it.
    Frame bare;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[", &bare),
          "a frame cut at the bracket parses");
    check(!bare.hasSessions,
          "an unterminated empty array is not an empty list");
    d.applyFrame(bare, 3000);
    d.tick(3000);
    checkStr(d.currentLabel(), "realwork",
             "a bracket-only frame left the label alone");

    // A COMPLETE empty list still clears the screen. The fix must not break
    // the real "everything ended" instruction.
    Frame empty;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[]}", &empty),
          "a complete empty list parses");
    check(empty.hasSessions, "a closed empty array IS a session list");
    d.applyFrame(empty, 4000);
    d.tick(4000);
    checkInt(d.sessionCount(), 0, "a complete empty list still clears");
  }

  // A GLUED FRAME keeps no stale colour. When the link drops mid-frame the
  // newline is lost, so the next frame's bytes append to the orphaned prefix
  // and the reader hands parseFrame one garbled line. That line must be refused
  // whole: reading its leading "color" as the aggregate kept the dropped
  // frame's stale colour and lost the fresh one, which is the reported bug.
  {
    Frame glued;
    // yellow dropped mid-"sessions"; green glued on after the lost newline.
    check(!parseFrame("{\"color\":\"yellow\",\"sess{\"color\":\"green\","
                      "\"sessions\":[]}",
                      &glued),
          "a truncated frame glued to the next one is refused, not kept stale");

    // A key with no colon because a second object was glued on. Same shape,
    // reached without a prior readable key.
    Frame glued2;
    check(!parseFrame("{\"ses{\"color\":\"red\"}", &glued2),
          "a glued frame with no readable leading key is refused");

    // The refusal is specific to GARBLE, not to a short line. A frame merely
    // cut mid-token still parses and still surrenders its aggregate: the salvage
    // the device depends on must not be lost to the fix above.
    Frame cut;
    check(parseFrame("{\"color\":\"red\",\"sess", &cut),
          "a frame cut mid-key still parses");
    check(cut.hasColor && cut.color == COLOR_RED,
          "a cut frame still salvages its aggregate");

    // Two legitimately back-to-back top level keys, the second value present and
    // well formed, is VALID JSON and must still parse: the later "color" wins,
    // exactly as the relay intends when it resends.
    Frame valid;
    check(parseFrame("{\"color\":\"yellow\",\"x\":5,\"color\":\"green\"}",
                     &valid),
          "a well formed frame with a repeated key still parses");
    check(valid.hasColor && valid.color == COLOR_GREEN,
          "and the later colour wins");
  }

  // Non-JSON never reaches the display at all. parseFrame rejects anything
  // that is not an object, and the sketch only calls applyFrame on success,
  // so stray text on the wire cannot become content.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"realwork\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the seeding frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);

    static const char* const junk[] = {
        "n=137",                                  // a stray counter
        "  t+4s  lamp=red  n=5  some/branch",     // a log line
        "not json at all",
        "[]",
        "null",
        "",
    };
    for (unsigned i = 0; i < sizeof(junk) / sizeof(junk[0]); i++) {
      Frame g;
      check(!parseFrame(junk[i], &g), "non-JSON is rejected outright");
      // The sketch guards applyFrame on the parse result, so nothing is
      // applied here. Ticking proves the screen is not disturbed anyway.
      d.tick(2000 + i * 10);
    }
    checkStr(d.currentLabel(), "realwork", "stray text never reached the glass");
  }
}

// ---------------------------------------------------------------------------
// The end-to-end failing scenario from the defect report: five concurrent
// sessions, one goes blocked, the relay emits a frame carrying red. Before
// the fix the line was dropped and the lamps held green forever, because
// frames are sent only on change and every later frame was oversized too.
// ---------------------------------------------------------------------------
void testFiveSessionsTurnRed() {
  section("the reported failure: five sessions, one blocked");

  Lamps lamps;
  lamps.begin();
  LineReader reader;
  Display display;

  // Five sessions, all working. The lamp goes yellow.
  std::string green = buildFrame(5, "yellow", 36, 12, "working", "yellow");
  FeedResult a = feedLine(&reader, green);
  check(a.completed, "the five-session yellow frame completes");
  Frame f;
  check(parseFrame(a.line.c_str(), &f), "it parses");
  if (f.hasColor) lamps.set(f.color);
  display.applyFrame(f, 1000);
  check(lamps.current() == COLOR_YELLOW, "the lamp is yellow");
  checkInt(display.sessionCount(), 5, "all five are on the screen");

  // One goes blocked. This is the 543 byte frame that used to be dropped.
  std::string red = buildFrame(5, "red", 36, 12, "needs you", "red");
  check(red.size() > 512, "the red frame is over the old 512 byte cap");
  FeedResult b = feedLine(&reader, red);
  check(b.completed, "the 543 byte red frame is no longer dropped");
  Frame g;
  check(parseFrame(b.line.c_str(), &g), "the red frame parses");
  if (g.hasColor) lamps.set(g.color);
  display.applyFrame(g, 2000);
  check(lamps.current() == COLOR_RED,
        "the lamp went red: the reported defect is fixed");

  // Even if a frame does somehow exceed the cap, the lamp still tracks it.
  std::string enormous = buildFrame(60, "green", 40, 48, "done", "green");
  FeedResult c = feedLine(&reader, enormous);
  check(!c.completed, "the enormous frame yields no session list");
  if (c.salvaged) lamps.set(c.salvagedColor);
  check(lamps.current() == COLOR_GREEN,
        "the lamp still followed an oversized frame's aggregate");
  checkInt(display.sessionCount(), 5,
           "the oversized frame left the screen untouched");
}

// ---------------------------------------------------------------------------
// The structural invariant: a pinned or rotating screen can never hide a red
// lamp. Enforced by structure, and checked here behaviourally.
// ---------------------------------------------------------------------------
void testPinNeverHidesRed() {
  section("the pin never hides a red lamp");

  Lamps lamps;
  lamps.begin();
  Display display;

  Frame f;
  check(parseFrame("{\"color\":\"red\",\"sessions\":["
                   "{\"id\":\"g1\",\"label\":\"green-one\",\"state\":\"done\","
                   "\"color\":\"green\"},"
                   "{\"id\":\"r1\",\"label\":\"red-one\",\"state\":\"needs "
                   "you\",\"color\":\"red\"}]}",
                   &f),
        "the mixed frame parses");

  lamps.set(f.color);
  display.applyFrame(f, 1000);

  // Pin the green session, which is what a user does when they want to watch
  // one thing. The lamp must stay red regardless.
  display.applyFrame(f, 1000);
  check(lamps.current() == COLOR_RED, "the lamp is red before pinning");

  display.togglePin(1000);
  check(display.isPinned(), "a session is pinned");
  check(lamps.current() == COLOR_RED,
        "pinning did not touch the lamp: the core invariant holds");

  // Rotate manually. Still red.
  display.advance(2000);
  check(lamps.current() == COLOR_RED, "advancing did not touch the lamp");

  // Ticking the display for a long simulated while must never move a lamp.
  for (uint32_t t = 2000; t < 60000; t += 250) {
    display.tick(t);
  }
  check(lamps.current() == COLOR_RED, "no amount of ticking moved the lamp");
}

// ---------------------------------------------------------------------------
// Rollover. millis() wraps at about 49 days and every elapsed-time comparison
// must stay correct across it.
// ---------------------------------------------------------------------------
void testRolloverSafety() {
  section("millis() rollover");

  // A press that begins before the wrap and is released after it must still
  // classify as a short press.
  {
    Button b;
    b.begin();
    const uint32_t before = 0xFFFFFF00u;

    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(before);

    // Press.
    slTestPinLevel[BUTTON_PIN] = LOW;
    b.update(before);
    b.update(before + 60);  // debounce settles, press registers

    // Release 100ms later, which is past the wrap.
    slTestPinLevel[BUTTON_PIN] = HIGH;
    uint32_t after = before + 160;  // wraps
    b.update(after);
    ButtonEvent e = b.update(after + 60);
    check(e == BUTTON_SHORT_PRESS, "a short press survives the rollover");
  }

  // A hold across the wrap classifies as a long press.
  {
    Button b;
    b.begin();
    const uint32_t before = 0xFFFFFF00u;

    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(before);
    slTestPinLevel[BUTTON_PIN] = LOW;
    b.update(before);
    b.update(before + 60);

    ButtonEvent e = BUTTON_NONE;
    for (uint32_t t = 100; t <= 700; t += 100) {
      ButtonEvent got = b.update(before + 60 + t);
      if (got != BUTTON_NONE) e = got;
    }
    check(e == BUTTON_LONG_PRESS, "a long press survives the rollover");
  }

  // Rotation across the wrap still advances rather than stalling for 49 days.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the two-session frame parses");

    const uint32_t before = 0xFFFFF000u;
    d.applyFrame(f, before);
    checkStr(d.currentLabel(), "one", "rotation starts on the first");

    // The previous form of this check was a tautology. It read
    //
    //   label != "one" || label == "two" || sessionCount() == 2
    //
    // and the third disjunct is true in every reachable case, so it passed
    // even with rotation completely stalled. What must actually be proven is
    // that the label CHANGES, so capture it and require a different one.
    //
    // Both labels are 3 characters, well inside the 12 the panel holds, so
    // neither scrolls and nothing but the 3s minimum paces the rotation.
    char startLabel[SL_MAX_LABEL + 1];
    snprintf(startLabel, sizeof(startLabel), "%s", d.currentLabel());

    // Tick to just past one rotation minimum. before + 4000 is 0xFFFFFFB0,
    // still short of the wrap, so this half proves rotation runs at all.
    for (uint32_t t = 0; t <= 4000u; t += 250u) {
      d.tick(before + t);
    }
    check(strcmp(d.currentLabel(), startLabel) != 0,
          "rotation advanced within 4s: the label really changed");

    // Now cross the wrap. before + 4096 is 0, so every tick from here on has
    // a smaller now than slotEnteredAt_, which is precisely where signed
    // elapsed-time arithmetic would stall for 49 days.
    char acrossStart[SL_MAX_LABEL + 1];
    snprintf(acrossStart, sizeof(acrossStart), "%s", d.currentLabel());
    for (uint32_t t = 4250u; t <= 12000u; t += 250u) {
      d.tick(before + t);  // wraps through zero partway along
    }
    check(strcmp(d.currentLabel(), acrossStart) != 0,
          "rotation advanced across the wrap itself, not only before it");

    // And it keeps going rather than advancing once and stopping. Count the
    // label changes over a long run that spans the wrap: a stalled rotation
    // scores 0, and a healthy one scores several.
    char last[SL_MAX_LABEL + 1];
    snprintf(last, sizeof(last), "%s", d.currentLabel());
    int changes = 0;
    for (uint32_t t = 12250u; t <= 60000u; t += 250u) {
      d.tick(before + t);
      if (strcmp(d.currentLabel(), last) != 0) {
        changes++;
        snprintf(last, sizeof(last), "%s", d.currentLabel());
      }
    }
    check(changes >= 10, "rotation kept cycling across the rollover");

    checkInt(d.sessionCount(), 2, "both sessions survived the rollover");
  }

  // The red hold across the wrap.
  //
  // holdUntil_ used 0 as BOTH "no hold" and a timestamp, and 0 is a reachable
  // deadline: at now == 0xFFFFF448 the deadline now + DISP_ROTATE_MIN_MS
  // wraps to exactly 0. A zero-means-no-hold test then reads a live hold as
  // absent.
  //
  // The rotation minimum happens to coincide with the hold in every path
  // through tick(), so this could NOT be caught by watching the label — which
  // is exactly what made it dangerous, and why the fix is a separate flag
  // that no timestamp can alias rather than a bumped value. The checks below
  // therefore assert on the hold state itself, through the observers
  // Display exposes for this purpose, as well as on the behaviour.
  {
    // The instant where the deadline wraps to exactly 0.
    const uint32_t atWrap = 0u - (uint32_t)DISP_ROTATE_MIN_MS;
    checkInt((long)(uint32_t)(atWrap + DISP_ROTATE_MIN_MS), 0,
             "the chosen instant really makes the hold deadline zero");

    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"two\",\"state\":\"working\","
                     "\"color\":\"green\"}]}",
                     &f),
          "the all-green frame parses");
    d.applyFrame(f, atWrap - 10000u);
    checkStr(d.currentLabel(), "one", "the screen starts on the first");
    check(!d.holdingRed(), "no hold before anything turns red");

    // The SECOND session turns red at the wrap instant, so the screen jumps
    // to it and the hold's deadline lands on 0.
    Frame g;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"needs you\",\"color\":\"red\"}]}",
                     &g),
          "the turned-red frame parses");
    d.applyFrame(g, atWrap);
    checkStr(d.currentLabel(), "two", "the screen jumped to the red session");

    // THE CHECK THAT PINS THE DEFECT. The deadline really is 0, and the hold
    // is nonetheless in force. Under the old sentinel these two could not
    // both be true: a deadline of 0 WAS the absence of a hold.
    checkInt((long)d.holdDeadline(), 0,
             "the hold deadline wrapped to exactly zero, as intended");
    check(d.holdingRed(),
          "a hold with a deadline of zero is still a hold, not an absence");

    // Ticking inside the interval must not clear it.
    for (uint32_t t = 1u; t < (uint32_t)DISP_ROTATE_MIN_MS; t += 100u) {
      d.tick(atWrap + t);
    }
    check(d.holdingRed(), "the hold survives ticking through the wrap");
    checkStr(d.currentLabel(), "two",
             "the red session held the screen across the wrap");

    // Past the deadline the hold releases, so the flag holds the screen
    // without wedging it for the remaining 49 days.
    d.tick(atWrap + (uint32_t)DISP_ROTATE_MIN_MS);
    // Past the deadline the interval hold releases, but the screen STAYS on
    // the red session: rotation is confined to the reds while any session is
    // red, and here there is exactly one. The lamp says something needs a
    // human and the screen has to keep saying which.
    d.tick(atWrap + (uint32_t)DISP_ROTATE_MIN_MS);
    checkStr(d.currentLabel(), "two",
             "the red session keeps the screen past the interval");

    // Still there much later. A red session does not scroll away on a timer.
    for (uint32_t t = (uint32_t)DISP_ROTATE_MIN_MS + 100u; t <= 30000u;
         t += 100u) {
      d.tick(atWrap + t);
    }
    checkStr(d.currentLabel(), "two",
             "and it is still there thirty seconds later");

    // Once it is no longer red, normal rotation resumes across the wrap.
    Frame h;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"done\",\"color\":\"green\"}]}",
                     &h),
          "the resolved frame parses");
    d.applyFrame(h, atWrap + 31000u);
    bool moved = false;
    for (uint32_t t = 31000u; t <= 60000u; t += 100u) {
      d.tick(atWrap + t);
      if (strcmp(d.currentLabel(), "one") == 0) {
        moved = true;
        break;
      }
    }
    check(moved, "rotation resumed once nothing was red any more");
  }

  // The same at a non-wrapping instant, so the flag is not merely correct at
  // the one boundary. A hold set well away from the wrap behaves identically.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"two\",\"state\":\"working\","
                     "\"color\":\"green\"}]}",
                     &f),
          "the all-green frame parses away from the wrap");
    d.applyFrame(f, 1000);
    Frame g;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"needs you\",\"color\":\"red\"}]}",
                     &g),
          "the turned-red frame parses away from the wrap");
    d.applyFrame(g, 5000);
    check(d.holdingRed(), "a hold away from the wrap is in force");
    checkInt((long)d.holdDeadline(), 5000 + DISP_ROTATE_MIN_MS,
             "and its deadline is the plain sum");
    d.tick(5000 + DISP_ROTATE_MIN_MS);
    checkStr(d.currentLabel(), "two",
             "and the red session still holds the screen away from the wrap");
  }

  // A manual advance overrides the hold rather than waiting it out: the user
  // is driving. This must clear the flag, not merely zero the deadline.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"two\",\"state\":\"needs "
                     "you\",\"color\":\"red\"}]}",
                     &f),
          "the frame with a red session parses");
    d.applyFrame(f, 1000);
    check(d.holdingRed(), "the red session took a hold");
    d.advance(1100);
    check(!d.holdingRed(), "a manual advance cleared the hold");
  }

  // A lone red session's hold must expire on its own deadline, not wait for a
  // rotation that can never happen. With one session rotation never runs, so
  // the only thing that can clear the flag is the deadline itself. A hold that
  // outlives its deadline is the defect: it blocks rotation for the next 24.9
  // days once (int32_t)(now - holdUntil_) wraps negative and a second session
  // appears.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"solo\",\"state\":\"needs you\","
                     "\"color\":\"red\"}]}",
                     &f),
          "the lone red frame parses");
    d.applyFrame(f, 1000);
    check(d.holdingRed(), "the lone red session took a hold");

    // Just short of the deadline the hold is still in force.
    d.tick(1000 + DISP_ROTATE_MIN_MS - 1);
    check(d.holdingRed(), "the hold is still in force before its deadline");

    // On the deadline it clears, even though rotation never ran.
    d.tick(1000 + DISP_ROTATE_MIN_MS);
    check(!d.holdingRed(),
          "the hold expired on its deadline without a rotation");

    // The session is lone and red, so the screen stays on it: the lamp names
    // something that needs a human and the screen must keep saying which.
    checkStr(d.currentLabel(), "solo",
             "the lone red session keeps the screen past the hold");
  }

  // The wedge the expiry prevents: a hold left standing past 2^31 ms makes
  // (int32_t)(now - holdUntil_) negative again, which blocked rotation for the
  // next 24.9 days once a second session appeared. With the hold expired on
  // time, a later second session rotates normally.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"solo\",\"state\":\"needs you\","
                     "\"color\":\"red\"}]}",
                     &f),
          "the lone red frame parses");
    d.applyFrame(f, 1000);
    check(d.holdingRed(), "the lone red session took a hold");

    // Let the hold expire, then run time well past 2^31 ms from the deadline,
    // where a stale flag would read (int32_t)(now - holdUntil_) < 0 again.
    d.tick(1000 + DISP_ROTATE_MIN_MS);
    check(!d.holdingRed(), "the hold is gone before the sign flips");

    // Two green sessions arrive long after the deadline. Rotation must advance
    // rather than stay wedged on the hold.
    Frame g;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"working\",\"color\":\"green\"}]}",
                     &g),
          "the two-green frame parses");
    const uint32_t late = 1000u + 0x80000000u + 50000u;  // well past 2^31 ms
    d.applyFrame(g, late);
    check(!d.holdingRed(), "no hold once nothing is red");
    char startLabel[SL_MAX_LABEL + 1];
    snprintf(startLabel, sizeof(startLabel), "%s", d.currentLabel());
    bool rotated = false;
    for (uint32_t t = 0; t <= 60000u; t += 250u) {
      d.tick(late + t);
      if (strcmp(d.currentLabel(), startLabel) != 0) {
        rotated = true;
        break;
      }
    }
    check(rotated,
          "rotation runs within 60s: a stale hold did not wedge it for days");
  }
}

// ---------------------------------------------------------------------------
// Display behaviour: rotation waits for scrolling, red jumps, the pin holds,
// and a session keeps its slot as others come and go.
// ---------------------------------------------------------------------------
void testDisplayBehaviour() {
  section("display");

  // A short label never scrolls and never holds up rotation.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 0);
    checkStr(d.currentLabel(), "one", "starts on the first session");

    d.tick(DISP_ROTATE_MIN_MS - 1);
    checkStr(d.currentLabel(), "one", "does not rotate before the minimum");

    d.tick(DISP_ROTATE_MIN_MS + 1);
    checkStr(d.currentLabel(), "two", "rotates once the minimum has passed");
  }

  // One session does not rotate.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"solo\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the single-session frame parses");
    d.applyFrame(f, 0);
    for (uint32_t t = 0; t < 30000; t += 500) d.tick(t);
    checkStr(d.currentLabel(), "solo", "a single session never rotates away");
  }

  // Rotation waits for a long label to finish scrolling: the minimum is a
  // minimum, not a period.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":"
                     "\"feature-a-very-long-branch-name-indeed\",\"state\":"
                     "\"working\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the long-label frame parses");
    d.applyFrame(f, 0);

    // Well past the rotation minimum, but the marquee is still running. The
    // overhang is derived from DISP_CHAR_W rather than written out, so that
    // changing the label font moves this expectation with it instead of
    // leaving the test asserting against the previous font's geometry.
    d.tick(DISP_ROTATE_MIN_MS + 100);
    checkStr(d.currentLabel(), "feature-a-very-long-branch-name-indeed",
             "rotation waits for the label to finish scrolling");

    // The cycle is the start hold + travel + the end hold, and it runs to
    // several times the rotation minimum. Ticking to just before the end must
    // still show the long label: the slot is being held by its content, which
    // is the whole point of the rule.
    const uint32_t overhangPx =
        (uint32_t)(strlen("feature-a-very-long-branch-name-indeed") *
                   DISP_CHAR_W) -
        DISP_W;
    const uint32_t cycleMs = DISP_SCROLL_HOLD_START_MS +
                             overhangPx * DISP_SCROLL_STEP_MS +
                             DISP_SCROLL_HOLD_END_MS;
    for (uint32_t t = DISP_ROTATE_MIN_MS + 200; t < cycleMs; t += 100) {
      d.tick(t);
    }
    checkStr(d.currentLabel(), "feature-a-very-long-branch-name-indeed",
             "the long label still holds the screen just before its cycle ends");

    // Tick past the end of the cycle and it finally advances.
    for (uint32_t t = cycleMs; t < cycleMs + 1000; t += 100) {
      d.tick(t);
    }
    checkStr(d.currentLabel(), "two", "it advances after the scroll finishes");
  }

  // A session keeps its slot as others come and go.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"first\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":"
                     "\"second\",\"state\":\"working\"}]}",
                     &f),
          "the two-session frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkStr(d.currentLabel(), "first", "showing the first");

    // The first session disappears. The screen must land somewhere sane.
    Frame g;
    check(parseFrame("{\"sessions\":[{\"id\":\"b2\",\"label\":\"second\","
                     "\"state\":\"working\"}]}",
                     &g),
          "the one-session frame parses");
    d.applyFrame(g, 2000);
    checkInt(d.sessionCount(), 1, "one session remains");
    checkStr(d.currentLabel(), "second", "the screen moved to the survivor");
  }

  // A pin releases itself when its session disappears.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"pinned\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":"
                     "\"other\",\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 1000);
    d.togglePin(1000);
    check(d.isPinned(), "pinned");

    Frame g;
    check(parseFrame("{\"sessions\":[{\"id\":\"b2\",\"label\":\"other\","
                     "\"state\":\"working\"}]}",
                     &g),
          "the frame without the pinned session parses");
    d.applyFrame(g, 2000);
    check(!d.isPinned(), "the pin released when its session disappeared");
  }

  // A pin expires after the timeout even if the session is still alive.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"pinned\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":"
                     "\"other\",\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 1000);
    d.togglePin(1000);
    check(d.isPinned(), "pinned");

    d.tick(1000 + DISP_PIN_TIMEOUT_MS - 1);
    check(d.isPinned(), "the pin holds until the timeout");
    d.tick(1000 + DISP_PIN_TIMEOUT_MS + 1);
    check(!d.isPinned(), "the pin expired after the timeout");
  }

  // A pin stops rotation.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"},{\"id\":\"b2\",\"label\":\"two\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 0);
    d.togglePin(0);
    for (uint32_t t = 0; t < 30000; t += 250) d.tick(t);
    checkStr(d.currentLabel(), "one", "a pinned session does not rotate away");
  }

  // The screen sleeps and any frame wakes it. The lamps are unaffected: that
  // is checked in the invariant test above.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 1000);
    check(!d.asleep(), "awake after a frame");
    d.sleep();
    check(d.asleep(), "asleep after sleep()");
    d.applyFrame(f, 2000);
    check(!d.asleep(), "a frame woke the screen");
  }

  // THE PERIODIC PANEL RE-ASSERT, the recovery for a controller that has lost
  // its configuration.
  //
  // There is no fault to inject here. The whole point of the design is that a
  // blanked SSD1306 still ACKs its address and still accepts writes, so the
  // firmware cannot tell a dead panel from a live one and re-runs the init
  // unconditionally. What the harness CAN check is that the init actually
  // fires, that it fires on schedule, and that it does not misbehave around
  // sleep or the millis() rollover.
  {
    Display d;
    slTestScreen.beginCount = 0;
    d.begin();
    checkInt((long)slTestScreen.beginCount, 1, "begin() inits the panel once");
    checkInt((long)d.reinitCount(), 0, "no re-assert has happened yet");

    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 0);

    // Just under the interval: nothing yet.
    d.tick(DISP_REINIT_INTERVAL_MS - 1);
    checkInt((long)d.reinitCount(), 0, "no re-assert before the interval");

    // On the interval: the panel is re-initialised.
    d.tick(DISP_REINIT_INTERVAL_MS);
    checkInt((long)d.reinitCount(), 1, "the panel re-asserts on the interval");
    checkInt((long)slTestScreen.beginCount, 2, "begin() ran again");
    checkStr(d.currentLabel(), "one", "the label survives a re-assert");

    // It repeats, rather than firing once and stopping.
    d.tick(DISP_REINIT_INTERVAL_MS * 2);
    checkInt((long)d.reinitCount(), 2, "the re-assert repeats");

    // Ticking rapidly in between must not re-init on every call.
    for (uint32_t t = DISP_REINIT_INTERVAL_MS * 2 + 1;
         t < DISP_REINIT_INTERVAL_MS * 3; t += 250) {
      d.tick(t);
    }
    checkInt((long)d.reinitCount(), 2, "the re-assert is rate limited");
  }

  // A re-assert while the screen is blanked must leave it blanked. begin()
  // powers the controller back on, so the recovery has to put it straight
  // back to sleep or an idle desk lights up every ten seconds.
  {
    Display d;
    d.begin();
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 0);
    d.sleep();
    check(slTestScreen.powerSave, "the panel powered down");

    d.tick(DISP_REINIT_INTERVAL_MS);
    checkInt((long)d.reinitCount(), 1, "a sleeping panel still re-asserts");
    check(d.asleep(), "the re-assert did not wake the screen");
    check(slTestScreen.powerSave, "the panel is still powered down");

    // And a frame still wakes it normally afterwards.
    d.applyFrame(f, DISP_REINIT_INTERVAL_MS + 100);
    check(!d.asleep(), "a frame still wakes a re-asserted panel");
    check(!slTestScreen.powerSave, "the panel powered back on");
  }

  // The re-assert timer survives the millis() rollover. Unsigned subtraction
  // makes this work; a naive now >= last + interval would stall for 49 days.
  {
    Display d;
    d.begin();
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");

    const uint32_t nearEnd = 0xFFFFFFFFUL - 100UL;
    d.applyFrame(f, nearEnd);
    d.tick(nearEnd);
    uint32_t before = d.reinitCount();

    // Cross the wrap. The elapsed time is only just over the interval. The
    // cast keeps the arithmetic in uint32_t: DISP_REINIT_INTERVAL_MS is UL, so
    // unqualified this promotes to 64-bit, computes past 2^32 and narrows on
    // assignment, testing a truncation rather than the wrap it claims to.
    uint32_t after = nearEnd + (uint32_t)DISP_REINIT_INTERVAL_MS + 1U;  // wraps
    d.tick(after);
    checkInt((long)(d.reinitCount() - before), 1,
             "the re-assert fires across the millis() rollover");
  }

  // An empty session list clears the screen.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the frame parses");
    d.applyFrame(f, 1000);
    checkInt(d.sessionCount(), 1, "one session");

    Frame g;
    check(parseFrame("{\"color\":\"off\",\"sessions\":[]}", &g),
          "the empty frame parses");
    check(g.hasSessions, "an empty array is still a sessions key");
    d.applyFrame(g, 2000);
    checkInt(d.sessionCount(), 0, "the screen cleared");
    checkStr(d.currentLabel(), "", "no label with no sessions");
  }

  // The position dots that replaced the "2/4" counter. These read the stub's
  // record of what was drawn, which is the only place the header layout is
  // observable on a host.
  {
    // One session draws no dots. A lone dot says nothing the label does not.
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"solo\","
                     "\"state\":\"working\"}]}",
                     &f),
          "the single-session frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkInt(slTestScreen.dots, 0, "one session draws no position dots");
  }

  {
    // Four sessions draw four dots, and the filled one tracks the rotation.
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"one\",\"state\":\"working\"},"
                     "{\"id\":\"b2\",\"label\":\"two\",\"state\":\"working\"},"
                     "{\"id\":\"c3\",\"label\":\"three\",\"state\":\"working\"},"
                     "{\"id\":\"d4\",\"label\":\"four\",\"state\":\"working\"}]}",
                     &f),
          "the four-session frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkInt(slTestScreen.dots, 4, "four sessions draw four dots");
    checkInt(slTestScreen.filledDot, 0, "the first dot is filled at first");

    // Advancing moves the fill along without changing the count.
    d.advance(1000);
    checkInt(slTestScreen.dots, 4, "advancing does not change the dot count");
    checkInt(slTestScreen.filledDot, 1, "the filled dot follows the rotation");

    d.advance(1100);
    checkInt(slTestScreen.filledDot, 2, "and again on the next advance");

    // The dots must stay on the panel. This is the assertion that a spacing
    // or radius change cannot quietly push the last dot off the right edge.
    for (uint8_t i = 0; i < slTestScreen.dots; i++) {
      check(slTestScreen.dotX[i] + DISP_DOT_R < DISP_W,
            "the dot sits inside the panel");
    }
  }

  {
    // The cap. SL_MAX_SESSIONS dots is the widest the row ever gets, and it
    // must still fit: this is the case the 7px spacing was chosen for.
    Display d;
    Frame f;
    std::string s = "{\"sessions\":[";
    for (int i = 0; i < SL_MAX_SESSIONS; i++) {
      if (i > 0) s += ",";
      s += "{\"id\":\"s";
      s += (char)('0' + i);
      s += "\",\"label\":\"x\",\"state\":\"working\"}";
    }
    s += "]}";
    check(parseFrame(s.c_str(), &f), "the full frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkInt(slTestScreen.dots, SL_MAX_SESSIONS,
             "a full set draws one dot each");
    for (uint8_t i = 0; i < slTestScreen.dots; i++) {
      check(slTestScreen.dotX[i] + DISP_DOT_R < DISP_W,
            "every dot fits at the session cap");
    }
  }

  {
    // N distinct sessions yield N dots, for every N the panel can show. The
    // dot row is the only place the session COUNT is visible, so an entry
    // dropped anywhere between the wire and the glass shows up here and in
    // no other assertion.
    for (int n = 2; n <= SL_MAX_SESSIONS; n++) {
      Display d;
      Frame f;
      std::string s = "{\"sessions\":[";
      for (int i = 0; i < n; i++) {
        if (i > 0) s += ",";
        s += "{\"id\":\"id";
        s += (char)('0' + i);
        s += "\",\"label\":\"L";
        s += (char)('0' + i);
        s += "\",\"state\":\"working\"}";
      }
      s += "]}";
      check(parseFrame(s.c_str(), &f), "the N-distinct frame parses");
      checkInt(f.count, n, "the parser keeps every distinct session");
      d.applyFrame(f, 1000);
      d.tick(1000);
      checkInt(d.sessionCount(), n, "the display keeps every distinct session");
      checkInt(slTestScreen.dots, n, "N distinct sessions draw N dots");
    }
  }

  {
    // The collision case, taken from the real relay. Two sessions on the same
    // branch carry byte-identical labels, and two more share a label prefix.
    // The parser drops duplicates by ID, so identical labels with distinct ids
    // are six separate sessions and must draw six dots. Deduplicating on the
    // label instead of the id would collapse them to two, which is exactly the
    // reported failure.
    Display d;
    Frame f;
    check(parseFrame(
              "{\"color\":\"red\",\"sessions\":["
              "{\"id\":\"a7e2435b-65b1-42a8-9cd7-45258d323f43\",\"label\":\"stoplight\","
              "\"state\":\"working\",\"color\":\"yellow\"},"
              "{\"id\":\"e2edd28f-ea67-4656-aff2-b5c1dd1ce661\","
              "\"label\":\"feat/per-event-tracking-user-stamp\","
              "\"state\":\"needs you\",\"color\":\"red\"},"
              "{\"id\":\"f10494b7-4c98-4323-835b-beabc0fc95d6\","
              "\"label\":\"feat/per-event-tracking-user-stamp\","
              "\"state\":\"needs you\",\"color\":\"red\"},"
              "{\"id\":\"x1\",\"label\":\"api\",\"state\":\"needs you\",\"color\":\"red\"},"
              "{\"id\":\"x2\",\"label\":\"payments-service\","
              "\"state\":\"working\",\"color\":\"yellow\"},"
              "{\"id\":\"x3\",\"label\":\"feat/auth-refactor\","
              "\"state\":\"done\",\"color\":\"green\"}]}",
              &f),
          "the duplicate-label frame parses");
    checkInt(f.count, 6, "two byte-identical labels are still two sessions");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkInt(d.sessionCount(), 6, "the display keeps both same-label sessions");
    checkInt(slTestScreen.dots, 6, "duplicate labels still draw six dots");

    // The dedup compares whole ids. Two UUIDs can agree for many characters,
    // so a cap that truncated them to a shared prefix would make the dedup
    // treat distinct sessions as one. This is the guard on that.
    check(SL_MAX_ID >= 36, "SL_MAX_ID holds a whole 36-char UUID");
  }

  {
    // The pin marker survives the counter's removal, and pinning does not
    // move the dots: a row that shifted sideways on a pin would read as the
    // position having changed.
    Display d;
    Frame f;
    check(parseFrame("{\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"one\",\"state\":\"working\"},"
                     "{\"id\":\"b2\",\"label\":\"two\",\"state\":\"working\"}]}",
                     &f),
          "the two-session frame parses");
    d.applyFrame(f, 1000);
    d.tick(1000);
    checkStr(slTestScreen.header, "", "no pin marker when not pinned");
    uint16_t unpinnedFirstX = slTestScreen.dotX[0];

    d.togglePin(1000);
    d.tick(1000);
    check(d.isPinned(), "the session pinned");
    checkStr(slTestScreen.header, "*", "the pin marker is drawn when pinned");
    checkInt(slTestScreen.dotX[0], unpinnedFirstX,
             "the dots do not shift when the pin marker appears");
  }

  // ---------------------------------------------------------------------
  // Rotation is confined to the red sessions while anything is red.
  //
  // The lamp says something needs a human. The screen has to say WHICH, and
  // a red session that scrolled away on a three-second timer made the screen
  // useless for its one job at the moment it mattered most.
  // ---------------------------------------------------------------------

  // One red among several: the screen holds it and does not wander off.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"green-one\","
                     "\"state\":\"done\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"the-red\","
                     "\"state\":\"needs you\",\"color\":\"red\"},"
                     "{\"id\":\"c3\",\"label\":\"yellow-one\","
                     "\"state\":\"working\",\"color\":\"yellow\"}]}",
                     &f),
          "the one-red frame parses");
    d.applyFrame(f, 1000);
    checkStr(d.currentLabel(), "the-red", "the screen jumped to the red one");

    // Tick for a minute. Nothing may take the screen off it.
    bool strayed = false;
    for (uint32_t t = 1100u; t <= 61000u; t += 100u) {
      d.tick(t);
      if (strcmp(d.currentLabel(), "the-red") != 0) {
        strayed = true;
        break;
      }
    }
    check(!strayed, "a red session never scrolls away while it is red");
  }

  // THE CASE A PLAIN HOLD GETS WRONG. Two red sessions must both be named:
  // holding the first would mean the second is never seen, so the human deals
  // with one problem and does not learn there are two.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"red-one\","
                     "\"state\":\"needs you\",\"color\":\"red\"},"
                     "{\"id\":\"b2\",\"label\":\"green-mid\","
                     "\"state\":\"done\",\"color\":\"green\"},"
                     "{\"id\":\"c3\",\"label\":\"red-two\","
                     "\"state\":\"needs you\",\"color\":\"red\"}]}",
                     &f),
          "the two-red frame parses");
    d.applyFrame(f, 1000);

    bool sawOne = false, sawTwo = false, sawGreen = false;
    for (uint32_t t = 1100u; t <= 61000u; t += 100u) {
      d.tick(t);
      const char* label = d.currentLabel();
      if (strcmp(label, "red-one") == 0) sawOne = true;
      if (strcmp(label, "red-two") == 0) sawTwo = true;
      if (strcmp(label, "green-mid") == 0) sawGreen = true;
    }
    check(sawOne, "the first red session was shown");
    check(sawTwo, "the second red session was shown too, not starved");
    check(!sawGreen,
          "and the green session between them was skipped while reds waited");
  }

  // Once nothing is red, full rotation comes back.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"was-red\","
                     "\"state\":\"needs you\",\"color\":\"red\"},"
                     "{\"id\":\"b2\",\"label\":\"other\","
                     "\"state\":\"working\",\"color\":\"yellow\"}]}",
                     &f),
          "the red frame parses");
    d.applyFrame(f, 1000);
    for (uint32_t t = 1100u; t <= 20000u; t += 100u) d.tick(t);
    checkStr(d.currentLabel(), "was-red", "the red session held");

    Frame g;
    check(parseFrame("{\"color\":\"yellow\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"was-red\","
                     "\"state\":\"working\",\"color\":\"yellow\"},"
                     "{\"id\":\"b2\",\"label\":\"other\","
                     "\"state\":\"working\",\"color\":\"yellow\"}]}",
                     &g),
          "the resolved frame parses");
    d.applyFrame(g, 20100);

    bool rotated = false;
    for (uint32_t t = 20200u; t <= 40000u; t += 100u) {
      d.tick(t);
      if (strcmp(d.currentLabel(), "other") == 0) {
        rotated = true;
        break;
      }
    }
    check(rotated, "rotation resumed once the red was resolved");
  }

  // The pin still wins. A pin is a deliberate instruction to watch one
  // session, and the lamp is red regardless, so the alert is not lost.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"pinned-green\","
                     "\"state\":\"done\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"the-red\","
                     "\"state\":\"needs you\",\"color\":\"red\"}]}",
                     &f),
          "the frame parses");

    // Pin the green session BEFORE the red arrives, by applying an all-green
    // frame first so the jump does not move the index.
    Frame green;
    check(parseFrame("{\"color\":\"green\",\"sessions\":["
                     "{\"id\":\"a1\",\"label\":\"pinned-green\","
                     "\"state\":\"done\",\"color\":\"green\"},"
                     "{\"id\":\"b2\",\"label\":\"other\","
                     "\"state\":\"done\",\"color\":\"green\"}]}",
                     &green),
          "the all-green frame parses");
    d.applyFrame(green, 1000);
    d.togglePin(1000);
    check(d.isPinned(), "the green session is pinned");

    d.applyFrame(f, 2000);
    for (uint32_t t = 2100u; t <= 30000u; t += 100u) d.tick(t);
    checkStr(d.currentLabel(), "pinned-green",
             "the pin held even though another session was red");
  }

  // A single session that is red must not thrash its own scroll. Advancing to
  // the slot you are already on would restart the marquee every interval,
  // and a label that keeps jumping back is harder to read than one that
  // holds.
  {
    Display d;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"a-very-long-label-that-scrolls\","
                     "\"state\":\"needs you\",\"color\":\"red\"}]}",
                     &f),
          "the single red frame parses");
    d.applyFrame(f, 1000);
    for (uint32_t t = 1100u; t <= 30000u; t += 100u) d.tick(t);
    checkStr(d.currentLabel(), "a-very-long-label-that-scrolls",
             "a single red session is still the one on screen");
  }
}

// ---------------------------------------------------------------------------
// String handling at the buffer boundaries, and the escape rules.
// ---------------------------------------------------------------------------
void testStringHandling() {
  section("strings and escapes");

  // An over-long label truncates rather than refusing the frame.
  {
    std::string label(SL_MAX_LABEL + 40, 'q');
    std::string s = "{\"sessions\":[{\"id\":\"a1\",\"label\":\"" + label +
                    "\",\"state\":\"working\"}]}";
    Frame f;
    check(parseFrame(s.c_str(), &f), "an over-long label still parses");
    checkInt((long)strlen(f.sessions[0].label), SL_MAX_LABEL,
             "the label truncated to SL_MAX_LABEL");
  }

  // Escapes are decoded, and non-ASCII collapses to a single '?'.
  {
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"a\\\"b\\\\c\","
                     "\"state\":\"working\"}]}",
                     &f),
          "escaped quotes and backslashes parse");
    checkStr(f.sessions[0].label, "a\"b\\c", "the escapes were decoded");
  }
  {
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"caf\xc3\xa9\","
                     "\"state\":\"working\"}]}",
                     &f),
          "a UTF-8 label parses");
    checkStr(f.sessions[0].label, "caf?",
             "a multi-byte sequence collapses to one '?'");
  }
  {
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"a\\u0041b\","
                     "\"state\":\"working\"}]}",
                     &f),
          "a \\u escape parses");
    checkStr(f.sessions[0].label, "aAb", "\\u0041 became 'A'");
  }

  // A \u escape cut off by the end of the line. The scan reads four bytes
  // past the 'u' before it can classify them, so every truncation length has
  // to be bounded explicitly rather than relying on NUL happening not to be a
  // hex digit. Under ASan an overread here is a crash, not a wrong answer,
  // which is the point of running these under the sanitisers.
  {
    const char* cut[] = {
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u0",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u00",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u004",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u0041",
    };
    for (size_t i = 0; i < sizeof(cut) / sizeof(cut[0]); i++) {
      Frame f;
      char what[96];
      snprintf(what, sizeof(what),
               "a \\u escape cut after %zu digits does not overread", i);
      // The frame is truncated, so it parses to whatever it managed. What
      // matters is that it returns at all, without reading past the NUL.
      check(parseFrame(cut[i], &f), what);
      snprintf(what, sizeof(what),
               "a \\u escape cut after %zu digits yields no aggregate", i);
      check(!f.hasColor, what);
    }
  }
  {
    // The same, with the escape at the very end of the buffer rather than
    // followed by more of a frame, and with a non-hex byte in each position.
    const char* bad[] = {
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\uZZZZ\"}]}",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u00Z1\"}]}",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u0 41\"}]}",
    };
    for (size_t i = 0; i < sizeof(bad) / sizeof(bad[0]); i++) {
      Frame f;
      char what[96];
      snprintf(what, sizeof(what), "a non-hex \\u escape (%zu) is refused "
                                   "without overreading", i);
      check(parseFrame(bad[i], &f), what);
      checkInt(f.count, 0, "a session with a bad escape is not stored");
    }
  }
  {
    // A valid escape immediately before the closing quote, which is the
    // in-bounds boundary the guard must NOT reject.
    Frame f;
    check(parseFrame("{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u0041\"}]}",
                     &f),
          "a \\u escape at the end of a value parses");
    checkStr(f.sessions[0].label, "xA", "the boundary escape decoded");
  }
  {
    // The truncated escape on a HEAP buffer sized exactly to the input, so
    // there is an ASan redzone immediately after the terminator. On the stack
    // or in a string literal an overread lands in other valid memory and goes
    // unnoticed; here the four-byte lookahead past the 'u' is caught.
    //
    // This is the check that gives the bounds guard teeth. Without it the
    // scan is safe only because NUL is not a hex digit, which is a property
    // of the classifier rather than a stated rule, and one edit from wrong.
    const char* srcs[] = {
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u0",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u00",
        "{\"sessions\":[{\"id\":\"a1\",\"label\":\"x\\u004",
    };
    for (size_t i = 0; i < sizeof(srcs) / sizeof(srcs[0]); i++) {
      size_t n = strlen(srcs[i]);
      char* exact = new char[n + 1];
      memcpy(exact, srcs[i], n + 1);
      Frame f;
      bool ok = parseFrame(exact, &f);
      delete[] exact;
      char what[112];
      snprintf(what, sizeof(what),
               "a \\u escape cut after %zu digits reads nothing past the "
               "terminator", i);
      // Reaching this line at all means ASan saw no overread. The return
      // value is incidental; the sanitiser is the assertion.
      check(ok, what);
    }
  }

  // Colour names.
  {
    Color c;
    check(parseColor("off", &c) && c == COLOR_OFF, "\"off\" parses");
    check(parseColor("green", &c) && c == COLOR_GREEN, "\"green\" parses");
    check(parseColor("yellow", &c) && c == COLOR_YELLOW, "\"yellow\" parses");
    check(parseColor("red", &c) && c == COLOR_RED, "\"red\" parses");
    check(!parseColor("purple", &c), "an unknown colour is refused");
    check(!parseColor(nullptr, &c), "a null colour name is refused");
  }

  // Non-objects are refused outright: there is nothing to act on.
  {
    Frame f;
    check(!parseFrame("[1,2,3]", &f), "an array is not a frame");
    check(!parseFrame("", &f), "an empty string is not a frame");
    check(!parseFrame("hello", &f), "bare text is not a frame");
    check(!parseFrame(nullptr, &f), "a null line is not a frame");
    check(parseFrame("{}", &f), "an empty object is a valid, empty frame");
    check(!f.hasColor && !f.hasSessions, "{} instructs nothing");
  }

  // A CRLF sender must not produce a phantom blank frame.
  {
    LineReader r;
    const char* line = "{\"color\":\"red\"}";
    bool completed = false;
    for (const char* p = line; *p; p++) {
      if (r.feed(*p)) completed = true;
    }
    check(r.feed('\r'), "CR terminates the line");
    check(!r.feed('\n'), "the following LF does not produce a blank frame");
    (void)completed;
  }
}

// ---------------------------------------------------------------------------
// The button, at the short/long boundary.
// ---------------------------------------------------------------------------
void testButton() {
  section("button");

  // Bounce inside the debounce window produces nothing.
  {
    Button b;
    b.begin();
    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(0);

    ButtonEvent seen = BUTTON_NONE;
    for (uint32_t t = 0; t < 40; t += 5) {
      slTestPinLevel[BUTTON_PIN] = (t / 5) % 2 ? LOW : HIGH;
      ButtonEvent e = b.update(t);
      if (e != BUTTON_NONE) seen = e;
    }
    check(seen == BUTTON_NONE, "contact bounce produces no event");
  }

  // A clean short press.
  {
    Button b;
    b.begin();
    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(0);
    slTestPinLevel[BUTTON_PIN] = LOW;
    b.update(100);
    b.update(200);  // debounced press
    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(300);
    ButtonEvent e = b.update(400);
    check(e == BUTTON_SHORT_PRESS, "a 200ms press is a short press");
  }

  // A long press fires while the button is still held.
  {
    Button b;
    b.begin();
    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(0);
    slTestPinLevel[BUTTON_PIN] = LOW;
    b.update(100);
    b.update(200);

    ButtonEvent e = BUTTON_NONE;
    for (uint32_t t = 250; t <= 900; t += 50) {
      ButtonEvent got = b.update(t);
      if (got != BUTTON_NONE) e = got;
    }
    check(e == BUTTON_LONG_PRESS, "a held button fires a long press");

    // The release must not also count as a short press.
    slTestPinLevel[BUTTON_PIN] = HIGH;
    b.update(950);
    ButtonEvent after = b.update(1050);
    check(after == BUTTON_NONE, "the release after a long press is silent");
  }
}

}  // namespace

// ---------------------------------------------------------------------------
// The BLE link: the byte queue, the drop rule, and reassembly through the
// SAME LineReader the serial path uses.
//
// What these checks cannot reach is the radio itself. BleLink::begin() is a
// no-op on a host, so nothing here proves the service is advertised, the UUID
// is right, or the MTU is negotiated. That gap is the same shape as the panel
// offset: a passing suite says nothing about it. See firmware/readme.md.
// ---------------------------------------------------------------------------
void testBleLink() {
  section("BLE link");

  // A chunk boundary is a byte offset and nothing else. It may fall inside a
  // string, inside a number, or between the last '}' and the '\n'. The
  // contract says so explicitly, and this walks EVERY boundary of a real
  // frame to prove no particular one is special.
  {
    const char* json =
        "{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\",\"label\":\"auth\","
        "\"state\":\"needs you\",\"color\":\"red\"}]}";
    std::string framed = std::string(json) + "\n";

    bool everySplitWorked = true;
    for (size_t cut = 0; cut <= framed.size(); cut++) {
      BleLink link;
      LineReader rd;

      // Two writes, split at `cut`. This is what a two-chunk frame looks
      // like at that MTU.
      link.push((const uint8_t*)framed.data(), (uint16_t)cut);
      link.push((const uint8_t*)framed.data() + cut,
                (uint16_t)(framed.size() - cut));

      Frame f;
      bool got = false;
      while (link.available() > 0) {
        int c = link.read();
        if (c < 0) break;
        if (rd.feed((char)c)) {
          got = parseFrame(rd.line(), &f);
        }
      }
      if (!got || !f.hasColor || f.color != COLOR_RED || f.count != 1) {
        everySplitWorked = false;
        break;
      }
    }
    check(everySplitWorked,
          "a frame reassembles correctly at every possible chunk boundary");
  }

  // The minimum chunk size from the contract: 20 bytes, what every stack
  // supports when no MTU is negotiated.
  {
    const char* json =
        "{\"color\":\"yellow\",\"sessions\":[{\"id\":\"b2\","
        "\"label\":\"build\",\"state\":\"working\",\"color\":\"yellow\"}]}";
    std::string framed = std::string(json) + "\n";

    BleLink link;
    LineReader rd;
    for (size_t off = 0; off < framed.size(); off += 20) {
      size_t n = framed.size() - off;
      if (n > 20) n = 20;
      link.push((const uint8_t*)framed.data() + off, (uint16_t)n);
    }

    Frame f;
    bool got = false;
    while (link.available() > 0) {
      int c = link.read();
      if (c < 0) break;
      if (rd.feed((char)c)) got = parseFrame(rd.line(), &f);
    }
    check(got && f.hasColor && f.color == COLOR_YELLOW,
          "a frame in 20-byte chunks reassembles, the no-MTU minimum");
  }

  // Rule 5: a frame whose length is an exact multiple of the chunk size
  // produces no empty trailing chunk, and the peripheral must not wait for
  // one. Feeding exact multiples must still yield the frame.
  {
    std::string framed = "{\"color\":\"green\"}\n";
    // Pad the label-free frame to a length divisible by 4.
    while (framed.size() % 4 != 0) {
      framed.insert(framed.size() - 1, " ");
    }

    BleLink link;
    LineReader rd;
    for (size_t off = 0; off < framed.size(); off += 4) {
      link.push((const uint8_t*)framed.data() + off, 4);
    }

    Frame f;
    bool got = false;
    while (link.available() > 0) {
      int c = link.read();
      if (c < 0) break;
      if (rd.feed((char)c)) got = parseFrame(rd.line(), &f);
    }
    check(got && f.hasColor && f.color == COLOR_GREEN,
          "an exact multiple of the chunk size needs no empty final chunk");
  }

  // Two frames back to back in one write. The '\n' is the only delimiter, so
  // the bytes after it begin the next frame.
  {
    std::string two =
        "{\"color\":\"green\"}\n{\"color\":\"red\"}\n";

    BleLink link;
    LineReader rd;
    link.push((const uint8_t*)two.data(), (uint16_t)two.size());

    Frame f;
    int frames = 0;
    Color last = COLOR_OFF;
    while (link.available() > 0) {
      int c = link.read();
      if (c < 0) break;
      if (rd.feed((char)c) && parseFrame(rd.line(), &f)) {
        frames++;
        if (f.hasColor) last = f.color;
      }
    }
    checkInt(frames, 2, "two frames in one write are two frames");
    check(last == COLOR_RED, "and the second one is the one that stands");
  }

  // An empty queue reads -1, mirroring Serial.read(), so the drain loop in
  // the sketch reads the same for both transports.
  {
    BleLink link;
    checkInt((long)link.available(), 0, "a fresh queue is empty");
    checkInt(link.read(), -1, "and reading it returns -1 like Serial does");
  }

  // The drop rule. A chunk that does not fit is dropped WHOLE: a partial
  // write would put a hole in the middle of a frame, which costs that frame
  // and can eat the following frame's newline too.
  {
    BleLink link;
    std::string big(BLE_RX_CAPACITY - 1, 'x');
    link.push((const uint8_t*)big.data(), (uint16_t)big.size());
    uint16_t filled = link.available();
    checkInt((long)filled, (long)(BLE_RX_CAPACITY - 1),
             "the queue fills to capacity");

    // One more byte cannot fit.
    const uint8_t one = 'y';
    link.push(&one, 1);
    checkInt((long)link.available(), (long)filled,
             "a chunk that does not fit is dropped rather than truncated");
  }

  // The queue wraps. Draining and refilling repeatedly must not corrupt
  // anything, or a long-running light would rot after a few thousand frames.
  {
    BleLink link;
    LineReader rd;
    std::string framed = "{\"color\":\"red\"}\n";

    int parsed = 0;
    for (int round = 0; round < 400; round++) {
      link.push((const uint8_t*)framed.data(), (uint16_t)framed.size());
      Frame f;
      while (link.available() > 0) {
        int c = link.read();
        if (c < 0) break;
        if (rd.feed((char)c) && parseFrame(rd.line(), &f)) parsed++;
      }
    }
    checkInt(parsed, 400, "400 frames through a wrapping queue all parse");
  }

  // A truncated frame, the failure the contract names first. The link drops
  // mid-frame so the '\n' never arrives; the next frame's bytes append to
  // the remains, producing one corrupt line. The cost must be ONE update,
  // not a wedged reader.
  {
    BleLink link;
    LineReader rd;

    // Half a frame, no newline.
    const char* half = "{\"color\":\"red\",\"sess";
    link.push((const uint8_t*)half, (uint16_t)strlen(half));

    Frame f;
    int parsed = 0;
    while (link.available() > 0) {
      int c = link.read();
      if (c < 0) break;
      if (rd.feed((char)c) && parseFrame(rd.line(), &f)) parsed++;
    }
    checkInt(parsed, 0, "a truncated frame yields nothing");

    // A whole frame follows. Its bytes join the orphaned prefix, so the first
    // line is garbled: the dropped prefix was {"color":"red","sess and the
    // fresh frame glues on at its "color", so the corrupt line reads
    // {"color":"red","sess{"color":"green"}. That line MUST be refused, not
    // read as red: the stale colour the central dropped on must not survive
    // the fresh green frame that follows it.
    std::string next = "{\"color\":\"green\"}\n{\"color\":\"yellow\"}\n";
    link.push((const uint8_t*)next.data(), (uint16_t)next.size());
    Color last = COLOR_OFF;
    bool keptStaleRed = false;
    while (link.available() > 0) {
      int c = link.read();
      if (c < 0) break;
      if (rd.feed((char)c) && parseFrame(rd.line(), &f)) {
        if (f.hasColor) {
          if (f.color == COLOR_RED) keptStaleRed = true;
          last = f.color;
        }
      }
    }
    check(!keptStaleRed,
          "the glued line never surfaces the dropped frame's stale colour");
    check(last == COLOR_YELLOW,
          "the reader recovers on the frame after a truncation");
  }

  // A repeated frame, the second failure the contract names. After a
  // reconnect the relay resends the current frame, so the same content can
  // arrive twice. Applying it twice must be indistinguishable from once.
  {
    BleLink link;
    LineReader rd;
    Display d;
    std::string framed =
        "{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\",\"label\":\"one\","
        "\"state\":\"needs you\",\"color\":\"red\"}]}\n";

    for (int i = 0; i < 2; i++) {
      link.push((const uint8_t*)framed.data(), (uint16_t)framed.size());
      Frame f;
      while (link.available() > 0) {
        int c = link.read();
        if (c < 0) break;
        if (rd.feed((char)c) && parseFrame(rd.line(), &f)) {
          d.applyFrame(f, (uint32_t)(1000 + i * 1000));
        }
      }
    }
    checkInt(d.sessionCount(), 1, "a frame applied twice leaves one session");
    checkStr(d.currentLabel(), "one", "and the same label");
  }

  // An oversized line must be discarded rather than growing the buffer, and
  // the aggregate salvaged, exactly as over serial. The transport changes
  // nothing about this.
  {
    BleLink link;
    LineReader rd;

    std::string huge = "{\"color\":\"red\",\"sessions\":[";
    while (huge.size() < (size_t)SL_LINE_MAX + 200) {
      huge += "{\"id\":\"padpadpadpadpadpad\",\"label\":\"padpadpadpad\"},";
    }
    huge += "]}\n";

    // Push it in MTU-sized pieces, as the radio would.
    Color salvaged = COLOR_OFF;
    bool sawSalvage = false;
    for (size_t off = 0; off < huge.size(); off += 180) {
      size_t n = huge.size() - off;
      if (n > 180) n = 180;
      link.push((const uint8_t*)huge.data() + off, (uint16_t)n);

      while (link.available() > 0) {
        int c = link.read();
        if (c < 0) break;
        if (!rd.feed((char)c)) {
          Color out;
          if (rd.overflowColor(&out)) {
            salvaged = out;
            sawSalvage = true;
          }
        }
      }
    }
    check(sawSalvage && salvaged == COLOR_RED,
          "an oversized frame over BLE still surrenders its aggregate");
  }

  // A null or zero-length chunk is a no-op rather than a crash. A stack that
  // reports a write with no payload must not take the light down.
  {
    BleLink link;
    link.push(nullptr, 10);
    link.push((const uint8_t*)"x", 0);
    checkInt((long)link.available(), 0,
             "a null or empty chunk changes nothing");
  }

  // Connection state is bookkeeping only. Nothing about frame handling reads
  // it, so a frame that arrives while the flag says disconnected is still a
  // frame.
  {
    BleLink link;
    check(!link.connected(), "a fresh link is not connected");
    link.onConnect();
    check(link.connected(), "onConnect sets it");
    link.onDisconnect();
    check(!link.connected(), "onDisconnect clears it");

    // And the queue is untouched by a disconnect: a partial line survives,
    // which is what the contract requires.
    link.push((const uint8_t*)"ab", 2);
    link.onDisconnect();
    checkInt((long)link.available(), 2,
             "a disconnect does not discard queued bytes");
  }
}

// ---------------------------------------------------------------------------
// Standby when the link drops (issue #39).
//
// A device left powered on with no central connected held its last lamp colour
// for ten hours. When the BLE link is down AND no frame has arrived for the
// grace window, the light must clear the lamps to COLOR_OFF and sleep the
// panel, and come straight back on a reconnect.
//
// loop() does standby in one statement: if standby.update() reports the engage
// transition, it clears the lamps and sleeps the display. These checks drive
// the same state machine against real Lamps and Display instances and assert on
// what the hardware actually did, so the lamp power-down and the OLED sleep are
// covered, not just the predicate.
// ---------------------------------------------------------------------------
void testStandby() {
  section("standby on link drop");

  // engageStep mirrors loop() step 5 exactly: advance the state machine, and on
  // the engage transition clear the lamps and sleep the panel. Returning the
  // transition lets a test assert it fired exactly once.
  auto engageStep = [](Standby* sb, Lamps* lamps, Display* display, uint32_t now,
                       bool connected) -> bool {
    if (sb->update(now, connected)) {
      lamps->set(COLOR_OFF);
      if (!display->asleep()) {
        display->sleep();
      }
      return true;
    }
    return false;
  };

  // (1) and (4): after a disconnect and the full grace window with no frame,
  // the lamps go COLOR_OFF and the OLED standby path is invoked.
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;

    // The light is driven and connected, then shows red.
    sb.begin(0);
    slTestScreen.powerSave = false;
    Frame f;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"needs you\","
                     "\"color\":\"red\"}]}",
                     &f),
          "the red frame parses");
    lamps.set(f.color);
    display.applyFrame(f, 1000);
    sb.noteActivity(1000);
    check(lamps.current() == COLOR_RED, "the lamp is red while driven");

    // The link drops at t=1000. While connected() was true the timer rearmed,
    // so the grace window is measured from the drop.
    bool engaged = false;
    for (uint32_t t = 1000; t <= 1000 + STANDBY_GRACE_MS; t += 250) {
      if (engageStep(&sb, &lamps, &display, t, /*connected=*/false)) {
        engaged = true;
      }
    }
    check(engaged, "standby engaged after the link dropped past the grace");
    check(sb.active(), "the state machine reports standby active");
    check(lamps.current() == COLOR_OFF,
          "the lamps cleared to COLOR_OFF: the stuck-lamp defect is fixed");
    checkInt((long)slTestLedcDuty[LAMP_PIN_RED], 0, "the red LED is dark");
    check(display.asleep(), "the OLED standby path was invoked on disconnect");
    check(slTestScreen.powerSave, "the panel powered down");
  }

  // (2): a sub-grace blip does not blank. The link drops and comes back inside
  // the grace window, so the lamps never go off and the panel never sleeps.
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;
    sb.begin(0);
    slTestScreen.powerSave = false;

    Frame f;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"}]}",
                     &f),
          "the green frame parses");
    lamps.set(f.color);
    display.applyFrame(f, 1000);
    sb.noteActivity(1000);

    // Down for less than the grace window.
    const uint32_t blipEnds = 1000 + STANDBY_GRACE_MS - 250;
    bool engaged = false;
    for (uint32_t t = 1000; t < blipEnds; t += 250) {
      if (engageStep(&sb, &lamps, &display, t, /*connected=*/false)) {
        engaged = true;
      }
    }
    check(!engaged, "a sub-grace blip does not enter standby");
    check(lamps.current() == COLOR_GREEN,
          "the lamp stayed green through the blip: no flicker off");
    check(!display.asleep(), "the panel stayed awake through the blip");

    // The link comes back and a fresh frame arrives. update() with connected
    // true keeps the timer rearmed, so standby never fires.
    sb.noteActivity(blipEnds);
    for (uint32_t t = blipEnds; t <= blipEnds + STANDBY_GRACE_MS * 2;
         t += 250) {
      engageStep(&sb, &lamps, &display, t, /*connected=*/true);
    }
    check(!sb.active(), "a reconnected link never enters standby");
    check(lamps.current() == COLOR_GREEN, "and the lamp is still green");
  }

  // (3): reconnect resumes driving the lamps. The light goes into standby, then
  // a frame arrives after the reconnect; the lamp follows the frame and the
  // panel wakes, exactly as loop() does it (the frame drives lamps and display,
  // and standby.update lifts itself once the timer is rearmed).
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;
    sb.begin(0);
    slTestScreen.powerSave = false;

    Frame red;
    check(parseFrame("{\"color\":\"red\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"needs you\","
                     "\"color\":\"red\"}]}",
                     &red),
          "the red frame parses");
    lamps.set(red.color);
    display.applyFrame(red, 1000);
    sb.noteActivity(1000);

    // Drop and run past the grace: standby engages.
    for (uint32_t t = 1000; t <= 1000 + STANDBY_GRACE_MS; t += 250) {
      engageStep(&sb, &lamps, &display, t, /*connected=*/false);
    }
    check(sb.active(), "standby engaged");
    check(lamps.current() == COLOR_OFF, "the lamp is off in standby");
    check(display.asleep(), "the panel is asleep in standby");

    // Reconnect and a fresh green frame. This is one loop() iteration: the
    // frame drives the lamp and wakes the display, noteActivity rearms the
    // timer, and the standby step then lifts itself rather than re-engaging.
    const uint32_t resumeAt = 1000 + STANDBY_GRACE_MS + 1000;
    Frame green;
    check(parseFrame("{\"color\":\"green\",\"sessions\":[{\"id\":\"a1\","
                     "\"label\":\"one\",\"state\":\"working\","
                     "\"color\":\"green\"}]}",
                     &green),
          "the resume frame parses");
    lamps.set(green.color);          // the lamp follows the frame
    display.applyFrame(green, resumeAt);  // applyFrame wakes the panel
    sb.noteActivity(resumeAt);
    bool reEngaged = engageStep(&sb, &lamps, &display, resumeAt,
                                /*connected=*/true);

    check(!reEngaged, "the resume iteration does not re-enter standby");
    check(!sb.active(), "standby lifted on reconnect");
    check(lamps.current() == COLOR_GREEN,
          "the lamp resumed from the next frame after reconnect");
    check(!display.asleep(), "the panel woke on the resume frame");
    check(!slTestScreen.powerSave, "the panel powered back on");
  }

  // Never-connected-at-startup is the same path: a board powered on with no
  // central ever present powers down once the grace window passes, rather than
  // holding a dark-but-live light forever.
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;
    sb.begin(0);  // armed at boot, never connected, never a frame

    bool engaged = false;
    for (uint32_t t = 0; t <= STANDBY_GRACE_MS; t += 250) {
      if (engageStep(&sb, &lamps, &display, t, /*connected=*/false)) {
        engaged = true;
      }
    }
    check(engaged, "a never-connected light powers down after the grace");
    check(lamps.current() == COLOR_OFF, "its lamps are off");
    check(display.asleep(), "its panel is asleep");
  }

  // The grace timer survives the millis() rollover. A drop that straddles the
  // wrap must still power down after exactly the grace window, not stall for 49
  // days, because the elapsed-time test uses unsigned subtraction.
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;

    // Last driven just before the wrap.
    const uint32_t before = 0xFFFFFF00u;
    sb.begin(before);
    lamps.set(COLOR_YELLOW);
    sb.noteActivity(before);

    bool engaged = false;
    // Step from just before the wrap to just past before + grace, which wraps
    // through zero partway along.
    for (uint32_t dt = 0; dt <= STANDBY_GRACE_MS; dt += 100) {
      if (engageStep(&sb, &lamps, &display, before + dt, /*connected=*/false)) {
        engaged = true;
      }
    }
    check(engaged, "standby engages across the millis() rollover");
    check(lamps.current() == COLOR_OFF, "the lamp cleared across the wrap");
  }

  // noteActivity alone keeps a disconnected-but-serial-driven light awake: the
  // relay can drive the light over the cable while BLE never connects, and that
  // must not blank it. update() is called with connected=false throughout; the
  // steady stream of activity is the only thing holding standby off.
  {
    Lamps lamps;
    lamps.begin();
    Display display;
    display.begin();
    Standby sb;
    sb.begin(0);
    lamps.set(COLOR_GREEN);

    bool engaged = false;
    for (uint32_t t = 0; t <= STANDBY_GRACE_MS * 4; t += 250) {
      // A frame arrives every grace-window-minus-a-bit, as a live serial feed
      // would: enough to keep rearming the timer.
      if (t % (STANDBY_GRACE_MS - 500) == 0) {
        sb.noteActivity(t);
      }
      if (engageStep(&sb, &lamps, &display, t, /*connected=*/false)) {
        engaged = true;
      }
    }
    check(!engaged,
          "a serial-driven but BLE-disconnected light never enters standby");
    check(lamps.current() == COLOR_GREEN, "and keeps showing its colour");
  }
}

int main() {
  printf("stoplight firmware host checks\n\n");

  testFrameSizes();
  testOverflowDegradation();
  testUuidIdsDoNotCollide();
  testUnknownKeysSkipped();
  testTruncatedInputLeavesLampsAlone();
  testFiveSessionsTurnRed();
  testPinNeverHidesRed();
  testRolloverSafety();
  testDisplayBehaviour();
  testStringHandling();
  testButton();
  testBleLink();
  testStandby();

  printf("\n%d checks, %d failures\n", gChecks, gFailures);
  if (gFailures != 0) {
    printf("FAILED\n");
    return 1;
  }
  printf("OK\n");
  return 0;
}
