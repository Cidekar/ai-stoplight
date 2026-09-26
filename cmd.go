package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cidekar/stoplight/internal/adapter"
	"github.com/cidekar/stoplight/internal/light"
	"github.com/cidekar/stoplight/internal/notify"
	"github.com/cidekar/stoplight/internal/relay"
	"github.com/cidekar/stoplight/internal/service"
	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
	"github.com/cidekar/stoplight/internal/transport/ble"
	"github.com/cidekar/stoplight/internal/transport/serial"
)

// Exit codes. Every command uses these except notify, which always exits 0.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// defaultAddr is the loopback address the relay listens on, per RFC 1 section
// 4.1. Loopback only: a status light is for the machine you are sitting at.
const defaultAddr = relay.DefaultListenAddr

// defaultTimeout is how long a session may stay silent before the relay
// expires it, per RFC 1 section 7.
const defaultTimeout = relay.DefaultSessionTimeout

// probeTimeout bounds the check for a listening relay in `stoplight status`.
// The relay is on loopback, so anything slower than this is not there.
const probeTimeout = 250 * time.Millisecond

// run dispatches one command and returns the process exit code. Arguments and
// streams are parameters so tests can drive every command without touching the
// real process.
func run(args []string, stdout, stderr io.Writer) int {
	// The bare command runs the relay, so no arguments is not an error, and a
	// leading dash is a flag to that command rather than a subcommand.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdRelay(args, stdout, stderr)
	}

	name, rest := args[0], args[1:]

	switch name {
	case "relay":
		// The same command the bare invocation runs, named explicitly. The
		// service definitions on all three platforms invoke the binary this
		// way, and so does the message errUnsupported prints, so a plist or
		// unit file states what it starts rather than relying on the reader
		// knowing that a bare path means the relay.
		return cmdRelay(rest, stdout, stderr)
	case "notify":
		// Never returns anything but 0. See cmdNotify.
		return cmdNotify(rest)
	case "install":
		return cmdInstall(rest, stdout, stderr)
	case "uninstall":
		return cmdUninstall(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "restart":
		return cmdRestart(rest, stdout, stderr)
	case "logs":
		return cmdLogs(rest, stdout, stderr)
	case "service":
		return cmdService(rest, stdout, stderr)
	case "task":
		return cmdTask(rest, stdout, stderr)
	case "help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "stoplight: unknown command %q\n\n", name)
		usage(stderr)
		return exitUsage
	}
}

// usage lists every command with a one-line description. It goes to stdout for
// an explicit help request and to stderr for a usage error, which is why the
// writer is a parameter.
func usage(w io.Writer) {
	fmt.Fprintf(w, `stoplight - a desk light that shows what your agents are doing

Usage:
  stoplight [flags]              run the relay in the foreground
  stoplight <command> [args]

Commands:
  relay              run the relay in the foreground, the same as the bare
                     command. This is what the background service runs.
  install            detect installed agents, wire up an adapter for each,
                     install the background service and start it. Pass --ble
                     or --ble-name to run the service on a Bluetooth light.
  uninstall          remove the adapters and the service
  status             service state, connection, sessions, and the underlying
                     service command
  restart            bounce the service
  logs               tail the log file
  service stop       stop the service until next login
  service disable    stop the service and do not start it at login
  task <text>        override the screen label for this session
  task --clear       remove the override and show the derived label again
  notify <event>     report one event. Called by hooks, not by you.

Flags for the bare command:
  --virtual              use the virtual light instead of hardware
  --serial <path>        use a specific serial device
  --ble                  use a Bluetooth light instead of a USB one
  --ble-name <name>      connect to one named Bluetooth light
  --addr <host:port>     HTTP listen address (default %s)
  --timeout <duration>   session timeout (default %s)

Install and uninstall are both idempotent: running them twice changes nothing.
Full protocol in rfc.md, internals in design.md.
`, defaultAddr, defaultTimeout)
}

// newFlagSet builds a flag set that prints the full command list on -h, so
// help reads the same wherever it is asked for.
func newFlagSet(name string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() { usage(out) }
	return fs
}

