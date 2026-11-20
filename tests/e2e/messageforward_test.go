package e2e

import (
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type forwardJSON struct {
	Conversation model.Conversation
	Message      string
	Provider     string `json:"provider_message"`
	To           string
	Effect       string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// TestMessageForwardAndFindTheCopy drives the inbox triage move between two
// accounts: account A forwards a message to B's address; the copy arrives
// in B's inbox (the fake provider reports it as the mail provider would,
// with the original's Message-ID in its References); B finds exactly it by
// identifier, then labels and archives that message alone.
func TestMessageForwardAndFindTheCopy(t *testing.T) {
	withE2EBinary(t, func() {
		const orig = "<orig-1@remote.test>"
		const dest = "desk-b@example.test"
		s := newEffectSite(t, noRateLimit)
		s.inject(s.fw, adapterstest.Inject{Event: "ev-o", Message: "gm-o", From: peerMail, Destination: "fw-1", Text: "invoice", Parent: "gm-o", EmailMessageID: orig})
		var p pageJSON[model.Message]
		s.json(&p, "message", "list", "--inbox", "fwin")
		if len(p.Items) != 1 {
			t.Fatalf("messages in fwin = %d", len(p.Items))
		}
		src := string(p.Items[0].Ref.ID)

		// Preview: recipient, attachment of the original, size, how it is found; nothing written.
		var plan planJSON
		s.json(&plan, "message", "forward", src, "--to", dest, "--dry-run")
		for _, want := range []string{dest, "message/rfc822", "gm-o", "google.mail.forward", "irreversible"} {
			if !strings.Contains(plan.Effects[0].Detail, want) && !strings.Contains(string(plan.Args["attachment"])+string(plan.Args["to"])+string(plan.Args["provider_message"]), want) {
				t.Errorf("preview lacks %q: %s / %v", want, plan.Effects[0].Detail, plan.Args)
			}
		}
		for _, k := range []string{"size", "carries", "finds", "original_message_id"} {
			if _, ok := plan.Args[k]; !ok {
				t.Errorf("preview args lack %s: %v", k, plan.Args)
			}
		}
		start := s.ledger()
		wantStderr(t, s.heo(2, "message", "forward", src, "--to", "Dest <d@example.test>", "--dry-run"), "bare address")
		wantStderr(t, s.heo(2, "message", "forward", src), `"to" not set`)
		wantStderr(t, s.heo(3, "message", "forward", "00000ad0-0000-7000-8000-0000000009ff", "--to", dest), "NOT_FOUND")
		s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		var pl pageJSON[model.Message]
		s.json(&pl, "message", "list", "--inbox", "plmail")
		if len(pl.Items) != 1 {
			t.Fatalf("messages in plmail = %d", len(pl.Items))
		}
		pm := string(pl.Items[0].Ref.ID)
		wantStderr(t, s.heo(2, "message", "forward", pm, "--to", dest, "--dry-run"), "UNSUPPORTED")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("a preview or refusal wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		// Forward: queued, replayed under its key, one provider effect.
		var out, again forwardJSON
		s.json(&out, "message", "forward", src, "--to", dest, "--text", "yours", "--idempotency-key", "fwd-1")
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Effect != "google.mail.forward" || out.Provider != "gm-o" || out.To != dest || out.Message != src {
			t.Fatalf("forward = %+v", out)
		}
		s.json(&again, "message", "forward", src, "--to", dest, "--text", "yours", "--idempotency-key", "fwd-1")
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replay = %+v", again)
		}
		if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("forward failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if line := s.applied()[0]; !strings.HasPrefix(line, "effect google.mail.forward gm-o ") || !strings.Contains(line, `"to":"`+dest+`"`) || !strings.Contains(line, `"text":"yours"`) {
			t.Fatalf("provider was asked %q", line)
		}
		var d struct {
			References []model.ExternalRef `json:"provider_references"`
		}
		s.json(&d, "delivery", "get", string(out.Operation.ID))
		fwdID := ""
		for _, r := range d.References {
			if r.Namespace == model.EmailHeaderNamespace && r.Kind == model.RefKindEmailMessageID {
				fwdID = r.ID
			}
		}
		if fwdID == "" {
			t.Fatalf("the operation reports no Message-ID of the copy: %+v", d.References)
		}

		// The copy arrives in B's inbox, beside an unrelated mail and a reply to the original.
		s.inject(s.mb, adapterstest.Inject{Event: "ev-c", Message: "gm-c", From: "a@example.test", Destination: "mb-1", Text: "forwarded", Parent: "gm-c",
			EmailMessageID: fwdID, EmailReferences: []string{orig}})
		s.inject(s.mb, adapterstest.Inject{Event: "ev-x", Message: "gm-x", From: peerMail, Destination: "mb-1", Text: "unrelated", Parent: "gm-x",
			EmailMessageID: "<other@remote.test>"})
		s.inject(s.mb, adapterstest.Inject{Event: "ev-r", Message: "gm-r", From: peerMail, Destination: "mb-1", Text: "a reply", Parent: "gm-r",
			EmailMessageID: "<reply@remote.test>", EmailInReplyTo: orig, EmailReferences: []string{orig}})
		find := func(flag, id string) []model.Message {
			var f pageJSON[model.Message]
			type nextPage struct {
				Items    []model.Message
				Next     string `json:"next"`
				Complete bool
			}
			for page, cur := 0, ""; page < 20; page++ {
				args := []string{"message", "list", "--inbox", "mbin", flag, id}
				if cur != "" {
					args = append(args, "--cursor", cur)
				}
				var got nextPage
				s.json(&got, args...)
				f.Items = append(f.Items, got.Items...)
				if got.Complete || got.Next == "" {
					break
				}
				cur = got.Next
			}
			return f.Items
		}
		byOwn := find("--message-id", fwdID)
		if len(byOwn) != 1 || len(byOwn[0].External) == 0 {
			t.Fatalf("--message-id %s found %d messages, want exactly the copy", fwdID, len(byOwn))
		}
		if got := find("--message-id", orig); len(got) != 0 {
			t.Errorf("--message-id is the message's own id: the original's id found %d", len(got))
		}
		if got := find("--message-id", "<nope@remote.test>"); len(got) != 0 {
			t.Errorf("unknown id found %d", len(got))
		}
		if got := find("--references", orig); len(got) != 2 {
			t.Errorf("--references %s found %d, want the copy and the reply", orig, len(got))
		}
		if got := find("--provider-id", "gm-c"); len(got) != 1 || got[0].Ref.ID != byOwn[0].Ref.ID {
			t.Errorf("--provider-id gm-c = %d messages, want the copy", len(got))
		}
		wantStderr(t, s.heo(2, "message", "list", "--message-id", "not an id"), "Message-ID")
		wantStderr(t, s.heo(2, "message", "list", "--message-id", " "), "--message-id")

		// Label and archive that message alone.
		cp := byOwn[0]
		bconv := string(cp.Ref.Conversation.ID)
		before := len(s.applied())
		s.heo(0, "conversation", "tag", bconv, "--add", "Label_1", "--message", string(cp.Ref.ID), "--idempotency-key", "b-tag")
		s.heo(0, "conversation", "archive", bconv, "--message", string(cp.Ref.ID), "--idempotency-key", "b-arch")
		s.wantApplied(before + 2)
		got := strings.Join(s.applied()[before:], "\n")
		for _, want := range []string{"effect google.mail.labels gm-c ", "state archive", "items=gm-c"} {
			if !strings.Contains(got, want) {
				t.Errorf("provider was asked\n%s\nwant %q", got, want)
			}
		}
		if strings.Contains(got, "gm-x") || strings.Contains(got, "gm-r") {
			t.Errorf("another message was touched:\n%s", got)
		}
	})
}
