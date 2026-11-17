package desk_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	"hop.top/uma/model"
)

// TestReplyDraftQueuesMailEffects: a reply draft is the channel's own
// reply_draft effect (Gmail's, Outlook's) on the conversation,
// authorized as a message reply; it names the message the reply answers;
// planning writes nothing; submitting queues one operation, adds no
// message to uma and changes no recipient; a retry replays.
func TestReplyDraftQueuesMailEffects(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()

	for _, tc := range []struct {
		conv   model.ConversationRef
		effect string
		parent string
	}{
		{gm, google.ExtensionReplyDraft, peerMessage},
		{ms, microsoft.ExtensionReplyDraft, "AAMk-1"},
	} {
		key := model.OperationID("rd-" + tc.effect)
		p, err := d.PlanReplyDraft(ctx, string(tc.conv.ID), desk.MailDraft{Text: "Thanks, on it."}, key, channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		if p.Request.Target != tc.conv.Ref() || p.Request.Action.Name != tc.effect || p.Request.Action.Version != 1 ||
			p.Request.Audience.Visibility != model.Internal || p.Message != tc.parent || p.Declaration != nil {
			t.Fatalf("plan = %+v", p)
		}
		if data := dataOf(t, p.Request.Action.Data); len(data) != 1 || data["text"] != "Thanks, on it." {
			t.Fatalf("data = %s: only the text is sent, no recipient", p.Request.Action.Data)
		}
		if n := w.queued(); n != before {
			t.Fatalf("planning queued %d provider work items", n-before)
		}
		conv0 := w.conversation(tc.conv)
		c, acc, err := d.SubmitReplyDraft(ctx, p)
		if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
			t.Fatalf("submit = %+v, %v", acc, err)
		}
		if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionMessageReply || rec.Key.Target != tc.conv.Ref() {
			t.Fatalf("operation key = %+v, want a message reply on the conversation", rec.Key)
		}
		if c.Revision != conv0.Revision {
			t.Errorf("the conversation after the draft = %+v: a draft is no message of uma's", c)
		}
		before++
		if n := w.queued(); n != before {
			t.Fatalf("queued %d provider work items, want %d", n, before)
		}
		if _, again, err := d.SubmitReplyDraft(ctx, p); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
			t.Fatalf("retry = %+v, %v, want %v replayed", again, err, acc.Operation.Ref)
		}
		if n := w.queued(); n != before {
			t.Fatalf("a replay queued work: %d", n)
		}
	}

	// Update and delete name the draft.
	p, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "v2", Draft: "r-77"}, "rd-up", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if data := dataOf(t, p.Request.Action.Data); len(data) != 2 || data["draft"] != "r-77" || data["text"] != "v2" {
		t.Fatalf("update data = %s", p.Request.Action.Data)
	}
	p, err = d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Draft: "r-77", Delete: true}, "rd-del", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if data := dataOf(t, p.Request.Action.Data); len(data) != 2 || data["draft"] != "r-77" || data["delete"] != true {
		t.Fatalf("delete data = %s", p.Request.Action.Data)
	}
	for _, tc := range []struct {
		d    desk.MailDraft
		want string
	}{
		{desk.MailDraft{Text: "x"}, "create"}, {desk.MailDraft{Text: "x", Draft: "d"}, "update"}, {desk.MailDraft{Draft: "d", Delete: true}, "delete"},
	} {
		if tc.d.Verb() != tc.want {
			t.Errorf("%+v verb = %s, want %s", tc.d, tc.d.Verb(), tc.want)
		}
	}
}

