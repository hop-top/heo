package desk_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

const (
	tenant   model.TenantID = "acme"
	operator model.ActorID  = "uid:1001"
	viewer   model.ActorID  = "uid:1002"
	agent    model.ActorID  = "uid:1003"
	agent2   model.ActorID  = "uid:1005"
	plugin   model.ActorID  = "plugin:triage"
	plugin2  model.ActorID  = "plugin:other"
	external model.ActorID  = "uid:1004"
	service  model.ActorID  = "svc:heo-runtime"
)

var call = model.Call{Correlation: "test"}

// world is one tenant's engine over an in-memory store, decided by
// heo's policy, with two inboxes on one channel: "local" keeps notes in
// uma, "provider" delivers them. The desk agent started one
// conversation in each.
type world struct {
	t      *testing.T
	e      uma.Engine
	pol    *authz.Policy
	gate   *attachmentGate
	clock  *portstest.FixedClock
	ch     model.ChannelRef
	fake   *adapterstest.Factory
	local  model.Conversation
	remote model.Conversation
	n      int
}

// attachmentGate is heo's policy, except that it refuses attachment reads
// by the actors in denied: a policy that tells attachments from the
// messages that hold them.
type attachmentGate struct {
	inner  policy.Authorizer
	denied map[model.ActorID]bool
	// refuse names actions refused to everyone: a policy that reads
	// without acting.
	refuse map[string]bool
	// hide names conversations (by ID) that no desk actor may read or
	// act on: the operator and the runtime still do.
	hide map[model.ID]bool
}

func (g *attachmentGate) Authorize(ctx context.Context, a policy.Authorization) (policy.Decision, error) {
	if g.refuse[a.Action] {
		return policy.Decision{PolicyVersion: "test", ReasonCode: "test.refused"}, nil
	}
	if g.hide[a.Resource.ID] && a.Principal.Actor != operator && a.Principal.Actor != service {
		return policy.Decision{PolicyVersion: "test", ReasonCode: "test.hidden"}, nil
	}
	if a.Action == model.ActionAttachmentRead && g.denied[a.Principal.Actor] {
		return policy.Decision{PolicyVersion: "test", ReasonCode: "test.attachment_denied"}, nil
	}
	return g.inner.Authorize(ctx, a)
}

func newWorld(t *testing.T, opts ...func(*uma.Config)) *world {
	t.Helper()
	return newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareEffects() }, opts...)
}

// newWorldFake is newWorld over a fake adapter set up by setup, before
// the engine starts: DeliverNotes always, and what setup adds.
func newWorldFake(t *testing.T, setup func(*adapterstest.Factory), opts ...func(*uma.Config)) *world {
	t.Helper()
	pol := authz.New(authz.Config{
		Owner: []authz.Role{authz.RoleOperator},
		Grants: []authz.Grant{
			{Actor: viewer, Roles: []authz.Role{authz.RoleViewer}},
			{Actor: agent, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: agent2, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: plugin, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: plugin2, Roles: []authz.Role{authz.RoleDesk}},
			{Actor: external, Roles: []authz.Role{authz.RoleExternal}},
		},
	}, tenant, operator, service)
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	fake := adapterstest.New()
	fake.DeliverNotes()
	setup(fake)
	gate := &attachmentGate{inner: pol}
	cfg := uma.Config{
		Store: memory.New(memory.Options{Clock: clock}), Authorizer: gate,
		Adapters: []adapter.Factory{fake}, Clock: clock, IDs: portstest.NewSequenceIDs(1),
		IdentityVerifiers: desk.IdentityVerifiers(), Blobs: &portstest.MemoryBlobs{},
	}
	for _, o := range opts {
		o(&cfg)
	}
	e, err := uma.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	w := &world{t: t, e: e, pol: pol, gate: gate, fake: fake, clock: clock}
	ctx := t.Context()
	op := w.client(operator)
	acc, err := op.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: w.cmd(), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: tenant}, Key: "fake", Type: adapterstest.Type,
		Configuration: adapterstest.Config{Account: "acct"}.Configuration(),
	}})
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	w.ch, _ = acc.Operation.Result.AsChannel()
	w.local = w.start(w.inbox("local", false), "ada")
	w.remote = w.start(w.inbox("provider", true), "bob")
	return w
}

func (w *world) cmd() model.Command {
	w.n++
	return model.Command{Call: call, Operation: model.OperationID(fmt.Sprintf("op-%d", w.n))}
}

