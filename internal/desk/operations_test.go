package desk_test

import (
	"testing"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func TestTerminalStates(t *testing.T) {
	for st, want := range map[model.DeliveryState]bool{
		model.Queued: false, model.RetryPending: false, "from_the_future": false,
		model.ProviderAccepted: true, model.Delivered: true, model.Committed: true,
		model.Failed: true, model.Rejected: true, model.Unknown: true, model.DeadLettered: true,
		model.Held: false, model.Expired: true,
	} {
		if got := desk.Terminal(st); got != want {
			t.Errorf("Terminal(%s) = %v, want %v", st, got, want)
		}
	}
}

// TestLookupOperationIsTheCallers: uma finds an operation by its key,
// kind and target for the caller who submitted it alone; another caller
// with a role to read operations finds nothing, and one without the role
// is refused.
func TestLookupOperationIsTheCallers(t *testing.T) {
	w := newWorld(t)
	s := w.as(agent)
	p, err := s.PlanReply(t.Context(), string(w.local.Ref.ID), "hi", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.Reply(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	target := w.local.Ref.Ref()
	got, err := s.LookupOperation(t.Context(), model.ActionMessageReply, target, "key-1")
	if err != nil || got.Ref != acc.Operation.Ref {
		t.Fatalf("the agent's lookup = %+v, %v; want %s", got, err, acc.Operation.Ref.ID)
	}
	_, err = w.as(operator).LookupOperation(t.Context(), model.ActionMessageReply, target, "key-1")
	wantCode(t, err, model.NotFound, "")
	if _, err = w.as(viewer).LookupOperation(t.Context(), model.ActionMessageReply, target, "key-1"); err == nil {
		t.Fatal("a viewer found an operation")
	}
	// Kind and target are part of the key.
	_, err = s.LookupOperation(t.Context(), model.ActionMessageNote, target, "key-1")
	wantCode(t, err, model.NotFound, "")
	_, err = s.LookupOperation(t.Context(), model.ActionMessageReply, w.remote.Ref.Ref(), "key-1")
	wantCode(t, err, model.NotFound, "")
}
