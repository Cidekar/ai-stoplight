package stoplight

// Frame is the complete display state at a moment. One is sent to the light
// per change, carrying the whole set rather than a delta, so a dropped frame
// costs nothing once the next one arrives.
type Frame struct {
	Color    Color          `json:"color"` // aggregate, drives the lamps
	Sessions []FrameSession `json:"sessions"`
}

// FrameSession is one entry in the rotation.
type FrameSession struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	State string `json:"state"`
	Color Color  `json:"color"`
}

// Equal reports whether two frames would put the same thing on the device.
// The relay transmits only on change, so this is what decides whether the
// radio wakes up.
func (f Frame) Equal(other Frame) bool {
	if f.Color != other.Color || len(f.Sessions) != len(other.Sessions) {
		return false
	}
	for i := range f.Sessions {
		if f.Sessions[i] != other.Sessions[i] {
			return false
		}
	}
	return true
}
