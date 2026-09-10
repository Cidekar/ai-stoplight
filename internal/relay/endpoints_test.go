package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// serve runs one request against the relay's mux and returns the recorder.
// Nothing binds, so nothing can hang.
func serve(t *testing.T, r *Relay, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.httpHandler().ServeHTTP(rec, req)
	return rec
}

// getStatus fetches and decodes GET /v1/status, failing the test on anything
// other than a 200 with a JSON body.
func getStatus(t *testing.T, r *Relay) StatusResponse {
	t.Helper()
	rec := serve(t, r, http.MethodGet, StatusPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status body %q: %v", rec.Body.String(), err)
	}
	return body
}

// postTask sets an override over HTTP and returns the status code.
func postTask(t *testing.T, r *Relay, body string) int {
	t.Helper()
	return serve(t, r, http.MethodPost, TaskPath, body).Code
}

// ingestOK posts a report to the ingest endpoint and requires a 204, so a test
// that is really about status or task fails loudly if its setup broke.
func ingestOK(t *testing.T, r *Relay, body string) {
	t.Helper()
	if code := serve(t, r, http.MethodPost, SessionPath, body).Code; code != http.StatusNoContent {
		t.Fatalf("ingest %s: status = %d, want 204", body, code)
	}
}

// --- GET /v1/status ---------------------------------------------------------

// The snapshot for zero, one and several sessions. A CLI reads this to print
// `stoplight status`, so every field has to be right.
func TestHandleStatusSessionCounts(t *testing.T) {
	tests := []struct {
		name    string
		ingests []string
		want    []StatusSession
		colour  stoplight.Color
	}{
		{
			name:   "no sessions",
			want:   []StatusSession{},
			colour: stoplight.ColorOff,
		},
		{
			name:    "one session",
			ingests: []string{`{"session_id":"a1","event":"blocked","label":"auth-api","provider":"claude-code"}`},
			want: []StatusSession{
				{ID: "a1", Label: "auth-api", State: "needs you", Color: stoplight.ColorRed, Provider: "claude-code"},
			},
			colour: stoplight.ColorRed,
		},
		{
			name: "several sessions",
			ingests: []string{
				`{"session_id":"a1","event":"started","label":"auth-api","provider":"claude-code"}`,
				`{"session_id":"b2","event":"blocked","label":"payments","provider":"ci"}`,
				`{"session_id":"c3","event":"finished","label":"docs"}`,
			},
			want: []StatusSession{
				{ID: "a1", Label: "auth-api", State: "working", Color: stoplight.ColorYellow, Provider: "claude-code"},
				{ID: "b2", Label: "payments", State: "needs you", Color: stoplight.ColorRed, Provider: "ci"},
				{ID: "c3", Label: "docs", State: "done", Color: stoplight.ColorGreen},
			},
			colour: stoplight.ColorRed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			for _, body := range tt.ingests {
				ingestOK(t, r, body)
			}

			got := getStatus(t, r)

			if got.Transport != "fake" {
				t.Errorf("transport = %q, want fake", got.Transport)
			}
			if !got.Connected {
				t.Error("connected = false, want true")
			}
			if got.Aggregate != tt.colour {
				t.Errorf("aggregate = %v, want %v", got.Aggregate, tt.colour)
			}
			if len(got.Sessions) != len(tt.want) {
				t.Fatalf("sessions = %d, want %d (%+v)", len(got.Sessions), len(tt.want), got.Sessions)
			}
			for i, want := range tt.want {
				session := got.Sessions[i]
				if session.ID != want.ID || session.Label != want.Label ||
					session.State != want.State || session.Color != want.Color ||
					session.Provider != want.Provider {
					t.Errorf("sessions[%d] = %+v, want %+v", i, session, want)
				}
				if session.Started.IsZero() || session.LastSeen.IsZero() {
					t.Errorf("sessions[%d] has a zero timestamp: %+v", i, session)
				}
			}
		})
	}
}

// Colour goes out as a name, never the number Go would emit by default. The
// frame format does the same, so a client reads one vocabulary throughout.
func TestHandleStatusColorIsAString(t *testing.T) {
	r, _ := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"blocked","label":"auth-api"}`)

	rec := serve(t, r, http.MethodGet, StatusPath, "")

	// Assert on the raw JSON, because a typed decode would hide a number.
	var raw struct {
		Aggregate json.RawMessage `json:"aggregate"`
		Sessions  []struct {
			Color json.RawMessage `json:"color"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := string(raw.Aggregate); got != `"red"` {
		t.Errorf("aggregate = %s, want \"red\" as a string", got)
	}
	if len(raw.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(raw.Sessions))
	}
	if got := string(raw.Sessions[0].Color); got != `"red"` {
		t.Errorf("sessions[0].color = %s, want \"red\" as a string", got)
	}
	if strings.Contains(rec.Body.String(), `"aggregate":3`) {
		t.Error("aggregate leaked as a number")
	}
}

