// Package relay is the long-running process at the heart of Stoplight. It owns
// the tracker, the transport and both ingest listeners, and it ties them
// together with a run loop that transmits a frame only when something changed.
//
// The rule that shapes this package: the relay never exits because of a
// transport failure. A light that is off, asleep or out of range is an
// expected condition. Exiting would look like a crash to the service manager,
// which would restart the process, which would loop forever.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cidekar/stoplight/internal/adapter"
	"github.com/cidekar/stoplight/internal/stoplight"
	"github.com/cidekar/stoplight/internal/transport"
)

// Defaults applied by NewRelay when a Config field is left zero.
const (
	// DefaultSessionTimeout is how long a session may stay silent before the
	// sweep expires it. A crashed producer never says goodbye, and without
	// this one dead session would hold the lamp red forever.
	DefaultSessionTimeout = 30 * time.Minute

	// DefaultSweepInterval is how often the run loop looks for silent
	// sessions.
	DefaultSweepInterval = 30 * time.Second

	// DefaultListenAddr is the loopback address and port from RFC 1 section
	// 4.1.
	DefaultListenAddr = "127.0.0.1:7373"

	// MaxBodyBytes caps an ingest request at 8KB, per RFC 1 section 12.
	MaxBodyBytes = 8 << 10

	// DefaultPollInterval is how often a Poller is asked for its agent's
	// full session list.
	//
	// Longer than the sweep, because a poll spawns a child process where a
	// sweep walks a map. It is a correction rather than the primary signal:
	// hooks move the lamp immediately, and this catches what they cannot see,
	// so the interval only bounds how long a stale session can linger.
	DefaultPollInterval = 20 * time.Second

	// MaxSyncBodyBytes caps a full-state sync at 64KB, per RFC 1 section 5.4.
	// A sync carries every live session where ingest carries one, and the
	// tracker's own cap of MaxSessions entries at the Appendix A field sizes
	// fits inside this comfortably.
	MaxSyncBodyBytes = 64 << 10
)

// reconnectPollInterval is how often the run loop re-examines the transport's
// connection state. It is short enough that a light coming back into range
// catches up quickly, and cheap because it only reads a boolean.
const reconnectPollInterval = time.Second

// HTTP server timeouts. Only ReadHeaderTimeout used to be set, which left a
// client that sent complete headers and then stalled mid-body holding a
// goroutine and a file descriptor for as long as it liked. The unix socket has
// always bounded a silent producer with socketReadTimeout; HTTP is the listener
// bound to a port, so it needs the same discipline.
const (
	// defaultReadHeaderTimeout bounds how long the request line and headers
	// may take to arrive.
	defaultReadHeaderTimeout = 5 * time.Second

	// defaultReadTimeout bounds the whole request, headers and body together.
	defaultReadTimeout = 10 * time.Second

	// defaultWriteTimeout bounds the response. handleStatus writes a body, so
	// a client that stops reading must not park the writer forever.
	defaultWriteTimeout = 10 * time.Second

	// defaultIdleTimeout bounds a keep-alive connection between requests.
	defaultIdleTimeout = 60 * time.Second
)

