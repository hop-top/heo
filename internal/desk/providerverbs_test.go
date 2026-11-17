package desk_test

import (
	"context"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// undeclared is a world's adapter lookup that declares only what the
// fake declared before edit, delete and react.
func undeclared(w *world) func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
	return func(ctx context.Context, ref model.ChannelRef) (adapter.Descriptor, error) {
		d, err := w.adapter(ctx, ref)
		d.Capabilities = []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend}
		return d, err
	}
}

// firstMessage is the first message of w's local conversation.
func (w *world) firstMessage() model.Message {
	w.t.Helper()
	p, err := w.as(agent).ListMessages(w.t.Context(), desk.Filter{Conversation: string(w.local.Ref.ID)}, model.PageRequest{Limit: 10})
	if err != nil || len(p.Items) == 0 {
		w.t.Fatalf("messages = %+v, %v", p.Items, err)
	}
	return p.Items[0]
}

// TestProviderVerbPreviewRefusesUndeclared: a preview resolves the
// target as the caller, then refuses what uma would refuse at
// submission — an adapter that does not declare the capability — with
// model.Unsupported before any plan, for edit, delete and react. A
// submission plans without the check: uma gates it, atomically with the
// claim, so heo adds no second check there.
func TestProviderVerbPreviewRefusesUndeclared(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	s.Adapter = undeclared(w)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)

	_, err := s.PlanEdit(ctx, id, conv, m.Revision, "new", "op-e", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare edit")
	_, err = s.PlanDelete(ctx, id, conv, m.Revision, "op-d", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare delete")
	_, err = s.PlanReact(ctx, id, conv, "👍", false, "", "op-r", channels.Preview)
	wantCode(t, err, model.Unsupported, "does not declare react")
	_, err = s.PlanReact(ctx, id, conv, "👍", true, "", "op-r", channels.Preview)
	wantCode(t, err, model.Unsupported, "reaction remove")
	// The refusal names where to look.
	if _, err = s.PlanEdit(ctx, id, conv, m.Revision, "new", "op-e", channels.Preview); !strings.Contains(err.Error(), "heo channel get "+string(w.ch.ID)) {
		t.Errorf("refusal %q does not name channel get", err)
	}

	// A submission is not checked here.
	if _, err := s.PlanEdit(ctx, id, conv, m.Revision, "new", "op-e", channels.Submit); err != nil {
		t.Fatalf("submit plan: %v", err)
	}

	// With no adapter lookup, a preview refuses everything.
	s.Adapter = nil
	_, err = s.PlanEdit(ctx, id, conv, m.Revision, "new", "op-e", channels.Preview)
	wantCode(t, err, model.Unsupported, "adapter is unknown")
}

// TestProviderVerbPreviewNeedsNoChannelRead: the declaration check reads
// the binary through the runtime, not the channel as the caller, so a
// desk actor, who may not read channels, previews an edit.
func TestProviderVerbPreviewNeedsNoChannelRead(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	m := w.firstMessage()
	if _, err := w.client(agent).Configuration().Capabilities(ctx, model.Lookup[model.ChannelRef]{Call: call, Ref: w.ch}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("desk reads channels: %v; the premise is that it may not", err)
	}
	p, err := w.as(agent).PlanEdit(ctx, string(m.Ref.ID), "", m.Revision, "new", "op-e", channels.Preview)
	if err != nil {
		t.Fatalf("desk preview: %v", err)
	}
	d := p.Declaration
	if d == nil || d.Capability != model.CapabilityEdit || d.Adapter != adapterstest.Type || d.Channel != w.ch {
		t.Fatalf("declaration = %+v", d)
	}
	for _, want := range []string{"edit declared by " + string(adapterstest.Type), "live availability not checked", "heo channel check"} {
		if !strings.Contains(d.Line(), want) {
			t.Errorf("capability line %q lacks %q", d.Line(), want)
		}
	}
}

// TestEditSendsExpectedRevisionAndApplies: an edit always carries the
// revision it replaces; uma queues it, the provider applies it, and
// the message changes only then. A stale revision is a conflict that
// names the current one, in a preview and, left to uma, on submission.
func TestEditSendsExpectedRevisionAndApplies(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)

	p, err := s.PlanEdit(ctx, id, conv, m.Revision, "hello, edited", "op-edit", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.ExpectedRevision != m.Revision || p.Request.Message != m.Ref {
		t.Fatalf("request = %+v, want expected revision %s on %v", p.Request, m.Revision, m.Ref)
	}
	_, err = s.PlanEdit(ctx, id, conv, "", "x", "op-x", channels.Submit)
	wantCode(t, err, model.Validation, "revision")
	_, err = s.PlanEdit(ctx, id, conv, m.Revision, " ", "op-x", channels.Submit)
	wantCode(t, err, model.Validation, "text")

	acc, err := s.Edit(ctx, p)
	if err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("edit = %+v, %v", acc, err)
	}
	if cur := w.firstMessage(); desk.Text(cur.Content) != desk.Text(m.Content) {
		t.Fatalf("message changed before the provider applied the edit: %q", desk.Text(cur.Content))
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted {
		t.Fatalf("edit delivery = %s %+v", got.State, got.Failure)
	}
	cur := w.firstMessage()
	if desk.Text(cur.Content) != "hello, edited" || cur.LastEdit == nil {
		t.Fatalf("message after the edit = %q last edit %+v", desk.Text(cur.Content), cur.LastEdit)
	}
	select {
	case e := <-w.fake.Effects():
		if e.Kind != "edit" || e.Text != "hello, edited" {
			t.Fatalf("provider effect = %+v", e)
		}
	default:
		t.Fatal("the provider saw no edit")
	}

	// m is stale now: the message moved on.
	_, err = s.PlanEdit(ctx, id, conv, m.Revision, "again", "op-2", channels.Preview)
	wantCode(t, err, model.Conflict, "message "+id+" is at revision "+string(cur.Revision))
	p, err = s.PlanEdit(ctx, id, conv, m.Revision, "again", "op-2", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan at a stale revision: %v", err)
	}
	_, err = s.Edit(ctx, p)
	wantCode(t, err, model.Conflict, "is at revision "+string(cur.Revision))

	// The same edit under the same operation replays.
	p, _ = s.PlanEdit(ctx, id, conv, cur.Revision, "third", "op-3", channels.Submit)
	first, err := s.Edit(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Edit(ctx, p)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want the replayed operation", again, err)
	}
}

// TestDeleteTombstones: a delete is a tombstone applied by the provider,
// not a privacy redaction; a deleted message cannot be edited or deleted
// again, a conflict in a preview.
func TestDeleteTombstones(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)
	// A stale revision conflicts, naming the current one: refused in a
	// preview, explained from uma's refusal on submission.
	_, err := s.PlanDelete(ctx, id, conv, "stale", "op-stale", channels.Preview)
	wantCode(t, err, model.Conflict, "message "+id+" is at revision "+string(m.Revision))
	stale, err := s.PlanDelete(ctx, id, conv, "stale", "op-stale", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan at a stale revision: %v", err)
	}
	_, err = s.Delete(ctx, stale)
	wantCode(t, err, model.Conflict, "is at revision "+string(m.Revision))

	p, err := s.PlanDelete(ctx, id, conv, m.Revision, "op-del", channels.Submit)
	if err != nil || p.Request.ExpectedRevision != m.Revision {
		t.Fatalf("plan = %+v, %v", p, err)
	}
	acc, err := s.Delete(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted {
		t.Fatalf("delete delivery = %s %+v", got.State, got.Failure)
	}
	cur := w.firstMessage()
	if !cur.Deleted || cur.Redacted || desk.Text(cur.Content) != "" {
		t.Fatalf("message after the delete = %+v", cur)
	}
	_, err = s.PlanEdit(ctx, id, conv, cur.Revision, "x", "op-x", channels.Preview)
	wantCode(t, err, model.Conflict, "is deleted")
	_, err = s.PlanDelete(ctx, id, conv, cur.Revision, "op-y", channels.Preview)
	wantCode(t, err, model.Conflict, "is deleted")
}

// TestReactAddsAndRemoves: a reaction rides Messages.React with its
// remove flag; the expected revision is optional; a bad emoji is refused
// before uma; the reaction list shows what uma stores and says when the
// set is not complete.
func TestReactAddsAndRemoves(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	s := w.as(agent)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)

	_, err := s.PlanReact(ctx, id, conv, "two words", false, "", "op-x", channels.Submit)
	wantCode(t, err, model.Validation, "emoji")
	p, err := s.PlanReact(ctx, id, conv, "👍", false, "", "op-react", channels.Submit)
	if err != nil || p.Request.ExpectedRevision != "" || p.Request.Remove || p.Request.Emoji != "👍" {
		t.Fatalf("plan = %+v, %v", p.Request, err)
	}
	acc, err := s.React(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted {
		t.Fatalf("react delivery = %s %+v", got.State, got.Failure)
	}
	l, err := s.Reactions(ctx, id, conv)
	if err != nil || len(l.Reactions) != 1 || l.Reactions[0].Emoji != "👍" || l.Reactions[0].Actor != agent {
		t.Fatalf("reactions = %+v, %v", l, err)
	}
	if l.Complete {
		t.Error("reactions derived from our own command reported complete")
	}

	cur := w.firstMessage()
	p, err = s.PlanReact(ctx, id, conv, "👍", true, cur.Revision, "op-unreact", channels.Submit)
	if err != nil || !p.Request.Remove || p.Request.ExpectedRevision != cur.Revision {
		t.Fatalf("remove plan = %+v, %v", p.Request, err)
	}
	acc, err = s.React(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := w.settle(acc.Operation.Ref); got.State != model.ProviderAccepted {
		t.Fatalf("unreact delivery = %s %+v", got.State, got.Failure)
	}
	if l, err = s.Reactions(ctx, id, conv); err != nil || len(l.Reactions) != 0 || l.Reactions == nil {
		t.Fatalf("reactions after remove = %+v, %v; want an empty list", l, err)
	}
	// Reading reactions is a read: it needs no adapter at all.
	s.Adapter = nil
	if _, err := s.Reactions(ctx, id, conv); err != nil {
		t.Fatalf("reaction list without an adapter: %v", err)
	}
}

// TestSubmitUnsupportedByUma: uma refuses an edit, delete or reaction
// on an adapter that does not declare it, synchronously and before it
// stores anything, so heo's own preview check is never the only gate.
func TestSubmitUnsupportedByUma(t *testing.T) {
	w := newWorldFake(t, func(*adapterstest.Factory) {})
	ctx := t.Context()
	s := w.as(agent)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)

	pe, _ := s.PlanEdit(ctx, id, conv, m.Revision, "x", "op-e", channels.Submit)
	_, err := s.Edit(ctx, pe)
	wantCode(t, err, model.Unsupported, "")
	pd, _ := s.PlanDelete(ctx, id, conv, m.Revision, "op-d", channels.Submit)
	_, err = s.Delete(ctx, pd)
	wantCode(t, err, model.Unsupported, "")
	pr, _ := s.PlanReact(ctx, id, conv, "👍", false, "", "op-r", channels.Submit)
	_, err = s.React(ctx, pr)
	wantCode(t, err, model.Unsupported, "")
	// Nothing was stored: the operation IDs are free.
	for kind, op := range map[string]model.OperationID{model.ActionMessageEdit: "op-e", model.ActionMessageDelete: "op-d", model.ActionMessageReact: "op-r"} {
		if _, err := w.client(agent).Operations().Lookup(ctx, model.OperationLookup{Call: call, Kind: kind, Target: m.Ref.Ref(), ID: op}); !model.IsCode(err, model.NotFound) {
			t.Errorf("operation %s of %s: lookup = %v, want not found", op, kind, err)
		}
	}
}

// TestViewerCannotEditDeleteReact: uma refuses the three to a viewer,
// who may preview them (the plan names no secret).
func TestViewerCannotEditDeleteReact(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	v := w.as(viewer)
	m := w.firstMessage()
	id, conv := string(m.Ref.ID), string(w.local.Ref.ID)
	pe, err := v.PlanEdit(ctx, id, conv, m.Revision, "x", "op-e", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Edit(ctx, pe); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer edit: %v", err)
	}
	pd, _ := v.PlanDelete(ctx, id, conv, m.Revision, "op-d", channels.Submit)
	if _, err := v.Delete(ctx, pd); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer delete: %v", err)
	}
	pr, _ := v.PlanReact(ctx, id, conv, "👍", false, "", "op-r", channels.Submit)
	if _, err := v.React(ctx, pr); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("viewer react: %v", err)
	}
}

