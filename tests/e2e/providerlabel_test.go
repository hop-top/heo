package e2e

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

type providerLabelJSON struct {
	Channel string
	ID      string
	Name    string
	Kind    string
	Role    string
}

type providerLabelCreateJSON struct {
	Channel      string
	Name         string
	Effect       string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

// TestProviderLabelLifecycle drives the e2e build on Postgres through the
// provider mailbox verbs on a fake provider that lists and creates
// mailboxes as the mail adapters do: the list is one stable array of
// channel, id, name, kind and role; a create previews (and says the name
// exists), queues durably under its key, replays, reaches the provider
// with the name alone and the mailbox is then listed with its ID, which the
// operation's provider references carry; conversation move and tag take the
// mailbox by name and send its ID. A channel without the extension, an
// unknown channel, a usage error and a role that may not do it are refused
// before anything is written.
func TestProviderLabelLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		conv := s.mail(s.mb, "mbin", "mb-1", "ev-mb", "gmail-msg-1")
		cid := string(conv.Ref.ID)

		// The list: the machine form is stable and complete.
		var allPage, namedPage pageJSON[providerLabelJSON]
		s.json(&allPage, "provider-label", "list")
		s.json(&namedPage, "provider-label", "list", "--channel", "mb")
		all, named := allPage.Items, namedPage.Items
		if !allPage.Complete || !namedPage.Complete {
			t.Errorf("the list is one page: %+v %+v", allPage, namedPage)
		}
		want := []providerLabelJSON{
			{"mb", "Label_2", "Billing", "label", ""},
			{"mb", "Label_1", "Clients/Acme", "label", ""},
			{"mb", "INBOX", "INBOX", "label", "inbox"},
		}
		if len(all) != 3 || len(named) != 3 {
			t.Fatalf("list = %+v, named %+v", all, named)
		}
		for i := range want {
			if all[i] != want[i] || named[i] != want[i] {
				t.Errorf("row %d = %+v / %+v, want %+v", i, all[i], named[i], want[i])
			}
		}
		if r := s.heo(0, "provider-label", "list", "--channel", string(s.mb.Ref.ID), "--format", "json"); !strings.Contains(r.stdout, `"items"`) {
			t.Errorf("json is not a listing:\n%s", r.stdout)
		}
		r := s.heo(0, "provider-label", "list")
		for _, w := range []string{"CHANNEL", "Clients/Acme", "Label_1", "inbox"} {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("table lacks %q:\n%s", w, r.stdout)
			}
		}
		wantStderr(t, s.heo(2, "provider-label", "list", "--channel", "plain"), "UNSUPPORTED", "extension_query")
		wantStderr(t, s.heo(3, "provider-label", "list", "--channel", "nope"), "NOT_FOUND")

		// Create: preview, then queue, replay, apply.
		start := s.ledger()
		var plan planJSON
		s.json(&plan, "provider-label", "create", "Clients/Beta", "--channel", "mb", "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" || !strings.Contains(plan.Effects[0].Detail, "external") ||
			!strings.Contains(plan.Effects[0].Detail, "test.heo.fake.mailbox.mailbox_create") {
			t.Fatalf("preview = %+v", plan)
		}
		var exists planJSON
		s.json(&exists, "provider-label", "create", "clients/acme", "--dry-run")
		if !strings.Contains(exists.Effects[0].Detail, "Label_1") || !strings.Contains(exists.Effects[0].Detail, "idempotent") {
			t.Errorf("preview of an existing name = %+v", exists.Effects)
		}
		wantStderr(t, s.heo(2, "provider-label", "create", "a//b", "--dry-run"), "VALIDATION")
		wantStderr(t, s.heo(2, "provider-label", "create", "X", "--channel", "plain", "--dry-run"), "UNSUPPORTED")
		wantStderr(t, s.heo(3, "provider-label", "create", "X", "--channel", "nope"), "NOT_FOUND")
		wantStderr(t, s.heo(2, "provider-label", "create"), "")
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("a preview or a refusal wrote: operations %d, was %d; provider saw %v", n, start, s.applied())
		}

		var out, again providerLabelCreateJSON
		s.json(&out, "provider-label", "create", "Clients/Beta", "--idempotency-key", "pl-1")
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "pl-1" ||
			out.Channel != "mb" || out.Name != "Clients/Beta" || out.Effect != "test.heo.fake.mailbox.mailbox_create" {
			t.Fatalf("create = %+v", out)
		}
		s.json(&again, "provider-label", "create", "Clients/Beta", "--idempotency-key", "pl-1")
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed create = %+v, first %+v", again, out)
		}
		if d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("create failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); lines[0] != `effect test.heo.fake.mailbox.mailbox_create -  {"name":"Clients/Beta","kind":""}` {
			t.Fatalf("provider was asked %q", lines)
		}
		var afterPage pageJSON[providerLabelJSON]
		s.json(&afterPage, "provider-label", "list", "--channel", "mb")
		after := afterPage.Items
		created := providerLabelJSON{"mb", "Label_4", "Clients/Beta", "label", ""}
		if len(after) != 4 || after[2] != created {
			t.Fatalf("list after the create = %+v, want %+v third", after, created)
		}
		var dj struct {
			Refs []model.ExternalRef `json:"provider_references"`
		}
		s.json(&dj, "delivery", "get", string(out.Operation.ID))
		if len(dj.Refs) != 1 || dj.Refs[0].Kind != model.RefKindMailbox || dj.Refs[0].ID != "Label_4" {
			t.Errorf("provider references = %+v, want the mailbox ID", dj.Refs)
		}
		// Idempotent by name at the provider: a second create under another
		// key changes nothing there.
		s.heo(0, "provider-label", "create", "CLIENTS/BETA", "--idempotency-key", "pl-2")
		s.wantApplied(2)
		var samePage pageJSON[providerLabelJSON]
		s.json(&samePage, "provider-label", "list")
		if same := samePage.Items; len(same) != 4 {
			t.Errorf("a second create of the name made another mailbox: %+v", same)
		}

		// Names resolve in move and tag; IDs still go as given.
		before := len(s.applied())
		var plan2 planJSON
		s.json(&plan2, "conversation", "move", cid, "--to", "Clients/Beta", "--dry-run")
		if got := string(plan2.Args["resolved"]); !strings.Contains(got, "Clients/Beta = Label_4") {
			t.Errorf("move preview resolved = %s", got)
		}
		for i, c := range []struct {
			args []string
			line string
		}{
			{[]string{"conversation", "move", cid, "--to", "Clients/Acme"}, "state move " + peerMail + " to=Label_1"},
			{[]string{"conversation", "move", cid, "--to", "Label_2"}, "state move " + peerMail + " to=Label_2"},
			{[]string{"conversation", "move", cid, "--add", "Billing,Clients/Beta", "--remove", "INBOX"}, "state move " + peerMail + " +Label_2 +Label_4 -INBOX"},
			{[]string{"conversation", "move", cid, "--to", "Not A Mailbox"}, "state move " + peerMail + " to=Not A Mailbox"},
			{[]string{"conversation", "tag", cid, "--add", "Clients/Acme,STARRED", "--remove", "Billing"}, `effect google.mail.labels gmail-msg-1 ` + peerMail + ` {"add":["Label_1","STARRED"],"remove":["Label_2"]}`},
		} {
			var o struct{ Operation opJSON }
			s.json(&o, append(c.args, "--idempotency-key", "rs-"+string(rune('a'+i)))...)
			if d := s.waitDelivery(o.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
				t.Fatalf("%v failed: %+v", c.args, d.Failure)
			}
			s.wantApplied(before + i + 1)
			if got := s.applied()[before+i]; got != c.line {
				t.Errorf("%v: provider was asked %q, want %q", c.args, got, c.line)
			}
		}
		ops := s.ledger()
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.stop(s.d)
		s.d = s.start()
		wantDenied(t, s.s.run(nil, "provider-label", "list"), "heo.authz.not_granted")
		wantDenied(t, s.s.run(nil, "provider-label", "create", "V", "--dry-run"), "heo.authz.not_granted")
		if n := s.ledger(); n != ops {
			t.Errorf("a denied create wrote: operations %d, was %d", n, ops)
		}
		s.stop(s.d)
	})
}
