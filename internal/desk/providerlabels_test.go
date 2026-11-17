package desk_test

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/model"
)

// boxes are the mailboxes the fake provider holds in these tests.
func boxes() []model.ProviderMailbox {
	return []model.ProviderMailbox{
		{ID: "INBOX", Name: "INBOX", Kind: model.MailboxLabel, Role: model.RoleInbox},
		{ID: "Label_1", Name: "Clients/Acme", Kind: model.MailboxLabel},
		{ID: "Label_2", Name: "Billing", Kind: model.MailboxLabel},
		{ID: "Label_3", Name: "billing", Kind: model.MailboxLabel},
		{ID: "Label_4", Name: "Plans", Kind: model.MailboxLabel},
		{ID: "Cat_1", Name: "Plans", Kind: model.MailboxCategory},
		{ID: "Label_5", Name: "Label_1", Kind: model.MailboxLabel},
	}
}

// mailboxWorld is a world whose provider lists and creates mailboxes,
// files conversations (state) and tags a message (Gmail's label effect).
func mailboxWorld(t *testing.T) *world {
	t.Helper()
	var fake *adapterstest.Factory
	return newWorldFake(t, func(f *adapterstest.Factory) {
		fake = f
		f.DeclareMailboxes(boxes()...)
		f.DeclareState(adapterstest.StateOptions{})
		f.DeclareEffect(google.ExtensionLabels)
	}, func(c *uma.Config) { c.Extensions = fake.Adapter().Extensions })
}

// labelDesk is actor's desk with the runtime's view of the channels: the
// adapters' effects and the channel list, which heo's daemon supplies.
func (w *world) labelDesk(actor model.ActorID) desk.Service {
	s := w.as(actor)
	s.Effects = func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
		return w.fake.Adapter().Extensions, nil
	}
	s.ValidateAction = func(_ context.Context, _ model.ChannelRef, a model.Audience, x model.Extension) ([]model.ValidationIssue, error) {
		return w.fake.ValidateAction(a, x), nil
	}
	s.Channels = func(ctx context.Context) ([]model.Channel, error) {
		p, err := w.client(service).Configuration().Channels(ctx, model.ListRequest{Call: call, Page: model.PageRequest{Limit: 50}})
		return p.Items, err
	}
	return s
}

