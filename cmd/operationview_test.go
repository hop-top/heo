package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestOperationViewSaysAlreadyHeld: a duplicate proposal's output is the
// held operation, state held, with already_held set.
func TestOperationViewSaysAlreadyHeld(t *testing.T) {
	acc := model.Acceptance{Operation: model.Operation{Ref: model.OperationRef{ID: "op_9"}, State: model.Held}, Replayed: true, AlreadyHeld: true}
	v := operationOf(acc)
	b, _ := json.Marshal(v)
	if v.State != model.Held || !strings.Contains(string(b), `"already_held":true`) {
		t.Fatalf("view = %s", b)
	}
	if rows := operationRows(v, "k"); !strings.Contains(rows[0][1], "already held") {
		t.Fatalf("rows = %v", rows)
	}
	if b, _ := json.Marshal(operationOf(model.Acceptance{})); strings.Contains(string(b), "already_held") {
		t.Fatalf("unset already_held leaks: %s", b)
	}
}
