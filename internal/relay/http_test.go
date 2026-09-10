package relay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitForAddr returns the bound HTTP address once Run has started listening.
// The relay is configured with port 0 in tests, so the real port is only known
// after the bind.
// The budget is generous on purpose. Binding is normally instant, so a long
// deadline costs nothing on a passing run: this returns as soon as the listener
// appears. It only matters on a loaded machine, where a relay that binds a
// socket path another relay is still draining can legitimately take seconds. A
// tight 5s budget failed exactly that case as "never bound a listener", which
// blamed the relay for the scheduler.
func waitForAddr(t *testing.T, r *Relay) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		r.mu.Lock()
		listener := r.listener
		r.mu.Unlock()
		if listener != nil {
			return listener.Addr().String()
		}
		select {
		case <-deadline:
			t.Fatal("relay never bound a listener")
		case <-time.After(time.Millisecond):
		}
	}
}

// postSession posts a body to the ingest endpoint and returns the status code.
//
// It reports a failed post with Errorf and returns 0 rather than calling
// Fatalf. Fatalf must run on the goroutine that calls the test function; from
// any other goroutine it stops that goroutine without reliably failing the
// test, so a post failing inside a worker would be swallowed. Errorf is safe
// from anywhere, and 0 matches no expected status, so every caller still fails.
func postSession(t *testing.T, addr, body string) int {
	t.Helper()
	resp, err := http.Post("http://"+addr+SessionPath, "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("post: %v", err)
		return 0
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// netDial connects to a unix socket, used to prove one is or is not live.
func netDial(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, time.Second)
}

// writeFile creates an empty regular file, standing in for a socket path left
// behind by a crash.
func writeFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// osStat is a thin alias so the socket tests can check permissions without
// importing os for one call.
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

// tempSocket returns a socket path inside a short-lived temporary directory.
// Any extra elements are appended without being created, so a caller can test
// that listenSocket makes them itself.
//
// It deliberately avoids t.TempDir: a unix socket path is capped at around 104
// bytes on macOS, and the directory names the testing package generates
// overrun that limit on their own.
func tempSocket(t *testing.T, elem ...string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sl")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(append([]string{dir}, elem...)...)
}

// newHandlerRelay builds a relay for handler-level tests, with no listeners
// running. The handler is exercised through httptest, so nothing binds.
func newHandlerRelay(t *testing.T) (*Relay, *fakeTransport) {
	t.Helper()
	tr := &fakeTransport{connected: true}
	r, err := NewRelay(Config{
		Transport:  tr,
		ListenAddr: "127.0.0.1:0",
		SocketPath: tempSocket(t),
		LogPath:    t.TempDir() + "/relay.log",
	})
	if err != nil {
		t.Fatalf("NewRelay: %v", err)
	}
	// There is no Run here, so no sender goroutine. Ask transmit to write on
	// the calling goroutine, which is what lets these tests observe frames.
	// Production never sets this: an ingest handler must not block on a radio.
	r.sendInline = true
	return r, tr
}

// The response code matrix from RFC 1 section 6.
func TestHandleSessionStatusCodes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid blocked", `{"session_id":"a","event":"blocked"}`, http.StatusNoContent},
		{"valid started with label", `{"session_id":"a","event":"started","label":"api"}`, http.StatusNoContent},
		{"all optional fields", `{"session_id":"a","event":"idle","label":"l","provider":"p","detail":"d","cwd":"/tmp"}`, http.StatusNoContent},

		{"malformed json", `{"session_id":`, http.StatusBadRequest},
		{"not an object", `"hello"`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"missing session_id", `{"event":"blocked"}`, http.StatusBadRequest},
		{"empty session_id", `{"session_id":"","event":"blocked"}`, http.StatusBadRequest},
		{"missing event", `{"session_id":"a"}`, http.StatusBadRequest},
		{"empty event", `{"session_id":"a","event":""}`, http.StatusBadRequest},

		// Unknown events are accepted and ignored, per RFC 1 section 11.
		{"unknown event", `{"session_id":"a","event":"exploded"}`, http.StatusNoContent},
		{"internal timeout event is not accepted over the wire", `{"session_id":"a","event":"timeout"}`, http.StatusNoContent},

		// Unknown fields are ignored, never rejected.
		{"unknown field", `{"session_id":"a","event":"blocked","future_field":42}`, http.StatusNoContent},
		{"nested unknown field", `{"session_id":"a","event":"blocked","meta":{"x":[1,2]}}`, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			req := httptest.NewRequest(http.MethodPost, SessionPath, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			r.httpHandler().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.want, rec.Body.String())
			}
			// Success MUST carry no body.
			if rec.Code == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Errorf("204 carried a body %q, want empty", rec.Body.String())
			}
		})
	}
}

// Every method other than POST is a 405.
func TestHandleSessionMethodNotAllowed(t *testing.T) {
	for _, method := range []string{
		http.MethodGet, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			req := httptest.NewRequest(method, SessionPath, nil)
			rec := httptest.NewRecorder()

			r.httpHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
		})
	}
}

