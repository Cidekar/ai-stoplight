// protocol.cpp - a hand-rolled parser for the relay frame format.
//
// WHY NOT ArduinoJson: the frame is a flat object with four scalar keys and
// one array of flat objects with four scalar keys. There is no nesting to
// speak of, no numbers to convert, and no need to re-serialise. A recursive
// descent scanner for that shape is about 200 lines and costs no dependency,
// no library version to pin, and no allocator. The device must survive a
// truncated line arriving mid-transmission, and it is easier to be sure of
// that in code we can read end to end.
//
// The parser is deliberately permissive. It skips over any value it does not
// recognise, at any depth, so a future relay can add fields freely. It never
// writes past a buffer: every string copy is bounded and over-long values are
// truncated rather than refused.

#include "protocol.h"

#include <string.h>

namespace {

// skipWhitespace advances p past JSON insignificant whitespace.
void skipWhitespace(const char** p) {
  while (**p == ' ' || **p == '\t' || **p == '\r' || **p == '\n') {
    (*p)++;
  }
}

// parseString reads a JSON string literal at *p into out, which holds at
// most cap characters plus a terminator. Over-long strings are truncated:
// a 200 character label is a relay bug, not a reason to drop a red light.
//
// Returns false only if the token is not a well formed string, which means
// the line is malformed and the caller should stop.
bool parseString(const char** p, char* out, size_t cap) {
  skipWhitespace(p);
  if (**p != '"') {
    return false;
  }
  (*p)++;  // opening quote

  size_t n = 0;
  while (**p != '\0' && **p != '"') {
    char c = **p;

    if (c == '\\') {
      (*p)++;
      char e = **p;
      if (e == '\0') {
        return false;  // truncated mid escape
      }
      switch (e) {
        case 'n': c = '\n'; break;
        case 't': c = '\t'; break;
        case 'r': c = '\r'; break;
        case 'b': c = '\b'; break;
        case 'f': c = '\f'; break;
        case '"': c = '"'; break;
        case '\\': c = '\\'; break;
        case '/': c = '/'; break;
        case 'u': {
          // \uXXXX. The screen font is ASCII, so anything outside it becomes
          // '?' rather than mangled bytes. Four hex digits must follow.
          //
          // BOUNDS FIRST, EXPLICITLY. The loop below reads *p + 1 + i for i up
          // to 3, which is four bytes past the 'u'. That is only in bounds
          // while the digits are really there. The loop's own hex test does
          // reject the NUL and stops, so today nothing is read past the
          // terminator, but that safety is a side effect of NUL not being a
          // hex digit rather than a check anyone wrote on purpose. One edit to
          // the classification below and it becomes an overread. Verify the
          // four bytes exist here, so the guard is stated rather than implied.
          {
            const char* q = *p + 1;
            for (int i = 0; i < 4; i++) {
              if (q[i] == '\0') {
                return false;  // truncated mid escape
              }
            }
          }
          uint16_t cp = 0;
          for (int i = 0; i < 4; i++) {
            char h = *(*p + 1 + i);
            uint8_t v;
            if (h >= '0' && h <= '9') {
              v = (uint8_t)(h - '0');
            } else if (h >= 'a' && h <= 'f') {
              v = (uint8_t)(h - 'a' + 10);
            } else if (h >= 'A' && h <= 'F') {
              v = (uint8_t)(h - 'A' + 10);
            } else {
              return false;  // truncated or invalid escape
            }
            cp = (uint16_t)((cp << 4) | v);
          }
          *p += 4;  // the loop's own increment consumes the last digit
          c = (cp >= 0x20 && cp < 0x7F) ? (char)cp : '?';
          break;
        }
        default:
          c = e;  // unknown escape: take the character literally
          break;
      }
    } else if ((uint8_t)c < 0x20) {
      // Raw control character inside a string. Replace rather than reject:
      // it cannot render, but it is not worth losing the frame over.
      c = '?';
    } else if ((uint8_t)c > 0x7E) {
      // Non-ASCII byte, most likely part of a UTF-8 sequence. The 5x7 font
      // has no glyph for it, so collapse it to one '?' rather than emitting
      // several. Skip the continuation bytes of the sequence.
      c = '?';
      while ((uint8_t)*(*p + 1) >= 0x80 && (uint8_t)*(*p + 1) <= 0xBF) {
        (*p)++;
      }
    }

    if (n < cap) {
      out[n++] = c;
    }
    // Past cap the character is consumed but discarded: truncate, do not
    // overflow, and keep scanning so the closing quote is still found.
    (*p)++;
  }

  if (**p != '"') {
    return false;  // ran off the end of the line before the closing quote
  }
  (*p)++;  // closing quote

  out[n] = '\0';
  return true;
}

// skipValue advances p past exactly one JSON value of any type, including
// nested objects and arrays. This is what makes unknown fields free: we do
// not need to understand a value in order to step over it.
//
// depth guards against a pathological input of nested brackets exhausting
// the stack. Beyond the limit we refuse, which drops one bad frame rather
// than resetting the board.
bool skipValue(const char** p, uint8_t depth) {
  if (depth > 16) {
    return false;
  }
  skipWhitespace(p);

  char c = **p;

  if (c == '"') {
    // Walk the string without storing it, honouring escapes so that a
    // quote inside the value does not look like the end of it.
    (*p)++;
    while (**p != '\0' && **p != '"') {
      if (**p == '\\' && *(*p + 1) != '\0') {
        (*p)++;
      }
      (*p)++;
    }
    if (**p != '"') {
      return false;
    }
    (*p)++;
    return true;
  }

  if (c == '{' || c == '[') {
    char close = (c == '{') ? '}' : ']';
    (*p)++;
    skipWhitespace(p);
    if (**p == close) {
      (*p)++;
      return true;  // empty object or array
    }
    for (;;) {
      skipWhitespace(p);
      if (c == '{') {
        // Inside an object, step over "key": before the value.
        if (**p != '"') {
          return false;
        }
        (*p)++;
        while (**p != '\0' && **p != '"') {
          if (**p == '\\' && *(*p + 1) != '\0') {
            (*p)++;
          }
          (*p)++;
        }
        if (**p != '"') {
          return false;
        }
        (*p)++;
        skipWhitespace(p);
        if (**p != ':') {
          return false;
        }
        (*p)++;
      }
      if (!skipValue(p, (uint8_t)(depth + 1))) {
        return false;
      }
      skipWhitespace(p);
      if (**p == ',') {
        (*p)++;
        continue;
      }
      if (**p == close) {
        (*p)++;
        return true;
      }
      return false;  // neither a separator nor a terminator
    }
  }

  // A bare literal: number, true, false, null. We never need the value, so
  // consume characters until something that can only be a delimiter.
  if (c == '\0') {
    return false;
  }
  while (**p != '\0' && **p != ',' && **p != '}' && **p != ']' &&
         **p != ' ' && **p != '\t' && **p != '\r' && **p != '\n') {
    (*p)++;
  }
  return true;
}

// recoverToNextTopLevelKey advances p past a value skipValue refused, to the
// comma that separates it from the next top level key.
//
// p MUST point at the START of the refused value, not at wherever skipValue
// stopped. skipValue consumes as it descends, so on failure it has left the
// cursor somewhere inside the nesting at an unknown depth, and a scan from
// there cannot tell a closing bracket of the refused value from the frame's
// own. Callers therefore save the cursor before calling skipValue and pass
// the saved one here.
//
// WHY THIS EXISTS: skipValue refuses a value nested deeper than its guard, so
// a single over-nested value under a key the firmware does not even read used
// to abandon every key after it. A frame like
//
//   {"x":[[[ ...1000 deep... ]]],"color":"red"}
//
// therefore dropped the aggregate and the red lamp with it. A malformed value
// under an unknown key must cost that key and nothing else.
//
// The scan is string aware, so a comma or a bracket inside a string literal
// is not mistaken for structure, and it counts brackets so that a comma
// inside the refused value is not mistaken for the top level separator. It
// stops at the top level ',' (leaving p on it, for the caller's own comma
// handling) or at the top level '}' or the end of the line, returning false
// in those two cases because there is no next key to read.
bool recoverToNextTopLevelKey(const char** p) {
  // Depth 0 is the frame object itself: the caller is between its keys.
  int32_t depth = 0;

  for (;;) {
    char c = **p;

    if (c == '\0') {
      return false;  // truncated: nothing further to read
    }

    if (c == '"') {
      // Step over the whole string literal, honouring escapes, so that a
      // brace or comma inside it is never counted as structure.
      (*p)++;
      while (**p != '\0' && **p != '"') {
        if (**p == '\\' && *(*p + 1) != '\0') {
          (*p)++;
        }
        (*p)++;
      }
      if (**p == '\0') {
        return false;  // unterminated string: the line is cut
      }
      (*p)++;  // closing quote
      continue;
    }

    if (c == '{' || c == '[') {
      depth++;
      (*p)++;
      continue;
    }

    if (c == '}' || c == ']') {
      if (depth == 0) {
        return false;  // the frame closed; there is no next key
      }
      depth--;
      (*p)++;
      continue;
    }

    if (c == ',' && depth == 0) {
      return true;  // the separator before the next top level key
    }

    (*p)++;
  }
}

// parseSession reads one object from the sessions array. Missing keys leave
// their fields empty, which is legal: every field is optional.
bool parseSession(const char** p, Session* s) {
  s->id[0] = '\0';
  s->label[0] = '\0';
  s->state[0] = '\0';
  s->color = COLOR_OFF;

  skipWhitespace(p);
  if (**p != '{') {
    return false;
  }
  (*p)++;

  skipWhitespace(p);
  if (**p == '}') {
    (*p)++;
    return true;  // {} is a valid, if useless, session
  }

  for (;;) {
    char key[24];
    if (!parseString(p, key, sizeof(key) - 1)) {
      return false;
    }
    skipWhitespace(p);
    if (**p != ':') {
      return false;
    }
    (*p)++;

    if (strcmp(key, "id") == 0) {
      if (!parseString(p, s->id, SL_MAX_ID)) {
        return false;
      }
    } else if (strcmp(key, "label") == 0) {
      if (!parseString(p, s->label, SL_MAX_LABEL)) {
        return false;
      }
    } else if (strcmp(key, "state") == 0) {
      if (!parseString(p, s->state, SL_MAX_STATE)) {
        return false;
      }
    } else if (strcmp(key, "color") == 0) {
      char c[12];
      if (!parseString(p, c, sizeof(c) - 1)) {
        return false;
      }
      Color parsed;
      if (parseColor(c, &parsed)) {
        s->color = parsed;
      }
      // An unrecognised colour name leaves COLOR_OFF rather than failing.
    } else {
      // Unknown key. Step over whatever it holds and carry on.
      if (!skipValue(p, 0)) {
        return false;
      }
    }

    skipWhitespace(p);
    if (**p == ',') {
      (*p)++;
      continue;
    }
    if (**p == '}') {
      (*p)++;
      return true;
    }
    return false;
  }
}

}  // namespace