// Config configures a Relay. The zero value of every field except Transport
// selects a sensible default.
type Config struct {
	// Transport carries frames to the light. Required.
	Transport transport.Transport

	// SessionTimeout is the silence after which a session expires.
	// Defaults to DefaultSessionTimeout.
	SessionTimeout time.Duration

	// SweepInterval is how often to expire silent sessions.
	// Defaults to DefaultSweepInterval.
	SweepInterval time.Duration

	// Pollers are adapters that can be asked for their agent's full session
	// list, per RFC 1 section 10.1. Empty is the normal case: an agent that
	// only affords hooks contributes no poller, and the relay never polls.
	//
	// The relay owns the schedule so that a poll cannot land on a hook's code
	// path, where it would hold the agent up for as long as the query took.
	Pollers []adapter.Poller

	// PollInterval is how often each poller is asked. Defaults to
	// DefaultPollInterval.
	PollInterval time.Duration

	// ListenAddr is the HTTP ingest address. Defaults to DefaultListenAddr.
	// It MUST resolve to a loopback address: NewRelay rejects anything else,
	// because a status light is for the machine you are sitting at.
	ListenAddr string

	// SocketPath is the unix socket ingest path. Defaults to
	// ~/.local/state/stoplight/sock.
	SocketPath string

	// LogPath names a file to append logs to. Empty logs to stderr.
	LogPath string

	// httpTimeouts overrides the HTTP server's read, write and idle timeouts.
	// It is unexported because operators have no reason to tune it: the
	// defaults are generous for a loopback listener. Tests set it so a timeout
	// test finishes in milliseconds instead of tens of seconds.
	httpTimeouts httpTimeouts
}

// httpTimeouts holds the http.Server deadlines. A zero field selects the
// matching default.
type httpTimeouts struct {
	readHeader time.Duration
	read       time.Duration
	write      time.Duration
	idle       time.Duration
}

// withDefaults fills the zero fields, so Run always builds a fully bounded
// server whether or not a test supplied overrides.
func (t httpTimeouts) withDefaults() httpTimeouts {
	if t.readHeader <= 0 {
		t.readHeader = defaultReadHeaderTimeout
	}
	if t.read <= 0 {
		t.read = defaultReadTimeout
	}
	if t.write <= 0 {
		t.write = defaultWriteTimeout
	}
	if t.idle <= 0 {
		t.idle = defaultIdleTimeout
	}
	return t
}

// Relay owns the tracker, the transport, and both ingest listeners.
// It is safe for concurrent use.
type Relay struct {
	cfg     Config
	tracker *stoplight.Tracker
	logger  *log.Logger

	// logFile is the handle opened for cfg.LogPath, closed on shutdown.
	// It is nil when logging to stderr.
	logFile *os.File

	// mu guards the fields below. It is never held across a transport call,
	// because Send may block on a radio.
	mu       sync.Mutex
	started  time.Time
	listener net.Listener
	socket   net.Listener

	// running is set for the duration of Run. A Relay owns one listen address,
	// one socket path and one log file, so a second concurrent Run would have
	// two goroutines binding the same paths and closing each other's handles.
	// One at a time, checked under mu.
	running bool

	// ran records that a Run got as far as serving, which makes a Relay
	// one-shot. Shutdown closes the log file and the transport, and neither is
	// reopened, so a second Run would serve happily while writing every log
	// line into a closed descriptor and would close the transport twice. It is
	// set only after both listeners bind: a Run that failed to bind opened
	// nothing, so the operator may correct the address and try again.
	ran bool

	// reconnecting is set while a Connect attempt is in flight, so the
	// reconnect ticker starts at most one at a time. It is atomic rather than
	// guarded by mu because the attempt outlives the tick that started it, and
	// mu must never be held across a transport call.
	reconnecting atomic.Bool

	// acked reports that the light holds the current frame. It is cleared the
	// moment the frame moves and whenever a Send fails, and set only by a Send
	// that succeeds. The reconnect tick resends on any tick where it is false
	// and the transport is connected, which is what finally covers three gaps
	// that "transmit only on change" left open:
	//
	//   - A Send that fails is logged and otherwise lost. The transport
	//     contract says an error means this frame was lost, not that the link
	//     is dead, so a transient write error used to leave the lamp on the
	//     previous colour until the tracker next moved. Now the next tick
	//     resends it.
	//   - A light power-cycled or carried out of range and back may come back
	//     with no Connect from this relay and no edge the loop can see. It
	//     holds its boot state while Connected still reports true. An unacked
	//     frame is pushed regardless of whether an edge was observed.
	//   - After the first connect nothing used to transmit until the state
	//     changed. acked starts false, so the first tick that sees a connected
	//     transport pushes the current frame once, with no separate startup
	//     path to keep in step.
	//
	// It is atomic because transmit runs on ingest goroutines, send runs on the
	// sender goroutine, and the reconnect tick reads it on the loop goroutine,
	// and none of them may take mu across a transport call.
	acked atomic.Bool

	// wake signals the sender goroutine that the frame moved. It has capacity
	// one and is written with a non-blocking send, which is what makes the
	// sender coalescing: a signal that arrives while one is already pending is
	// dropped, because the pending one will read the newer frame anyway.
	//
	// It is nil until Run starts the sender, and guarded by mu because Run may
	// begin while a handler goroutine is between requests. See transmit.
	wake chan struct{}

	// sendInline makes transmit write to the transport on the calling
	// goroutine when no sender is running. It exists for handler-level tests,
	// which exercise the endpoints through httptest with no Run behind them and
	// still need the frame to reach the transport.
	//
	// It is an explicit opt-in rather than a nil-wake fallback because that
	// fallback was reachable in production, between Run clearing wake and the
	// sender stopping. An HTTP handler blocking on a radio write is precisely
	// what the sender goroutine exists to prevent, so production must never
	// take that path.
	sendInline bool

	// ingestLog* rate limit the socket's rejected-payload log line. The socket
	// is writable by any local process, so logging every malformed payload let
	// one such process fill the disk a line at a time. They have their own
	// mutex because a socket handler must not contend with Run for mu just to
	// decide whether to log. See logIngestFailure.
	ingestLogMu      sync.Mutex
	ingestLogAt      time.Time
	ingestLogDropped int

	// now is the clock, indirected so tests can control time.
	now func() time.Time
}

