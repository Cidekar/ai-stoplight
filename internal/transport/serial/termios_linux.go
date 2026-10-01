//go:build linux

package serial

import "golang.org/x/sys/unix"

// The termios get/set ioctl requests are spelled differently per platform.
// Linux uses the System V TCGETS/TCSETS pair.
const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