// TestReplyDraftRefusals: the usage rules, and the adapter's rules at
// the preview; an adapter that ships no reply_draft effect, or declares
// no extension_execute, is refused in a preview and on submission alike,
// before anything is written. A conversation with no parent message still
// drafts (the provider starts a thread).
func TestReplyDraftRefusals(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	before := w.queued()
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{}, "rd-1", mode)
		wantCode(t, err, model.Validation, "--text is required")
		_, err = d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Delete: true}, "rd-1", mode)
		wantCode(t, err, model.Validation, "--delete needs the draft ID")
		_, err = d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Draft: "a", Delete: true, Text: "x"}, "rd-1", mode)
		wantCode(t, err, model.Validation, "takes no --text")
		_, err = d.PlanReplyDraft(ctx, "no-such", desk.MailDraft{Text: "x"}, "rd-1", mode)
		wantCode(t, err, model.NotFound, "")
		// Zendesk declares extension_execute for its own effects, no draft.
		_, err = d.PlanReplyDraft(ctx, string(ticketConv), desk.MailDraft{Text: "x"}, "rd-1", mode)
		wantCode(t, err, model.Unsupported, "ships no effect "+google.ExtensionReplyDraft)
	}
	// The adapter's own rules, at the preview: blank text, a padded draft ID.
	for name, dr := range map[string]desk.MailDraft{
		"blank":  {Text: "   "},
		"padded": {Text: "x", Draft: " r-1"},
	} {
		_, err := d.PlanReplyDraft(ctx, string(gm.ID), dr, "rd-v", channels.Preview)
		wantCode(t, err, model.Validation, "")
		if t.Failed() {
			t.Fatalf("%s accepted at the preview", name)
		}
	}
	_, err := w.as(agent).PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "x"}, "rd-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "unknown here")
	d.Adapter = func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
		desc := google.NewFactory(google.Options{}).Describe()
		desc.Capabilities = slices.DeleteFunc(slices.Clone(desc.Capabilities), func(c model.Capability) bool {
			return c == model.CapabilityExtensionExecute
		})
		return desc, nil
	}
	_, err = d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "x"}, "rd-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "does not declare extension_execute")
	if n := w.queued(); n != before {
		t.Fatalf("a refusal queued %d provider work items", n-before)
	}
	// No parent: still a draft.
	empty := w.gmailThread("")
	p, err := w.asKnowing(agent).PlanReplyDraft(ctx, string(empty.ID), desk.MailDraft{Text: "hello"}, "rd-np", channels.Submit)
	if err != nil || p.Message != "" {
		t.Fatalf("no-parent plan = %+v, %v", p, err)
	}
}

// TestReplyDraftPreviewChecks: a preview records what was checked of the
// adapter and writes nothing.
func TestReplyDraftPreviewChecks(t *testing.T) {
	w := newTicketWorld(t)
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	before := w.queued()
	p, err := d.PlanReplyDraft(t.Context(), string(gm.ID), desk.MailDraft{Text: "hi"}, "rd-pv", channels.Preview)
	if err != nil || p.Declaration == nil || p.Declaration.Adapter != google.Type || p.Declaration.Effect != google.ExtensionReplyDraft ||
		p.Declaration.Capability != model.CapabilityExtensionExecute {
		t.Fatalf("preview = %+v, %v", p, err)
	}
	if w.queued() != before {
		t.Fatal("a preview queued work")
	}
}

