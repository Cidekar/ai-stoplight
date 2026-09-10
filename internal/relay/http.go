package relay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// The served paths. The protocol versions through the prefix, so a v2 relay
// would serve both generations side by side.
const (
	// SessionPath is the ingest endpoint from RFC 1 section 4.1.
	SessionPath = "/v1/session"

	// StatusPath is the read-only snapshot `stoplight status` prints.
	StatusPath = "/v1/status"

	// TaskPath sets an explicit screen label, as `stoplight task` does.
	TaskPath = "/v1/task"
)

// httpHandler builds the mux. Only the paths above are served: anything else
// is a 404 from the mux itself.
func (r *Relay) httpHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(SessionPath, r.handleSession)
	mux.HandleFunc(StatusPath, r.handleStatus)
	mux.HandleFunc(TaskPath, r.handleTask)
	return mux
}

// requireMethod writes a 405 with an Allow header unless the request used
// want, reporting whether the handler should carry on. Every endpoint goes
// through it, so the three can never disagree about the shape of a 405.
func requireMethod(w http.ResponseWriter, req *http.Request, want string) bool {
	if req.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// readBody caps the request body at MaxBodyBytes and returns a reader for it.
// MaxBytesReader makes the decoder fail once the limit is passed, so an
// oversized request never reaches the tracker.
func readBody(w http.ResponseWriter, req *http.Request) io.Reader {
	return http.MaxBytesReader(w, req.Body, MaxBodyBytes)
}

// writeIngestError maps an error from a decode-and-apply path onto a status
// code. It is shared so that every endpoint reports the same failure the same
// way, and so 413 is never accidentally reported as 400.
func writeIngestError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request", http.StatusBadRequest)
}

// decodeJSON reads one JSON object from body into value. The decoder does not
// reject unknown fields: RFC 1 section 5 requires them to be ignored so a
// producer written against a later spec keeps working against this relay.
//
// It then requires EOF. Decode stops at the first complete value and never
// touches the rest, so on its own it made the 8KB cap meaningless: a valid
// object followed by a megabyte of junk decoded cleanly, MaxBytesReader was
// never read past its limit and so never tripped, and the request was applied.
// Reading on until EOF is what makes the cap bound the REQUEST rather than
// just the JSON prefix, which is what RFC 1 section 12 asks for. It also
// rejects a second document in one body, which decoding once would drop.
//
// The error is classified as a badRequestError so callers can tell a payload
// the producer got wrong from a body that was simply too large. A
// MaxBytesError is deliberately NOT wrapped that way: writeIngestError must
// still see it through errors.As to report 413 rather than 400.
func decodeJSON(body io.Reader, value any) error {
	dec := json.NewDecoder(body)
	if err := dec.Decode(value); err != nil {
		return classifyDecodeError(err)
	}
	// Draining to EOF is what enforces the size cap, so the error from the
	// drain matters as much as the one from the decode.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		if err == nil {
			return &badRequestError{err: errors.New("unexpected data after the JSON object")}
		}
		return classifyDecodeError(err)
	}
	return nil
}

// classifyDecodeError keeps an over-limit body distinguishable from a
// malformed one. Everything else becomes a badRequestError, which the HTTP
// layer maps to 400 and the socket layer simply logs.
func classifyDecodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return err
	}
	var overSize *bodyTooLargeError
	if errors.As(err, &overSize) {
		return err
	}
	return &badRequestError{err: err}
}

// handleSession implements POST /v1/session, per RFC 1 sections 4.1 and 6.
//
//	204  accepted, empty body
//	400  malformed JSON, or a missing required field
//	405  any method other than POST
//	413  body over 8KB
//
// An unknown event name is a 204, not a 400: RFC 1 section 11 requires a relay
// to ignore events it does not know, so a producer written against a later
// spec keeps working. Unknown JSON fields are ignored for the same reason.
func (r *Relay) handleSession(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	defer req.Body.Close()

	changed, err := r.ingest(readBody(w, req))
	if err != nil {
		writeIngestError(w, err)
		return
	}

	if changed {
		r.transmit()
	}

	// 204 carries no body, per RFC 1 section 6.
	w.WriteHeader(http.StatusNoContent)
}

// handleStatus implements GET /v1/status, the snapshot `stoplight status`
// prints. Before it existed the CLI could do no better than check whether the
// port accepted a connection, which says nothing about what the relay is
// doing.
//
//	200  a StatusResponse
//	405  any method other than GET
//
// It is read-only: it never touches the tracker's state, never transmits a
// frame, and cannot fail. A relay with no light attached and no sessions still
// answers, because "connected: false, sessions: []" is the answer.
func (r *Relay) handleStatus(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodGet) {
		return
	}

	body := r.Status().response()

	// Marshal before writing the header, so a marshalling failure is a 500
	// rather than a 200 with a truncated body. Color and time.Time both
	// marshal cleanly, so this is belt and braces.
	encoded, err := json.Marshal(body)
	if err != nil {
		r.logger.Printf("encode status: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(encoded); err != nil {
		r.logger.Printf("write status: %v", err)
	}
}

// handleTask implements POST /v1/task, which sets the explicit screen label
// `stoplight task` asks for.
//
//	204  accepted, empty body
//	400  malformed JSON, or a missing session_id
//	405  any method other than POST
//	413  body over 8KB
//
// An EMPTY label CLEARS the override, so the derived label shows again. That
// is how the command undoes itself without needing a second verb.
//
// A session that was never seen is NOT created. The request is still accepted
// rather than rejected, but a label carries no state: unlike an ingest report
// there is no light to lose, so creating a session here only invented a green
// one on an empty desk. SetOverride parks the name and applies it when the
// session first genuinely reports.
//
// The override reaches the screen only. Aggregation reads state and never
// labels, so naming a task cannot change what the lamp says.
func (r *Relay) handleTask(w http.ResponseWriter, req *http.Request) {
	if !requireMethod(w, req, http.MethodPost) {
		return
	}
	defer req.Body.Close()

	var task TaskRequest
	if err := decodeJSON(readBody(w, req), &task); err != nil {
		writeIngestError(w, err)
		return
	}
	if task.SessionID == "" {
		// Unlike an unknown event, a missing session_id names nothing at all.
		// There is no session to be lenient about, so this is a 400.
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if r.tracker.SetOverride(task.SessionID, task.Label, r.now()) {
		r.transmit()
	}

	// 204 carries no body, matching the ingest endpoint.
	w.WriteHeader(http.StatusNoContent)
}
