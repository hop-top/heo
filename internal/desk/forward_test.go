package desk_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// seedWith stores one message of conv holding exactly refs, as ingress
// records a mail, and returns its record ID.
func (w *ticketWorld) seedWith(conv model.ConversationRef, mutate func(*model.Message), refs ...model.ExternalRef) string {
	w.t.Helper()
	w.n++
	id := model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-0000000005%02x", w.n))
	at := time.Date(2026, 10, 11, 9, w.n, 0, 0, time.UTC)
	m := model.Message{
		Ref: model.MessageRef{Conversation: conv, ID: id}, Audience: model.Audience{Visibility: model.Public},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
		Source:  "src", OccurredAt: at, RecordedAt: at, Revision: "m1", External: refs,
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := w.st.Transact(w.t.Context(), tenant, func(tx store.Tx) error { return tx.PutMessage(w.t.Context(), m, "") }); err != nil {
		w.t.Fatalf("seed message: %v", err)
	}
	return string(id)
}

func emailRef(ch model.ChannelRef, kind, id string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: model.EmailHeaderNamespace, Kind: kind, ID: id}
}

// TestForwardQueuesMailEffect: a forward plans google.mail.forward on the
// message's conversation, aimed at that message, with the address and the
// note as its data; planning queues nothing, submitting queues once and
// replays, and the conversation uma holds is untouched.
func TestForwardQueuesMailEffect(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread("gm1")
	first := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm1"), emailRef(w.gm, model.RefKindEmailMessageID, "<o1@remote.test>"))
	second := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm2"), emailRef(w.gm, model.RefKindEmailMessageID, "<o2@remote.test>"))
	before := w.queued()

	f := desk.MailForward{To: "dest@example.net", Text: "yours"}
	p, err := d.PlanForward(ctx, second, f, "fw-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Target != gm.Ref() || p.Request.Action.Name != google.ExtensionForward || p.Request.Action.Version != 1 ||
		p.Request.Audience.Visibility != model.Internal || p.Request.Message == nil || p.Request.Message.ID != model.ID(second) ||
		p.Provider != "gm2" || p.Message != second || p.OriginalID != "<o2@remote.test>" || p.Declaration != nil {
		t.Fatalf("plan = %+v", p)
	}
	if data := dataOf(t, p.Request.Action.Data); len(data) != 2 || data["to"] != "dest@example.net" || data["text"] != "yours" {
		t.Fatalf("data = %s: the address and the note only", p.Request.Action.Data)
	}
	if n := w.queued(); n != before {
		t.Fatalf("planning queued %d provider work items", n-before)
	}
	conv0 := w.conversation(gm)
	c, acc, err := d.ForwardMessage(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
		t.Fatalf("forward = %+v, %v", acc, err)
	}
	if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionExtensionExecute || rec.Key.Target != gm.Ref() {
		t.Fatalf("operation key = %+v, want an extension execution on the conversation", rec.Key)
	}
	if c.Revision != conv0.Revision {
		t.Errorf("the conversation after the forward = %+v: uma records none of it", c)
	}
	if _, again, err := d.ForwardMessage(ctx, p); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if n := w.queued(); n != before+1 {
		t.Fatalf("queued %d provider work items, want %d", n, before+1)
	}
	// Another message under the same key is another request.
	p1, err := d.PlanForward(ctx, first, f, "fw-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ForwardMessage(ctx, p1); !model.IsCode(err, model.Conflict) {
		t.Fatalf("same key, other message = %v, want conflict", err)
	}
	// A preview records what was checked.
	pv, err := d.PlanForward(ctx, second, f, "fw-pv", channels.Preview)
	if err != nil || pv.Declaration == nil || pv.Declaration.Adapter != google.Type || pv.Declaration.Effect != google.ExtensionForward || pv.Size != -1 {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
}

// TestForwardRefusals: usage errors, an adapter that cannot attach the
// original, a note, a deleted message, a message the provider holds no
// copy of; each in a preview and on submission, before anything is written.
func TestForwardRefusals(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread("gm1"), w.outlookThread("om1")
	ok := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm1"))
	note := w.seedWith(gm, func(m *model.Message) { m.Audience.Visibility = model.Internal }, model.ProviderMessageRef(w.gm, "gm2"))
	gone := w.seedWith(gm, func(m *model.Message) { m.Deleted = true }, model.ProviderMessageRef(w.gm, "gm3"))
	bare := w.seedWith(gm, nil)
	graph := w.seedWith(ms, nil, model.ProviderMessageRef(w.ms, "om1"))
	before := w.queued()
	to := desk.MailForward{To: "dest@example.net"}
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		for _, c := range []struct {
			name string
			id   string
			f    desk.MailForward
			code model.ErrorCode
			msg  string
		}{
			{"no address", ok, desk.MailForward{}, model.Validation, "--to is required"},
			{"display name", ok, desk.MailForward{To: "Dest <dest@example.net>"}, model.Validation, "bare address"},
			{"long note", ok, desk.MailForward{To: "d@example.net", Text: strings.Repeat("x", desk.MaxForwardNote+1)}, model.Validation, "longer than"},
			{"internal note", note, to, model.Validation, "internal note"},
			{"deleted", gone, to, model.Conflict, "deleted or redacted"},
			{"no provider copy", bare, to, model.Conflict, "no provider message"},
			{"unknown", "00000ad0-0000-7000-8000-0000000009ff", to, model.NotFound, ""},
			{"adapter cannot attach", graph, to, model.Unsupported, "ships no effect " + google.ExtensionForward},
		} {
			_, err := d.PlanForward(ctx, c.id, c.f, "fw-r", mode)
			if !model.IsCode(err, c.code) || !strings.Contains(err.Error(), c.msg) {
				t.Errorf("%v %s: err = %v, want %s containing %q", mode, c.name, err, c.code, c.msg)
			}
		}
	}
	if n := w.queued(); n != before {
		t.Fatalf("a refusal queued %d provider work items", n-before)
	}
}

// TestHeldForward: a forward proposed by a plugin actor is stored held
// with no provider work queued and no mail sent; approve queues it exactly
// once, reject queues nothing. A person's yes releases a copy of the mail
// to a third party.
func TestHeldForward(t *testing.T) {
	w := newTicketWorld(t)
	heldMailEffect(t, w, "forward", "", func(s desk.Service, gm model.ConversationRef, key string) (model.Acceptance, error) {
		id := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm-h"))
		p, err := s.PlanForward(t.Context(), id, desk.MailForward{To: "dest@example.net"}, model.OperationID(key), channels.Submit)
		if err != nil {
			return model.Acceptance{}, err
		}
		h, err := s.Hold(t.Context(), &p.Request.Command, desk.HoldInput{Action: model.ActionExtensionExecute, Verb: "forward",
			Channel: string(gm.Inbox.ID), Target: gm.Ref(), Preview: "p"})
		if err != nil {
			return model.Acceptance{}, err
		}
		_, acc, err := s.ForwardMessage(t.Context(), p)
		return h.Settle(t.Context(), acc, err)
	})
}

// TestListMessagesByIdentifiers: --message-id is the message's own
// Message-ID, --references a Message-ID it names in References or
// In-Reply-To, --provider-id the provider's ID, each exactly and
// combined by AND; a near miss matches nothing.
func TestListMessagesByIdentifiers(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread("gm1")
	orig := "<orig@remote.test>"
	a := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm-a"), emailRef(w.gm, model.RefKindEmailMessageID, "<fwd-a@desk.test>"), emailRef(w.gm, model.RefKindEmailReferences, orig))
	b := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm-b"), emailRef(w.gm, model.RefKindEmailMessageID, "<fwd-b@desk.test>"), emailRef(w.gm, model.RefKindEmailReferences, "<other@remote.test>"))
	r := w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm-r"), emailRef(w.gm, model.RefKindEmailMessageID, "<reply@remote.test>"), emailRef(w.gm, model.RefKindEmailInReplyTo, orig))
	w.seedWith(gm, nil, model.ProviderMessageRef(w.gm, "gm-n"))

	list := func(f desk.Filter) []string {
		t.Helper()
		var out []string
		page := model.PageRequest{Limit: 2}
		for range 20 {
			p, err := d.ListMessages(ctx, f, page)
			if err != nil {
				t.Fatalf("ListMessages(%+v) = %v", f, err)
			}
			for _, m := range p.Items {
				out = append(out, string(m.Ref.ID))
			}
			if p.Complete || p.Next == "" {
				return out
			}
			page.After = p.Next
		}
		t.Fatal("listing did not end")
		return nil
	}
	for name, c := range map[string]struct {
		f    desk.Filter
		want []string
	}{
		"own id":                   {desk.Filter{MessageID: "<fwd-a@desk.test>"}, []string{a}},
		"own id, no brackets":      {desk.Filter{MessageID: "fwd-b@desk.test"}, []string{b}},
		"an id it only references": {desk.Filter{MessageID: orig}, nil},
		"references":               {desk.Filter{References: orig}, []string{a, r}},
		"references and own":       {desk.Filter{References: orig, MessageID: "<fwd-a@desk.test>"}, []string{a}},
		"conflicting identifiers":  {desk.Filter{References: orig, MessageID: "<fwd-b@desk.test>"}, nil},
		"provider id":              {desk.Filter{ProviderID: "gm-b"}, []string{b}},
		"near miss":                {desk.Filter{MessageID: "<fwd-a@desk.tes>"}, nil},
		"provider id is exact":     {desk.Filter{ProviderID: "gm-"}, nil},
		"unknown":                  {desk.Filter{MessageID: "<nope@x.y>"}, nil},
	} {
		got := list(c.f)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: matched %v, want %v", name, got, c.want)
		}
	}
	if _, err := d.ListMessages(ctx, desk.Filter{MessageID: "not an id"}, model.PageRequest{Limit: 5}); !model.IsCode(err, model.Validation) {
		t.Errorf("malformed id: err = %v, want validation", err)
	}
	if _, err := d.ListMessages(ctx, desk.Filter{References: "a b"}, model.PageRequest{Limit: 5}); !model.IsCode(err, model.Validation) {
		t.Errorf("malformed references: err = %v, want validation", err)
	}
}