// ErrAlreadyRunning reports a second Run on a Relay that is already serving.
var ErrAlreadyRunning = errors.New("relay: already running")

// NewRelay builds a Relay from cfg, applying defaults and validating the
// listen address. It does not bind anything: that happens in Run.
//
// It returns an error if cfg.Transport is nil, or if cfg.ListenAddr resolves
// to a non-loopback interface. The second is a security requirement from RFC 1
// section 12, not a preference.
func NewRelay(cfg Config) (*Relay, error) {
	if cfg.Transport == nil {
		return nil, errors.New("relay: Config.Transport is required")
	}

	if cfg.SessionTimeout <= 0 {
		cfg.SessionTimeout = DefaultSessionTimeout
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	if cfg.SocketPath == "" {
		path, err := DefaultSocketPath()
		if err != nil {
			return nil, err
		}
		cfg.SocketPath = path
	}

	if err := checkLoopback(cfg.ListenAddr); err != nil {
		return nil, err
	}

	r := &Relay{
		cfg:     cfg,
		tracker: stoplight.NewTracker(cfg.SessionTimeout),
		now:     time.Now,
	}

	logger, file, err := newLogger(cfg.LogPath)
	if err != nil {
		return nil, err
	}
	r.logger = logger
	r.logFile = file

	return r, nil
}

// newLogger opens the log destination. An empty path logs to stderr and
// returns a nil file, so shutdown knows there is nothing to close.
func newLogger(path string) (*log.Logger, *os.File, error) {
	if path == "" {
		return log.New(os.Stderr, "stoplight: ", log.LstdFlags), nil, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("relay: create log directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("relay: open log file: %w", err)
	}
	return log.New(file, "stoplight: ", log.LstdFlags), file, nil
}

// DefaultSocketPath is ~/.local/state/stoplight/sock, the path RFC 1 section
// 4.2 specifies. Parent directories are not created here; Run does that when
// it binds.
func DefaultSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("relay: locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "stoplight", "sock"), nil
}

// CheckListenAddr reports whether addr is usable as the relay's listen
// address, without building a relay or opening anything.
//
// It exists so a caller can reject a bad address before it acquires hardware.
// NewRelay makes the same check, but by the time it runs the CLI has already
// chosen a transport, and choosing one costs a serial glob and a multi-second
// Bluetooth scan. Validating first turns a typo in --addr into an immediate
// error instead of one that arrives after a scan of the airwaves.
func CheckListenAddr(addr string) error { return checkLoopback(addr) }

// checkLoopback reports an error unless addr's host resolves exclusively to
// loopback addresses. An empty or wildcard host is rejected: binding to every
// interface would expose the relay to the network, which RFC 1 section 12
// forbids without explicit configuration.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("relay: parse listen address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("relay: listen address %q binds every interface; use a loopback address", addr)
	}

	// An IP literal is the common case and needs no resolver.
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("relay: listen address %q is not loopback", addr)
		}
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("relay: resolve listen host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("relay: listen host %q resolves to no addresses", host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("relay: listen host %q resolves to non-loopback address %s", host, ip)
		}
	}
	return nil
}