bool parseColor(const char* s, Color* out) {
  if (s == nullptr) {
    return false;
  }
  if (strcmp(s, "off") == 0) {
    *out = COLOR_OFF;
    return true;
  }
  if (strcmp(s, "green") == 0) {
    *out = COLOR_GREEN;
    return true;
  }
  if (strcmp(s, "yellow") == 0) {
    *out = COLOR_YELLOW;
    return true;
  }
  if (strcmp(s, "red") == 0) {
    *out = COLOR_RED;
    return true;
  }
  return false;
}

bool parseFrame(const char* line, Frame* out) {
  if (line == nullptr || out == nullptr) {
    return false;
  }

  out->color = COLOR_OFF;
  out->hasColor = false;
  out->hasSessions = false;
  out->count = 0;

  const char* p = line;
  skipWhitespace(&p);
  if (*p != '{') {
    return false;  // not an object: nothing here to act on
  }
  p++;

  skipWhitespace(&p);
  if (*p == '}') {
    return true;  // {} parses fine and instructs nothing
  }

  for (;;) {
    char key[24];
    if (!parseString(&p, key, sizeof(key) - 1)) {
      break;  // malformed from here on; keep whatever we already read
    }
    skipWhitespace(&p);
    if (*p != ':') {
      break;
    }
    p++;

    if (strcmp(key, "color") == 0) {
      char c[12];
      if (!parseString(&p, c, sizeof(c) - 1)) {
        break;
      }
      Color parsed;
      if (parseColor(c, &parsed)) {
        out->color = parsed;
        out->hasColor = true;
      }
      // Unknown aggregate name: leave hasColor false so the caller keeps
      // the lamps where they are rather than guessing.
    } else if (strcmp(key, "sessions") == 0) {
      skipWhitespace(&p);
      if (*p != '[') {
        // Wrong type for a key we know. Skip it and keep going, and if it is
        // unskippable recover to the next key rather than losing the rest of
        // the frame, exactly as for an unknown key.
        const char* valueStart = p;
        if (!skipValue(&p, 0)) {
          p = valueStart;
          if (!recoverToNextTopLevelKey(&p)) {
            break;
          }
          p++;
          continue;
        }
      } else {
        p++;
        // hasSessions is committed where the array CLOSES, never here.
        //
        // It used to be set on the opening bracket, "present, even if it
        // turns out empty". That was the permanently blank screen. A frame
        // truncated mid-array left hasSessions true with count still 0, and
        // parseFrame returned true anyway because a partial frame is
        // deliberately salvageable for the lamps. applyFrame then read
        // "sessions present, zero of them" as the relay reporting that every
        // session had ended, and cleared the panel. The lamps stayed correct
        // throughout, which is exactly the reported signature: right colour,
        // dead screen, and nothing redraws it until a whole frame arrives.
        //
        // An unterminated list is missing information, not an instruction to
        // clear. Leaving hasSessions false makes applyFrame treat the frame
        // as colour-only: the lamps still move and the screen keeps the last
        // list it was sure about.
        skipWhitespace(&p);
        if (*p == ']') {
          p++;
          out->hasSessions = true;  // a real, complete, empty list
        } else {
          for (;;) {
            if (out->count < SL_MAX_SESSIONS) {
              if (!parseSession(&p, &out->sessions[out->count])) {
                goto done;
              }
              // An entry with no id cannot be pinned or tracked across
              // frames, so it is not worth a rotation slot.
              //
              // Nor can an entry whose id we already hold. Ids are the key
              // for the pin and for preserving a rotation slot, and both use
              // the FIRST match, so a duplicate takes a slot that nothing can
              // ever address: a pin on the second anchors to the first, and
              // the did-this-turn-red check reads the first's colour. This is
              // the same failure the SL_MAX_ID cap fixed, arriving from the
              // relay instead of from truncation. Keep the first, drop the
              // rest, rather than showing a session that cannot be selected.
              const char* id = out->sessions[out->count].id;
              bool duplicate = false;
              for (uint8_t k = 0; k < out->count; k++) {
                if (strcmp(out->sessions[k].id, id) == 0) {
                  duplicate = true;
                  break;
                }
              }
              if (id[0] != '\0' && !duplicate) {
                out->count++;
              }
            } else {
              // Over the cap. Consume the rest so the array still closes
              // cleanly and any keys after it are still parsed.
              if (!skipValue(&p, 0)) {
                goto done;
              }
            }
            skipWhitespace(&p);
            if (*p == ',') {
              p++;
              continue;
            }
            if (*p == ']') {
              p++;
              // The list closed. Only now is it safe to tell the display
              // that this frame carries a complete session list.
              out->hasSessions = true;
              break;
            }
            goto done;  // malformed array, hasSessions stays false
          }
        }
      }
    } else {
      // Save the cursor: on failure skipValue has descended to an unknown
      // depth, and the recovery has to restart from the top of the value.
      const char* valueStart = p;
      if (!skipValue(&p, 0)) {
        // An unknown key whose value skipValue refused: over-nested, or
        // malformed in some way we do not need to understand. Stepping to the
        // next top level key rather than giving up is the difference between
        // losing one field we never read and losing the aggregate behind it.
        p = valueStart;
        if (!recoverToNextTopLevelKey(&p)) {
          break;  // no next key: the frame closed or the line was cut
        }
        // p is on the separating comma. Consume it and read the next key.
        p++;
        continue;
      }
    }

    skipWhitespace(&p);
    if (*p == ',') {
      p++;
      continue;
    }
    break;  // '}' or truncation; either way we are finished
  }

done:
  // Always true if we saw an opening brace. A partially parsed frame still
  // carries useful state, and dropping it would mean dropping a red lamp
  // because a field we do not even read was malformed.
  return true;
}