// A body over 8KB is rejected, per RFC 1 section 12.
//
// The junk has to be tried both inside and outside the JSON. Only the first
// was covered before, and it passes even with no working cap at all: the
// oversized string makes the object itself unparseable, so the decoder fails
// on its own. Moving the junk outside the object is the real test, because
// there the decoder finds a complete value and stops, which is exactly how the
// cap was bypassed.
func TestHandleSessionBodyTooLarge(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{
			// Over the cap and unparseable. MaxBytesReader trips mid-string,
			// so this is a 413.
			name: "oversize inside the json",
			body: `{"session_id":"a","event":"blocked","label":"` + strings.Repeat("x", MaxBodyBytes+100) + `"}`,
			want: http.StatusRequestEntityTooLarge,
		},
		{
			// A complete valid object, then junk. The decoder stops at the
			// closing brace, so this used to be a 204 with the report applied.
			// Draining to EOF now rejects it: the trailing bytes are not valid
			// JSON, so it is a 400 rather than a 413.
			name: "valid object then junk",
			body: `{"session_id":"a","event":"blocked"}` + strings.Repeat("x", MaxBodyBytes+100),
			want: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, tr := newHandlerRelay(t)
			req := httptest.NewRequest(http.MethodPost, SessionPath, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			r.httpHandler().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
			if tr.sendCount() != 0 {
				t.Error("an oversized body reached the transport")
			}
			// The status code alone would not catch a body that was rejected
			// AND applied, so check the tracker too.
			if got := r.Status().Sessions; len(got) != 0 {
				t.Errorf("tracker holds %d session(s), want 0", len(got))
			}
		})
	}
}

// A body just under the cap is still accepted, so the limit is not off by
// enough to reject legitimate reports.
func TestHandleSessionBodyUnderLimitAccepted(t *testing.T) {
	prefix := `{"session_id":"a","event":"blocked","label":"`
	suffix := `"}`
	fill := MaxBodyBytes - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("x", fill) + suffix
	if len(body) != MaxBodyBytes {
		t.Fatalf("test body is %d bytes, want exactly %d", len(body), MaxBodyBytes)
	}

	r, _ := newHandlerRelay(t)
	req := httptest.NewRequest(http.MethodPost, SessionPath, strings.NewReader(body))
	rec := httptest.NewRecorder()

	r.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for a body at exactly the cap", rec.Code)
	}
}

// An unknown event must not create a session or change the aggregate.
func TestUnknownEventLeavesStateUnchanged(t *testing.T) {
	r, _ := newHandlerRelay(t)
	handler := r.httpHandler()

	post := func(body string) {
		req := httptest.NewRequest(http.MethodPost, SessionPath, strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	}

	post(`{"session_id":"ghost","event":"levitating"}`)

	st := r.Status()
	if len(st.Sessions) != 0 {
		t.Errorf("sessions = %d, want 0: an unknown event must not create one", len(st.Sessions))
	}
	if st.Aggregate != 0 {
		t.Errorf("aggregate = %v, want off", st.Aggregate)
	}
}

// An unknown field must be ignored while the known ones still apply.
func TestUnknownFieldsIgnoredButReportApplied(t *testing.T) {
	r, _ := newHandlerRelay(t)
	req := httptest.NewRequest(http.MethodPost, SessionPath,
		strings.NewReader(`{"session_id":"a","event":"blocked","label":"api","v2_priority":9,"tags":["x"]}`))
	rec := httptest.NewRecorder()

	r.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	st := r.Status()
	if len(st.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(st.Sessions))
	}
	if st.Sessions[0].Label != "api" {
		t.Errorf("label = %q, want api", st.Sessions[0].Label)
	}
}

// An unrelated path is a 404, so the version prefix means something.
func TestUnknownPathNotFound(t *testing.T) {
	r, _ := newHandlerRelay(t)
	req := httptest.NewRequest(http.MethodPost, "/v2/session", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()

	r.httpHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// Concurrent posts must not race, because two listeners and a ticker share the
// tracker. Run with -race for this to mean anything.
func TestConcurrentIngest(t *testing.T) {
	r := newTestRelay(t, &fakeTransport{connected: true})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	addr := waitForAddr(t, r)

	const workers = 8
	ready := make(chan struct{})
	errs := make(chan error, workers)
	for w := range workers {
		go func(w int) {
			<-ready
			for range 25 {
				body := `{"session_id":"s` + string(rune('a'+w)) + `","event":"started"}`
				resp, err := http.Post("http://"+addr+SessionPath, "application/json", strings.NewReader(body))
				if err != nil {
					errs <- err
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			errs <- nil
		}(w)
	}
	close(ready)
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent post: %v", err)
		}
	}

	if got := len(r.Status().Sessions); got != workers {
		t.Errorf("sessions = %d, want %d", got, workers)
	}

	cancel()
	<-done
}