// Every colour survives the round trip by name.
func TestHandleStatusColorNames(t *testing.T) {
	tests := []struct {
		event string
		want  string
	}{
		{"idle", "green"},
		{"started", "yellow"},
		{"blocked", "red"},
		{"finished", "green"},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			ingestOK(t, r, `{"session_id":"a1","event":"`+tt.event+`"}`)

			rec := serve(t, r, http.MethodGet, StatusPath, "")
			if !strings.Contains(rec.Body.String(), `"aggregate":"`+tt.want+`"`) {
				t.Errorf("body %s, want aggregate %q", rec.Body.String(), tt.want)
			}
		})
	}
}

// An empty desk marshals as [], not null. A client must never have to
// special-case nil before ranging over the list.
func TestHandleStatusEmptySessionsIsArrayNotNull(t *testing.T) {
	r, _ := newHandlerRelay(t)

	rec := serve(t, r, http.MethodGet, StatusPath, "")

	body := rec.Body.String()
	if !strings.Contains(body, `"sessions":[]`) {
		t.Errorf("body = %s, want \"sessions\":[]", body)
	}
	if strings.Contains(body, `"sessions":null`) {
		t.Errorf("body = %s, sessions marshalled as null", body)
	}

	// And it decodes to a non-nil empty slice.
	var decoded StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Sessions == nil {
		t.Error("sessions decoded to nil, want an empty slice")
	}
	if len(decoded.Sessions) != 0 {
		t.Errorf("sessions = %d, want 0", len(decoded.Sessions))
	}
	if decoded.Aggregate != stoplight.ColorOff {
		t.Errorf("aggregate = %v, want off for an empty desk", decoded.Aggregate)
	}
}

// A session that expires between two reads leaves [] behind, not null.
func TestHandleStatusEmptyAfterSessionsEnd(t *testing.T) {
	r, _ := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started"}`)
	if got := len(getStatus(t, r).Sessions); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}

	ingestOK(t, r, `{"session_id":"a1","event":"ended"}`)

	rec := serve(t, r, http.MethodGet, StatusPath, "")
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Errorf("body = %s, want \"sessions\":[] after the last session ended", rec.Body.String())
	}
}

// Uptime goes out in whole seconds, not the nanosecond count a time.Duration
// would marshal to.
func TestHandleStatusUptimeInSeconds(t *testing.T) {
	r, _ := newHandlerRelay(t)

	// Before Run, uptime is zero rather than a huge number from the zero time.
	if got := getStatus(t, r).UptimeSeconds; got != 0 {
		t.Errorf("uptime_seconds before Run = %d, want 0", got)
	}

	// Pin the clock so the arithmetic is exact rather than timing-dependent.
	start := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	r.mu.Lock()
	r.started = start
	r.mu.Unlock()
	r.now = func() time.Time { return start.Add(3721 * time.Second) }

	if got := getStatus(t, r).UptimeSeconds; got != 3721 {
		t.Errorf("uptime_seconds = %d, want 3721", got)
	}
}

// Timestamps are RFC 3339 in UTC, so a client never has to reason about the
// relay's local zone.
func TestHandleStatusTimestampsAreUTC(t *testing.T) {
	r, _ := newHandlerRelay(t)
	moment := time.Date(2026, 9, 6, 10, 4, 0, 0, time.FixedZone("test", 5*3600))
	r.now = func() time.Time { return moment }

	ingestOK(t, r, `{"session_id":"a1","event":"started"}`)

	rec := serve(t, r, http.MethodGet, StatusPath, "")
	if !strings.Contains(rec.Body.String(), `"started":"2026-09-06T05:04:00Z"`) {
		t.Errorf("body = %s, want started as RFC 3339 UTC", rec.Body.String())
	}

	got := getStatus(t, r)
	if _, offset := got.Sessions[0].Started.Zone(); offset != 0 {
		t.Errorf("started zone offset = %d, want UTC", offset)
	}
}