// ingest is the one path both listeners use. It decodes a report, applies it
// to the tracker, and reports whether the frame changed.
//
// Both HTTP and the unix socket call this, so the two can never disagree about
// what a payload means. Errors are classified rather than wrapped, because the
// HTTP handler must map them onto status codes and the socket must not.
func (r *Relay) ingest(body io.Reader) (changed bool, err error) {
	var report stoplight.Report
	if err := decodeJSON(body, &report); err != nil {
		return false, err
	}
	if err := report.Validate(); err != nil {
		return false, &badRequestError{err: err}
	}

	// An unknown event is accepted and ignored, per RFC 1 section 11. It is
	// not an error, so the producer still sees success.
	if _, ok := stoplight.ParseEvent(report.Event); !ok {
		return false, nil
	}

	return r.tracker.Apply(report, r.now()), nil
}

// reconcile decodes a full-state sync and applies it, per RFC 1 section 5.4.
//
// Unlike ingest, an entry naming an unknown event is dropped rather than
// making the whole sync a no-op. A sync is a set of independent claims about
// separate sessions, and one entry a later spec added must not cost the
// removals the rest of the request implies: that would leave a session lit
// forever on the strength of a field this relay does not read.
func (r *Relay) reconcile(body io.Reader) (changed bool, err error) {
	var sync stoplight.Sync
	if err := decodeJSON(body, &sync); err != nil {
		return false, err
	}
	if sync.Provider == "" {
		return false, &badRequestError{err: errors.New("missing provider")}
	}
	if sync.ObservedAt.IsZero() {
		return false, &badRequestError{err: errors.New("missing observed_at")}
	}

	keep := make([]stoplight.Report, 0, len(sync.Sessions))
	for _, entry := range sync.Sessions {
		if err := entry.Validate(); err != nil {
			continue
		}
		if _, ok := stoplight.ParseEvent(entry.Event); !ok {
			continue
		}
		keep = append(keep, entry)
	}
	sync.Sessions = keep

	return r.tracker.Reconcile(sync, r.now()), nil
}

// newPollTicker returns a ticker that fires on interval, or one that never
// fires when there is nothing to poll.
//
// A nil channel blocks forever in a select, which is what "no pollers" should
// cost: nothing. Guarding the case here keeps the loop body free of a
// conditional that would otherwise have to be right in two places.
func newPollTicker(interval time.Duration, enabled bool) *time.Ticker {
	if !enabled {
		return &time.Ticker{C: nil}
	}
	return time.NewTicker(interval)
}

