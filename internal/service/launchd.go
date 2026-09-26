//go:build darwin

package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// launchdLabel is the reverse-DNS identifier launchd knows the job by. It
// is also the plist filename and the argument to launchctl.
const launchdLabel = "com.stoplight.relay"

// launchd manages the relay as a launchd user agent.
//
// User agents live in ~/Library/LaunchAgents and run in the logged-in
// GUI session, which is what gives the relay access to Bluetooth.
type launchd struct {
	// plistPath is the agent definition. Tests redirect it.
	plistPath string
	// logFile is where stdout and stderr are sent.
	logFile string
	// uid identifies the launchd domain, as in "gui/501".
	uid int
	// run executes a command. Tests replace it so no real launchctl is
	// invoked.
	run func(name string, args ...string) ([]byte, error)
}

// newManager returns the launchd manager for the current user.
func newManager() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory: %w", err)
	}
	log, err := logPath()
	if err != nil {
		return nil, err
	}
	return &launchd{
		plistPath: filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
		logFile:   log,
		uid:       os.Getuid(),
		run:       runCommand,
	}, nil
}

// runCommand executes a command and returns its combined output. Callers
// need the output because launchctl reports failures on stderr with a
// zero exit status in some versions.
func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// domain is the launchd domain target for this user, such as "gui/501".
func (l *launchd) domain() string {
	return "gui/" + strconv.Itoa(l.uid)
}

// target is the service target, such as "gui/501/com.stoplight.relay".
func (l *launchd) target() string {
	return l.domain() + "/" + launchdLabel
}

// Command implements Manager.
func (l *launchd) Command() string {
	return "launchctl bootstrap " + l.domain() + " " + l.plistPath
}

