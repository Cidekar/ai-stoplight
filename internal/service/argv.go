package service

// GeneratedArgv returns the command line the platform's service definition
// starts, as an argv slice with binPath at element zero.
//
// This exists for one reason: nothing else spans the boundary between the
// definition this package writes and the dispatch in package main that has
// to parse it. Every test here checked that the generated plist or unit
// held the right text, and package main checked that its own commands
// worked, and in between the two a definition that named a subcommand the
// binary did not have shipped and crash-looped under KeepAlive.
//
// The argv is extracted from the generated definition rather than restated,
// so a change to the plist, the unit or the schtasks arguments moves this
// return value with it and the test in package main fails. A copy of the
// expected arguments here would agree with itself forever and prove nothing.
//
// The second return is false on a platform with no service manager, which
// generates no definition to check.
func GeneratedArgv(binPath string) ([]string, bool) {
	return generatedArgv(binPath)
}
