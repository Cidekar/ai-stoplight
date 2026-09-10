// Package notify is the one-shot client that posts a single report to the
// relay and gets out of the way.
//
// It exists to serve one rule from RFC 1 section 6: a producer must never
// break its caller. Everything here is built around that.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// Timeout bounds a post to the relay. RFC 1 section 6 asks producers for
// 250ms or less: the relay is on loopback, so anything slower means it is not
// really there, and the caller has real work to do.
const Timeout = 250 * time.Millisecond

// The relay endpoints. They are duplicated from the relay rather than
// imported: notify is a producer, and the dependency graph in design.md keeps
// producers from importing the relay.
const (
	// sessionPath is the ingest endpoint from RFC 1 section 4.1. It carries an
	// event, so posting to it runs a state transition.
	sessionPath = "/v1/session"

	// taskPath sets an explicit screen label. It carries no event, so it can
	// never move a session's state. That separation is the whole point of the
	// endpoint: naming the task you are waiting on must not clear the red
	// light you are waiting because of.
	taskPath = "/v1/task"
)

// Task is the body of a POST to taskPath. It mirrors the relay's TaskRequest,
// duplicated for the same reason the paths are.
type Task struct {
	SessionID string `json:"session_id"`

	// Label is the text to show in place of the derived label. An empty label
	// clears the override, so the derived label shows again.
	Label string `json:"label"`
}

// Send posts r to the relay listening at addr, which is a host:port such as
// "127.0.0.1:7373".
//
// The returned error is for tests only. Callers in the CLI MUST discard it and
// exit 0 on every path: relay down, port closed, socket missing, malformed
// input, disk full. A status light is an accessory, and a producer that fails
// because an accessory is missing has broken the thing it was decorating.
//
// This is the most important rule in RFC 1, and it is why the reference
// clients in section 13 all end with `|| true`.
func Send(addr string, r stoplight.Report) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return post(addr, sessionPath, r)
}

// SendTask sets the screen label for a session on the relay at addr.
//
// It posts to the task endpoint rather than to ingest, and the difference
// matters more than the path does. An ingest report must name an event, and
// every event runs a state transition; there is no event that means "only
// relabel". Sending `idle` to relabel a session would drive it to Idle, which
// is green, and a blocked session would lose its red light at the exact moment
// the human named the thing they are blocked on.
//
// The task endpoint carries no event. It sets the override and nothing else.
//
// An empty label clears the override, so the derived label shows again. Unlike
// Send, the returned error is meant to be reported: `stoplight task` is typed
// by a human who is owed an answer, not called from a hook that must not fail.
func SendTask(addr string, t Task) error {
	if t.SessionID == "" {
		return stoplight.ErrMissingSessionID
	}
	return post(addr, taskPath, t)
}

// post marshals body and POSTs it to path on the relay at addr, expecting the
// 204 both endpoints answer with. Send and SendTask share it so the two can
// never disagree about timeouts, headers or what counts as success.
func post(addr, path string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("notify: encode request: %w", err)
	}

	url := "http://" + addr + path
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("notify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: Timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify: post to %s: %w", addr, err)
	}
	// The body is empty on success and small on failure. Draining it is not
	// worth the read: this process is about to exit.
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("notify: relay returned %s", resp.Status)
	}
	return nil
}
