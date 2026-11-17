package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
)

func TestHeldHistoryJSONLIsOneRecordPerLine(t *testing.T) {
	at := time.Unix(1_700_000_000, 0).UTC()
	recs := []desk.HeldAuditRecord{
		{V: 1, Operation: "op_1", Event: desk.AuditProposed, At: &at, Subject: "plugin:triage", Shown: json.RawMessage(`{"text":"a"}`)},
		{V: 1, Operation: "op_1", Event: desk.AuditEdited, At: &at, Subject: "uid:1", Before: json.RawMessage(`{"text":"a"}`), After: json.RawMessage(`{"text":"b"}`)},
	}
	var buf bytes.Buffer
	if err := writeHeldJSONL(&buf, recs); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var back desk.HeldAuditRecord
	if err := json.Unmarshal([]byte(lines[1]), &back); err != nil || back.Event != desk.AuditEdited ||
		string(back.Before) != `{"text":"a"}` || string(back.After) != `{"text":"b"}` {
		t.Fatalf("edit line = %v, %+v", err, back)
	}
}

func TestHeldHistoryRowShowsTheChange(t *testing.T) {
	r := heldHistoryRowOf(desk.HeldAuditRecord{Event: desk.AuditEdited, Subject: "uid:1",
		Before: json.RawMessage(`"x"`), After: json.RawMessage(`"y"`)})
	if r.Who != "uid:1" || !strings.Contains(r.Detail, `before "x"`) || !strings.Contains(r.Detail, `after "y"`) {
		t.Fatalf("row = %+v", r)
	}
}