bool findAggregateColor(const char* prefix, Color* out) {
  if (prefix == nullptr || out == nullptr) {
    return false;
  }

  const char* p = prefix;
  skipWhitespace(&p);
  if (*p != '{') {
    return false;
  }
  p++;

  // Walk top level keys only. Nested objects and arrays are stepped over by
  // skipValue, so a per-session "color" is never mistaken for the aggregate.
  for (;;) {
    char key[24];
    if (!parseString(&p, key, sizeof(key) - 1)) {
      return false;  // truncated mid key: nothing more to recover
    }
    skipWhitespace(&p);
    if (*p != ':') {
      return false;
    }
    p++;

    if (strcmp(key, "color") == 0) {
      char c[12];
      if (!parseString(&p, c, sizeof(c) - 1)) {
        return false;  // the value itself was cut off
      }
      return parseColor(c, out);
    }

    const char* valueStart = p;
    if (!skipValue(&p, 0)) {
      // Either the truncation, or a value too deeply nested for skipValue.
      // The second is recoverable: step to the next top level key, because a
      // "color" after an over-nested field is still the aggregate and still
      // has to reach the lamps. Restart from the top of the value, since
      // skipValue left the cursor at an unknown depth inside it.
      p = valueStart;
      if (!recoverToNextTopLevelKey(&p)) {
        return false;  // genuinely the end of what we kept
      }
      p++;  // the separating comma
      continue;
    }

    skipWhitespace(&p);
    if (*p == ',') {
      p++;
      continue;
    }
    return false;  // '}' or truncation, with no colour seen
  }
}

