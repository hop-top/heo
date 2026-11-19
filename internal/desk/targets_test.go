package desk_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// seedMessages stores one provider message of conv per provider ID, as
// ingress records them, and returns their record IDs in order. A message
// with an empty provider ID holds no provider reference.
func (w *ticketWorld) seedMessages(conv model.ConversationRef, ch model.ChannelRef, providerIDs ...string) []string {
	w.t.Helper()
	var out []string
	for _, pid := range providerIDs {
		w.n++
		id := model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-0000000004%02x", w.n))
		m := model.Message{
			Ref: model.MessageRef{Conversation: conv, ID: id}, Audience: model.Audience{Visibility: model.Public},
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
			Source:  "src", OccurredAt: time.Date(2026, 10, 11, 9, w.n, 0, 0, time.UTC), RecordedAt: time.Date(2026, 10, 11, 9, w.n, 0, 0, time.UTC),
			Revision: "m1",
		}
		if pid != "" {
			m.External = []model.ExternalRef{model.ProviderMessageRef(ch, pid)}
		}
		if err := w.st.Transact(w.t.Context(), tenant, func(tx store.Tx) error { return tx.PutMessage(w.t.Context(), m, "") }); err != nil {
			w.t.Fatalf("seed message: %v", err)
		}
		out = append(out, string(id))
	}
	return out
}

// TestStateTargetsNamedMessages: a state change that names messages plans
// exactly those provider messages, in the conversation's order, and submits with
// them; a name that is not a message of the conversation the provider
// holds is a validation error and nothing is queued.
func TestStateTargetsNamedMessages(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread("gm1"), w.outlookThread("om1")
	m := w.seedMessages(gm, w.gm, "gm1", "gm2", "gm3", "")
	other := w.seedMessages(ms, w.ms, "om1")

	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		p, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive, Messages: []string{m[2], m[1], m[2]}}, "", "tg-1", mode)
		if err != nil {
			t.Fatal(err)
		}
		want := []desk.MessageItem{{Message: m[1], Provider: "gm2"}, {Message: m[2], Provider: "gm3"}}
		if fmt.Sprint(p.Items) != fmt.Sprint(want) || p.ThreadWide {
			t.Fatalf("%v: items = %+v thread-wide = %v, want %+v and not thread-wide", mode, p.Items, p.ThreadWide, want)
		}
		if got, want := p.Change.Asked(), "archive on message "+m[2]+", "+m[1]; got != want {
			t.Errorf("asked = %q, want %q", got, want)
		}
		for name, bad := range map[string][]string{
			"unknown":            {"00000ad0-0000-7000-8000-0000000009ff"},
			"another's":          {other[0]},
			"one bad of several": {m[0], "nope"},
			"no provider ref":    {m[3]},
		} {
			_, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbArchive, Messages: bad}, "", "tg-2", mode)
			if !model.IsCode(err, model.Validation) || !strings.Contains(err.Error(), "--message") {
				t.Errorf("%v %s: err = %v, want a --message validation error", mode, name, err)
			}
		}
	}
	p, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbTrash, Messages: []string{m[1]}}, "", "tg-3", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, acc, err := d.SubmitState(ctx, p); err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("submit = %+v, %v", acc, err)
	}
	if got := w.queued(); got != 1 {
		t.Fatalf("queued %d provider work items, want 1", got)
	}
	// The stored intent carries the set: a different set under the same key conflicts.
	p2, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbTrash, Messages: []string{m[0]}}, "", "tg-3", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.SubmitState(ctx, p2); !model.IsCode(err, model.Conflict) {
		t.Fatalf("same key, other message = %v, want conflict", err)
	}
}