// pollAll asks every configured poller for its agent's full session list and
// reconciles each answer, per RFC 1 section 10.1.
//
// Each poll runs in its own goroutine because it spawns a child process and
// waits on it. Doing that inline would stall the run loop for the length of
// the query, which delays the sweep and, worse, delays the transmit that a
// hook arriving in the meantime asked for.
//
// A poll that fails is logged once and otherwise ignored. RFC 1 section 5.4
// distinguishes an empty answer from no answer: an agent that reports nothing
// live means remove everything, but an agent that could not be asked means
// leave its sessions exactly where they are. Treating a failed query as an
// empty one would clear the whole light every time the command was missing.
func (r *Relay) pollAll(ctx context.Context, wg *sync.WaitGroup) {
	for _, poller := range r.cfg.Pollers {
		wg.Add(1)
		go func(p adapter.Poller) {
			defer wg.Done()

			sessions, observedAt, err := p.Poll(ctx)
			if err != nil {
				// Cancellation on shutdown is not a fault worth logging.
				if ctx.Err() == nil {
					r.logger.Printf("poll %s: %v", p.Name(), err)
				}
				return
			}

			if r.tracker.Reconcile(stoplight.Sync{
				Provider:   p.Provider(),
				ObservedAt: observedAt,
				Sessions:   sessions,
			}, r.now()) {
				r.transmit()
			}
		}(poller)
	}
}

// badRequestError marks a payload the producer got wrong: malformed JSON, or a
// missing required field. The HTTP handler turns it into a 400.
type badRequestError struct{ err error }

func (e *badRequestError) Error() string { return e.err.Error() }
func (e *badRequestError) Unwrap() error { return e.err }

