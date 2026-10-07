//go:build !darwin && !linux

package serial

import (
	"io"
	"os"
)

// openDevice is the fallback for platforms without termios. There is no POSIX
// tty to put into raw mode and no controlling-terminal hazard to avoid, so the
// device is opened as an ordinary file. Discover returns nothing on these
// platforms anyway, so this path is reached only by a test that names a device
// directly.
func openDevice(devicePath string) (io.ReadWriteCloser, error) {
	return os.OpenFile(devicePath, os.O_RDWR, 0)
}