// parseFlags parses args and reports whether the caller should return. The
// done return separates a help request, which succeeds, from a parse error,
// which is a usage failure. Both stop the command.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) (code int, done bool) {
	err := fs.Parse(args)
	switch {
	case err == nil:
		return exitOK, false
	case errors.Is(err, flag.ErrHelp):
		// The flag package already wrote the usage text to the set's output.
		return exitOK, true
	default:
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitUsage, true
	}
}

// cmdRelay runs the relay in the foreground, which is what you want while
// developing or debugging. The service runs this same code with no terminal.
func cmdRelay(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("stoplight", stdout)
	virtual := fs.Bool("virtual", false, "use the virtual light instead of hardware")
	device := fs.String("serial", "", "use a specific serial device")
	useBLE := fs.Bool("ble", false, "use a Bluetooth light instead of a USB one")
	bleName := fs.String("ble-name", "", "connect to one named Bluetooth light")
	addr := fs.String("addr", defaultAddr, "HTTP listen address")
	timeout := fs.Duration("timeout", defaultTimeout, "session timeout")

	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	// The address is checked before a transport is chosen, because choosing one
	// is the expensive, side-effecting step: it globs for serial devices and,
	// finding none, scans the airwaves for several seconds. A malformed --addr
	// is certain to fail, so paying that cost first only delays the error and
	// powers up a radio for a run that was never going to start.
	if err := relay.CheckListenAddr(*addr); err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	tr, err := selectTransport(context.Background(), transportChoice{
		virtual: *virtual,
		device:  *device,
		ble:     *useBLE,
		bleName: *bleName,
	}, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	r, err := relay.NewRelay(relay.Config{
		Transport:      tr,
		SessionTimeout: *timeout,
		ListenAddr:     *addr,
		Pollers:        installedPollers(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	// Ctrl-C and SIGTERM cancel the context rather than killing the process,
	// so Run closes its listeners and releases the serial port on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stdout, "stoplight: listening on %s, transport %s\n", *addr, tr.Name())

	// Run returns only on cancellation or a bind failure. A light that is off
	// or out of range is expected and never reaches here.
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	return exitOK
}

// transportChoice is what the user asked for on the command line. It is a
// struct rather than four parameters because the flags are mutually
// constrained, and validating them together in one place is what keeps the
// combinations from drifting apart.
type transportChoice struct {
	virtual bool
	device  string
	ble     bool
	bleName string
}

// bleScanTimeout bounds the auto-discovery scan. It is short on purpose: this
// runs before the relay binds its listener, so every second here is a second
// the light is not accepting reports. A light that is switched on answers well
// inside this, and one that does not is handled by the fallback.
const bleScanTimeout = 3 * time.Second

// selectTransport picks the light to drive. An explicit choice always wins.
//
// With no flag it auto-discovers, and the order is serial first, then BLE,
// then the virtual light. Serial leads for three reasons. A plugged-in cable
// is an unambiguous statement of intent, where a radio in range is not: a
// light on someone else's desk can advertise into this room, and a cable
// cannot. Serial discovery is a filesystem glob that answers in microseconds,
// while a BLE scan costs seconds of radio time before the relay can listen, so
// trying it first would delay every start on every machine that has a cable.
// And serial is the debuggable link, so a user with both attached is almost
// always mid-debug and wants the wire.
//
// Every fallback says what it did. A silent fallback is indistinguishable from
// broken hardware.
// The context bounds the Bluetooth scan only; every other branch returns
// without blocking. See discoverBLE for why a test needs to be able to cancel
// it.
func selectTransport(ctx context.Context, choice transportChoice, stdout io.Writer) (transport.Transport, error) {
	if err := choice.validate(); err != nil {
		return nil, err
	}

	switch {
	case choice.virtual:
		return light.NewVirtual(stdout), nil
	case choice.device != "":
		return serial.New(choice.device), nil
	case choice.ble || choice.bleName != "":
		// An explicit --ble is a decision, not a hint, so this does not fall
		// back to serial. The transport itself retries forever, so a light
		// that is switched on later is still picked up.
		return ble.New(choice.bleName), nil
	}

	if ports, err := serial.Discover(); err == nil && len(ports) > 0 {
		// Discover returns candidates in preference order, so the first is the
		// best guess. A wrong guess is corrected with --serial.
		fmt.Fprintf(stdout, "stoplight: found a light on %s\n", ports[0])
		return serial.New(ports[0]), nil
	}

	if tr := discoverBLE(ctx, stdout); tr != nil {
		return tr, nil
	}

	fmt.Fprintln(stdout, "stoplight: no light found, using the virtual light")
	fmt.Fprintln(stdout, "stoplight: plug one in and restart, or pass --serial <path> or --ble")
	return light.NewVirtual(stdout), nil
}

// relayArgs renders the choice as the flags a service definition passes to
// `stoplight relay`. Only the transport is expressed here: the service keeps
// the relay's own defaults for the address and the timeout, and an empty slice
// means the service auto-discovers exactly as the bare relay does.
//
// A name implies --ble, matching selectTransport, so --ble-name alone is enough
// and a definition never carries a name without the flag it belongs to.
func (c transportChoice) relayArgs() []string {
	switch {
	case c.bleName != "":
		return []string{"--ble-name", c.bleName}
	case c.ble:
		return []string{"--ble"}
	}
	return nil
}

// validate rejects flag combinations that ask for two different lights. Each
// message names both flags, because a message that names one leaves the reader
// guessing which of the two to drop.
func (c transportChoice) validate() error {
	switch {
	case c.virtual && c.device != "":
		return errors.New("--virtual and --serial cannot both be set")
	case c.virtual && (c.ble || c.bleName != ""):
		return errors.New("--virtual and --ble cannot both be set")
	case c.device != "" && (c.ble || c.bleName != ""):
		return errors.New("--serial and --ble cannot both be set")
	}
	return nil
}

// discoverBLE scans briefly for a Bluetooth light and returns a transport for
// the nearest one, or nil if there is nothing to connect to.
//
// A scan failure is not fatal here. No adapter, the radio switched off, or a
// user who declined the Bluetooth prompt all mean "not this transport", and
// auto-discovery moves on to the virtual light. Only an explicit --ble treats
// those as errors worth stopping for, because only there did the user ask for
// BLE specifically.
// The parent context bounds the scan. Production passes a background context,
// so the only limit is bleScanTimeout below. A test passes a cancelled one to
// exercise the whole selection path without powering up the radio, which is
// the only way to run it under the race detector: the library's Scan and
// StopScan share an unsynchronised field, so any real scan reports a race that
// no amount of locking on this side can remove.
func discoverBLE(parent context.Context, stdout io.Writer) transport.Transport {
	ctx, cancel := context.WithTimeout(parent, bleScanTimeout)
	defer cancel()

	lights, err := ble.Discover(ctx)
	if err != nil {
		// Worth one line: a user who denied the permission prompt needs to
		// know that is why their Bluetooth light was skipped.
		if errors.Is(err, ble.ErrPermissionDenied) {
			fmt.Fprintf(stdout, "stoplight: skipping Bluetooth: %v\n", err)
		}
		return nil
	}
	if len(lights) == 0 {
		return nil
	}

	// Discover sorts by signal strength, so the first is the nearest light.
	found := lights[0]
	fmt.Fprintf(stdout, "stoplight: found a Bluetooth light, %s\n", found.Name)
	return ble.New(found.Name)
}

// cmdNotify reports one event to the relay.
//
// It returns 0 on every path, without exception. This command runs inside
// somebody's editor hook, and a status light that can break a coding session
// is worse than no status light at all. Relay down, port closed, malformed
// arguments, an unknown flag, a panic in any package it calls: the answer is
// always 0. This is the invariant in design.md and RFC 1 section 6.
//
// It writes nothing to stdout or stderr for the same reason: a hook's output
// can land in the middle of somebody's terminal session.
func cmdNotify(args []string) (code int) {
	// The recover is the outermost statement, so a panic anywhere below still
	// leaves the named return at 0.
	defer func() {
		_ = recover()
		code = exitOK
	}()

	fs, values := newNotifyFlagSet()
	sessionID, cwd := values.sessionID, values.cwd
	label, provider, addr := values.label, values.provider, values.addr

	// The flag package stops at the first non-flag argument, but a hook is far
	// more likely to write `notify blocked --session-id X` than to put the
	// event last. Splitting the event out first lets flags sit on either side
	// of it.
	event, flags := splitEvent(args)

	if err := fs.Parse(flags); err != nil {
		return exitOK
	}

	// The error is deliberately discarded. Send returns one for tests only.
	_ = notify.Send(*addr, stoplight.Report{
		SessionID: *sessionID,
		Event:     event,
		Label:     *label,
		Provider:  *provider,
		Cwd:       *cwd,
	})

	return exitOK
}

// notifyValues holds the destinations for the notify flag set.
type notifyValues struct {
	sessionID *string
	cwd       *string
	label     *string
	provider  *string
	addr      *string
}

// newNotifyFlagSet builds the flag set for notify.
//
// It is separate from cmdNotify so that a test can ask which flags exist
// rather than restating them. The adapter writes these names into a hook
// command, and notify exits zero on a name it does not know, so a copy of the
// list that drifted from this one would fail silently in the only place it
// matters.
func newNotifyFlagSet() (*flag.FlagSet, notifyValues) {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	// A parse error prints nothing and changes nothing. A hook passing a flag
	// this build does not know must still succeed.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	return fs, notifyValues{
		sessionID: fs.String("session-id", "", "stable identifier for this session"),
		cwd:       fs.String("cwd", "", "working directory, used to derive a label"),
		label:     fs.String("label", "", "text shown on the screen"),
		provider:  fs.String("provider", "", "which tool is reporting"),
		addr:      fs.String("addr", defaultAddr, "relay address"),
	}
}

// notifyValueFlags are the notify flags that take a separate value argument.
// splitEvent needs them to tell `--label blocked` from a bare event name.
var notifyValueFlags = map[string]bool{
	"session-id": true,
	"cwd":        true,
	"label":      true,
	"provider":   true,
	"addr":       true,
}

// splitEvent pulls the first positional argument out of args and returns it
// with the remaining flags. The flag package stops parsing at the first
// non-flag argument, so without this a hook writing the event before its flags
// would silently lose every one of them.
//
// It returns an empty event when there is no positional argument. That is not
// an error here: notify reports nothing rather than failing.
func splitEvent(args []string) (event string, flags []string) {
	flags = make([]string, 0, len(args))

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Everything after a bare -- is positional by definition.
		if arg == "--" {
			if event == "" && i+1 < len(args) {
				event = args[i+1]
				flags = append(flags, args[i+2:]...)
			}
			return event, flags
		}

		if strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
			// A --flag=value form carries its value already. Otherwise the
			// next argument belongs to this flag, not to the event.
			name := strings.TrimLeft(arg, "-")
			if !strings.Contains(arg, "=") && notifyValueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}

		// A positional argument. The first is the event; later ones are extra
		// and ignored, because a hook adding an argument must not break.
		if event == "" {
			event = arg
		}
	}

	return event, flags
}

// cmdInstall is the only setup step. It installs an adapter for every agent it
// finds, installs the service, and starts it. Every step is idempotent, so a
// second run reports the same result rather than duplicating anything.
func cmdInstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("install", stdout)
	useBLE := fs.Bool("ble", false, "run the service against a Bluetooth light instead of auto-discovering")
	bleName := fs.String("ble-name", "", "run the service against one named Bluetooth light")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	// The transport flags are baked into the service definition, so the service
	// launches with the same choice a bare `stoplight relay --ble` would make.
	// Validating them here reuses the relay's own rules, so `install` rejects an
	// impossible combination rather than writing a definition that cannot start.
	choice := transportChoice{ble: *useBLE, bleName: *bleName}
	if err := choice.validate(); err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitUsage
	}
	relayArgs := choice.relayArgs()

	// Adapters write this path into hook entries that outlive the shell, so it
	// has to be the resolved binary rather than whatever name argv[0] held.
	binPath, err := binaryPath()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	// Three outcomes have to stay distinct here, because they send the user
	// somewhere different. No adapter compiled in at all is a build problem.
	// An adapter that ran and failed is a problem with that agent's config,
	// and its error was already printed. Only the first deserves the "no
	// supported agents" line: printing it after a failure told the user the
	// agent was absent when in fact it was found and had just reported why it
	// could not be written.
	adapters := adapter.All()
	installed := 0
	failed := 0
	for _, a := range adapters {
		// Installed is checked first only to word the output correctly. An
		// error here is not fatal: Install is idempotent and can still run.
		present, _ := a.Installed()
		if err := a.Install(binPath); err != nil {
			fmt.Fprintf(stderr, "stoplight: %s: %v\n", a.Name(), err)
			failed++
			continue
		}
		if present {
			fmt.Fprintf(stdout, "  %-12s already installed\n", a.Name())
		} else {
			fmt.Fprintf(stdout, "  %-12s installed\n", a.Name())
		}
		installed++
	}
	switch {
	case len(adapters) == 0:
		// No adapter is linked into this binary. On a release build this is a
		// wiring bug, not a property of the user's machine, so it must not be
		// worded as though we looked for agents and found none.
		fmt.Fprintln(stdout, "  no agent adapters are built into this binary")
		fmt.Fprintln(stdout, "  any tool can post to the endpoint directly, see rfc.md")
	case installed == 0:
		// Every adapter present failed. The individual errors are already on
		// stderr, so this only has to stop the user reading the silence as
		// success.
		fmt.Fprintf(stdout, "  no agent was configured: %d of %d failed, see the errors above\n",
			failed, len(adapters))
	}

	mgr, err := service.New()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	if err := mgr.Install(binPath, relayArgs...); err != nil {
		fmt.Fprintf(stderr, "stoplight: install the service: %v\n", err)
		return exitError
	}
	if err := mgr.Start(); err != nil {
		fmt.Fprintf(stderr, "stoplight: start the service: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "  %-12s installed and started\n", "service")

	fmt.Fprintf(stdout, "\nThe relay starts at login. Check it with: stoplight status\n")
	return exitOK
}

// cmdUninstall reverses install. It is idempotent, and it keeps going after a
// failure so one stuck adapter cannot strand the rest.
func cmdUninstall(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("uninstall", stdout)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	failed := false

	if mgr, err := service.New(); err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		failed = true
	} else {
		// A service that is already stopped is the state we want, so a stop
		// error is reported but does not fail the command.
		if err := mgr.Stop(); err != nil {
			fmt.Fprintf(stderr, "stoplight: stop the service: %v\n", err)
		}
		if err := mgr.Uninstall(); err != nil {
			fmt.Fprintf(stderr, "stoplight: remove the service: %v\n", err)
			failed = true
		} else {
			fmt.Fprintf(stdout, "  %-12s removed\n", "service")
		}
	}

	for _, a := range adapter.All() {
		if err := a.Uninstall(); err != nil {
			fmt.Fprintf(stderr, "stoplight: %s: %v\n", a.Name(), err)
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "  %-12s removed\n", a.Name())
	}

	if failed {
		return exitError
	}
	return exitOK
}

