//go:build windows

package service

import "strings"

// generatedArgv reads the /TR value back out of the schtasks arguments this
// package builds and splits it the way a command line is split.
func generatedArgv(binPath string) ([]string, bool) {
	args := createArgs(binPath)
	for i, a := range args {
		if a == "/TR" && i+1 < len(args) {
			return splitCommandLine(args[i+1]), true
		}
	}
	return nil, false
}

// splitCommandLine splits a Windows command line on whitespace, honouring
// double quotes. createArgs quotes the binary path and nothing else, so this
// only has to invert that.
func splitCommandLine(value string) []string {
	var (
		out     []string
		cur     strings.Builder
		inQuote bool
		started bool
	)
	flush := func() {
		if started {
			out = append(out, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '"':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return out
}