// Run blocks until ctx is cancelled, serving both ingest listeners, sweeping
// silent sessions, and transmitting a frame whenever the state changes.
//
// It never returns an error because the transport failed. A light that is off,
// asleep or out of range is expected, and exiting would look like a crash to
// the service manager, which would restart the relay in a loop. The only
// errors it returns are from binding the listeners, which is a configuration
// problem the operator must fix.
func (r *Relay) Run(ctx context.Context) error {
	// Claim the relay before binding anything. Two concurrent Runs would both
	// bind (port 0 makes that succeed), both overwrite r.listener and r.socket,
	// and the second listenSocket would unlink the socket file the first is
	// serving. Whichever returned first would then close the shared transport
	// and log file out from under the other.
	r.mu.Lock()
	if r.running || r.ran {
		r.mu.Unlock()
		return ErrAlreadyRunning
	}
	r.running = true
	r.mu.Unlock()

	// Whatever happens below, the claim is released and the log file closed on
	// the way out. A failed bind used to leave both behind: Status reported a
	// growing uptime for a relay that never ran, and the log file opened by
	// NewRelay stayed open.
	//
	// A Relay is ONE-SHOT: ran is set once the listeners are up, and it
	// makes every later Run return ErrAlreadyRunning. Without that, a second
	// Run bound, served, and wrote every log line into the descriptor this
	// teardown had already closed, losing the lot in silence, and it closed the
	// shared transport a second time while still using it. Guarding only
	// CONCURRENT Run left that sequential case wide open. The claim is NOT
	// latched on a failed bind: nothing was opened or closed in that case, so
	// the operator may fix the address and call Run again.
	defer func() {
		r.mu.Lock()
		r.running = false
		r.started = time.Time{}
		file := r.logFile
		r.logFile = nil
		r.mu.Unlock()
		if file != nil {
			file.Close()
		}
	}()

	httpListener, err := net.Listen("tcp", r.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("relay: listen on %s: %w", r.cfg.ListenAddr, err)
	}
	socketListener, err := listenSocket(r.cfg.SocketPath)
	if err != nil {
		httpListener.Close()
		return err
	}

	// started is set only now that both listeners are bound. Setting it before
	// the binds meant a relay that failed to start still reported uptime.
	r.mu.Lock()
	r.started = r.now()
	r.listener = httpListener
	r.socket = socketListener
	// Both listeners are up, so this Run will reach the teardown that closes
	// the log file and the transport. Latch the one-shot claim here rather than
	// at entry, so a failed bind still leaves the relay startable.
	r.ran = true
	r.mu.Unlock()

	r.logger.Printf("listening on http://%s and %s", httpListener.Addr(), r.cfg.SocketPath)

	var wg sync.WaitGroup

	// The sender owns the transport from here until shutdown. It starts before
	// the listeners so the first request can never find wake nil and fall back
	// to sending inline on its own goroutine.
	//
	// senderCtx is separate from ctx so the sender outlives the run loop: the
	// listeners are still draining after loop returns, and a request finishing
	// during the drain must still be able to signal.
	senderCtx, stopSender := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	r.mu.Lock()
	r.wake = wake
	r.mu.Unlock()

	var senderWG sync.WaitGroup
	senderWG.Add(1)
	go func() {
		defer senderWG.Done()
		r.sender(senderCtx, wake)
	}()

	// Every deadline is set, not just the header one. A client that sends
	// complete headers and then one byte of a promised body would otherwise
	// hold a goroutine and a descriptor until the process exits.
	timeouts := r.cfg.httpTimeouts.withDefaults()
	server := &http.Server{
		Handler:           r.httpHandler(),
		ReadHeaderTimeout: timeouts.readHeader,
		ReadTimeout:       timeouts.read,
		WriteTimeout:      timeouts.write,
		IdleTimeout:       timeouts.idle,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		// ErrServerClosed is the expected result of shutdown, not a failure.
		if err := server.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.logger.Printf("http listener stopped: %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.serveSocket(ctx, socketListener)
	}()

	// Connect is allowed to block and retry internally. It runs in its own
	// goroutine so a light that never appears cannot stop ingest from
	// working: the relay is still useful with no light attached.
	//
	// It shares the in-flight guard with the run loop's reconnect, so a slow
	// first connect and a reconnect tick can never open the device twice.
	r.startConnect(ctx, &wg, false)

	r.loop(ctx)

	// Shutdown: stop accepting, drain, then release the transport and the
	// socket file.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		r.logger.Printf("http shutdown: %v", err)
	}
	socketListener.Close()
	wg.Wait()

	// The listeners have drained, so no new signal can arrive. Clear wake
	// before stopping the sender: a transmit racing this point then sends
	// inline rather than signalling a goroutine that is about to exit, so the
	// frame is never silently dropped.
	r.mu.Lock()
	r.wake = nil
	r.mu.Unlock()
	stopSender()
	senderWG.Wait()

	// Only now is the transport unused by any goroutine, so closing it cannot
	// race a send in flight.
	if err := r.cfg.Transport.Close(); err != nil {
		r.logger.Printf("transport close: %v", err)
	}

	// The socket file is NOT removed here. socketListener.Close above already
	// unlinked it, so an unconditional remove could only delete a file some
	// other process created. That is reachable: a restart starts a new relay
	// which binds the same path while this one is still draining in wg.Wait,
	// and the remove would silently kill the new relay's ingest. A stale file
	// from a crash is handled where it belongs, by listenSocket at startup.

	// The log file and the running claim are released by the deferred
	// teardown, so an early return above frees them too.
	return nil
}