// installedPollers returns the adapters that can be polled for a full session
// list, per RFC 1 section 10.1, and that are actually installed.
//
// Both halves matter. An adapter that affords no query is not a Poller and
// contributes nothing. An adapter that is a Poller but is NOT installed is
// skipped too: polling it would shell out to an agent the user never wired up,
// once per interval, forever, to be told nothing is running. An agent whose
// hooks are absent is an agent this relay was not asked to watch.
//
// An adapter that cannot say whether it is installed is skipped. The question
// failing means its configuration could not be read, and a poller is a
// correction to hooks that in that case are equally unreadable.
func installedPollers() []adapter.Poller {
	var pollers []adapter.Poller
	for _, a := range adapter.All() {
		p, ok := a.(adapter.Poller)
		if !ok {
			continue
		}
		if installed, err := a.Installed(); err != nil || !installed {
			continue
		}
		pollers = append(pollers, p)
	}
	return pollers
}

// cmdStatus prints the service state, whether the relay is listening, and the
// platform command behind it all. That last line is deliberate: the wrapper is
// never a black box, and launchctl or systemctl still work directly.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("status", stdout)
	addr := fs.String("addr", defaultAddr, "relay address")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	mgr, err := service.New()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	running, err := mgr.Running()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	fmt.Fprintf(stdout, "service   %s\n", runState(running))

	// Ask the relay what it is doing rather than only checking that the port
	// opens. The two answers are different problems for the user: a relay that
	// is not running is fixed by starting the service, and a relay that is
	// running with no light reachable is fixed by plugging the light in.
	//
	// Neither is an error. A stopped service is a state to report, not a
	// failure of this command.
	if snapshot, err := fetchStatus(*addr); err != nil {
		fmt.Fprintf(stdout, "relay     not listening on %s\n", *addr)
	} else {
		fmt.Fprintf(stdout, "relay     listening on %s\n", *addr)
		if snapshot.Connected {
			fmt.Fprintf(stdout, "light     connected over %s\n", snapshot.Transport)
		} else {
			// The relay is up and tracking, so ingest works: only the light is
			// missing. Saying "not listening" here would send the user after
			// the wrong thing entirely.
			fmt.Fprintf(stdout, "light     not connected over %s\n", snapshot.Transport)
		}
		fmt.Fprintf(stdout, "sessions  %d, aggregate %s\n", len(snapshot.Sessions), snapshot.Aggregate)
	}

	if path, err := logPath(); err == nil {
		fmt.Fprintf(stdout, "log       %s\n", path)
	}

	fmt.Fprintf(stdout, "command   %s\n", mgr.Command())
	return exitOK
}

