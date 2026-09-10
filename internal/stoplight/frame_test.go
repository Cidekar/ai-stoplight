package stoplight

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

// The exact wire form from RFC 1: colour is a string, never a number.
func TestFrameMarshalMatchesRFCWireForm(t *testing.T) {
	frame := Frame{
		Color: ColorRed,
		Sessions: []FrameSession{
			{ID: "a1", Label: "auth-api", State: "needs you", Color: ColorRed},
			{ID: "b2", Label: "stoplight", State: "working", Color: ColorYellow},
		},
	}
	got, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const want = `{"color":"red","sessions":[` +
		`{"id":"a1","label":"auth-api","state":"needs you","color":"red"},` +
		`{"id":"b2","label":"stoplight","state":"working","color":"yellow"}]}`
	if string(got) != want {
		t.Errorf("Marshal() =\n%s\nwant\n%s", got, want)
	}
}

// The firmware depends on "color" being the FIRST key in the frame, and this
// test is what makes that dependency loud instead of silent.
//
// encoding/json emits struct fields in declaration order, so the wire order is
// whatever order Frame declares. That is invisible from the Go side: reordering
// two fields changes nothing any Go test would notice. It is not invisible to
// the device.
//
// A frame longer than the firmware's SL_LINE_MAX cannot be parsed as a session
// list, because the tail is gone and a truncated list looks like the list
// simply ending. The aggregate colour still has to reach the lamps, so the
// firmware salvages it two ways: a forward parse of the prefix it kept, which
// is exact and works however far past the cap the line ran, and a scan of a
// small retained tail, which only works if the key and its value land in the
// last SL_TAIL_MAX bytes. Which one applies is decided entirely by whether
// "color" comes before or after the sessions array.
//
// Both paths work, so moving the field would not break the light today. It
// would silently downgrade every oversized frame from the strong guarantee to
// the weak one, and nothing on either side would say so. Assert the order
// here, where a struct reorder fails a test rather than quietly costing the
// lamp its margin. See firmware/stoplight/protocol.h, "KEY ORDER".
func TestFrameSerialisesColorFirst(t *testing.T) {
	frames := map[string]Frame{
		"with sessions": {
			Color: ColorRed,
			Sessions: []FrameSession{
				{ID: "a1", Label: "auth-api", State: "needs you", Color: ColorRed},
			},
		},
		"empty session list": {Color: ColorOff, Sessions: []FrameSession{}},
		"nil session list":   {Color: ColorGreen},
		"eight sessions":     manySessionFrame(8),
	}

	for name, frame := range frames {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}

			// The literal prefix, rather than a decode-and-compare: the
			// firmware reads bytes, so bytes are what must be asserted.
			const prefix = `{"color":`
			if !bytes.HasPrefix(encoded, []byte(prefix)) {
				t.Errorf("frame JSON must begin with %s, got:\n%s\n\n"+
					"The firmware's overflow salvage recovers the aggregate "+
					"from the prefix of an oversized line. Moving Color out "+
					"of first position in the Frame struct pushes it past the "+
					"truncation point and onto the weaker tail-scan path. "+
					"Put Color back at the top of the struct.",
					prefix, encoded)
			}

			// And no other top level key may precede it. Decoding into an
			// ordered list of keys catches a field that sorts before "color"
			// as well as one merely declared before it.
			keys, err := topLevelKeys(encoded)
			if err != nil {
				t.Fatalf("topLevelKeys() error = %v", err)
			}
			if len(keys) == 0 {
				t.Fatal("the frame encoded no keys at all")
			}
			if keys[0] != "color" {
				t.Errorf("first top level key = %q, want \"color\" (keys: %v)",
					keys[0], keys)
			}
		})
	}
}

// manySessionFrame builds a frame at the relay's eight-session cap, which is
// the largest the firmware is sized for and the case where the aggregate's
// position matters most.
func manySessionFrame(n int) Frame {
	frame := Frame{Color: ColorRed}
	for i := 0; i < n; i++ {
		frame.Sessions = append(frame.Sessions, FrameSession{
			ID:    fmt.Sprintf("3f2a1b7c-9d4e-4a10-b8c3-%012d", i),
			Label: "a-reasonably-long-branch-name",
			State: "needs you",
			Color: ColorRed,
		})
	}
	return frame
}