bool findTailAggregateColor(const char* tail, Color* out) {
  if (tail == nullptr || out == nullptr) {
    return false;
  }

  // The tail starts mid-frame at an unknown depth, so the forward parse used
  // for the prefix cannot be reused: there is no way to tell from the tail
  // alone whether a given brace opened a session or the frame. What CAN be
  // decided locally is whether a "color" pair is the LAST thing in the frame,
  // because a top level key is followed only by the frame's own closing brace.
  //
  // So: find the last "color":"<name>" in the tail, then walk from the end of
  // its value to the end of the line. Exactly one '}' and no other structural
  // character means the pair sat at the top level. A ']' or a second '}'
  // means it was inside the sessions array and belongs to one session, and it
  // must not be mistaken for the aggregate.
  const size_t n = strlen(tail);
  static const char kKey[] = "\"color\"";
  const size_t keyLen = sizeof(kKey) - 1;

  if (n < keyLen) {
    return false;
  }

  // Search backwards so a per-session "color" earlier in the tail never wins
  // over the trailing aggregate.
  for (size_t start = n - keyLen + 1; start-- > 0;) {
    if (memcmp(tail + start, kKey, keyLen) != 0) {
      continue;
    }

    // A key is preceded by '{' or ',' once whitespace is dropped. Requiring
    // that stops a match inside some other string's contents.
    size_t before = start;
    while (before > 0 && (tail[before - 1] == ' ' || tail[before - 1] == '\t' ||
                          tail[before - 1] == '\r' || tail[before - 1] == '\n')) {
      before--;
    }
    if (before == 0 || (tail[before - 1] != '{' && tail[before - 1] != ',')) {
      continue;
    }

    const char* p = tail + start + keyLen;
    skipWhitespace(&p);
    if (*p != ':') {
      continue;
    }
    p++;

    char name[12];
    if (!parseString(&p, name, sizeof(name) - 1)) {
      continue;  // the value ran off the end of the line
    }

    // Everything after the value must be the frame's closing brace and
    // nothing else. This is the whole of the top level test.
    skipWhitespace(&p);
    if (*p != '}') {
      continue;
    }
    p++;
    skipWhitespace(&p);
    if (*p != '\0') {
      continue;  // more structure follows, so the pair was nested
    }

    return parseColor(name, out);
  }

  return false;
}