// The status endpoint shows the override, because that is what the screen
// shows.
func TestHandleStatusShowsOverrideAsLabel(t *testing.T) {
	r, _ := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)

	if code := postTask(t, r, `{"session_id":"a1","label":"nightly integration run"}`); code != http.StatusNoContent {
		t.Fatalf("task: status = %d, want 204", code)
	}

	if got := getStatus(t, r).Sessions[0].Label; got != "nightly integration run" {
		t.Errorf("label = %q, want the override", got)
	}
}

// Reading status must not move the light: it is a snapshot, not an event.
func TestHandleStatusSendsNoFrame(t *testing.T) {
	r, tr := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"blocked"}`)
	before := tr.sendCount()

	for range 5 {
		if rec := serve(t, r, http.MethodGet, StatusPath, ""); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	if got := tr.sendCount(); got != before {
		t.Errorf("send count = %d after reading status, want %d", got, before)
	}
}

// Every method other than GET is a 405 with an Allow header.
func TestHandleStatusMethodNotAllowed(t *testing.T) {
	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			r, _ := newHandlerRelay(t)

			rec := serve(t, r, method, StatusPath, `{}`)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow = %q, want GET", got)
			}
		})
	}
}

// --- POST /v1/task ----------------------------------------------------------

// The response code matrix for the task endpoint.
func TestHandleTaskStatusCodes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{"sets an override", `{"session_id":"a1","label":"nightly run"}`, http.StatusNoContent},
		{"empty label clears", `{"session_id":"a1","label":""}`, http.StatusNoContent},
		{"absent label clears", `{"session_id":"a1"}`, http.StatusNoContent},
		{"unknown fields ignored", `{"session_id":"a1","label":"x","future":42}`, http.StatusNoContent},

		{"malformed json", `{"session_id":`, http.StatusBadRequest},
		{"not an object", `"hello"`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"missing session_id", `{"label":"nightly run"}`, http.StatusBadRequest},
		{"empty session_id", `{"session_id":"","label":"nightly run"}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			rec := serve(t, r, http.MethodPost, TaskPath, tt.body)

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}
			// Success MUST carry no body.
			if rec.Code == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Errorf("204 carried a body %q, want empty", rec.Body.String())
			}
		})
	}
}

// A body over 8KB is a 413, matching the ingest cap.
func TestHandleTaskBodyTooLarge(t *testing.T) {
	huge := `{"session_id":"a1","label":"` + strings.Repeat("x", MaxBodyBytes+100) + `"}`

	r, _ := newHandlerRelay(t)
	rec := serve(t, r, http.MethodPost, TaskPath, huge)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if got := len(getStatus(t, r).Sessions); got != 0 {
		t.Errorf("sessions = %d, want 0: an oversized body reached the tracker", got)
	}
}