// topLevelKeys returns the object's keys in the order they appear on the wire.
// json.Unmarshal into a map loses that order, which is the whole property
// under test, so the stream is decoded token by token instead.
func topLevelKeys(encoded []byte) ([]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))

	opening, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("frame is not a JSON object, starts with %v", opening)
	}

	var keys []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string: %v", token)
		}
		keys = append(keys, key)

		// Step over the value, whatever shape it is, so the next token read
		// is the following key rather than part of this value.
		var discard json.RawMessage
		if err := decoder.Decode(&discard); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// An empty desk sends off with an empty array, not null: the firmware clears
// the screen on this frame.
func TestEmptyFrameMarshalsOffWithEmptyArray(t *testing.T) {
	got, err := json.Marshal(Frame{Color: ColorOff, Sessions: []FrameSession{}})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const want = `{"color":"off","sessions":[]}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}

func TestFrameUnmarshal(t *testing.T) {
	const wire = `{"color":"yellow","sessions":[{"id":"a1","label":"x","state":"working","color":"yellow"}]}`

	var got Frame
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Color != ColorYellow {
		t.Errorf("Color = %v, want yellow", got.Color)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("len(Sessions) = %d, want 1", len(got.Sessions))
	}
	want := FrameSession{ID: "a1", Label: "x", State: "working", Color: ColorYellow}
	if got.Sessions[0] != want {
		t.Errorf("Sessions[0] = %+v, want %+v", got.Sessions[0], want)
	}
}

// Forward compatibility: either side may add fields without breaking the other.
func TestFrameIgnoresUnknownFields(t *testing.T) {
	const wire = `{"color":"green","brightness":7,"sessions":[{"id":"a1","pinned":true}]}`

	var got Frame
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Color != ColorGreen {
		t.Errorf("Color = %v, want green", got.Color)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].ID != "a1" {
		t.Errorf("Sessions = %+v, want one session with id a1", got.Sessions)
	}
}

func TestFrameRoundTrip(t *testing.T) {
	frame := Frame{
		Color: ColorRed,
		Sessions: []FrameSession{
			{ID: "a1", Label: "auth", State: "needs you", Color: ColorRed},
		},
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded Frame
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !decoded.Equal(frame) {
		t.Errorf("round trip = %+v, want %+v", decoded, frame)
	}
}

func TestFrameEqual(t *testing.T) {
	base := Frame{
		Color:    ColorRed,
		Sessions: []FrameSession{{ID: "a1", Label: "auth", State: "needs you", Color: ColorRed}},
	}
	tests := []struct {
		name  string
		other Frame
		want  bool
	}{
		{
			name:  "identical",
			other: Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "a1", Label: "auth", State: "needs you", Color: ColorRed}}},
			want:  true,
		},
		{
			name:  "different aggregate",
			other: Frame{Color: ColorYellow, Sessions: []FrameSession{{ID: "a1", Label: "auth", State: "needs you", Color: ColorRed}}},
			want:  false,
		},
		{
			name:  "different label",
			other: Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "a1", Label: "other", State: "needs you", Color: ColorRed}}},
			want:  false,
		},
		{
			name:  "different session state",
			other: Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "a1", Label: "auth", State: "working", Color: ColorRed}}},
			want:  false,
		},
		{
			name:  "extra session",
			other: Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "a1", Label: "auth", State: "needs you", Color: ColorRed}, {ID: "b2"}}},
			want:  false,
		},
		{
			name:  "no sessions",
			other: Frame{Color: ColorRed, Sessions: []FrameSession{}},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := base.Equal(tt.other); got != tt.want {
				t.Errorf("Equal() = %v, want %v", got, tt.want)
			}
			if got := tt.other.Equal(base); got != tt.want {
				t.Errorf("Equal() is not symmetric for %s", tt.name)
			}
		})
	}
}

// Order matters: the rotation order is part of what the device shows.
func TestFrameEqualIsOrderSensitive(t *testing.T) {
	first := Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "a1"}, {ID: "b2"}}}
	second := Frame{Color: ColorRed, Sessions: []FrameSession{{ID: "b2"}, {ID: "a1"}}}
	if first.Equal(second) {
		t.Error("frames with reordered sessions must not compare equal")
	}
}

// A nil slice and an empty slice put the same thing on the device.
func TestFrameEqualTreatsNilAndEmptyAlike(t *testing.T) {
	nilSessions := Frame{Color: ColorOff}
	emptySessions := Frame{Color: ColorOff, Sessions: []FrameSession{}}
	if !nilSessions.Equal(emptySessions) {
		t.Error("a nil session slice must equal an empty one")
	}
}
