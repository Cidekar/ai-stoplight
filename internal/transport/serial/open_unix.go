//go:build darwin || linux

package serial

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// openDevice opens a CDC-ACM device for raw, non-controlling use.
//
// Three things have to be true for the port to behave, and the default open
// gives none of them.
//
// O_NOCTTY: a process that is a session leader with no controlling terminal
// (every systemd user service, and launchd agents in practice) otherwise
// adopts the first tty it opens as its controlling terminal. The port becomes
// that terminal. Unplug the cable and the kernel hangup sends the whole
// process group a SIGHUP, which the relay does not catch, so the daemon dies
// and takes its in-memory session state with it. O_NOCTTY says "this is a data
// line, not my console".
//
// Raw mode: a tty opens cooked, with line discipline on. ECHO sends the
// device's own bytes back out the port, so the ESP32-C3 ROM boot banner lands
// in the middle of an outgoing frame. ISIG turns a 0x03 from the device into a
// SIGINT and 0x13 (XOFF, IXON) halts host output until an XON arrives, which is
// one of the ways a write wedges forever. cfmakeraw clears all of it so the
// port carries bytes and nothing else.
//
// O_NONBLOCK on the open, cleared afterwards: opening a cu device does not
// block on carrier the way a tty device does, but opening non-blocking is the
// portable guarantee that the open itself returns at once. The flag is cleared
// before the port is used so writes behave normally; the relay guards against a
// wedged write by never holding its mutex across one, not by polling the fd.
func openDevice(devicePath string) (io.ReadWriteCloser, error) {
	f, err := os.OpenFile(devicePath, os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}

	fd := int(f.Fd())

	term, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) {
			// The path resolved to something that is not a terminal. A real
			// CDC-ACM device is always a tty, so in the field this never fires;
			// it is reached only when a test points New at a regular file.
			// Nothing to configure, so hand back the open file as-is.
			if cerr := unix.SetNonblock(fd, false); cerr != nil {
				f.Close()
				return nil, fmt.Errorf("clear O_NONBLOCK: %w", cerr)
			}
			return f, nil
		}
		f.Close()
		return nil, fmt.Errorf("read termios: %w", err)
	}

	makeRaw(term)

	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, term); err != nil {
		f.Close()
		return nil, fmt.Errorf("set raw termios: %w", err)
	}

	// Restore blocking I/O now that the port is configured. The open was
	// non-blocking only to guarantee the open returned; writes from here on are
	// ordinary blocking writes.
	if err := unix.SetNonblock(fd, false); err != nil {
		f.Close()
		return nil, fmt.Errorf("clear O_NONBLOCK: %w", err)
	}

	return f, nil
}

// makeRaw clears the line-discipline flags, the cfmakeraw transform written
// out because the x/sys package has no cfmakeraw helper. The masks are the ones
// the C library uses: no echo, no canonical input, no signal generation, no
// output post-processing, and 8-bit characters with no parity. It is a
// standalone function so the flag arithmetic can be tested without a tty.
func makeRaw(term *unix.Termios) {
	term.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	term.Oflag &^= unix.OPOST
	term.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	term.Cflag &^= unix.CSIZE | unix.PARENB
	term.Cflag |= unix.CS8
}
