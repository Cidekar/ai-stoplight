//go:build linux

package service

import "strings"

// generatedArgv reads ExecStart back out of the unit this package writes and
// splits it the way systemd does.
func generatedArgv(binPath string) ([]string, bool) {
	s := &systemd{unitPath: "unit", logFile: "log"}

	for _, line := range strings.Split(string(s.unit(binPath)), "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		return splitExecStart(value), true
	}
	return nil, false
}

// splitExecStart splits a unit file command the way systemd does: on
// whitespace, honouring double quotes, and undoubling the percent that
// escapeUnitPath doubled.
//
// It is deliberately only as clever as the escaping in systemd.go. The point
// is to invert what this package writes, so that a path with a space or a
// percent comes back out as the single argument systemd would pass.
func splitExecStart(value string) []string {
	var (
		args    []string
		cur     strings.Builder
		inQuote bool
		started bool
	)
	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			started = true
		case c == '\\' && inQuote && i+1 < len(value):
			i++
			cur.WriteByte(value[i])
			started = true
		case c == '%' && i+1 < len(value) && value[i+1] == '%':
			// A doubled percent is one literal percent to systemd.
			i++
			cur.WriteByte('%')
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			flush()
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	flush()
	return args
}