// The 8KB cap on /v1/task must not be bypassed by a valid JSON prefix.
//
// The defect: json.Decoder stops at the first complete value, so an object
// followed by junk decoded cleanly, MaxBytesReader was never read past its
// limit and so never tripped, and the override was applied with a 204.
// decodeJSON's drain to EOF is what closes it. TestHandleTaskBodyTooLarge
// above cannot catch this: its junk sits INSIDE the JSON string, so the
// decode fails on its own and the case passes even with no cap at all.
//
// The override is the payload that matters here. A leaked one renames a
// session from a request the relay claims to have rejected.
func TestHandleTaskRejectsValidPrefixThenJunk(t *testing.T) {
	r, tr := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"blocked","label":"auth-api"}`)
	sendsBefore := tr.sendCount()

	body := `{"session_id":"a1","label":"leaked"}` + strings.Repeat("x", MaxBodyBytes+100)
	rec := serve(t, r, http.MethodPost, TaskPath, body)

	// The trailing bytes are not valid JSON, so the drain fails as a 400
	// rather than tripping MaxBytesReader for a 413.
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	// A rejection must also mean nothing was applied. The status code alone
	// would not catch a body that was rejected AND applied.
	if got := getStatus(t, r).Sessions[0].Label; got != "auth-api" {
		t.Errorf("label = %q, want auth-api: the override from a rejected body was applied", got)
	}
	if got := tr.sendCount(); got != sendsBefore {
		t.Errorf("send count = %d, want %d: a rejected body moved the light", got, sendsBefore)
	}
}

// Every method other than POST is a 405 with an Allow header.
func TestHandleTaskMethodNotAllowed(t *testing.T) {
	for _, method := range []string{
		http.MethodGet, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			r, _ := newHandlerRelay(t)

			rec := serve(t, r, method, TaskPath, `{"session_id":"a1","label":"x"}`)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodPost {
				t.Errorf("Allow = %q, want POST", got)
			}
		})
	}
}

// The override is what the screen shows, in place of the producer's label.
func TestHandleTaskSetsOverride(t *testing.T) {
	r, _ := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)

	if code := postTask(t, r, `{"session_id":"a1","label":"nightly integration run"}`); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}

	sessions := r.Status().Sessions
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if sessions[0].Override != "nightly integration run" {
		t.Errorf("Override = %q, want the task text", sessions[0].Override)
	}
	if sessions[0].Label != "auth-api" {
		t.Errorf("Label = %q, want the derived label kept underneath", sessions[0].Label)
	}
	if got := getStatus(t, r).Sessions[0].Label; got != "nightly integration run" {
		t.Errorf("status label = %q, want the override", got)
	}
}

// The reason /v1/task exists rather than riding in on the ingest path: a later
// report carrying a derived label must NOT clobber the override.
func TestHandleTaskOverrideSurvivesLaterIngest(t *testing.T) {
	r, tr := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)

	if code := postTask(t, r, `{"session_id":"a1","label":"nightly integration run"}`); code != http.StatusNoContent {
		t.Fatalf("task: status = %d, want 204", code)
	}

	// Every one of these carries a derived label, exactly as a shell hook
	// would send it. None may reach the screen.
	ingestOK(t, r, `{"session_id":"a1","event":"blocked","label":"auth-api"}`)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"some-branch"}`)
	ingestOK(t, r, `{"session_id":"a1","event":"finished","label":"another-branch","cwd":"/tmp/elsewhere"}`)

	if got := getStatus(t, r).Sessions[0].Label; got != "nightly integration run" {
		t.Errorf("label = %q, want the override to survive three reports", got)
	}

	// The frame the light sees says the same thing.
	frame, ok := tr.lastFrame()
	if !ok {
		t.Fatal("no frame recorded")
	}
	if len(frame.Sessions) != 1 || frame.Sessions[0].Label != "nightly integration run" {
		t.Errorf("frame = %+v, want the override on the screen", frame.Sessions)
	}
}

// An empty label clears the override, so the derived label shows again.
func TestHandleTaskEmptyLabelClearsOverride(t *testing.T) {
	for _, body := range []string{
		`{"session_id":"a1","label":""}`,
		`{"session_id":"a1"}`, // absent is the same as empty
	} {
		t.Run(body, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)
			if code := postTask(t, r, `{"session_id":"a1","label":"nightly run"}`); code != http.StatusNoContent {
				t.Fatalf("set: status = %d, want 204", code)
			}
			if got := getStatus(t, r).Sessions[0].Label; got != "nightly run" {
				t.Fatalf("label = %q, want the override before clearing", got)
			}

			if code := postTask(t, r, body); code != http.StatusNoContent {
				t.Fatalf("clear: status = %d, want 204", code)
			}

			if got := r.Status().Sessions[0].Override; got != "" {
				t.Errorf("Override = %q, want it cleared", got)
			}
			if got := getStatus(t, r).Sessions[0].Label; got != "auth-api" {
				t.Errorf("label = %q, want the derived label back", got)
			}
		})
	}
}

// A task for a session we never saw creates NOTHING. It is accepted, so the
// caller still gets a 204, but a label carries no state: unlike ingest there
// is no light to lose, and creating a session here put a GREEN one on an empty
// desk, against RFC 1 section 8. The name is parked and applies when the
// session really starts.
func TestHandleTaskCreatesNoSessionForUnknownID(t *testing.T) {
	r, _ := newHandlerRelay(t)

	if code := postTask(t, r, `{"session_id":"ghost","label":"mystery work"}`); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}

	body := getStatus(t, r)
	if len(body.Sessions) != 0 {
		t.Fatalf("sessions = %d, want 0: naming a task must not create a session", len(body.Sessions))
	}
	if body.Aggregate != stoplight.ColorOff {
		t.Errorf("aggregate = %v, want off: an empty desk stays dark", body.Aggregate)
	}

	// The parked name applies once the session genuinely reports.
	ingestOK(t, r, `{"session_id":"ghost","event":"started"}`)
	body = getStatus(t, r)
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 once the session started", len(body.Sessions))
	}
	if body.Sessions[0].ID != "ghost" {
		t.Errorf("id = %q, want ghost", body.Sessions[0].ID)
	}
	if body.Sessions[0].Label != "mystery work" {
		t.Errorf("label = %q, want the parked override", body.Sessions[0].Label)
	}
	if body.Sessions[0].State != "working" {
		t.Errorf("state = %q, want working", body.Sessions[0].State)
	}
	if body.Sessions[0].Color != stoplight.ColorYellow {
		t.Errorf("colour = %v, want yellow", body.Sessions[0].Color)
	}
}