// taggable has the provider report a message with a parent, so the
// conversation's reply route names a provider message to tag.
func (w *world) taggable() model.Conversation {
	w.t.Helper()
	w.inject(adapterstest.Inject{Event: "tg", Message: "tg", From: "zed", Destination: "local", Text: "hi", Parent: "gmail-msg-1"})
	p, err := w.client(agent).Conversations().List(w.t.Context(), model.ListRequest{Call: call, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, c := range p.Items {
		if c.ReplyRoute.ParentID == "gmail-msg-1" {
			return c
		}
	}
	w.t.Fatalf("no conversation replies to the provider message: %+v", p.Items)
	return model.Conversation{}
}

// ListProviderLabels reads the provider's list as the caller: every
// mailbox with its channel, in the one order, by channel key or ID or
// across every channel the caller reaches.
func TestListProviderLabels(t *testing.T) {
	w := mailboxWorld(t)
	d := w.labelDesk(agent)
	all, err := d.ListProviderLabels(t.Context(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != len(boxes()) {
		t.Fatalf("list = %+v", all)
	}
	for i, l := range all[1:] {
		prev := all[i]
		if prev.Kind > l.Kind || (prev.Kind == l.Kind && prev.Name > l.Name) {
			t.Errorf("not in channel, kind, name order: %+v before %+v", prev, l)
		}
	}
	for _, l := range all {
		if l.Channel != "fake" {
			t.Errorf("channel = %q, want its key", l.Channel)
		}
	}
	if !slices.Contains(all, desk.ProviderLabel{Channel: "fake", ID: "INBOX", Name: "INBOX", Kind: "label", Role: "inbox"}) {
		t.Errorf("INBOX is not listed with its role: %+v", all)
	}
	for _, ref := range []string{"fake", string(w.ch.ID)} {
		got, err := d.ListProviderLabels(t.Context(), ref)
		if err != nil || !reflect.DeepEqual(got, all) {
			t.Errorf("list --channel %s = %+v, %v", ref, got, err)
		}
	}
	// Stable: the same order every time.
	again, _ := d.ListProviderLabels(t.Context(), "")
	if !reflect.DeepEqual(again, all) {
		t.Errorf("a second list differs: %+v", again)
	}
	_, err = d.ListProviderLabels(t.Context(), "nope")
	wantCode(t, err, model.NotFound, "no channel with this key or ID")
	// A viewer may not run the provider's read; a caller without
	// channel knowledge is told.
	_, err = w.labelDesk(viewer).ListProviderLabels(t.Context(), "")
	wantCode(t, err, model.Unauthorized, "")
	_, err = w.as(agent).ListProviderLabels(t.Context(), "")
	wantCode(t, err, model.Unsupported, "channels are unknown")
}

// A channel whose adapter declares no mailbox list is left out of the
// listing and refused when named.
func TestListProviderLabelsGate(t *testing.T) {
	w := newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareEffect(google.ExtensionLabels) })
	d := w.labelDesk(agent)
	got, err := d.ListProviderLabels(t.Context(), "")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("list = %#v, %v, want an empty list", got, err)
	}
	_, err = d.ListProviderLabels(t.Context(), "fake")
	wantCode(t, err, model.Unsupported, "does not declare extension_query")
	_, err = d.PlanProviderLabelCreate(t.Context(), "X", "", "", "pl-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "does not declare")
	_, err = d.PlanProviderLabelCreate(t.Context(), "X", "", "fake", "pl-2", channels.Submit)
	wantCode(t, err, model.Unsupported, "does not declare")
}

// A create is the adapter's effect on the channel: a preview checks the
// fields and says the name exists; a submission queues one operation
// under its key and replays; nothing else is written.
func TestProviderLabelCreate(t *testing.T) {
	w := mailboxWorld(t)
	d := w.labelDesk(agent)
	ctx := t.Context()

	p, err := d.PlanProviderLabelCreate(ctx, "Clients/Beta", "", "", "pc-1", channels.Preview)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Declaration == nil || p.Declaration.Effect != "test.heo.fake.mailbox_create" || p.Existing != nil ||
		p.Request.Action.Name != "test.heo.fake.mailbox_create" || p.Request.Target != w.ch.Ref() {
		t.Fatalf("preview = %+v", p)
	}
	var data map[string]string
	if err := json.Unmarshal(p.Request.Action.Data, &data); err != nil || data["name"] != "Clients/Beta" {
		t.Fatalf("action data = %s", p.Request.Action.Data)
	}
	p, err = d.PlanProviderLabelCreate(ctx, "clients/acme", "", "fake", "pc-2", channels.Preview)
	if err != nil || p.Existing == nil || p.Existing.ID != "Label_1" {
		t.Fatalf("preview of an existing name = %+v, %v", p, err)
	}
	for name, bad := range map[string]string{"empty": "", "slash": "/x", "doubled": "a//b", "padded": " a"} {
		if _, err := d.PlanProviderLabelCreate(ctx, bad, "", "", "pc-3", channels.Preview); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: %v, want a validation error", name, err)
		}
	}
	if _, err := d.PlanProviderLabelCreate(ctx, "X", "folder-ish", "", "pc-3", channels.Preview); !model.IsCode(err, model.Validation) {
		t.Errorf("a kind no provider has: %v", err)
	}

	p, err = d.PlanProviderLabelCreate(ctx, "Clients/Beta", "", "", "pc-4", channels.Submit)
	if err != nil || p.Declaration != nil {
		t.Fatalf("submit plan = %+v, %v", p, err)
	}
	acc, err := d.CreateProviderLabel(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
		t.Fatalf("create = %+v, %v", acc, err)
	}
	again, err := d.CreateProviderLabel(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v, want the same operation replayed", again, err)
	}
	vp, err := w.labelDesk(viewer).PlanProviderLabelCreate(ctx, "V", "", "", "pc-5", channels.Submit)
	if err != nil {
		t.Fatalf("a viewer's plan: %v", err)
	}
	_, err = w.labelDesk(viewer).CreateProviderLabel(ctx, vp)
	wantCode(t, err, model.Unauthorized, "")
}

// A plugin actor's create is offered to the hold point under
// provider-label:create on the channel and stored held, with nothing queued.
func TestHeldProviderLabelCreate(t *testing.T) {
	w := mailboxWorld(t)
	var asked []desk.HoldQuery
	plug, _ := holding(w.labelDesk(plugin), recordPolicy{&asked})
	ctx := t.Context()
	p, err := plug.PlanProviderLabelCreate(ctx, "Held", "", "", "hc-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := plug.Hold(ctx, &p.Request.Command, desk.HoldInput{Action: model.ActionExtensionExecute, Verb: "provider-label", Class: "create",
		Channel: string(p.Channel.Ref.ID), Target: p.Channel.Ref.Ref(), Preview: "p"})
	if err != nil {
		t.Fatal(err)
	}
	acc, err := plug.CreateProviderLabel(ctx, p)
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Held {
		t.Fatalf("hold = %+v, %v", acc, err)
	}
	if len(asked) != 1 || asked[0].Verb != "provider-label" || asked[0].Class != "create" || asked[0].Action != model.ActionExtensionExecute {
		t.Fatalf("hold query = %+v", asked)
	}
}