// TestUnsubscribeQueuesMailEffects: an unsubscribe is the channel's own
// unsubscribe effect on the conversation, authorized as an extension
// execution (its own permission, not a label's or a reply's), aimed at the
// message the peer wrote; the method defaults to auto; planning writes
// nothing; submitting queues one operation and changes nothing in uma; a
// retry replays and queues nothing more.
func TestUnsubscribeQueuesMailEffects(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()
	for _, tc := range []struct {
		conv           model.ConversationRef
		method, effect string
		sent           string
		parent         string
	}{
		{gm, "", google.ExtensionUnsubscribe, "auto", peerMessage},
		{gm, "one_click", google.ExtensionUnsubscribe, "one_click", peerMessage},
		{ms, "mailto", microsoft.ExtensionUnsubscribe, "mailto", "AAMk-1"},
	} {
		key := model.OperationID(fmt.Sprintf("un-%s-%s", tc.effect, tc.sent))
		p, err := d.PlanUnsubscribe(ctx, string(tc.conv.ID), tc.method, key, channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		if p.Request.Target != tc.conv.Ref() || p.Request.Action.Name != tc.effect || p.Request.Action.Version != 1 ||
			p.Request.Audience.Visibility != model.Internal || p.Message != tc.parent || p.Method != tc.sent {
			t.Fatalf("plan = %+v", p)
		}
		if data := dataOf(t, p.Request.Action.Data); len(data) != 1 || data["method"] != tc.sent {
			t.Fatalf("data = %s: only the method is sent, never a URL or an address", p.Request.Action.Data)
		}
		if n := w.queued(); n != before {
			t.Fatalf("planning queued %d provider work items", n-before)
		}
		conv0 := w.conversation(tc.conv)
		c, acc, err := d.Unsubscribe(ctx, p)
		if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
			t.Fatalf("unsubscribe = %+v, %v", acc, err)
		}
		if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionExtensionExecute || rec.Key.Target != tc.conv.Ref() {
			t.Fatalf("operation key = %+v, want an extension execution on the conversation", rec.Key)
		}
		if c.Revision != conv0.Revision {
			t.Errorf("the conversation after the unsubscribe = %+v: uma records none of it", c)
		}
		before++
		if _, again, err := d.Unsubscribe(ctx, p); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
			t.Fatalf("retry = %+v, %v", again, err)
		}
		if n := w.queued(); n != before {
			t.Fatalf("queued %d provider work items, want %d", n, before)
		}
	}
}

// TestUnsubscribeRefusals: an unknown method, an adapter without the
// effect, a conversation the peer has written no message in; each in a
// preview and on submission, before anything is written.
func TestUnsubscribeRefusals(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	empty := w.gmailThread("")
	before := w.queued()
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := d.PlanUnsubscribe(ctx, string(gm.ID), "get", "un-1", mode)
		wantCode(t, err, model.Validation, "want one of auto, one_click, mailto")
		_, err = d.PlanUnsubscribe(ctx, string(empty.ID), "", "un-1", mode)
		wantCode(t, err, model.Conflict, "names no provider message")
		_, err = d.PlanUnsubscribe(ctx, string(ticketConv), "", "un-1", mode)
		wantCode(t, err, model.Unsupported, "ships no effect "+google.ExtensionUnsubscribe)
		_, err = d.PlanUnsubscribe(ctx, "no-such", "", "un-1", mode)
		wantCode(t, err, model.NotFound, "")
	}
	d.Adapter = func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
		desc := google.NewFactory(google.Options{}).Describe()
		desc.Capabilities = slices.DeleteFunc(slices.Clone(desc.Capabilities), func(c model.Capability) bool {
			return c == model.CapabilityExtensionExecute
		})
		return desc, nil
	}
	_, err := d.PlanUnsubscribe(ctx, string(gm.ID), "", "un-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "does not declare extension_execute")
	if n := w.queued(); n != before {
		t.Fatalf("a refusal queued %d provider work items", n-before)
	}
}

// TestUnsubscribePreviewChecks: a preview records what was checked.
func TestUnsubscribePreviewChecks(t *testing.T) {
	w := newTicketWorld(t)
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	p, err := d.PlanUnsubscribe(t.Context(), string(gm.ID), "mailto", "un-pv", channels.Preview)
	if err != nil || p.Declaration == nil || p.Declaration.Adapter != google.Type || p.Declaration.Effect != google.ExtensionUnsubscribe {
		t.Fatalf("preview = %+v, %v", p, err)
	}
}

// TestHeldReplyDraft: a draft proposed by a plugin actor is stored held
// with no provider work queued; approve queues it exactly once, reject
// queues nothing.
func TestHeldReplyDraft(t *testing.T) {
	w := newTicketWorld(t)
	heldMailEffect(t, w, "reply", "draft", func(s desk.Service, gm model.ConversationRef, key string) (model.Acceptance, error) {
		p, err := s.PlanReplyDraft(t.Context(), string(gm.ID), desk.MailDraft{Text: "hi"}, model.OperationID(key), channels.Submit)
		if err != nil {
			return model.Acceptance{}, err
		}
		h, err := s.Hold(t.Context(), &p.Request.Command, desk.HoldInput{Action: model.ActionMessageReply, Verb: "reply", Class: "draft",
			Channel: string(gm.Inbox.ID), Target: gm.Ref(), Preview: "p"})
		if err != nil {
			return model.Acceptance{}, err
		}
		_, acc, err := s.SubmitReplyDraft(t.Context(), p)
		return h.Settle(t.Context(), acc, err)
	})
}

