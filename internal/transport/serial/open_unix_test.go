//go:build darwin || linux

package serial

// These tests pin the port-open contract that the field relies on: a CDC-ACM
// device must open as a non-controlling terminal in raw mode. A regression here
// is invisible on the desk and only shows up as a daemon that dies on unplug or
// a frame corrupted by an echoed boot banner, so the behaviour is asserted here
// rather than left to a code read.

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMakeRawClearsCookedModeFlags is the raw-mode half of issue #8. It starts
// from a fully cooked termios, the state a tty opens in, and checks that the
// flags which echo bytes, turn control characters into signals, let an XOFF
// halt output, or post-process output are all cleared.
func TestMakeRawClearsCookedModeFlags(t *testing.T) {
	// A cooked tty with every flag of interest set.
	term := &unix.Termios{
		Iflag: unix.ICRNL | unix.IXON | unix.BRKINT | unix.ISTRIP,
		Oflag: unix.OPOST,
		Lflag: unix.ECHO | unix.ICANON | unix.ISIG | unix.IEXTEN | unix.ECHONL,
		Cflag: unix.PARENB,
	}

	makeRaw(term)

	// The termios field width differs by platform (uint32 on linux, uint64 on
	// darwin), so compare as uint64 after widening both sides.
	cases := []struct {
		name string
		flag uint64
		got  uint64
	}{
		{"ECHO", uint64(unix.ECHO), uint64(term.Lflag)},
		{"ICANON", uint64(unix.ICANON), uint64(term.Lflag)},
		{"ISIG", uint64(unix.ISIG), uint64(term.Lflag)},
		{"IEXTEN", uint64(unix.IEXTEN), uint64(term.Lflag)},
		{"IXON", uint64(unix.IXON), uint64(term.Iflag)},
		{"ICRNL", uint64(unix.ICRNL), uint64(term.Iflag)},
		{"OPOST", uint64(unix.OPOST), uint64(term.Oflag)},
		{"PARENB", uint64(unix.PARENB), uint64(term.Cflag)},
	}
	for _, c := range cases {
		if c.got&c.flag != 0 {
			t.Errorf("%s is still set after makeRaw; port is not raw", c.name)
		}
	}

	// 8-bit characters, the one bit makeRaw sets rather than clears.
	if uint64(term.Cflag)&uint64(unix.CS8) == 0 {
		t.Error("CS8 is not set after makeRaw; characters are not 8-bit")
	}
}

// TestOpenDeviceClearsNonblock is the other half: the open is non-blocking so it
// returns at once, but the flag must be cleared before the port is used so Send
// does an ordinary blocking write. A regular file reaches the ENOTTY branch,
// which still runs the clear, so it exercises that path without hardware.
func TestOpenDeviceClearsNonblock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cu.usbmodemTEST")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake device: %v", err)
	}
	f.Close()

	rwc, err := openDevice(path)
	if err != nil {
		t.Fatalf("openDevice(%s): %v", path, err)
	}
	defer rwc.Close()

	fd := int(rwc.(interface{ Fd() uintptr }).Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatalf("F_GETFL: %v", err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Error("O_NONBLOCK is still set; the open flag was not cleared")
	}
}