func (w *world) client(actor model.ActorID) uma.Client {
	w.t.Helper()
	c, err := w.e.For(model.Principal{Tenant: tenant, Actor: actor})
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

// as is actor's desk, seeing what heo's policy lets it see.
func (w *world) as(actor model.ActorID) desk.Service {
	p := model.Principal{Tenant: tenant, Actor: actor}
	return desk.Service{Client: w.client(actor), Tenant: tenant, Sees: func(a model.Audience) bool { return w.pol.Sees(p, a) },
		Adapter: w.adapter, Capabilities: w.capabilities, Actor: actor, Now: w.clock.Now,
		Permits: func(who model.ActorID, action string) bool {
			ok, _ := w.pol.Permits(model.Principal{Tenant: tenant, Actor: who}, action)
			return ok
		}}
}

// adapter describes the adapter of channel ref, read as the runtime
// reads it.
func (w *world) adapter(ctx context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
	ch, err := w.client(service).Configuration().Channels(ctx, model.ListRequest{Call: call, Page: model.PageRequest{Limit: 10}})
	if err != nil {
		return adapter.Descriptor{}, err
	}
	for _, c := range ch.Items {
		if c.Ref == ref && c.Type == adapterstest.Type {
			return w.fake.Describe(), nil
		}
	}
	return adapter.Descriptor{}, model.NewError(model.NotFound, "channel not found")
}

// capabilities reads the live status of channel ref's capabilities, as
// the runtime reads it.
func (w *world) capabilities(ctx context.Context, ref model.ChannelRef) ([]model.CapabilityStatus, error) {
	return w.client(service).Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{Call: call, Ref: ref})
}

func (w *world) inbox(key string, notes bool) model.InboxRef {
	w.t.Helper()
	lock := true
	acc, err := w.client(operator).Configuration().ConfigureInbox(w.t.Context(), model.ConfigureInbox{Command: w.cmd(), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: w.ch, Key: key, Name: key, Destinations: []string{key},
		Selection:    model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		DeliverNotes: notes,
	}})
	if err != nil {
		w.t.Fatalf("inbox %s: %v", key, err)
	}
	ref, _ := acc.Operation.Result.AsInbox()
	return ref
}

// start has the agent start a conversation with who in inbox, and
// returns it once delivered.
func (w *world) start(inbox model.InboxRef, who string) model.Conversation {
	w.t.Helper()
	c := w.client(agent)
	acc, err := c.Conversations().Start(w.t.Context(), model.StartConversation{
		Command: w.cmd(), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: w.ch, Namespace: adapterstest.Namespace, Kind: "user", ID: who},
		Content:   desk.TextContent("hello " + who),
	})
	if err != nil {
		w.t.Fatalf("start: %v", err)
	}
	got := w.settle(acc.Operation.Ref)
	if got.ResultMessage == nil {
		w.t.Fatalf("start = %s %+v", got.State, got.Failure)
	}
	conv, err := c.Conversations().Get(w.t.Context(), model.Lookup[model.ConversationRef]{Call: call, Ref: got.ResultMessage.Conversation})
	if err != nil {
		w.t.Fatal(err)
	}
	return conv
}