// A move resolves provider mailbox names to the IDs the provider files
// by, through the channel's list: an ID the list holds is used as it is
// and wins over a name spelled like it; a name matches exactly, then
// without regard to case; a name two mailboxes share is refused with
// their IDs, never guessed; a value nothing holds goes as given; a role is
// no name.
func TestMoveResolvesProviderMailboxNames(t *testing.T) {
	w := mailboxWorld(t)
	conv := w.local
	d := w.labelDesk(agent)
	plan := func(ch desk.StateChange) (desk.StatePlan, error) {
		return d.PlanState(t.Context(), string(conv.Ref.ID), ch, "", "mv-1", channels.Submit)
	}
	move := func(to string, add, remove []string) desk.StateChange {
		return desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: to}, Add: add, Remove: remove}
	}
	// A name is its ID; an ID stays; the resolved pairs say so.
	p, err := plan(move("Clients/Acme", nil, nil))
	if err != nil || p.Change.Destination.ID != "Label_1" || !reflect.DeepEqual(p.Resolved, []desk.Resolved{{Given: "Clients/Acme", ID: "Label_1", Name: "Clients/Acme"}}) {
		t.Fatalf("move --to a name = %+v, %v", p, err)
	}
	if p, err = plan(move("Label_2", nil, nil)); err != nil || p.Change.Destination.ID != "Label_2" || len(p.Resolved) != 0 {
		t.Fatalf("move --to an ID = %+v, %v", p, err)
	}
	// The ID wins over a mailbox named like it (Label_5 is named Label_1).
	if p, err = plan(move("Label_1", nil, nil)); err != nil || p.Change.Destination.ID != "Label_1" {
		t.Fatalf("an ID spelled like a name = %+v, %v", p, err)
	}
	// Labels: names and IDs mix; case is matched after the exact spelling.
	p, err = plan(move("", []string{"Clients/Acme", "Label_2", "plans-nope"}, []string{"INBOX", "BILLING"}))
	if err == nil {
		t.Fatalf("BILLING names two labels: %+v", p)
	}
	wantCode(t, err, model.Validation, "Label_2")
	wantCode(t, err, model.Validation, "Label_3")
	p, err = plan(move("", []string{"Clients/Acme", "Label_2", "plans-nope"}, []string{"INBOX", "billing"}))
	if err != nil || !reflect.DeepEqual(p.Change.Add, []string{"Label_1", "Label_2", "plans-nope"}) || !reflect.DeepEqual(p.Change.Remove, []string{"INBOX", "Label_3"}) {
		t.Fatalf("move --add --remove = %+v, %v", p.Change, err)
	}
	if p.Change.Destination.ID != "" {
		t.Errorf("an empty destination became %q", p.Change.Destination.ID)
	}
	// Two mailboxes of different kinds share a name: refused, naming both.
	_, err = plan(move("Plans", nil, nil))
	wantCode(t, err, model.Validation, "Label_4")
	wantCode(t, err, model.Validation, "Cat_1")
	// A role is no name, and a caller's own slices are not changed.
	in := []string{"Clients/Acme"}
	if p, err = plan(desk.StateChange{Verb: model.StateVerbMove, Add: in}); err != nil || in[0] != "Clients/Acme" || p.Change.Add[0] != "Label_1" {
		t.Fatalf("move changed the caller's list: %v, %+v, %v", in, p.Change, err)
	}
	if p, err = plan(desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{Role: model.RoleJunk}}); err != nil || len(p.Resolved) != 0 {
		t.Fatalf("a role = %+v, %v", p, err)
	}
	if v, class := p.Change.HoldClass(); v != "state" || class != "spam" {
		t.Errorf("hold class = %s:%s", v, class)
	}
	// The hold class follows the resolved ID, not the name.
	p, _ = plan(move("Clients/Acme", nil, nil))
	if v, class := p.Change.HoldClass(); v != "move" || class != "Label_1" {
		t.Errorf("hold class = %s:%s, want move:Label_1", v, class)
	}
}