LineReader::LineReader()
    : len_(0),
      overflow_(false),
      tailLen_(0),
      tailHead_(0),
      haveOverflowColor_(false),
      overflowColor_(COLOR_OFF) {
  buf_[0] = '\0';
  tail_[0] = '\0';
}

void LineReader::tailAppend(char c) {
  tail_[tailHead_] = c;
  tailHead_ = (uint8_t)((tailHead_ + 1) % SL_TAIL_MAX);
  if (tailLen_ < SL_TAIL_MAX) {
    tailLen_++;
  }
}

void LineReader::tailLinearise(char* out) const {
  // The oldest byte is tailLen_ places behind the head once the ring has
  // filled, and at index 0 before that.
  uint8_t first = (uint8_t)((tailHead_ + SL_TAIL_MAX - tailLen_) % SL_TAIL_MAX);
  for (uint8_t i = 0; i < tailLen_; i++) {
    out[i] = tail_[(uint8_t)((first + i) % SL_TAIL_MAX)];
  }
  out[tailLen_] = '\0';
}

void LineReader::reset() {
  len_ = 0;
  overflow_ = false;
  buf_[0] = '\0';
  tailLen_ = 0;
  tailHead_ = 0;
  // haveOverflowColor_ is deliberately not cleared here. reset() runs from
  // feed() at the moment an overflowing line terminates, which is precisely
  // when the salvaged colour must survive for the caller to read.
}