// fetchStatus asks the relay at addr for its snapshot.
//
// An error means the relay is not reachable: the service is stopped, still
// starting, or listening somewhere else. That is different from a relay that
// answers with connected false, which means the relay is fine and the light is
// not, so the two must not collapse into one message.
//
// The deadline is the same short one the probe used. The relay is on loopback,
// so anything slower than this is not really there.
func fetchStatus(addr string) (relay.StatusResponse, error) {
	var out relay.StatusResponse

	client := &http.Client{Timeout: probeTimeout}
	resp, err := client.Get("http://" + addr + relay.StatusPath)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Something answered, but it is not a relay speaking this protocol.
		// Treat it as unreachable rather than reporting a half-read snapshot.
		io.Copy(io.Discard, resp.Body)
		return out, fmt.Errorf("relay returned %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, fmt.Errorf("decode status: %w", err)
	}
	return out, nil
}

// listening reports whether anything accepts a connection at addr. It dials
// and hangs up.
//
// Status no longer uses it: asking the status endpoint says both whether the
// relay is up AND whether a light is attached, which a bare connect cannot. It
// stays because "did the port open at all" is still the cheapest way to tell a
// relay that is down from one that is merely slow, with no protocol assumed.
func listening(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, probeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// runState renders the service state as the word status prints.
func runState(running bool) string {
	if running {
		return "running"
	}
	return "stopped"
}

// cmdRestart bounces the service. Stop then start, rather than a platform
// restart verb, because not every platform has one.
func cmdRestart(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("restart", stdout)
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	mgr, err := service.New()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	// A stop error is not fatal: a service that is already down is a fine
	// starting point for a start.
	if err := mgr.Stop(); err != nil {
		fmt.Fprintf(stderr, "stoplight: stop: %v\n", err)
	}
	if err := mgr.Start(); err != nil {
		fmt.Fprintf(stderr, "stoplight: start: %v\n", err)
		return exitError
	}

	fmt.Fprintln(stdout, "service restarted")
	fmt.Fprintf(stdout, "command %s\n", mgr.Command())
	return exitOK
}

// cmdLogs tails the log file. Every event is recorded on arrival, so one file
// answers whether a producer reported, whether the relay saw it, and whether
// the light acknowledged it.
func cmdLogs(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("logs", stdout)
	follow := fs.Bool("f", true, "keep watching for new lines")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	path, err := logPath()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	file, err := os.Open(path)
	if err != nil {
		// No log file means the relay has not run yet, which is worth saying
		// plainly rather than reporting as a missing file.
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "stoplight: no log at %s yet\n", path)
			fmt.Fprintln(stderr, "stoplight: start the relay with: stoplight restart")
			return exitError
		}
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	defer file.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := tail(ctx, file, stdout, *follow); err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}
	return exitOK
}