// An override changes the screen and never the lamp. Aggregation reads state,
// never labels.
func TestHandleTaskDoesNotChangeAggregate(t *testing.T) {
	tests := []struct {
		event string
		want  stoplight.Color
	}{
		{"started", stoplight.ColorYellow},
		{"blocked", stoplight.ColorRed},
		{"finished", stoplight.ColorGreen},
		{"idle", stoplight.ColorGreen},
	}
	for _, tt := range tests {
		t.Run(tt.event, func(t *testing.T) {
			r, tr := newHandlerRelay(t)
			ingestOK(t, r, `{"session_id":"a1","event":"`+tt.event+`","label":"auth-api"}`)
			before := r.Status().Aggregate

			if code := postTask(t, r, `{"session_id":"a1","label":"renamed"}`); code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", code)
			}

			after := getStatus(t, r)
			if after.Aggregate != tt.want || after.Aggregate != before {
				t.Errorf("aggregate = %v, want %v unchanged", after.Aggregate, tt.want)
			}
			if after.Sessions[0].Color != tt.want {
				t.Errorf("session colour = %v, want %v", after.Sessions[0].Color, tt.want)
			}
			if after.Sessions[0].State != stateWord(tt.event) {
				t.Errorf("state = %q, want %q", after.Sessions[0].State, stateWord(tt.event))
			}
			// The frame that reached the light kept its colour too.
			if frame, ok := tr.lastFrame(); ok && frame.Color != tt.want {
				t.Errorf("frame colour = %v, want %v", frame.Color, tt.want)
			}
		})
	}
}

// stateWord maps an ingest event onto the word the screen shows for it.
func stateWord(event string) string {
	switch event {
	case "started":
		return "working"
	case "blocked":
		return "needs you"
	case "finished":
		return "done"
	default:
		return "idle"
	}
}

// A task that does not move the frame must not wake the radio. A quiet desk is
// a quiet radio, and the override path obeys that like every other.
func TestHandleTaskSendsFrameOnlyOnChange(t *testing.T) {
	r, tr := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)
	before := tr.sendCount()

	if code := postTask(t, r, `{"session_id":"a1","label":"nightly run"}`); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	afterFirst := tr.sendCount()
	if afterFirst != before+1 {
		t.Fatalf("send count = %d, want %d: a new label is a change", afterFirst, before+1)
	}

	// Repeating the same override changes nothing.
	for range 5 {
		if code := postTask(t, r, `{"session_id":"a1","label":"nightly run"}`); code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", code)
		}
	}
	if got := tr.sendCount(); got != afterFirst {
		t.Errorf("send count = %d after repeats, want %d", got, afterFirst)
	}

	// Clearing it is a change again.
	if code := postTask(t, r, `{"session_id":"a1","label":""}`); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if got := tr.sendCount(); got != afterFirst+1 {
		t.Errorf("send count = %d after clearing, want %d", got, afterFirst+1)
	}
}

// A malformed task must not create a session or move the light.
func TestHandleTaskBadRequestLeavesStateUnchanged(t *testing.T) {
	r, tr := newHandlerRelay(t)

	for _, body := range []string{`{"session_id":`, `{"label":"orphan"}`, `{"session_id":""}`} {
		if code := postTask(t, r, body); code != http.StatusBadRequest {
			t.Errorf("post %q: status = %d, want 400", body, code)
		}
	}

	if got := len(getStatus(t, r).Sessions); got != 0 {
		t.Errorf("sessions = %d, want 0", got)
	}
	if got := tr.sendCount(); got != 0 {
		t.Errorf("send count = %d, want 0", got)
	}
}

