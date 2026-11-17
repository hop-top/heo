package daemon_test

import (
	"testing"
	"time"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/hooks/hookstest"
	"hop.top/uma/model"
)

// holdEdit has proposer propose an edit of a message of the rig's inbox,
// held for a human decision, and returns the held operation's ID.
func (r *hooksRig) holdEdit(proposer model.ActorID, key string) {
	r.t.Helper()
	ctx := r.t.Context()
	r.start(key, "hello")
	page, err := r.client().Messages().List(ctx, model.ListRequest{
		Call: model.Call{Correlation: "test"}, Filter: model.Filter{Inboxes: []model.InboxRef{r.inbox}}, Page: model.PageRequest{Limit: 100}})
	if err != nil || len(page.Items) == 0 {
		r.t.Fatalf("messages: %v %d", err, len(page.Items))
	}
	m := page.Items[len(page.Items)-1]
	client, err := r.seed.For(model.Principal{Tenant: r.tenant, Actor: proposer})
	if err != nil {
		r.t.Fatal(err)
	}
	s := desk.Service{Client: client, Tenant: r.tenant, Actor: proposer,
		Holds: &desk.Holds{Expiry: time.Hour, OnBehalfOf: deskActor}}
	ep, err := s.PlanEdit(ctx, string(m.Ref.ID), string(m.Ref.Conversation.ID), m.Revision, "edited", model.OperationID(key), channels.Submit)
	if err != nil {
		r.t.Fatal(err)
	}
	h, err := s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: string(r.ch.ID), Target: m.Ref.Ref(), Preview: map[string]any{"text": "edited"}})
	if err != nil || h == nil {
		r.t.Fatalf("hold = %v, %v", h, err)
	}
	acc, err := s.Edit(ctx, ep)
	acc, err = h.Settle(ctx, acc, err)
	if err != nil || acc.Operation.State != model.Held {
		r.t.Fatalf("propose %s = %v, %v", key, acc.Operation.State, err)
	}
}

// TestHooksDeliverHeldOperationEvents: a plugin subscribed to
// operation.held receives it when its actor may read desk content, the
// operations and the operation's target, with the operation as the only
// reference. A plugin without the desk role does not, and a plugin is not
// told of the operation it proposed itself.
func TestHooksDeliverHeldOperationEvents(t *testing.T) {
	r := newHooksRig(t, func(r *rig) { r.fake.DeclareEffects() })
	watch, blind, self := logPath(t), logPath(t), logPath(t)
	held := map[string]any{"filter": map[string]any{"kinds": []any{model.EventOperationHeld}}}
	r.plugins(map[model.ActorID][]authz.Role{
		"plugin:watch": {authz.RoleObserver, authz.RoleDesk},
		"plugin:blind": {authz.RoleObserver},
		"plugin:self":  {authz.RoleObserver, authz.RoleDesk},
	},
		r.plugin("watch", watch, hookstest.ModeOK, held),
		r.plugin("blind", blind, hookstest.ModeOK, held),
		r.plugin("self", self, hookstest.ModeOK, held))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)

	r.holdEdit("plugin:self", "held-1")
	eventually(t, "the watcher was told and counted it", func() bool {
		st, ok := r.status("watch")
		return len(hookstest.Seen(watch)) == 1 && ok && st.Delivered == 1
	})
	// Each plugin's consumer runs on its own, so the others' have no
	// order against the watcher's: wait for each to have passed the
	// held operation, not for a while. The proposer skips it, and
	// where it stands afterwards is past it; the blind plugin is past
	// it when its checkpoint is there too.
	var past model.Cursor
	eventually(t, "the proposer's own held operation was skipped", func() bool {
		st, ok := r.status("self")
		past = st.Cursor
		return ok && st.Skipped >= 1 && past != ""
	})
	eventually(t, "the blind plugin's batch was acknowledged", func() bool {
		st, ok := r.status("blind")
		return ok && st.Cursor == past
	})
	if got := hookstest.Seen(blind); len(got) != 0 {
		t.Errorf("a plugin without desk received %v", got)
	}
	if got := hookstest.Seen(self); len(got) != 0 {
		t.Errorf("the proposer received its own held operation: %v", got)
	}
	if st, _ := r.status("self"); st.Skipped == 0 {
		t.Errorf("the proposer's status %+v, want its own held operation skipped", st)
	}
	if st, _ := r.status("watch"); st.Delivered != 1 {
		t.Errorf("watcher status %+v", st)
	}
}