// TestHeldUnsubscribe: an unsubscribe proposed by a plugin actor is
// stored held with no provider work queued and no request sent; approve
// queues it exactly once, reject queues nothing. It is the one verb a
// person's yes releases a request to a third party for.
func TestHeldUnsubscribe(t *testing.T) {
	w := newTicketWorld(t)
	heldMailEffect(t, w, "unsubscribe", "", func(s desk.Service, gm model.ConversationRef, key string) (model.Acceptance, error) {
		p, err := s.PlanUnsubscribe(t.Context(), string(gm.ID), "", model.OperationID(key), channels.Submit)
		if err != nil {
			return model.Acceptance{}, err
		}
		h, err := s.Hold(t.Context(), &p.Request.Command, desk.HoldInput{Action: model.ActionExtensionExecute, Verb: "unsubscribe",
			Channel: string(gm.Inbox.ID), Target: gm.Ref(), Preview: "p"})
		if err != nil {
			return model.Acceptance{}, err
		}
		_, acc, err := s.Unsubscribe(t.Context(), p)
		return h.Settle(t.Context(), acc, err)
	})
}

// TestHoldableMailEffects pins the table: the two actions the mail
// effects are authorized as are holdable.
func TestHoldableMailEffects(t *testing.T) {
	for _, a := range []string{model.ActionMessageReply, model.ActionExtensionExecute} {
		if !desk.CanHold(a) {
			t.Errorf("%s is not holdable", a)
		}
	}
	for _, c := range []struct{ verb, class, want string }{{"unsubscribe", "", "unsubscribe"}, {"reply", "draft", "reply:draft"}} {
		key, labels := desk.TrustKey("plugin:triage", "uid:1001", "mail", c.verb, c.class)
		if !strings.Contains(key, c.verb) || labels["verb"] != c.verb {
			t.Errorf("key %q labels %v", key, labels)
		}
	}
}

// heldMailEffect probes uma's holding of a mail effect: held with nothing
// queued, rejected with nothing queued, approved once with exactly one
// provider work item, a second approval a replay.
func heldMailEffect(t *testing.T, w *ticketWorld, verb, class string,
	propose func(s desk.Service, gm model.ConversationRef, key string) (model.Acceptance, error)) {
	t.Helper()
	ctx := t.Context()
	plug, events := holding(w.asKnowing(plugin), nil)
	dec := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	run := func(key string) model.Acceptance {
		acc, err := propose(plug, gm, key)
		if err != nil {
			t.Fatal(err)
		}
		return acc
	}
	before := w.queued()
	acc := run(verb + "-rj")
	if acc.Operation.State != model.Held || len(*events) != 1 || w.queued() != before {
		t.Fatalf("hold = %v, events %d, queued %d", acc.Operation.State, len(*events), w.queued()-before)
	}
	wantTail := "/" + verb
	if class != "" {
		wantTail += ":" + class
	}
	if ev := (*events)[0]; !strings.HasSuffix(ev.TrustKey, wantTail) {
		t.Errorf("trust key = %q, want it to end %q", ev.TrustKey, wantTail)
	}
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject}); err != nil || r.Status != string(model.Rejected) {
		t.Fatalf("reject = %+v, %v", r, err)
	}
	if w.queued() != before {
		t.Fatalf("rejected %s queued %d", verb, w.queued()-before)
	}
	acc = run(verb + "-ap")
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || r.Replayed {
		t.Fatalf("approve = %+v, %v", r, err)
	}
	if r, err := dec.DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove}); err != nil || !r.Replayed {
		t.Fatalf("second approve = %+v, %v", r, err)
	}
	if w.queued() != before+1 {
		t.Fatalf("approved %s queued %d, want exactly 1", verb, w.queued()-before)
	}
}
