package stoplight

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestReportValidate(t *testing.T) {
	tests := []struct {
		name    string
		report  Report
		wantErr error
	}{
		{"valid", Report{SessionID: "a1", Event: "blocked"}, nil},
		{"missing session id", Report{Event: "blocked"}, ErrMissingSessionID},
		{"missing event", Report{SessionID: "a1"}, ErrMissingEvent},
		{"missing both reports session id first", Report{}, ErrMissingSessionID},
		// Invariant 5: an unknown event name is not a validation error. The
		// caller ignores it per RFC 1 section 11.
		{"unknown event name is valid", Report{SessionID: "a1", Event: "exploded"}, nil},
		{"optional fields absent", Report{SessionID: "a1", Event: "idle"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.report.Validate()
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestReportUnmarshalWireForm(t *testing.T) {
	const wire = `{"session_id":"a1b2c3","event":"blocked","label":"auth-api",` +
		`"provider":"deepseek","detail":"waiting for approval","cwd":"/tmp/x"}`

	var got Report
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	want := Report{
		SessionID: "a1b2c3",
		Event:     "blocked",
		Label:     "auth-api",
		Provider:  "deepseek",
		Detail:    "waiting for approval",
		Cwd:       "/tmp/x",
	}
	if got != want {
		t.Errorf("Unmarshal() = %+v, want %+v", got, want)
	}
}

// RFC 1 section 5: unknown fields must be ignored, not rejected.
func TestReportIgnoresUnknownFields(t *testing.T) {
	const wire = `{"session_id":"a1","event":"started","future_field":42}`

	var got Report
	if err := json.Unmarshal([]byte(wire), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.SessionID != "a1" || got.Event != "started" {
		t.Errorf("Unmarshal() = %+v, want the known fields preserved", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
	}
}

// Empty optional fields must not appear on the wire.
func TestReportMarshalOmitsEmptyOptionals(t *testing.T) {
	got, err := json.Marshal(Report{SessionID: "a1", Event: "idle"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	const want = `{"session_id":"a1","event":"idle"}`
	if string(got) != want {
		t.Errorf("Marshal() = %s, want %s", got, want)
	}
}
