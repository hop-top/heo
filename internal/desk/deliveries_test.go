package desk_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// deliveryWorld is a world with five writes by the desk agent, a minute
// apart: a delivered reply in the local conversation, a reply in the
// provider conversation whose outcome is unknown, a start to a new
// recipient through the provider inbox, a second reply left queued, and
// a transition (not a delivery). The operator configured the channel.
type deliveryWorld struct {
	*world
	localOp, remoteOp, two model.Operation
	startOp, transitionOp  model.Operation
	t0                     time.Time
}

func newDeliveryWorld(t *testing.T) *deliveryWorld {
	t.Helper()
	w := &deliveryWorld{world: newWorld(t)}
	s := w.as(agent)
	// Every write below is stamped by the clock, which a test advances.
	step := func() { w.clock.Advance(time.Hour) }
	step()
	w.t0 = w.clock.Now()
	reply := func(conv model.Conversation, key string) model.Operation {
		t.Helper()
		p, err := s.PlanReply(t.Context(), string(conv.Ref.ID), "hi "+key, model.OperationID(key))
		if err != nil {
			t.Fatal(err)
		}
		acc, err := s.Reply(t.Context(), p)
		if err != nil {
			t.Fatal(err)
		}
		return acc.Operation
	}

	w.localOp = w.settle(reply(w.local, "r-local").Ref)
	step()
	w.fake.FailSends(errors.New("connection reset after write"))
	w.remoteOp = w.settle(reply(w.remote, "r-remote").Ref)
	w.fake.FailSends(nil)
	step()
	plan, err := s.PlanStart(t.Context(), "provider", "user:cy", "welcome", "s1")
	if err != nil {
		t.Fatal(err)
	}
	acc, err := s.Start(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	w.startOp = w.settle(acc.Operation.Ref)
	step()
	w.two = reply(w.local, "r-local-2") // left queued: no worker runs
	step()
	tp, err := s.PlanTransition(t.Context(), string(w.local.Ref.ID), w.local.Revision, model.ConversationResolved, "t1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, acc, err = s.Transition(t.Context(), tp); err != nil {
		t.Fatal(err)
	}
	w.transitionOp = acc.Operation
	return w
}

func keysOf(p model.Page[model.Operation]) string {
	var ks []string
	for _, op := range p.Items {
		ks = append(ks, string(op.Key.ID))
	}
	return strings.Join(ks, ",")
}

func TestListDeliveries(t *testing.T) {
	w := newDeliveryWorld(t)
	if w.remoteOp.State != model.Unknown {
		t.Fatalf("remote reply = %s, want outcome_unknown", w.remoteOp.State)
	}
	s := w.as(agent)
	// The world's own starts, and the channel's configuration, come
	// before t0.
	list := func(f desk.OperationFilter, page model.PageRequest) model.Page[model.Operation] {
		t.Helper()
		f.Actor = agent
		if f.Since == nil {
			f.Since = &w.t0
		}
		p, err := s.ListDeliveries(t.Context(), f, page)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		return p
	}
	big := model.PageRequest{Limit: 50}

	// Newest change first, outbound kinds only: the transition is the
	// agent's too, and not a delivery.
	all := list(desk.OperationFilter{}, big)
	if got := keysOf(all); got != "r-local-2,s1,r-remote,r-local" || !all.Complete || all.Next != "" {
		t.Fatalf("all = %s complete=%v next=%q", got, all.Complete, all.Next)
	}
	if all.Items[0].State != model.Queued || all.Items[2].State != model.Unknown {
		t.Fatalf("states: %s, %s", all.Items[0].State, all.Items[2].State)
	}
	// Each row shows what recovers it.
	if k := all.Items[1].Key; k == nil || k.Kind != model.ActionConversationStart || k.Caller != agent || k.Target.ID != w.startOp.Key.Target.ID {
		t.Fatalf("the start's key = %+v", k)
	}

	// State.
	if got := keysOf(list(desk.OperationFilter{States: []string{"outcome_unknown"}}, big)); got != "r-remote" {
		t.Errorf("--state outcome_unknown = %q", got)
	}
	if got := keysOf(list(desk.OperationFilter{States: []string{"queued", "outcome_unknown"}}, big)); got != "r-local-2,r-remote" {
		t.Errorf("--state queued,outcome_unknown = %q", got)
	}
	if got := keysOf(list(desk.OperationFilter{States: []string{"failed"}}, big)); got != "" {
		t.Errorf("--state failed = %q, want none", got)
	}

	// Inbox, by key and by ID; conversation, including a start's, which
	// is its result's.
	if got := keysOf(list(desk.OperationFilter{Inbox: "local"}, big)); got != "r-local-2,r-local" {
		t.Errorf("--inbox local = %q", got)
	}
	if got := keysOf(list(desk.OperationFilter{Inbox: string(w.remote.Ref.Inbox.ID)}, big)); got != "s1,r-remote" {
		t.Errorf("--inbox provider (by ID) = %q", got)
	}
	if got := keysOf(list(desk.OperationFilter{Conversation: string(w.local.Ref.ID)}, big)); got != "r-local-2,r-local" {
		t.Errorf("--conversation local = %q", got)
	}
	if got := keysOf(list(desk.OperationFilter{Conversation: string(w.startOp.ResultMessage.Conversation.ID)}, big)); got != "s1" {
		t.Errorf("--conversation of the start = %q", got)
	}
	// With the inbox named the conversation is addressed without being
	// read, so a caller that reads operations but not conversations can
	// list one's deliveries.
	inbox := string(w.local.Ref.Inbox.ID)
	if got := keysOf(list(desk.OperationFilter{Inbox: inbox, Conversation: string(w.local.Ref.ID)}, big)); got != "r-local-2,r-local" {
		t.Errorf("--inbox and --conversation = %q", got)
	}
	op := w.as(operator)
	if _, err := op.ListDeliveries(t.Context(), desk.OperationFilter{Conversation: string(w.local.Ref.ID), Since: &w.t0}, big); err == nil {
		t.Error("the operator found a conversation it may not read")
	}
	p, err := op.ListDeliveries(t.Context(), desk.OperationFilter{Inbox: inbox, Conversation: string(w.local.Ref.ID), Since: &w.t0}, big)
	if err != nil || keysOf(p) != "r-local-2,r-local" {
		t.Errorf("the operator's --inbox and --conversation = %q, %v; want the agent's two (operation.read decides)", keysOf(p), err)
	}
	if got := keysOf(list(desk.OperationFilter{Inbox: "local", States: []string{"queued"}, Conversation: string(w.local.Ref.ID)}, big)); got != "r-local-2" {
		t.Errorf("combined filters = %q", got)
	}

	// Window: [since, until) over the time of the last change.
	since, until := w.t0.Add(time.Hour), w.t0.Add(3*time.Hour)
	if got := keysOf(list(desk.OperationFilter{Since: &since, Until: &until}, big)); got != "s1,r-remote" {
		t.Errorf("window = %q, want the 2nd and 3rd writes", got)
	}
	if got := keysOf(list(desk.OperationFilter{Since: &until}, big)); got != "r-local-2" {
		t.Errorf("--since = %q", got)
	}

	// Paging: each delivery once, then complete.
	var seen []string
	page := model.PageRequest{Limit: 1}
	for i := 0; i < 10; i++ {
		p := list(desk.OperationFilter{}, page)
		seen = append(seen, keysOf(p))
		if p.Complete {
			break
		}
		if p.Next == "" {
			t.Fatal("an incomplete page without a cursor")
		}
		page.After = p.Next
	}
	if got := strings.Join(seen, "|"); !strings.HasPrefix(got, "r-local-2|s1|r-remote|r-local") {
		t.Errorf("pages = %q", got)
	}

	// Refusals.
	_, err = s.ListDeliveries(t.Context(), desk.OperationFilter{States: []string{"lost"}}, big)
	wantCode(t, err, model.Validation, `state "lost"`)
	_, err = s.ListDeliveries(t.Context(), desk.OperationFilter{}, model.PageRequest{Limit: 5, After: "x"})
	wantCode(t, err, model.Validation, "cursor")
	_, err = s.ListDeliveries(t.Context(), desk.OperationFilter{Inbox: "nope"}, big)
	wantCode(t, err, model.NotFound, "nope")
}

// TestListOperationsFilters: any kind, not only the outbound ones, and
// only the actor's own when asked.
func TestListOperationsFilters(t *testing.T) {
	w := newDeliveryWorld(t)
	s := w.as(agent)
	big := model.PageRequest{Limit: 50}
	list := func(s desk.Service, f desk.OperationFilter) model.Page[model.Operation] {
		t.Helper()
		p, err := s.ListOperations(t.Context(), f, big)
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		return p
	}
	if got := keysOf(list(s, desk.OperationFilter{Actor: agent, Kinds: []string{model.ActionConversationTransition}})); got != "t1" {
		t.Errorf("--kind conversation.transition = %q", got)
	}
	if got := keysOf(list(s, desk.OperationFilter{Actor: agent, Kinds: []string{model.ActionMessageReply, model.ActionConversationTransition}})); got != "t1,r-local-2,r-remote,r-local" {
		t.Errorf("two kinds = %q", got)
	}
	if got := list(s, desk.OperationFilter{Actor: agent, Kinds: []string{"message.nothing"}}); len(got.Items) != 0 {
		t.Errorf("a kind nothing has = %+v", got.Items)
	}

	// The agent may read operations: with no actor named, uma shows every
	// operation it may read, the operator's configuration included; with
	// the agent named, its own.
	everyone := list(s, desk.OperationFilter{})
	mine := list(s, desk.OperationFilter{Actor: agent})
	theirs := list(s, desk.OperationFilter{Actor: operator})
	if len(theirs.Items) == 0 || len(mine.Items) == 0 || len(everyone.Items) != len(mine.Items)+len(theirs.Items) {
		t.Fatalf("operations: every %d, mine %d, the operator's %d", len(everyone.Items), len(mine.Items), len(theirs.Items))
	}
	for _, op := range mine.Items {
		if op.Key.Caller != agent {
			t.Errorf("the agent's list holds %+v", op.Key)
		}
	}
	for _, op := range theirs.Items {
		if op.Key.Caller != operator {
			t.Errorf("the operator's list holds %+v", op.Key)
		}
	}

	// A viewer reads no operation: they are left out without trace.
	p := list(w.as(viewer), desk.OperationFilter{})
	if len(p.Items) != 0 {
		t.Errorf("the viewer lists %d operations", len(p.Items))
	}
	p = list(w.as(external), desk.OperationFilter{Actor: agent})
	if len(p.Items) != 0 {
		t.Errorf("the external actor lists %d operations", len(p.Items))
	}
}

// TestFindOperations: the key alone recovers an operation from the
// caller's own, with or without its kind, and never another caller's.
func TestFindOperations(t *testing.T) {
	w := newDeliveryWorld(t)
	s := w.as(agent)
	found, err := s.FindOperations(t.Context(), agent, "r-local", "")
	if err != nil || len(found) != 1 || found[0].Ref != w.localOp.Ref {
		t.Fatalf("find r-local = %+v, %v; want %s", found, err, w.localOp.Ref.ID)
	}
	if found, err = s.FindOperations(t.Context(), agent, "r-local", model.ActionMessageReply); err != nil || len(found) != 1 {
		t.Fatalf("find r-local as a reply = %+v, %v", found, err)
	}
	if found, err = s.FindOperations(t.Context(), agent, "r-local", model.ActionMessageNote); err != nil || len(found) != 0 {
		t.Fatalf("find r-local as a note = %+v, %v; want none", found, err)
	}
	if found, err = s.FindOperations(t.Context(), agent, "never-claimed", ""); err != nil || len(found) != 0 {
		t.Fatalf("find a key never claimed = %+v, %v", found, err)
	}
	// Another caller's key: even an operator who may read the operation
	// does not find it as its own.
	if found, err = w.as(operator).FindOperations(t.Context(), operator, "r-local", ""); err != nil || len(found) != 0 {
		t.Fatalf("the operator finds the agent's key = %+v, %v", found, err)
	}
	// A key used twice names both.
	s2 := w.as(agent)
	p, err := s2.PlanReply(t.Context(), string(w.remote.Ref.ID), "again", "r-local")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s2.Reply(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if found, err = s.FindOperations(t.Context(), agent, "r-local", ""); err != nil || len(found) != 2 {
		t.Fatalf("a key used for two replies = %d, %v; want both", len(found), err)
	}
	// The lookup by kind and target stays for the caller alone.
	got, err := s.LookupOperation(t.Context(), model.ActionMessageReply, w.localOp.Key.Target, "r-local")
	if err != nil || got.Ref != w.localOp.Ref {
		t.Fatalf("the agent's own lookup = %+v, %v; want %s", got, err, w.localOp.Ref.ID)
	}
	_, err = w.as(operator).LookupOperation(t.Context(), model.ActionMessageReply, w.localOp.Key.Target, "r-local")
	wantCode(t, err, model.NotFound, "")
	// A viewer may not read operations at all.
	if _, err = w.as(viewer).LookupOperation(t.Context(), model.ActionMessageReply, w.localOp.Key.Target, "r-local"); err == nil {
		t.Fatal("a viewer found an operation")
	}
}

// TestReconcilerOf: heo tells which adapter a delivery left through and
// whether it declares reconcile, as the caller reads the path to it;
// an operation that produced no message says nothing.
func TestReconcilerOf(t *testing.T) {
	w := newDeliveryWorld(t)
	s := w.as(agent)
	got := s.ReconcilerOf(t.Context(), w.remoteOp)
	if got == nil || got.Adapter != adapterstest.Type || got.Reconciles {
		t.Fatalf("reconciler of the unknown reply = %+v, want the fake adapter without reconcile", got)
	}
	w.fake.EnableReconcile()
	if got = s.ReconcilerOf(t.Context(), w.remoteOp); got == nil || !got.Reconciles {
		t.Fatalf("after the adapter declares reconcile = %+v", got)
	}
	if got = s.ReconcilerOf(t.Context(), model.Operation{}); got != nil {
		t.Fatalf("an operation with no message = %+v, want nothing", got)
	}
	if got = (desk.Service{Client: s.Client, Tenant: tenant}).ReconcilerOf(t.Context(), w.remoteOp); got != nil {
		t.Fatalf("without an adapter lookup = %+v, want nothing", got)
	}
}
