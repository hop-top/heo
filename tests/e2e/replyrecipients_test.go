package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type replyRecipientsJSON struct {
	Conversation model.ID
	Recipients   *model.ReplyRecipients
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

type replyDraftRecipientsJSON struct {
	Recipients *model.ReplyRecipients
	Operation  opJSON
}

// TestMessageReplyRecipients drives the e2e build on Postgres through a
// reply and a reply draft that name recipients, on a fake provider that
// declares reply_recipients (reply-all and cc, no bcc, at most four
// addresses): a preview names the recipients, what it checked of the
// adapter and the limits the channel reports, and writes nothing; a
// preview refuses what the limits do not allow (a bcc, too many
// addresses); a reply carries the recipients to the provider and
// replays under its key; a draft carries them in the effect's fields; a
// channel that declares no reply_recipients, and a delete that names
// recipients, are refused before anything is written; the channel's view
// lists the capability and its limits.
func TestMessageReplyRecipients(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		rc := s.fakeOf(adapterstest.RecipientsType, "rc", adapterstest.Config{Account: "acct-rc"})
		s.heo(0, "inbox", "create", "--channel", "rc", "--key", "rcin", "--destination", "rc-1", "--mode", model.ModePerContact,
			"--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		conv := s.mail(rc, "rcin", "rc-1", "ev-rc", "msg-rc")
		cid := string(conv.Ref.ID)

		ask := []string{"--reply-all", "--cc", "lead@example.com,Ops@Example.com", "--cc", "audit@example.com"}
		reply := append([]string{"message", "reply", cid, "--text", "Looping you in."}, ask...)
		var plan planJSON
		s.json(&plan, append(reply, "--dry-run")...)
		if len(plan.Effects) != 1 || !strings.Contains(plan.Effects[0].Detail, "reply-all; cc lead@example.com, Ops@example.com, audit@example.com") {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		var line, limits string
		_ = json.Unmarshal(plan.Args["recipients_capability"], &line)
		_ = json.Unmarshal(plan.Args["recipient_limits"], &limits)
		if !strings.Contains(line, "reply_recipients declared by "+string(adapterstest.RecipientsType)) || limits != "reply_all=true cc=true bcc=false max_recipients=4" {
			t.Errorf("preview checked %q and %q", line, limits)
		}
		start := s.ledger()
		wantStderr(t, s.heo(2, "message", "reply", cid, "--text", "x", "--bcc", "a@example.com", "--dry-run"), "UNSUPPORTED", "does not send bcc")
		wantStderr(t, s.heo(2, "message", "reply", cid, "--text", "x", "--cc", "a@example.com,b@example.com,c@example.com,d@example.com", "--dry-run"),
			"UNSUPPORTED", "5 recipients", "at most 4")
		wantStderr(t, s.heo(2, "message", "reply", cid, "--text", "x", "--cc", "not an address", "--dry-run"), "VALIDATION")
		wantStderr(t, s.heo(2, "message", "reply-draft", cid, "--text", "x", "--bcc", "a@example.com", "--dry-run"), "UNSUPPORTED", "does not send bcc")
		wantStderr(t, s.heo(2, "message", "reply-draft", cid, "--draft", "r-1", "--delete", "--cc", "a@example.com"), "takes no")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("a preview or a refusal wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		var out, again replyRecipientsJSON
		s.json(&out, append(reply, "--idempotency-key", "rr-1")...)
		if out.Operation.State != model.Queued || out.Recipients == nil || !out.Recipients.ReplyAll || len(out.Recipients.Cc) != 3 || len(out.Recipients.Bcc) != 0 {
			t.Fatalf("reply = %+v", out)
		}
		s.json(&again, append(reply, "--idempotency-key", "rr-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed reply = %+v, first %+v", again, out)
		}
		if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("reply failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); !strings.HasPrefix(lines[0], "reply ") || !strings.HasSuffix(lines[0], " reply_all=true cc=lead@example.com,Ops@example.com,audit@example.com bcc=") {
			t.Fatalf("provider was asked %q", lines)
		}

		// A reply naming none carries none.
		var plainReply replyRecipientsJSON
		s.json(&plainReply, "message", "reply", cid, "--text", "Just you.", "--idempotency-key", "rr-2")
		if plainReply.Recipients != nil {
			t.Errorf("a reply naming no recipient = %+v", plainReply)
		}
		s.waitDelivery(plainReply.Operation.ID, model.ProviderAccepted, model.Delivered)

		// A draft carries them in the effect's fields.
		var dr replyDraftRecipientsJSON
		s.json(&dr, "message", "reply-draft", cid, "--text", "Draft.", "--reply-all", "--cc", "lead@example.com", "--idempotency-key", "rd-1")
		if dr.Recipients == nil || !dr.Recipients.ReplyAll || len(dr.Recipients.Cc) != 1 {
			t.Fatalf("draft = %+v", dr)
		}
		s.wantApplied(2)
		if lines := s.applied(); !strings.HasSuffix(lines[1], `{"cc":["lead@example.com"],"reply_all":true,"text":"Draft."}`) {
			t.Fatalf("provider was asked %q", lines)
		}

		// A channel that declares no reply_recipients refuses, in a preview
		// and on submission, before anything is written.
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		before := s.ledger()
		for _, verb := range []string{"reply", "reply-draft"} {
			for _, extra := range [][]string{{"--dry-run"}, {"--idempotency-key", "nc-" + verb}} {
				args := append([]string{"message", verb, string(pl.Ref.ID), "--text", "x", "--cc", "a@example.com"}, extra...)
				wantStderr(t, s.heo(2, args...), "UNSUPPORTED")
			}
		}
		if n := s.ledger(); n != before || len(s.applied()) != 2 {
			t.Errorf("a refused reply wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		// The channel's view lists the capability and its limits.
		var view struct {
			ReplyRecipients struct{ State, Capability string } `json:"reply_recipients"`
		}
		s.json(&view, "channel", "get", "rc")
		if view.ReplyRecipients.State != "declared" || view.ReplyRecipients.Capability != "reply_recipients" {
			t.Errorf("channel get: %+v", view)
		}
		var plainView struct {
			ReplyRecipients struct{ State, Reason string } `json:"reply_recipients"`
		}
		s.json(&plainView, "channel", "get", "plain")
		if plainView.ReplyRecipients.State != "unsupported" || plainView.ReplyRecipients.Reason != "not_declared" {
			t.Errorf("channel get plain: %+v", plainView)
		}
		s.stop(s.d)
	})
}