// plist renders the agent definition.
//
// RunAtLoad starts the relay at login. KeepAlive restarts it if it dies.
// Together they are the whole reason this package exists: a light that
// needs a terminal open is a light you forget to start.
func (l *launchd) plist(binPath string, relayArgs ...string) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n")
	// An XML comment, so that Install and Uninstall can tell this file
	// from a plist at the same path that somebody else wrote.
	b.WriteString("<!-- " + serviceMarker + " -->\n")
	b.WriteString("<dict>\n")
	b.WriteString("\t<key>Label</key>\n\t<string>" + escapeXML(launchdLabel) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range append([]string{binPath, "relay"}, relayArgs...) {
		b.WriteString("\t\t<string>" + escapeXML(arg) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n\t<string>" + escapeXML(l.logFile) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n\t<string>" + escapeXML(l.logFile) + "</string>\n")
	// Without this the agent inherits a minimal PATH and cannot find
	// helpers such as git, which the relay uses to derive a label.
	b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	b.WriteString("\t\t<key>PATH</key>\n\t\t<string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n")
	b.WriteString("\t</dict>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.Bytes()
}

// escapeXML escapes text for an XML character node. A home directory can
// contain an ampersand, which would otherwise produce an invalid plist
// that launchd silently refuses to load.
func escapeXML(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Install writes the plist and loads it. It is idempotent: an already
// loaded agent is booted out first so the new definition takes effect.
func (l *launchd) Install(binPath string, relayArgs ...string) error {
	// Refuse a plist we did not write, before anything is changed. A
	// definition at this path may belong to another tool, and overwriting
	// it would silently take over its job.
	if err := checkOwned(l.plistPath); err != nil {
		return err
	}
	if err := l.ensureLogDir(); err != nil {
		return err
	}
	if err := writeFileAtomic(l.plistPath, l.plist(binPath, relayArgs...), 0o644); err != nil {
		return err
	}
	// Ignore the error: the agent is usually not loaded yet, and an
	// unload failure here is not a reason to fail the install.
	_ = l.bootout()
	return l.bootstrap()
}

// ensureLogDir creates the log directory, because launchd will not create
// it and refuses to start a job whose StandardOutPath is unwritable.
func (l *launchd) ensureLogDir() error {
	dir := filepath.Dir(l.logFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}

// bootstrap loads the agent, preferring the modern subcommand and falling
// back to `load` on older systems where bootstrap is unavailable.
func (l *launchd) bootstrap() error {
	out, err := l.run("launchctl", "bootstrap", l.domain(), l.plistPath)
	if err == nil {
		return nil
	}
	// "already bootstrapped" means the desired state is met.
	if isAlreadyLoaded(out) {
		return nil
	}
	if legacyOut, legacyErr := l.run("launchctl", "load", "-w", l.plistPath); legacyErr == nil {
		return nil
	} else if isAlreadyLoaded(legacyOut) {
		return nil
	}
	return fmt.Errorf("launchctl bootstrap %s: %w: %s", l.plistPath, err, strings.TrimSpace(string(out)))
}

// bootout unloads the agent, with the same modern-then-legacy fallback.
func (l *launchd) bootout() error {
	out, err := l.run("launchctl", "bootout", l.target())
	if err == nil || isNotLoaded(out) {
		return nil
	}
	if legacyOut, legacyErr := l.run("launchctl", "unload", "-w", l.plistPath); legacyErr == nil {
		return nil
	} else if isNotLoaded(legacyOut) {
		return nil
	}
	return fmt.Errorf("launchctl bootout %s: %w: %s", l.target(), err, strings.TrimSpace(string(out)))
}

// isAlreadyLoaded recognises launchctl's several ways of saying the job
// is present. Matching on text is unpleasant, but launchctl returns the
// same generic exit status for most failures.
func isAlreadyLoaded(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "already bootstrapped") ||
		strings.Contains(s, "service already loaded") ||
		strings.Contains(s, "already loaded")
}

// isNotLoaded recognises the messages meaning there was nothing to unload.
func isNotLoaded(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "no such process") ||
		strings.Contains(s, "could not find specified service") ||
		strings.Contains(s, "not find") ||
		strings.Contains(s, "not loaded")
}

// Uninstall unloads the agent and deletes the plist.
//
// A plist without our marker is left alone and reported, because deleting
// another tool's job definition is not something the user can undo.
func (l *launchd) Uninstall() error {
	if err := checkOwned(l.plistPath); err != nil {
		return err
	}
	_ = l.bootout()
	if err := os.Remove(l.plistPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", l.plistPath, err)
	}
	return nil
}

// Start launches the relay now.
func (l *launchd) Start() error {
	if _, err := os.Stat(l.plistPath); err != nil {
		return fmt.Errorf("the service is not installed: run `stoplight install`")
	}
	// Bootstrap first: kickstart fails if the job is not in the domain,
	// which happens after a Stop that booted it out.
	_ = l.bootstrap()
	out, err := l.run("launchctl", "kickstart", "-k", l.target())
	if err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w: %s", l.target(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop halts the relay until the next login. KeepAlive would restart the
// process if we merely killed it, so the job is booted out of the domain
// instead.
func (l *launchd) Stop() error {
	return l.bootout()
}

// Disable stops the relay and marks it disabled, so a login does not
// bring it back. The plist stays on disk, so `stoplight service start`
// can re-enable it without a reinstall.
func (l *launchd) Disable() error {
	if err := l.bootout(); err != nil {
		return err
	}
	out, err := l.run("launchctl", "disable", l.target())
	if err != nil {
		return fmt.Errorf("launchctl disable %s: %w: %s", l.target(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Running reports whether launchd currently has the job running.
//
// `launchctl print` exits non-zero when the job is not loaded at all,
// which is a false answer rather than an error.
func (l *launchd) Running() (bool, error) {
	out, err := l.run("launchctl", "print", l.target())
	if err != nil {
		return false, nil
	}
	// A loaded but stopped job prints "state = not running".
	return strings.Contains(string(out), "state = running"), nil
}
