//go:build !darwin && !linux && !windows

package service

// newManager reports that this platform has no service manager.
//
// The other platform files each define newManager behind a GOOS tag. This
// file covers everything they do not, so that New fails at run time with a
// clear message rather than failing to compile. The rest of the binary is
// portable and worth building anyway.
func newManager() (Manager, error) {
	return nil, errUnsupported()
}
