package notify

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// testServer starts a loopback server with the given handler and returns its
// host:port, which is the form Send takes.
func testServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func TestSendPostsTheReport(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotType   string
		gotBody   []byte
	)
	addr := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	report := stoplight.Report{
		SessionID: "a1b2c3",
		Event:     "blocked",
		Label:     "auth-api",
		Provider:  "deepseek",
		Detail:    "waiting for approval",
		Cwd:       "/tmp/x",
	}
	if err := Send(addr, report); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/session" {
		t.Errorf("path = %q, want /v1/session", gotPath)
	}
	if gotType != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotType)
	}

	var decoded stoplight.Report
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode body %q: %v", gotBody, err)
	}
	if decoded != report {
		t.Errorf("report round-trip = %+v, want %+v", decoded, report)
	}
}

// Optional fields are omitted rather than sent empty, so the wire form matches
// the examples in RFC 1 section 5.
func TestSendOmitsEmptyOptionalFields(t *testing.T) {
	var body []byte
	addr := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))

	if err := Send(addr, stoplight.Report{SessionID: "a", Event: "started"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"label", "provider", "detail", "cwd"} {
		if _, present := fields[key]; present {
			t.Errorf("empty %q was sent, want it omitted", key)
		}
	}
	if fields["session_id"] != "a" || fields["event"] != "started" {
		t.Errorf("required fields = %+v", fields)
	}
}

// The error is returned for tests. The CLI discards it, which is what keeps a
// producer from breaking its caller.
func TestSendReturnsErrorWhenRelayIsDown(t *testing.T) {
	// Bind a port then release it, so nothing is listening there.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	if err := Send(addr, stoplight.Report{SessionID: "a", Event: "blocked"}); err == nil {
		t.Fatal("Send to a closed port: want error, got nil")
	}
}

// A relay that hangs must not hold the caller past the timeout.
func TestSendTimesOut(t *testing.T) {
	// The handler blocks until released. The release must be registered
	// after the server, so cleanup unblocks the handler before Close waits
	// on it: t.Cleanup runs last-in-first-out.
	release := make(chan struct{})
	addr := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release) })

	start := time.Now()
	err := Send(addr, stoplight.Report{SessionID: "a", Event: "blocked"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send against a hanging relay: want error, got nil")
	}
	// The assertion is that Send is bounded at all, not that it is fast. The
	// bug it guards against is a missing timeout, which does not return until
	// the handler is released at cleanup, so any finite budget catches it.
	//
	// The budget is a large multiple of the 250ms Timeout because a tight one
	// measures how busy the machine is: under -race with the rest of the suite
	// running, the request setup either side of the timeout is itself worth
	// tens of milliseconds, and the whole process can be descheduled for
	// longer than that.
	if elapsed > 15*time.Second {
		t.Errorf("Send took %v, want it bounded near %v", elapsed, Timeout)
	}
}

func TestSendRejectsInvalidReport(t *testing.T) {
	addr := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an invalid report was sent to the relay")
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name   string
		report stoplight.Report
	}{
		{"no session_id", stoplight.Report{Event: "blocked"}},
		{"no event", stoplight.Report{SessionID: "a"}},
		{"neither", stoplight.Report{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Send(addr, tc.report); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

// A non-204 response is reported, so tests can tell a rejected payload from an
// accepted one.
func TestSendReportsNon204(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusMethodNotAllowed, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			addr := testServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "nope", code)
			}))
			if err := Send(addr, stoplight.Report{SessionID: "a", Event: "blocked"}); err == nil {
				t.Errorf("status %d: want error, got nil", code)
			}
		})
	}
}

// A garbage address must return an error rather than panicking: the caller is
// about to discard it and carry on.
func TestSendHandlesBadAddress(t *testing.T) {
	for _, addr := range []string{"", "not a host", "http://double-scheme:1", "127.0.0.1:not-a-port"} {
		t.Run(addr, func(t *testing.T) {
			// The only requirement is that it returns rather than panics.
			_ = Send(addr, stoplight.Report{SessionID: "a", Event: "blocked"})
		})
	}
}

// The timeout matches what RFC 1 section 6 asks producers to use.
func TestTimeoutMatchesRFC(t *testing.T) {
	if Timeout != 250*time.Millisecond {
		t.Errorf("Timeout = %v, want 250ms", Timeout)
	}
}