// TestParticipants: the participants of a conversation with their
// identity, role and assurance where the caller may read identities.
func TestParticipants(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	conv, ps, err := w.as(agent).Participants(ctx, string(w.local.Ref.ID))
	if err != nil || conv.Ref != w.local.Ref {
		t.Fatalf("participants = %+v, %v", ps, err)
	}
	if len(ps) != len(w.local.Participants) {
		t.Fatalf("participants = %+v, conversation holds %+v", ps, w.local.Participants)
	}
	ada := w.identityOf("ada")
	for _, p := range ps {
		if p.Identity == ada.Ref && (p.Assurance != ada.Assurance || p.Contact != ada.Contact.ID) {
			t.Errorf("ada = %+v, want assurance %q contact %s", p, ada.Assurance, ada.Contact.ID)
		}
	}
	// An external actor reads the participants but not the identities.
	_, ps, err = w.as(external).Participants(ctx, string(w.local.Ref.ID))
	if err != nil {
		t.Fatalf("external participants: %v", err)
	}
	for _, p := range ps {
		if p.Assurance != "" || p.Contact != "" {
			t.Errorf("external sees identity data: %+v", p)
		}
	}
	_, _, err = w.as(agent).Participants(ctx, "conv_404")
	wantCode(t, err, model.NotFound, "conv_404")
}