// TestStatePreviewListsItems: a preview lists the provider messages the
// change resolves to, and says it is thread-wide on a provider that
// changes whole threads (Gmail) when no message is named, never for Graph.
func TestStatePreviewListsItems(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread("gm1"), w.outlookThread("om1")
	m := w.seedMessages(gm, w.gm, "gm1", "gm2")
	w.seedMessages(ms, w.ms, "om1", "om2")

	p, err := d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbRead}, "", "pl-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 || p.Items[0] != (desk.MessageItem{Message: m[0], Provider: "gm1"}) || !p.ThreadWide {
		t.Fatalf("gmail preview = items %+v thread-wide %v, want both messages and thread-wide", p.Items, p.ThreadWide)
	}
	p, err = d.PlanState(ctx, string(ms.ID), desk.StateChange{Verb: model.StateVerbRead}, "", "pl-2", channels.Preview)
	if err != nil || len(p.Items) != 2 || p.ThreadWide {
		t.Fatalf("graph preview = %+v, %v; want both messages, not thread-wide", p, err)
	}
	p, err = d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbRead, Messages: []string{m[0]}}, "", "pl-3", channels.Preview)
	if err != nil || len(p.Items) != 1 || p.ThreadWide {
		t.Fatalf("gmail preview with a message = %+v, %v; want one item, not thread-wide", p, err)
	}
	// A submission lists nothing it was not asked to.
	p, err = d.PlanState(ctx, string(gm.ID), desk.StateChange{Verb: model.StateVerbRead}, "", "pl-4", channels.Submit)
	if err != nil || p.Items != nil || p.ThreadWide {
		t.Fatalf("submission plan = %+v, %v", p, err)
	}
}

// TestTagAndUnsubscribeTargetNamedMessage: tag and unsubscribe act on the
// message the conversation's reply route names, the first, unless a
// message of the conversation is named; a name that is not one is a
// validation error.
func TestTagAndUnsubscribeTargetNamedMessage(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread("gm1"), w.outlookThread("om1")
	m := w.seedMessages(gm, w.gm, "gm1", "gm2")
	other := w.seedMessages(ms, w.ms, "om1")

	tag := desk.MailTag{Add: []string{"Label_1"}}
	p, err := d.PlanTag(ctx, string(gm.ID), tag, "tm-1", channels.Submit)
	if err != nil || p.Message != "gm1" || p.Request.Message != nil {
		t.Fatalf("default tag = %+v, %v; want the route parent gm1 and no message on the request", p, err)
	}
	tag.Message = m[1]
	p, err = d.PlanTag(ctx, string(gm.ID), tag, "tm-2", channels.Preview)
	if err != nil || p.Message != "gm2" || p.Request.Message == nil || p.Request.Message.ID != model.ID(m[1]) {
		t.Fatalf("tag --message = %+v, %v; want gm2 on the request", p, err)
	}
	if _, acc, err := d.TagConversation(ctx, p); err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("submit = %+v, %v", acc, err)
	}
	for name, bad := range map[string]string{"unknown": "nope", "another's": other[0]} {
		tag.Message = bad
		_, err := d.PlanTag(ctx, string(gm.ID), tag, "tm-3", channels.Submit)
		if !model.IsCode(err, model.Validation) {
			t.Errorf("tag --message %s: err = %v, want validation", name, err)
		}
	}

	u, err := d.PlanUnsubscribe(ctx, string(gm.ID), "", "um-1", channels.Submit)
	if err != nil || u.Message != "gm1" || u.Request.Message != nil {
		t.Fatalf("default unsubscribe = %+v, %v", u, err)
	}
	u, err = d.PlanUnsubscribeMessage(ctx, string(gm.ID), "", m[1], "um-2", channels.Preview)
	if err != nil || u.Message != "gm2" || u.Request.Message == nil || u.Request.Message.ID != model.ID(m[1]) {
		t.Fatalf("unsubscribe --message = %+v, %v", u, err)
	}
	if _, err = d.PlanUnsubscribeMessage(ctx, string(gm.ID), "", other[0], "um-3", channels.Submit); !model.IsCode(err, model.Validation) {
		t.Fatalf("unsubscribe --message another's = %v, want validation", err)
	}
	// With a message named, a route that names none is no obstacle.
	empty := w.gmailThread("")
	em := w.seedMessages(empty, w.gm, "gx1")
	if _, err = d.PlanUnsubscribe(ctx, string(empty.ID), "", "um-4", channels.Submit); !model.IsCode(err, model.Conflict) {
		t.Fatalf("default unsubscribe without a parent = %v, want conflict", err)
	}
	if u, err = d.PlanUnsubscribeMessage(ctx, string(empty.ID), "", em[0], "um-5", channels.Submit); err != nil || u.Message != "gx1" {
		t.Fatalf("unsubscribe --message without a parent = %+v, %v", u, err)
	}
}
