package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// socketReadTimeout bounds how long one connection may take to deliver its
// object. A producer that opens the socket and says nothing must not pin a
// goroutine forever.
const socketReadTimeout = 5 * time.Second

// ingestLogInterval is the shortest gap between two socket ingest failure
// lines. Anything rejected in between is counted and reported with the next
// line, so a flood costs one line a minute rather than one per connection.
const ingestLogInterval = time.Minute

// logIngestFailure reports a rejected socket payload at most once per
// ingestLogInterval, folding everything suppressed in between into a count.
//
// Logging every failure was a disk-fill vector: the socket is writable by any
// local process, so a loop that connects and sends junk wrote an unbounded
// number of lines. Dropping the log entirely would hide a genuinely broken
// producer, so the rate is capped instead.
func (r *Relay) logIngestFailure(err error) {
	now := time.Now()

	r.ingestLogMu.Lock()
	r.ingestLogDropped++
	if !r.ingestLogAt.IsZero() && now.Sub(r.ingestLogAt) < ingestLogInterval {
		r.ingestLogMu.Unlock()
		return
	}
	dropped := r.ingestLogDropped
	r.ingestLogDropped = 0
	r.ingestLogAt = now
	r.ingestLogMu.Unlock()

	if dropped > 1 {
		r.logger.Printf("socket ingest: %v (%d rejected in the last %v)", err, dropped, ingestLogInterval)
		return
	}
	r.logger.Printf("socket ingest: %v", err)
}

// bodyTooLargeError reports a payload past MaxBodyBytes. It is the socket's
// counterpart to http.MaxBytesError: both mean "over the cap", and both must
// stay distinguishable from a merely malformed body so the size limit is
// reported as a size limit.
type bodyTooLargeError struct{ limit int64 }

func (e *bodyTooLargeError) Error() string {
	return fmt.Sprintf("payload exceeds %d bytes", e.limit)
}

// limitedReader reads at most remaining bytes and then FAILS, where
// io.LimitReader would quietly report EOF. That difference is the whole point:
// a silent EOF turns an oversized payload into a valid short one, which is how
// the 8KB cap was bypassed on the socket.
type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, &bodyTooLargeError{limit: MaxBodyBytes}
	}
	// Read one byte past the budget when possible, so passing the cap is
	// detected on this call rather than only on the next one.
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, &bodyTooLargeError{limit: MaxBodyBytes}
	}
	return n, err
}

// listenSocket binds the unix socket at path, creating parent directories and
// removing a stale socket file left by a previous run.
//
// Removing the stale file is safe because a live relay holds the path open and
// would have failed to bind; a file left behind is by definition from a
// process that is gone.
func listenSocket(path string) (net.Listener, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("relay: create socket directory: %w", err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("relay: remove stale socket: %w", err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("relay: listen on socket %s: %w", path, err)
	}
	// The socket carries only a colour and a short label, but it can drive
	// the light, so keep it to the owning user.
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("relay: chmod socket: %w", err)
	}
	return listener, nil
}

// serveSocket accepts connections until ctx is cancelled or the listener is
// closed. One JSON object per connection, no response body, per RFC 1 section
// 4.2.
func (r *Relay) serveSocket(ctx context.Context, listener net.Listener) {
	var wg sync.WaitGroup
	defer wg.Wait()

	var delay time.Duration

	for {
		conn, err := listener.Accept()
		if err != nil {
			// A closed listener is how shutdown stops this loop, and a
			// cancelled context means the same. Neither is a failure.
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}

			// Anything else gets a backoff before the next attempt. Not every
			// accept error is transient: a process at its descriptor limit
			// gets EMFILE from every call, and an unpaced loop would spin a
			// core and flood the log until ctx was cancelled.
			delay = nextAcceptDelay(delay)
			r.logger.Printf("socket accept: %v (retrying in %v)", err, delay)

			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}

		// A connection got through, so whatever the trouble was has passed.
		delay = 0

		wg.Add(1)
		go func() {
			defer wg.Done()
			r.handleConn(ctx, conn)
		}()
	}
}

// Accept retry pacing. The first wait is short enough to be invisible when the
// error really was transient, and the cap keeps a permanent failure to roughly
// one log line a second.
const (
	minAcceptDelay = 5 * time.Millisecond
	maxAcceptDelay = time.Second
)

// nextAcceptDelay doubles current, starting at minAcceptDelay and stopping at
// maxAcceptDelay. A zero or negative current means this is the first failure
// since the last successful accept.
func nextAcceptDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return minAcceptDelay
	}
	next := current * 2
	if next > maxAcceptDelay {
		return maxAcceptDelay
	}
	return next
}

// handleConn reads one report from conn and closes it. It shares the decode
// and apply path with the HTTP handler, so the two listeners can never
// disagree about what a payload means.
//
// There is no response: the producer is a shell hook that has already moved
// on, and RFC 1 section 6 tells producers to ignore the response anyway. A
// malformed payload is logged rather than reported back.
//
// ctx is the shutdown signal. The read deadline alone is an ABSOLUTE five
// seconds, and cancelling a context does not interrupt a blocked read, so a
// single connection that opened and said nothing used to hold Run's wg.Wait
// for the remainder of those five seconds. A supervisor with a shorter stop
// timeout would SIGKILL the relay in that window, skipping Transport.Close and
// leaving the serial port held. Watching ctx here is what bounds shutdown.
func (r *Relay) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(socketReadTimeout)); err != nil {
		r.logger.Printf("socket deadline: %v", err)
		return
	}

	// On cancellation, pull the deadline into the past. That makes the pending
	// read return immediately with a timeout, which is the only way to unblock
	// a goroutine already sitting in Read. The watcher stops as soon as this
	// connection is done, so it cannot outlive the handler.
	handled := make(chan struct{})
	defer close(handled)
	go func() {
		select {
		case <-ctx.Done():
			conn.SetReadDeadline(time.Now())
		case <-handled:
		}
	}()

	// The same 8KB cap as HTTP, but it must ERROR rather than truncate. An
	// io.LimitReader simply reports EOF at the limit, which made an oversized
	// payload look like a short one: the decoder read a valid object out of the
	// prefix, saw a clean end, and the relay applied it. The cap has to be a
	// rejection, not a trim.
	body := &limitedReader{r: conn, remaining: MaxBodyBytes}

	changed, err := r.ingest(body)
	if err != nil {
		// Rate limited. This used to log one line per malformed payload, and
		// RFC 1 section 12 makes the socket writable by any local process, so
		// any such process could fill the disk one connection at a time. HTTP
		// logs nothing at all for the same failure; a summary keeps the two
		// roughly consistent while still leaving evidence of a misbehaving
		// producer.
		r.logIngestFailure(err)
		return
	}
	if changed {
		r.transmit()
	}
}