// loop drives the sweep ticker and the reconnect poll until ctx is cancelled.
// Every transmission decision funnels through here, so "frames are only sent
// on change" is enforced in exactly one place.
func (r *Relay) loop(ctx context.Context) {
	// reconnectWG lets Run's caller-facing behaviour stay simple while still
	// not leaking a Connect goroutine past loop's return.
	var reconnectWG sync.WaitGroup
	defer reconnectWG.Wait()

	sweep := time.NewTicker(r.cfg.SweepInterval)
	defer sweep.Stop()
	reconnect := time.NewTicker(reconnectPollInterval)
	defer reconnect.Stop()

	// pollWG keeps a poll's goroutine from outliving the loop, the same way
	// reconnectWG does for Connect.
	var pollWG sync.WaitGroup
	defer pollWG.Wait()

	poll := newPollTicker(r.cfg.PollInterval, len(r.cfg.Pollers) > 0)
	defer poll.Stop()

	// Sync once at startup, before waiting out the first interval. A relay
	// holds state in memory and loses it on restart, and frames are sent only
	// on change, so a session merely sitting idle is invisible until it next
	// moves. RFC 1 section 5.4 asks producers to sync on start for exactly
	// this; the relay does it on their behalf for the ones it can ask.
	r.pollAll(ctx, &pollWG)

	wasConnected := r.cfg.Transport.Connected()

	for {
		select {
		case <-ctx.Done():
			return

		case <-sweep.C:
			if r.tracker.Sweep(r.now()) {
				r.transmit()
			}

		case <-poll.C:
			r.pollAll(ctx, &pollWG)

		case <-reconnect.C:
			connected := r.cfg.Transport.Connected()

			// This is the ONE place a reconnect is announced and the frame is
			// pushed. startConnect used to do the same thing on a successful
			// retry, so a single reconnect logged the identical line twice and
			// transmitted twice. Owning it here covers every way the transport
			// can come back, including one that recovers with no Connect from
			// this relay at all.
			if connected && !wasConnected {
				r.logger.Printf("transport %s reconnected, resending frame", r.cfg.Transport.Name())
				r.transmit()
			}
			wasConnected = connected

			// A connected light that does not hold the current frame must get
			// it, whether or not an edge was seen. This covers the cases the
			// edge above misses: a Send that failed while the link stayed up, a
			// light that power-cycled without a Connect from this relay, and the
			// first frame after startup, which no edge announces because the
			// transport came up before the loop sampled it. transmit coalesces,
			// so a settled light costs one atomic read a tick.
			if connected && !r.acked.Load() {
				r.transmit()
			}

			// Nothing above ever reconnects on its own. Transport.Connect is
			// called once at startup, and a transport that drops after a
			// failed Send stays disconnected forever unless something calls
			// Connect again. That something is here.
			if !connected {
				r.startConnect(ctx, &reconnectWG, true)
			}
		}
	}
}

// startConnect launches a Transport.Connect attempt unless one is already in
// flight. It serves both the first connect at startup and every later
// reconnect; retry distinguishes the two for logging and for whether a frame
// is pushed on success.
//
// Before this existed, Connect had exactly one call site that ran once at
// startup. A serial transport that fails a Send drops its handle, so
// Connected reported false forever and the light stayed dark until the process
// restarted. The reconnect ticker only observed the flag; nothing acted on it.
//
// It never blocks the run loop: Connect retries internally with its own
// backoff and can sit there for the life of the process waiting for a light
// that is unplugged. Running it on the loop goroutine would stop the sweep and
// stall ingest's transmit path.
//
// A failed attempt is logged and nothing else. The relay never exits because
// the transport failed, and that rule covers reconnection too: the next tick
// that still sees a disconnected transport starts a fresh attempt.
func (r *Relay) startConnect(ctx context.Context, wg *sync.WaitGroup, retry bool) {
	// CompareAndSwap is the whole guard. Connect can outlast many ticks, and
	// starting a second one would open the device twice.
	if !r.reconnecting.CompareAndSwap(false, true) {
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer r.reconnecting.Store(false)

		err := r.cfg.Transport.Connect(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			// A returned error means the transport gave up, not that we
			// should. Log it; the next tick tries again.
			r.logger.Printf("transport %s connect failed: %v", r.cfg.Transport.Name(), err)
			return
		}
		if !retry {
			return
		}

		// A nil error is not proof the light is back. A transport may return
		// success from Connect and still report Connected false, and when it
		// does, the next tick starts another attempt: announcing "reconnected"
		// on each of them logged the line once a second forever and resent the
		// frame just as often, while the lamp was never actually reachable.
		if !r.cfg.Transport.Connected() {
			return
		}

		// The light is back, but the resend is NOT done here. The run loop
		// watches the same disconnected-to-connected edge, and when both acted
		// on it a single reconnect logged the identical line twice and put the
		// frame on the wire twice. The loop is the better place to own it: it
		// sees every way the transport can come back, including one that
		// recovers with no Connect from here, whereas this path sees only its
		// own attempts. Leaving it to the loop costs at most one poll interval
		// and keeps the announcement in exactly one place.
	}()
}