// Where the channel cannot list its mailboxes, a move takes what it
// always took: IDs, as given.
func TestMoveWithoutAMailboxListTakesIDsAsGiven(t *testing.T) {
	w := newWorldFake(t, func(f *adapterstest.Factory) { f.DeclareState(adapterstest.StateOptions{}) })
	d := w.labelDesk(agent)
	p, err := d.PlanState(t.Context(), string(w.local.Ref.ID), desk.StateChange{Verb: model.StateVerbMove, Destination: model.MoveDestination{ID: "Clients/Acme"}}, "", "mv-2", channels.Submit)
	if err != nil || p.Change.Destination.ID != "Clients/Acme" || len(p.Resolved) != 0 {
		t.Fatalf("move = %+v, %v", p, err)
	}
}

// A tag resolves Gmail label names the same way, in --add and --remove;
// Outlook's --set names go as given.
func TestTagResolvesProviderMailboxNames(t *testing.T) {
	w := mailboxWorld(t)
	conv := w.taggable()
	d := w.labelDesk(agent)
	p, err := d.PlanTag(t.Context(), string(conv.Ref.ID), desk.MailTag{Add: []string{"Clients/Acme", "STARRED"}, Remove: []string{"INBOX", "Billing"}}, "tg-1", channels.Preview)
	if err != nil {
		t.Fatalf("tag: %v", err)
	}
	var data map[string][]string
	if err := json.Unmarshal(p.Request.Action.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data["add"], []string{"Label_1", "STARRED"}) || !reflect.DeepEqual(data["remove"], []string{"INBOX", "Label_2"}) {
		t.Fatalf("action data = %s", p.Request.Action.Data)
	}
	if len(p.Resolved) != 2 || p.Tag.Add[0] != "Label_1" {
		t.Errorf("resolved = %+v, tag = %+v", p.Resolved, p.Tag)
	}
	if _, err = d.PlanTag(t.Context(), string(conv.Ref.ID), desk.MailTag{Add: []string{"Plans"}}, "tg-2", channels.Submit); !model.IsCode(err, model.Validation) ||
		!strings.Contains(err.Error(), "Cat_1") {
		t.Fatalf("an ambiguous name: %v", err)
	}
}

// A held write's Channel is an inbox, whose channel the trust counters
// are scoped to, or a channel itself, as a provider mailbox create's is.
func TestTrustChannel(t *testing.T) {
	w := mailboxWorld(t)
	d := w.labelDesk(agent)
	for name, ref := range map[string]string{
		"inbox ID":   string(w.local.Ref.Inbox.ID),
		"inbox key":  "local",
		"channel ID": string(w.ch.ID),
	} {
		got, err := d.TrustChannel(t.Context(), ref)
		if err != nil || got != w.ch {
			t.Errorf("%s: %+v, %v, want %+v", name, got, err, w.ch)
		}
	}
	_, err := d.TrustChannel(t.Context(), "nope")
	wantCode(t, err, model.NotFound, "")
}
