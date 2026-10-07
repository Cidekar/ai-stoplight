//go:build darwin

package serial

import "golang.org/x/sys/unix"

// The termios get/set ioctl requests are spelled differently per platform.
// Darwin uses the BSD TIOCGETA/TIOCSETA pair.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)