// settle runs the workers until op leaves the queue.
func (w *world) settle(op model.OperationRef) model.Operation {
	w.t.Helper()
	ctx, cancel := context.WithCancel(w.t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.e.Runtime().Run(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: tenant, Actor: service}, Call: call},
			uma.WorkerOptions{Owner: "test", Poll: 5 * time.Millisecond, Lease: 10 * time.Second})
	}()
	defer func() { cancel(); wg.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := w.client(agent).Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: call, Ref: op})
		if err != nil {
			w.t.Fatal(err)
		}
		if got.State != model.Queued && got.State != model.RetryPending {
			return got
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("operation still %s", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// note adds a local note to the local conversation as the agent.
func (w *world) note(text string) model.MessageRef {
	w.t.Helper()
	s := w.as(agent)
	p, err := s.PlanNote(w.t.Context(), string(w.local.Ref.ID), text, desk.PlacementLocal, w.cmd().Operation)
	if err != nil {
		w.t.Fatal(err)
	}
	acc, err := s.AddNote(w.t.Context(), p)
	if err != nil || acc.Operation.ResultMessage == nil {
		w.t.Fatalf("note = %+v, %v", acc, err)
	}
	return *acc.Operation.ResultMessage
}

func wantCode(t *testing.T, err error, code model.ErrorCode, contains string) {
	t.Helper()
	if !model.IsCode(err, code) || !strings.Contains(err.Error(), contains) {
		t.Fatalf("err = %v, want %s containing %q", err, code, contains)
	}
}

func ids(ms []model.Message) map[model.ID]bool {
	out := map[model.ID]bool{}
	for _, m := range ms {
		out[m.Ref.ID] = true
	}
	return out
}

// TestConversationsAcrossInboxes: the desk lists conversations in every
// inbox it may read, narrowed by inbox (key) and status, and finds one
// by record ID; an unknown ID, a malformed one and a bad status are
// refused.
func TestConversationsAcrossInboxes(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	page := model.PageRequest{Limit: 10}
	all, err := s.ListConversations(ctx, desk.Filter{}, page)
	if err != nil || len(all.Items) != 2 {
		t.Fatalf("list = %+v, %v; want both inboxes' conversations", all.Items, err)
	}
	one, err := s.ListConversations(ctx, desk.Filter{Inbox: "provider"}, page)
	if err != nil || len(one.Items) != 1 || one.Items[0].Ref != w.remote.Ref {
		t.Fatalf("list --inbox provider = %+v, %v", one.Items, err)
	}
	none, err := s.ListConversations(ctx, desk.Filter{Statuses: []string{model.ConversationResolved}}, page)
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("list --status resolved = %+v, %v", none.Items, err)
	}
	_, err = s.ListConversations(ctx, desk.Filter{Statuses: []string{"archived"}}, page)
	wantCode(t, err, model.Validation, `status "archived"`)
	_, err = s.ListConversations(ctx, desk.Filter{Inbox: "nope"}, page)
	wantCode(t, err, model.NotFound, "nope")

	got, err := s.FindConversation(ctx, string(w.local.Ref.ID))
	if err != nil || got.Ref != w.local.Ref {
		t.Fatalf("find = %+v, %v", got, err)
	}
	_, err = s.FindConversation(ctx, "conv_404")
	wantCode(t, err, model.NotFound, "conversation conv_404")
	_, err = s.FindConversation(ctx, "a b")
	wantCode(t, err, model.NotFound, "conversation a b")

	// A viewer reads the same conversations.
	if v, err := w.as(viewer).ListConversations(ctx, desk.Filter{}, page); err != nil || len(v.Items) != 2 {
		t.Fatalf("viewer list = %+v, %v", v.Items, err)
	}
}

// TestTransitionPlan: a transition names the revision it moves; a stale
// one is refused in a preview, left to uma on submission (which refuses
// it, explained); a retry under the same operation replays; whether a
// move is allowed is uma's alone — a closed conversation plans fine and
// uma refuses the move.
func TestTransitionPlan(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	id := string(w.local.Ref.ID)
	_, err := s.PlanTransition(ctx, id, "stale", model.ConversationResolved, "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "conversation "+id+" is at revision "+string(w.local.Revision))
	p, err := s.PlanTransition(ctx, id, "stale", model.ConversationResolved, "op-x", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan at a stale revision: %v", err)
	}
	_, _, err = s.Transition(ctx, p)
	wantCode(t, err, model.Conflict, "is at revision "+string(w.local.Revision))

	p, err = s.PlanTransition(ctx, id, w.local.Revision, model.ConversationResolved, "op-resolve", channels.Preview)
	if err != nil || len(p.Warnings) != 0 || p.Request.State != model.ConversationResolved || p.Current.Status != model.ConversationOpen {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	conv, acc, err := s.Transition(ctx, p)
	if err != nil || conv.Status != model.ConversationResolved || conv.Revision == w.local.Revision || acc.Operation.State != model.Committed {
		t.Fatalf("resolve = %+v %+v, %v", conv, acc, err)
	}
	_, again, err := s.Transition(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want the replayed operation", again, err)
	}
	p, err = s.PlanTransition(ctx, id, conv.Revision, model.ConversationResolved, "op-same", channels.Preview)
	if err != nil || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "already resolved") {
		t.Fatalf("plan to the current status = %+v, %v", p, err)
	}
	_, err = s.PlanTransition(ctx, id, conv.Revision, "archived", "op-bad", channels.Preview)
	wantCode(t, err, model.Validation, "state")

	// Closed is terminal: uma refuses reopening it.
	if _, err := w.client(agent).Conversations().Transition(ctx, model.Transition{
		Command: model.Command{Call: call, Operation: "op-close", ExpectedRevision: conv.Revision}, Conversation: conv.Ref, State: model.ConversationClosed,
	}); err != nil {
		t.Fatal(err)
	}
	closed, err := s.FindConversation(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.PlanTransition(ctx, id, closed.Revision, model.ConversationOpen, "op-reopen", channels.Preview)
	if err != nil {
		t.Fatalf("plan reopening a closed conversation: %v (heo leaves the lifecycle to uma)", err)
	}
	_, _, err = s.Transition(ctx, p)
	wantCode(t, err, model.Conflict, "terminal")

	// A viewer may not move conversations: uma refuses it.
	p, err = w.as(viewer).PlanTransition(ctx, string(w.remote.Ref.ID), w.remote.Revision, model.ConversationResolved, "op-v", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.as(viewer).Transition(ctx, p); err == nil {
		t.Fatal("viewer transition succeeded")
	}
}

// TestNotesForDeskOnly: the desk sees a conversation's public messages
// and its internal notes, the notes alone in a note listing; a viewer
// sees the public messages and never a note, listed or by ID.
func TestNotesForDeskOnly(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	note := w.note("internal only")
	page := model.PageRequest{Limit: 10}
	conv := string(w.local.Ref.ID)

	msgs, err := w.as(agent).ListMessages(ctx, desk.Filter{Conversation: conv}, page)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(msgs.Items); len(got) != 2 || !got[note.ID] {
		t.Fatalf("desk message list = %v, want the public start and the note", got)
	}
	notes, err := w.as(agent).ListNotes(ctx, desk.Filter{}, page)
	if err != nil || len(notes.Items) != 1 || notes.Items[0].Ref != note || !desk.IsNote(notes.Items[0]) {
		t.Fatalf("desk note list = %+v, %v", notes.Items, err)
	}
	if m, err := w.as(agent).FindMessage(ctx, string(note.ID), "", true); err != nil || m.Ref != note || desk.Text(m.Content) != "internal only" {
		t.Fatalf("desk note get = %+v, %v", m, err)
	}
	pub := msgs.Items[0]
	if desk.IsNote(pub) {
		pub = msgs.Items[1]
	}
	_, err = w.as(agent).FindMessage(ctx, string(pub.Ref.ID), conv, true)
	wantCode(t, err, model.NotFound, "not an internal note")

	v := w.as(viewer)
	msgs, err = v.ListMessages(ctx, desk.Filter{}, page)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(msgs.Items); got[note.ID] || !got[pub.Ref.ID] {
		t.Fatalf("viewer message list = %v, want the public messages only", got)
	}
	if notes, err := v.ListNotes(ctx, desk.Filter{}, page); err != nil || len(notes.Items) != 0 {
		t.Fatalf("viewer note list = %+v, %v", notes.Items, err)
	}
	for _, asNote := range []bool{false, true} {
		_, err = v.FindMessage(ctx, string(note.ID), "", asNote)
		wantCode(t, err, model.NotFound, string(note.ID))
	}
}

// TestHeoHidesWhatTheCallerMayNotSee: heo's own audience rule filters
// every message it returns, whatever uma returned: a desk client whose
// Sees refuses internal content gets no note on any read path.
func TestHeoHidesWhatTheCallerMayNotSee(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	note := w.note("internal only")
	s := w.as(agent)
	s.Sees = func(a model.Audience) bool { return a.Visibility == model.Public }
	page := model.PageRequest{Limit: 10}
	msgs, err := s.ListMessages(ctx, desk.Filter{}, page)
	if err != nil || ids(msgs.Items)[note.ID] || len(msgs.Items) == 0 {
		t.Fatalf("message list = %+v, %v; want public messages only", msgs.Items, err)
	}
	if notes, err := s.ListNotes(ctx, desk.Filter{}, page); err != nil || len(notes.Items) != 0 {
		t.Fatalf("note list = %+v, %v", notes.Items, err)
	}
	_, err = s.FindMessage(ctx, string(note.ID), "", false)
	wantCode(t, err, model.NotFound, string(note.ID))
	s.Sees = nil
	if notes, err := s.ListNotes(ctx, desk.Filter{}, page); err != nil || len(notes.Items) != 0 {
		t.Fatalf("note list without Sees = %+v, %v; want none (public only)", notes.Items, err)
	}
}

// TestNoteListPages: a note page holds at most --limit notes, however
// many public messages lie between them, and the next page continues
// where it stopped, missing none.
func TestNoteListPages(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	var want []model.MessageRef
	for i := range 5 {
		want = append(want, w.note(fmt.Sprintf("note %d", i)))
		p, err := s.PlanReply(ctx, string(w.local.Ref.ID), fmt.Sprintf("reply %d", i), w.cmd().Operation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Reply(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	var got []model.MessageRef
	req := model.PageRequest{Limit: 2}
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("note pages never complete")
		}
		p, err := s.ListNotes(ctx, desk.Filter{Conversation: string(w.local.Ref.ID)}, req)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Items) > 2 {
			t.Fatalf("page of %d notes, limit 2", len(p.Items))
		}
		for _, m := range p.Items {
			got = append(got, m.Ref)
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.After = p.Next
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("notes paged = %v, want %v", got, want)
	}
}

// TestNotePlacement: a note's stated placement must be its inbox's —
// local where the inbox keeps notes, provider where it delivers them —
// else a conflict naming the inbox; a local note commits, a provider
// one queues for delivery. Both are internal.
func TestNotePlacement(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	local, remote := string(w.local.Ref.ID), string(w.remote.Ref.ID)

	_, err := s.PlanNote(ctx, local, "x", desk.PlacementProvider, "op-1")
	var pe *desk.PlacementError
	if !errors.As(err, &pe) || pe.Inbox.Key != "local" || pe.Want != desk.PlacementProvider {
		t.Fatalf("provider note on a local inbox = %v", err)
	}
	wantCode(t, err, model.Conflict, "inbox local, which places notes local")
	_, err = s.PlanNote(ctx, remote, "x", desk.PlacementLocal, "op-2")
	wantCode(t, err, model.Conflict, "inbox provider, which places notes provider")
	_, err = s.PlanNote(ctx, local, "x", "both", "op-3")
	wantCode(t, err, model.Validation, `placement "both"`)
	_, err = s.PlanNote(ctx, local, " ", desk.PlacementLocal, "op-4")
	wantCode(t, err, model.Validation, "text")

	p, err := s.PlanNote(ctx, local, "kept here", desk.PlacementLocal, "op-5")
	if err != nil || p.Placement != desk.PlacementLocal || p.Inbox.Key != "local" {
		t.Fatalf("local plan = %+v, %v", p, err)
	}
	acc, err := s.AddNote(ctx, p)
	if err != nil || acc.Operation.State != model.Committed {
		t.Fatalf("local note = %+v, %v", acc, err)
	}
	p, err = s.PlanNote(ctx, remote, "sent there", desk.PlacementProvider, "op-6")
	if err != nil {
		t.Fatal(err)
	}
	acc, err = s.AddNote(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Operation.ResultMessage == nil {
		t.Fatalf("provider note = %+v, %v", acc, err)
	}
	m, err := s.FindMessage(ctx, string(acc.Operation.ResultMessage.ID), remote, true)
	if err != nil || !desk.IsNote(m) {
		t.Fatalf("provider note read back = %+v, %v", m, err)
	}
}

// TestReplyPlan: a reply plan names the conversation, its inbox and
// the stored reply route it leaves by; submitted, uma queues it (the
// in-memory store is not durable: the e2e suite checks durability on
// Postgres), and a retry under the same operation replays it.
func TestReplyPlan(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	_, err := s.PlanReply(ctx, string(w.local.Ref.ID), "", "op-1")
	wantCode(t, err, model.Validation, "text")
	_, err = s.PlanReply(ctx, "conv_404", "hi", "op-1")
	wantCode(t, err, model.NotFound, "conv_404")

	p, err := s.PlanReply(ctx, string(w.local.Ref.ID), "thanks", "op-reply")
	if err != nil {
		t.Fatal(err)
	}
	if p.Inbox.Key != "local" || p.Destination() != w.local.ReplyRoute || p.Destination().ID == "" ||
		p.Request.Conversation != w.local.Ref || desk.Text(p.Request.Content) != "thanks" {
		t.Fatalf("plan = %+v", p)
	}
	acc, err := s.Reply(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Operation.ResultMessage == nil {
		t.Fatalf("reply = %+v, %v", acc, err)
	}
	again, err := s.Reply(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Fatalf("delivered reply = %s %+v", got.State, got.Failure)
	}
}
