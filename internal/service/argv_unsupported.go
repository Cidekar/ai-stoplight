//go:build !darwin && !linux && !windows

package service

// generatedArgv reports that this platform writes no service definition, so
// there is no generated command line to check.
func generatedArgv(string) ([]string, bool) {
	return nil, false
}