// transmit asks for the current frame to reach the light. It does NOT send:
// it signals the sender goroutine, which reads the frame at send time.
//
// Every ingest path runs on its own goroutine, one per HTTP request and one
// per socket connection. When those called Send directly, two sends overlapped
// and landed in whatever order the transport finished them in. A session going
// working then blocked could put the red frame on the wire first and the stale
// yellow one second, leaving the lamp yellow while a session was blocked.
// Nothing corrected it, because the next Apply reports no change and so never
// transmits.
//
// Signalling instead of sending fixes the ordering at the root: one goroutine
// owns the transport, so sends cannot overlap, and it reads tracker.Frame() at
// send time, so what it writes is the state as of the moment it wrote it,
// never an older one.
//
// The signal is non-blocking. An ingest handler must never wait on a radio.
func (r *Relay) transmit() {
	// The frame moved, so the light no longer holds the current one. Clear the
	// ack before signalling: if the send that follows fails, the reconnect tick
	// must still see an unacked frame and retry it.
	r.acked.Store(false)

	r.mu.Lock()
	wake := r.wake
	inline := r.sendInline
	r.mu.Unlock()

	if wake == nil {
		// No sender. This used to fall through to an inline r.send(), justified
		// as a test-only path, but the window is reachable in production: Run
		// clears wake before stopping the sender, so a request finishing in
		// that window would block an HTTP handler on a radio write, which is
		// the exact ordering bug the sender goroutine exists to prevent.
		//
		// Sending inline is now opt-in, set only by tests that drive the
		// handlers with no Run behind them. Production leaves it false, so a
		// signal with no sender is dropped: the listeners have already drained
		// at that point, and the sender's own shutdown drain writes the final
		// frame.
		if inline {
			r.send()
		}
		return
	}

	select {
	case wake <- struct{}{}:
	default:
		// A signal is already pending. The sender has not yet read the frame,
		// so it will pick up this change too. Coalescing rapid changes into
		// one send is the point, not a compromise.
	}
}

// sender owns the transport for the life of Run. It is the only goroutine that
// calls Send, which is what serialises transmission.
//
// It reads the frame at send time rather than being handed one, so a frame
// cannot go stale between the change that triggered it and the write.
func (r *Relay) sender(ctx context.Context, wake <-chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			// Drain one last signal so a change made just before shutdown
			// still reaches the light. The frame is read now, so this writes
			// the final state rather than whatever triggered the signal.
			select {
			case <-wake:
				r.send()
			default:
			}
			return

		case <-wake:
			r.send()
		}
	}
}

// send writes the current frame to the transport. A failed send means this
// frame was lost, not that the transport is dead, so the error is logged and
// the relay carries on.
//
// It records whether the light now holds the current frame. A success marks it
// acked, so the reconnect tick leaves a settled light alone; a failure leaves
// it unacked, so the next tick resends rather than waiting for the tracker to
// move. That is what turns a lost frame from a permanent stale colour into a
// one-tick delay.
func (r *Relay) send() {
	if err := r.cfg.Transport.Send(r.tracker.Frame()); err != nil {
		r.acked.Store(false)
		r.logger.Printf("send frame: %v", err)
		return
	}
	r.acked.Store(true)
}

// Status returns a snapshot of what the relay is doing.
func (r *Relay) Status() Status {
	r.mu.Lock()
	started := r.started
	r.mu.Unlock()

	var uptime time.Duration
	if !started.IsZero() {
		uptime = r.now().Sub(started)
	}

	return Status{
		Transport: r.cfg.Transport.Name(),
		Connected: r.cfg.Transport.Connected(),
		Sessions:  r.tracker.Sessions(),
		Aggregate: r.tracker.Aggregate(),
		Uptime:    uptime,
	}
}