bool LineReader::feed(char c) {
  // The salvaged colour is valid for exactly one call, so clear it as soon
  // as the caller has had its chance to read it.
  haveOverflowColor_ = false;

  if (c == '\n' || c == '\r') {
    // Terminator. Suppress empty lines and anything we already gave up on,
    // so a CRLF sender does not produce a phantom blank frame.
    if (overflow_) {
      // The session list is beyond recovery, but the aggregate may still be
      // in reach. Recovering it is the difference between a stale green and
      // a correct red.
      //
      // The prefix is tried first: it is an exact forward parse, and it wins
      // whenever "color" came before the sessions array, which is the order
      // the relay emits. The tail is the fallback for the other order, where
      // the key sits past the truncation point entirely. Neither is allowed
      // to be the only path, because then the lamp would depend on the order
      // of fields in a Go struct.
      buf_[len_] = '\0';
      Color recovered;
      if (findAggregateColor(buf_, &recovered)) {
        overflowColor_ = recovered;
        haveOverflowColor_ = true;
      } else {
        char tail[SL_TAIL_MAX + 1];
        tailLinearise(tail);
        if (findTailAggregateColor(tail, &recovered)) {
          overflowColor_ = recovered;
          haveOverflowColor_ = true;
        }
      }
      reset();
      return false;
    }
    if (len_ == 0) {
      reset();
      return false;
    }
    buf_[len_] = '\0';
    len_ = 0;  // buf_ stays intact for line() until the next feed
    return true;
  }

  if (overflow_) {
    // Still discarding until the newline that ends this line, but the last
    // SL_TAIL_MAX bytes are kept: a "color" key after the sessions array
    // lives here and nowhere else.
    tailAppend(c);
    return false;
  }

  if (len_ >= SL_LINE_MAX) {
    // A line longer than any legitimate frame. The session list cannot be
    // trusted, because a truncated list looks like sessions ending, so the
    // line is not delivered. The aggregate is salvaged at the terminator
    // above rather than lost with it.
    overflow_ = true;
    tailAppend(c);
    return false;
  }

  buf_[len_++] = c;
  return false;
}