// An override on one session leaves the others alone.
func TestHandleTaskAffectsOnlyTheNamedSession(t *testing.T) {
	r, _ := newHandlerRelay(t)
	ingestOK(t, r, `{"session_id":"a1","event":"started","label":"auth-api"}`)
	ingestOK(t, r, `{"session_id":"b2","event":"blocked","label":"payments"}`)

	if code := postTask(t, r, `{"session_id":"a1","label":"renamed"}`); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}

	body := getStatus(t, r)
	if len(body.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(body.Sessions))
	}
	if body.Sessions[0].Label != "renamed" {
		t.Errorf("sessions[0].label = %q, want renamed", body.Sessions[0].Label)
	}
	if body.Sessions[1].Label != "payments" {
		t.Errorf("sessions[1].label = %q, want payments untouched", body.Sessions[1].Label)
	}
	if body.Aggregate != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red from b2", body.Aggregate)
	}
}

// --- over a real listener ---------------------------------------------------

// runningRelay starts a relay on a real loopback port and returns its address.
// Run is cancelled and joined through t.Cleanup, so no test can leave the
// server behind or hang waiting for it.
func runningRelay(t *testing.T) (*Relay, *fakeTransport, string) {
	t.Helper()
	tr := &fakeTransport{connected: true}
	r := newTestRelay(t, tr)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})

	return r, tr, waitForAddr(t, r)
}

// httpClient is bounded so a wedged server fails the test rather than hanging
// it.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// The CLI's whole job over the wire: start a session, name it, read it back.
func TestEndpointsOverRealListener(t *testing.T) {
	_, _, addr := runningRelay(t)

	// A producer reports a blocked session.
	if code := postSession(t, addr, `{"session_id":"a1","event":"blocked","label":"auth-api","provider":"claude-code"}`); code != http.StatusNoContent {
		t.Fatalf("session: status = %d, want 204", code)
	}

	// The human names it.
	resp, err := httpClient.Post("http://"+addr+TaskPath, "application/json",
		strings.NewReader(`{"session_id":"a1","label":"nightly integration run"}`))
	if err != nil {
		t.Fatalf("post task: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("task: status = %d, want 204 (body %q)", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Errorf("204 carried a body %q, want empty", body)
	}

	// The CLI reads it back.
	statusResp, err := httpClient.Get("http://" + addr + StatusPath)
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	raw, err := io.ReadAll(statusResp.Body)
	statusResp.Body.Close()
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status: code = %d, want 200 (body %q)", statusResp.StatusCode, raw)
	}

	var got StatusResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	if got.Transport != "fake" || !got.Connected {
		t.Errorf("transport = %q connected = %v, want fake/true", got.Transport, got.Connected)
	}
	if got.Aggregate != stoplight.ColorRed {
		t.Errorf("aggregate = %v, want red", got.Aggregate)
	}
	if got.UptimeSeconds < 0 {
		t.Errorf("uptime_seconds = %d, want non-negative", got.UptimeSeconds)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(got.Sessions))
	}
	if got.Sessions[0].Label != "nightly integration run" {
		t.Errorf("label = %q, want the override", got.Sessions[0].Label)
	}
	if got.Sessions[0].State != "needs you" || got.Sessions[0].Color != stoplight.ColorRed {
		t.Errorf("session = %+v, want a red \"needs you\"", got.Sessions[0])
	}
}

// A task over the wire must not be undone by the shell hooks that keep firing.
func TestOverrideSurvivesIngestOverRealListener(t *testing.T) {
	r, _, addr := runningRelay(t)

	postSession(t, addr, `{"session_id":"a1","event":"started","label":"auth-api"}`)

	resp, err := httpClient.Post("http://"+addr+TaskPath, "application/json",
		strings.NewReader(`{"session_id":"a1","label":"nightly run"}`))
	if err != nil {
		t.Fatalf("post task: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	for range 5 {
		postSession(t, addr, `{"session_id":"a1","event":"blocked","label":"auth-api"}`)
		postSession(t, addr, `{"session_id":"a1","event":"started","label":"auth-api"}`)
	}

	sessions := r.Status().Sessions
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if got := sessions[0].Display(); got != "nightly run" {
		t.Errorf("display = %q, want the override to survive ten reports", got)
	}
}

// --- routing ----------------------------------------------------------------

// Both new paths are versioned, so an unversioned or v2 path is a 404.
func TestUnversionedPathsNotFound(t *testing.T) {
	for _, path := range []string{"/status", "/task", "/v2/status", "/v2/task"} {
		t.Run(path, func(t *testing.T) {
			r, _ := newHandlerRelay(t)
			rec := serve(t, r, http.MethodGet, path, "")
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
		})
	}
}