// tail copies the file to w, then keeps polling for appended lines until ctx
// is cancelled. Polling rather than watching the filesystem keeps this to the
// standard library, and a log that updates within a second reads as live.
func tail(ctx context.Context, f *os.File, w io.Writer, follow bool) error {
	reader := bufio.NewReader(f)
	for {
		switch _, err := io.Copy(w, reader); {
		case err != nil:
			return err
		case !follow:
			return nil
		}

		select {
		case <-ctx.Done():
			// A cancelled tail is how this command ends normally.
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// cmdService handles the subcommands that stop things. There is no
// `service start`: install starts the service and restart bounces it, so a
// third way to start it would only be a third thing to document.
func cmdService(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "stoplight: service needs a subcommand: stop or disable")
		return exitUsage
	}

	sub, rest := args[0], args[1:]
	if sub != "stop" && sub != "disable" {
		fmt.Fprintf(stderr, "stoplight: unknown service subcommand %q\n\n", sub)
		usage(stderr)
		return exitUsage
	}

	fs := newFlagSet("service "+sub, stdout)
	if code, done := parseFlags(fs, rest, stderr); done {
		return code
	}

	mgr, err := service.New()
	if err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	if sub == "stop" {
		if err := mgr.Stop(); err != nil {
			fmt.Fprintf(stderr, "stoplight: %v\n", err)
			return exitError
		}
		fmt.Fprintln(stdout, "service stopped, and starts again at next login")
	} else {
		if err := mgr.Disable(); err != nil {
			fmt.Fprintf(stderr, "stoplight: %v\n", err)
			return exitError
		}
		fmt.Fprintln(stdout, "service stopped, and does not start at login")
	}

	fmt.Fprintf(stdout, "command %s\n", mgr.Command())
	return exitOK
}

// cmdTask overrides the screen label for a session. The override wins over
// whatever the producer sent, and lasts until the session ends.
//
// --clear removes the override so the derived label shows again. It is the same
// request with an empty label: the endpoint treats empty as "clear", so the flag
// needs no separate verb and no separate code path.
func cmdTask(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("task", stdout)
	sessionID := fs.String("session-id", "", "which session to relabel")
	clear := fs.Bool("clear", false, "remove the override and show the derived label again")
	addr := fs.String("addr", defaultAddr, "relay address")
	if code, done := parseFlags(fs, args, stderr); done {
		return code
	}

	// Everything left over is the label, so it reads naturally either quoted
	// or bare: stoplight task nightly integration run.
	text := strings.TrimSpace(strings.Join(fs.Args(), " "))

	// --clear and a label ask for opposite things. Guessing which one was meant
	// would silently discard the other, so say so instead.
	if *clear && text != "" {
		fmt.Fprintln(stderr, "stoplight: --clear takes no text")
		fmt.Fprintln(stderr, "usage: stoplight task --session-id <id> --clear")
		return exitUsage
	}
	if !*clear && text == "" {
		fmt.Fprintln(stderr, "stoplight: task needs some text")
		fmt.Fprintln(stderr, "usage: stoplight task <text>")
		return exitUsage
	}

	id := *sessionID
	if id == "" {
		fmt.Fprintln(stderr, "stoplight: task needs --session-id")
		fmt.Fprintln(stderr, "usage: stoplight task --session-id <id> <text>")
		return exitUsage
	}

	// The label goes to the task endpoint, NOT to ingest. Ingest requires an
	// event, and every event runs a state transition: an `idle` report, which
	// is what this used to send, drives the session to Idle and turns the lamp
	// green. Naming the task you are blocked on is precisely the moment the
	// light must stay red, so this path must carry no event at all.
	//
	// Unlike notify, the error is reported rather than discarded. This command
	// is typed by a human who is owed an answer: a relay that is down, or a
	// request the relay rejected, must not look like success.
	if err := notify.SendTask(*addr, notify.Task{
		SessionID: id,
		Label:     text,
	}); err != nil {
		fmt.Fprintf(stderr, "stoplight: %v\n", err)
		return exitError
	}

	if text == "" {
		fmt.Fprintf(stdout, "label cleared for %q\n", id)
	} else {
		fmt.Fprintf(stdout, "label set to %q\n", text)
	}
	return exitOK
}

// logPath is the file the relay appends to, per the readme:
// ~/.local/state/stoplight/stoplight.log, alongside the ingest socket.
func logPath() (string, error) {
	sock, err := relay.DefaultSocketPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(sock), "stoplight.log"), nil
}

// binaryPath resolves the running executable to an absolute path with symlinks
// followed. Adapters and service definitions embed this and outlive the shell
// that ran install, so a relative name or a symlink into a temporary directory
// would leave a hook pointing at nothing.
func binaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find the stoplight binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A path that does not resolve is still better than no path at all.
		return exe, nil
	}
	return resolved, nil
}
