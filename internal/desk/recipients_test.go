package desk_test

import (
	"reflect"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// recipientsAsked is what the tests ask of a reply: reply-all, a cc
// with a duplicate and a padded-case twin, and a bcc that is also a cc.
var recipientsAsked = model.ReplyRecipients{
	ReplyAll: true,
	Cc:       []string{"Lead@Example.com", "lead@example.com", "ops@example.com"},
	Bcc:      []string{"audit@example.com", "ops@example.com"},
}

// TestReplyRecipientsReachUma: a reply with recipients carries them to
// uma normalized and deduplicated (an address of cc leaves bcc), is
// offered to the hold point under the added-cc fact, and queues one
// operation; planning queues nothing. A reply that names none carries
// none and no fact.
func TestReplyRecipientsReachUma(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()
	for i, conv := range []model.ConversationRef{gm, ms} {
		key := model.OperationID([]string{"rr-g", "rr-m"}[i])
		p, err := d.PlanReplyTo(ctx, string(conv.ID), "Looping you in.", recipientsAsked, key, channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		want, err := recipientsAsked.Normalize()
		if err != nil || !want.ReplyAll || len(want.Cc) != 2 || len(want.Bcc) != 1 {
			t.Fatalf("normalized = %+v, %v: want reply-all, 2 cc, 1 bcc", want, err)
		}
		if p.Request.Recipients == nil || !reflect.DeepEqual(*p.Request.Recipients, want) || !reflect.DeepEqual(p.Recipients, p.Request.Recipients) {
			t.Fatalf("recipients = %+v, want %+v", p.Request.Recipients, want)
		}
		if !p.Facts.AddedCC || p.Declaration != nil || p.Limits != nil {
			t.Fatalf("plan = %+v: an added cc is a fact; a submission checks nothing live", p)
		}
		if n := w.queued(); n != before {
			t.Fatalf("planning queued %d provider work items", n-before)
		}
		acc, err := d.Reply(ctx, p)
		if err != nil || acc.Operation.State != model.Queued {
			t.Fatalf("reply = %+v, %v", acc, err)
		}
		before++
		if n := w.queued(); n != before {
			t.Fatalf("queued %d provider work items, want %d", n, before)
		}
	}
	p, err := d.PlanReply(ctx, string(gm.ID), "Plain.", "rr-plain")
	if err != nil || p.Request.Recipients != nil || p.Recipients != nil || p.Facts.AddedCC {
		t.Fatalf("a reply naming no recipient = %+v, %v", p, err)
	}
	if _, err := d.PlanReplyTo(ctx, string(gm.ID), "x", model.ReplyRecipients{Cc: []string{"not an address"}}, "rr-bad", channels.Submit); !model.IsCode(err, model.Validation) {
		t.Fatalf("a cc that is no bare address: %v, want validation", err)
	}
}

// TestReplyRecipientsPreviewReadsTheLimits: a preview of a reply with
// recipients checks that the adapter declares reply_recipients and
// reads the limits the channel reports now; a reply without any reads
// none.
func TestReplyRecipientsPreviewReadsTheLimits(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	for i, conv := range []model.ConversationRef{w.gmailThread(peerMessage), w.outlookThread("AAMk-1")} {
		p, err := d.PlanReplyTo(ctx, string(conv.ID), "x", recipientsAsked, model.OperationID([]string{"pv-g", "pv-m"}[i]), channels.Preview)
		if err != nil {
			t.Fatal(err)
		}
		if p.Declaration == nil || p.Declaration.Capability != model.CapabilityReplyRecipients || p.Limits == nil ||
			!p.Limits.ReplyAll || !p.Limits.Cc || !p.Limits.Bcc || p.Limits.MaxRecipients == 0 {
			t.Fatalf("preview = %+v", p)
		}
		plain, err := d.PlanReplyTo(ctx, string(conv.ID), "x", model.ReplyRecipients{}, "pv-plain", channels.Preview)
		if err != nil || plain.Declaration != nil || plain.Limits != nil {
			t.Fatalf("a preview of a reply naming no recipient = %+v, %v", plain, err)
		}
	}
}

// TestReplyRecipientsNeedTheCapability: a channel whose adapter does not
// declare reply_recipients refuses a reply that names any, UNSUPPORTED,
// in a preview and on submission alike, before anything is stored; the
// same reply without recipients still goes.
func TestReplyRecipientsNeedTheCapability(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	d := w.as(agent)
	id := string(w.local.Ref.ID)
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		for _, rec := range []model.ReplyRecipients{{ReplyAll: true}, {Cc: []string{"a@example.com"}}, {Bcc: []string{"a@example.com"}}} {
			_, err := d.PlanReplyTo(ctx, id, "x", rec, "nc-1", mode)
			if !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), "does not declare reply_recipients") {
				t.Errorf("mode %v, %+v: %v, want unsupported: does not declare reply_recipients", mode, rec, err)
			}
		}
	}
	if _, err := d.PlanReplyTo(ctx, id, "x", model.ReplyRecipients{}, "nc-2", channels.Preview); err != nil {
		t.Fatalf("a reply naming no recipient: %v", err)
	}
}

// TestReplyDraftRecipients: a reply draft carries the recipients as the
// effect's data (reply_all, cc, bcc, normalized), and is held under the
// added-cc fact; a preview reads the limits; a draft with none sends none;
// a delete takes none.
func TestReplyDraftRecipients(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm := w.gmailThread(peerMessage)
	p, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "Hi.", Recipients: recipientsAsked}, "dr-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	data := dataOf(t, p.Request.Action.Data)
	want, _ := recipientsAsked.Normalize()
	if data["reply_all"] != true || !reflect.DeepEqual(data["cc"], anys(want.Cc)) ||
		!reflect.DeepEqual(data["bcc"], anys(want.Bcc)) || data["text"] != "Hi." || len(data) != 4 {
		t.Fatalf("data = %s", p.Request.Action.Data)
	}
	if !p.Facts.AddedCC || p.Recipients != nil || p.Limits != nil {
		t.Fatalf("plan = %+v", p)
	}
	pv, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "Hi.", Recipients: recipientsAsked}, "dr-2", channels.Preview)
	if err != nil || pv.Recipients == nil || pv.Recipients.Capability != model.CapabilityReplyRecipients || pv.Limits == nil || !pv.Limits.Cc {
		t.Fatalf("preview = %+v, %v", pv, err)
	}
	none, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Text: "Hi."}, "dr-3", channels.Preview)
	if err != nil || none.Facts.AddedCC || none.Recipients != nil || none.Limits != nil {
		t.Fatalf("a draft naming no recipient = %+v, %v", none, err)
	}
	if _, err := d.PlanReplyDraft(ctx, string(gm.ID), desk.MailDraft{Draft: "r-1", Delete: true, Recipients: model.ReplyRecipients{Cc: []string{"a@example.com"}}}, "dr-4", channels.Submit); !model.IsCode(err, model.Validation) {
		t.Fatalf("a delete with recipients: %v, want validation", err)
	}
}

// anys is a string list as JSON decoding gives it.
func anys(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
