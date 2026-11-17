package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/model"
)

// verbsSite is a daemon with two fake channels on Postgres: "vb", whose
// adapter declares membership, receipt and history, and "plain", whose
// adapter declares none of them. Each has an inbox; ada wrote to both,
// and cy wrote to vb.
type verbsSite struct {
	*effectsSite
	vb, plain model.Channel
	adaConv   model.Conversation // ada in vb
	cyConv    model.Conversation // cy in vb
	plConv    model.Conversation // ada in plain
	adaMsg    model.Message
	plMsg     model.Message
}

func newVerbsSite(t *testing.T, profile string, vbConfig adapterstest.Config) *verbsSite {
	t.Helper()
	c := newCLISite(t, noBridge)
	e := &effectsSite{cliSite: c, effects: c.s.path("effects.log")}
	c.s.env["HEO_E2E_FAKE_EFFECT_LOG"] = e.effects
	if profile != "" {
		c.s.write("cfg/heo/profiles/default.yaml", profile)
	}
	e.d = c.start()
	if vbConfig.Account == "" {
		vbConfig.Account = "acct-vb"
	}
	v := &verbsSite{effectsSite: e}
	v.vb = c.fakeOf(adapterstest.VerbsType, "vb", vbConfig)
	v.plain = c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
	for _, in := range []struct{ ch, key, dest string }{{"vb", "vbin", "vb-1"}, {"plain", "plin", "pl-1"}} {
		c.heo(0, "inbox", "create", "--channel", in.ch, "--key", in.key, "--destination", in.dest,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	}
	v.adaMsg = e.receive(v.vb, "vbin", "vb-1", "ev-ada-1", "pm-vb-1", "hello from ada")
	e.inject(v.vb, adapterstest.Inject{Event: "ev-cy-1", Message: "pm-vb-2", From: "cy", Destination: "vb-1", Text: "hello from cy"})
	v.plMsg = e.receive(v.plain, "plin", "pl-1", "ev-pl-1", "pm-pl-1", "hello from ada")
	v.adaConv = v.conversation(v.adaMsg.Ref.Conversation.ID)
	v.plConv = v.conversation(v.plMsg.Ref.Conversation.ID)
	var convs pageJSON[model.Conversation]
	c.json(&convs, "conversation", "list", "--inbox", "vbin")
	for _, cv := range convs.Items {
		if cv.Ref.ID != v.adaConv.Ref.ID {
			v.cyConv = cv
		}
	}
	if v.cyConv.Ref.ID == "" {
		t.Fatalf("no conversation of cy in %+v", convs.Items)
	}
	return v
}

func (v *verbsSite) conversation(id model.ID) model.Conversation {
	v.t.Helper()
	var cv model.Conversation
	v.json(&cv, "conversation", "get", string(id))
	return cv
}

// identity is the identity of sender who in inbox.
func (v *verbsSite) identity(inbox, who string) model.Identity {
	v.t.Helper()
	var ids pageJSON[model.Identity]
	v.json(&ids, "identity", "list", "--inbox", inbox)
	for _, i := range ids.Items {
		if i.SourceID == who {
			return i
		}
	}
	v.t.Fatalf("no identity of %s in %s: %+v", who, inbox, ids.Items)
	return model.Identity{}
}

func (v *verbsSite) participants(cv model.Conversation) map[model.ID]string {
	v.t.Helper()
	var p participantsJSON
	v.json(&p, "participant", "list", "--conversation", string(cv.Ref.ID))
	out := map[model.ID]string{}
	for _, x := range p.Participants {
		out[model.ID(x.Identity)] = x.Role
	}
	return out
}

// restart runs the daemon again under profile.
func (v *verbsSite) restart(profile string) {
	v.t.Helper()
	v.stop(v.d)
	v.s.write("cfg/heo/profiles/default.yaml", profile)
	v.d = v.start()
}

// lines of the effects log that start with prefix.
func (v *verbsSite) applied(prefix string) []string {
	var out []string
	for _, l := range v.appliedLines() {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

type participantChangeJSON struct {
	Conversation model.ID
	Identity     model.ID
	Contact      model.ID
	Remove       bool
	Revision     model.Revision
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// TestParticipantLifecycle drives the e2e build on Postgres through
// adding and removing a participant on a fake provider that declares
// membership: a preview names the platform effect and what it checked of
// the adapter, and writes nothing; the add is queued durably with the
// conversation's revision always sent, applied by the provider, and
// listed only then; a contact with one identity resolves; the removal is
// destructive-shared, so the terminal asks for confirmation; stale
// revisions, a participant already held, one not held and another
// inbox's identity are refused; an adapter that does not declare
// membership is refused at the preview and on submission, exit 2, with
// nothing written; a viewer is refused.
func TestParticipantLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		v := newVerbsSite(t, noRateLimit, adapterstest.Config{})
		cv := v.adaConv
		id, rev := string(cv.Ref.ID), string(cv.Revision)
		cy := v.identity("vbin", "cy")
		add := []string{"participant", "add", "--conversation", id, "--revision", rev, string(cy.Ref.ID)}

		// The preview.
		var plan planJSON
		v.json(&plan, append(add, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "conversation:"+id ||
			!strings.Contains(plan.Effects[0].Detail, "applied only when the provider confirms") {
			t.Fatalf("add preview effects = %+v", plan.Effects)
		}
		for k, want := range map[string]string{
			"identity": `"` + string(cy.Ref.ID) + `"`, "expected_revision": `"` + rev + `"`, "audience": `"internal"`,
			"adapter": `"` + string(adapterstest.VerbsType) + `"`,
		} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		for _, want := range []string{"membership declared by " + string(adapterstest.VerbsType), "live availability not checked", "heo channel check " + string(v.vb.Ref.ID) + " --remote"} {
			if !strings.Contains(capLine, want) {
				t.Errorf("capability line %q lacks %q", capLine, want)
			}
		}
		start := v.ledger()
		v.heo(0, append(add, "--dry-run")...)
		if v.ledger() != start || len(v.appliedLines()) != 0 {
			t.Fatalf("--dry-run wrote: %d operations, was %d; provider %v", v.ledger(), start, v.appliedLines())
		}

		// The add: queued, never applied; the list shows it only once the
		// provider did.
		var out, again participantChangeJSON
		v.json(&out, append(add, "--idempotency-key", "add-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.Identity != cy.Ref.ID ||
			out.Remove || out.Revision != cv.Revision || out.OperationKey != "add-1" {
			t.Fatalf("add = %+v", out)
		}
		v.json(&again, append(add, "--idempotency-key", "add-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed add = %+v, first %+v", again, out)
		}
		if d := v.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("add failed: %+v", d.Failure)
		}
		if lines := v.applied("member"); len(lines) != 1 || lines[0] != "member add cy" {
			t.Fatalf("provider was asked %q, want one add of cy", lines)
		}
		ps := v.participants(cv)
		if ps[cy.Ref.ID] != "participant" || len(ps) != 2 {
			t.Fatalf("participants after the add = %v", ps)
		}
		got := v.conversation(cv.Ref.ID)
		if got.Revision == cv.Revision {
			t.Error("the conversation's revision did not move when the provider applied the add")
		}
		// The text form says queued, not applied.
		cur := string(got.Revision)

		// Adding a participant again, and removing a stranger, conflict.
		wantStderr(t, v.heo(4, "participant", "add", "--conversation", id, "--revision", cur, string(cy.Ref.ID), "--dry-run"), "CONFLICT", "already a participant")
		// A stale revision: conflict naming the current one, nothing queued.
		before := v.ledger()
		wantStderr(t, v.heo(4, "participant", "remove", "--conversation", id, "--revision", rev, string(cy.Ref.ID), "--confirm=yes"), "CONFLICT", "is at revision "+cur)
		if v.ledger() != before {
			t.Error("a stale removal queued an operation")
		}
		// A required revision.
		wantStderr(t, v.s.run(nil, "participant", "add", "--conversation", id, string(cy.Ref.ID)), "USAGE")

		// The removal is destructive-shared: preview, refused unconfirmed,
		// confirmed.
		rm := []string{"participant", "remove", "--conversation", id, "--revision", cur, string(cy.Ref.ID)}
		var rplan planJSON
		v.json(&rplan, append(rm, "--dry-run")...)
		if len(rplan.Effects) != 1 || !strings.Contains(rplan.Effects[0].Detail, "remove identity "+string(cy.Ref.ID)+" from the participants") {
			t.Fatalf("remove preview = %+v", rplan.Effects)
		}
		v.s.want(v.s.run(nil, append(rm, "--format", "json")...), 5, rm...)
		if v.ledger() != before || len(v.applied("member")) != 1 {
			t.Fatal("an unconfirmed removal wrote")
		}
		var rout participantChangeJSON
		v.json(&rout, append(rm, "--confirm=yes", "--idempotency-key", "rm-1")...)
		if rout.Operation.State != model.Queued || !rout.Remove || rout.OperationKey != "rm-1" {
			t.Fatalf("remove = %+v", rout)
		}
		if d := v.waitDelivery(rout.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("remove failed: %+v", d.Failure)
		}
		if lines := v.applied("member"); len(lines) != 2 || lines[1] != "member remove cy" {
			t.Fatalf("provider was asked %q", lines)
		}
		if ps := v.participants(cv); len(ps) != 1 {
			t.Fatalf("participants after the removal = %v", ps)
		}

		// A contact with one identity in the inbox names it.
		cur = string(v.conversation(cv.Ref.ID).Revision)
		var byContact participantChangeJSON
		v.json(&byContact, "participant", "add", "--conversation", id, "--revision", cur, string(cy.Contact.ID), "--idempotency-key", "add-2")
		if byContact.Identity != cy.Ref.ID || byContact.Contact != cy.Contact.ID {
			t.Fatalf("add by contact = %+v", byContact)
		}
		v.waitDelivery(byContact.Operation.ID, model.ProviderAccepted, model.Delivered)

		// An identity of another inbox is not this conversation's.
		pl := v.identity("plin", "ada")
		cur = string(v.conversation(cv.Ref.ID).Revision)
		wantStderr(t, v.heo(3, "participant", "add", "--conversation", id, "--revision", cur, string(pl.Ref.ID), "--dry-run"), "NOT_FOUND", "no identity or contact")

		// An adapter that does not declare membership: refused at the
		// preview and on submission, nothing written.
		afterAll := v.ledger()
		applied := len(v.appliedLines())
		pc := []string{"participant", "add", "--conversation", string(v.plConv.Ref.ID), "--revision", string(v.plConv.Revision), string(pl.Ref.ID)}
		wantStderr(t, v.heo(2, append(pc, "--dry-run")...), "UNSUPPORTED", "does not declare membership", "heo channel get "+string(v.plain.Ref.ID))
		wantStderr(t, v.heo(2, append(pc, "--idempotency-key", "add-plain")...), "UNSUPPORTED")
		if n := v.ledger(); n != afterAll || len(v.appliedLines()) != applied {
			t.Errorf("an undeclared add wrote: operations %d, was %d; provider %v", n, afterAll, v.appliedLines())
		}

		// A viewer may not, preview included.
		v.restart("authorization:\n  owner: [viewer]\n")
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, v.s.run(nil, append([]string{"participant", "add", "--conversation", id, "--revision", cur, string(cy.Ref.ID)}, extra...)...), "heo.authz.not_granted")
		}
		if len(v.applied("member")) != 3 {
			t.Errorf("provider saw %v", v.applied("member"))
		}
		v.stop(v.d)
	})
}

type receiptJSON struct {
	Message      model.ID
	Conversation model.ID
	Kind         string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// TestReceiptSendLifecycle: a receipt on a fake provider that declares
// receipt: previewed with what was checked of the adapter and that no
// revision is involved, queued durably, sent once, and nothing in heo
// changes: the operation is the record. The output never says the
// provider showed it. A message sent through heo, a bad kind, an
// undeclared adapter and a viewer are refused with nothing written.
func TestReceiptSendLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		v := newVerbsSite(t, "", adapterstest.Config{})
		m := v.adaMsg
		id := string(m.Ref.ID)
		send := []string{"receipt", "send", "--message", id, "--kind", "read"}

		var plan planJSON
		v.json(&plan, append(send, "--dry-run")...)
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if len(plan.Effects) != 1 || plan.Effects[0].Target != "message:"+id || string(plan.Args["kind"]) != `"read"` ||
			!strings.Contains(capLine, "receipt declared by "+string(adapterstest.VerbsType)) || !strings.Contains(capLine, "live availability not checked") ||
			!strings.Contains(plan.Effects[0].Detail, "what the sender is shown is the provider's") {
			t.Fatalf("preview = %+v capability %q", plan, capLine)
		}
		if _, has := plan.Args["expected_revision"]; has {
			t.Error("a receipt preview names a revision")
		}
		start := v.ledger()
		v.heo(0, append(send, "--dry-run")...)
		if v.ledger() != start || len(v.appliedLines()) != 0 {
			t.Fatal("--dry-run wrote")
		}

		var out, again receiptJSON
		v.json(&out, append(send, "--idempotency-key", "rcpt-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Message != m.Ref.ID || out.Kind != "read" || out.OperationKey != "rcpt-1" {
			t.Fatalf("receipt = %+v", out)
		}
		v.json(&again, append(send, "--idempotency-key", "rcpt-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed receipt = %+v, first %+v", again, out)
		}
		if d := v.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered); d.State == model.Failed {
			t.Fatalf("receipt failed: %+v", d.Failure)
		}
		if lines := v.applied("receipt"); len(lines) != 1 || lines[0] != "receipt pm-vb-1 read" {
			t.Fatalf("provider was asked %q, want one read receipt of pm-vb-1", lines)
		}
		// Nothing in heo changed: not the message, nor the conversation.
		if got := v.message(m); got.Revision != m.Revision {
			t.Errorf("the receipt moved the message to %s", got.Revision)
		}
		// The text never claims the provider showed it.
		txt := v.heo(0, "receipt", "send", "--message", id, "--kind", "delivered")
		for _, want := range []string{"queued", "receipt"} {
			if !strings.Contains(txt.stdout+txt.stderr, want) {
				t.Errorf("text receipt lacks %q:\nstdout: %s\nstderr: %s", want, txt.stdout, txt.stderr)
			}
		}
		if !strings.Contains(txt.stderr, "whether the provider shows it to the sender is the provider's") {
			t.Errorf("text receipt does not say the provider decides what the sender sees:\n%s", txt.stderr)
		}
		for _, bad := range []string{"was read", "was shown", "seen by", "shown to"} {
			if strings.Contains(txt.stdout+txt.stderr, bad) {
				t.Errorf("text receipt claims %q:\n%s%s", bad, txt.stdout, txt.stderr)
			}
		}
		// The same key under another kind is a conflict.
		wantStderr(t, v.heo(4, "receipt", "send", "--message", id, "--kind", "delivered", "--idempotency-key", "rcpt-1"), "CONFLICT")

		// What heo sent is no received message; a bad kind is a usage error.
		v.heo(0, "message", "reply", string(v.adaConv.Ref.ID), "--text", "our answer", "--idempotency-key", "reply-1")
		var msgs pageJSON[model.Message]
		v.json(&msgs, "message", "list", "--conversation", string(v.adaConv.Ref.ID))
		var ours model.Message
		for _, x := range msgs.Items {
			if desk.Text(x.Content) == "our answer" {
				ours = x
			}
		}
		if ours.Ref.ID == "" {
			t.Fatalf("our reply is not listed: %+v", msgs.Items)
		}
		wantStderr(t, v.heo(2, "receipt", "send", "--message", string(ours.Ref.ID), "--kind", "read", "--dry-run"), "VALIDATION", "did not arrive from the provider")
		wantStderr(t, v.heo(2, "receipt", "send", "--message", id, "--kind", "seen"), "VALIDATION", "want read or delivered")
		wantStderr(t, v.heo(3, "receipt", "send", "--message", "msg_404", "--kind", "read"), "NOT_FOUND")

		// An adapter that does not declare receipt.
		before := v.ledger()
		applied := len(v.appliedLines())
		pr := []string{"receipt", "send", "--message", string(v.plMsg.Ref.ID), "--kind", "read"}
		wantStderr(t, v.heo(2, append(pr, "--dry-run")...), "UNSUPPORTED", "does not declare receipt", "heo channel get "+string(v.plain.Ref.ID))
		wantStderr(t, v.heo(2, append(pr, "--idempotency-key", "rcpt-plain")...), "UNSUPPORTED")
		if n := v.ledger(); n != before || len(v.appliedLines()) != applied {
			t.Errorf("an undeclared receipt wrote: operations %d, was %d; provider %v", n, before, v.appliedLines())
		}
		// Not the local read state: `conversation mark-read` moves the
		// caller's own marker and sends nothing, on a channel that declares
		// receipt as on any other.
		before, applied = v.ledger(), len(v.appliedLines())
		v.heo(0, "conversation", "mark-read", string(v.adaConv.Ref.ID))
		v.heo(0, "conversation", "mark-unread", string(v.adaConv.Ref.ID))
		if n := v.ledger(); n != before || len(v.appliedLines()) != applied {
			t.Errorf("marking read wrote: operations %d, was %d; provider %v", n, before, v.appliedLines())
		}

		// A viewer may not, preview included.
		v.restart("authorization:\n  owner: [viewer]\n")
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, v.s.run(nil, append([]string{"receipt", "send", "--message", id, "--kind", "read"}, extra...)...), "heo.authz.not_granted")
		}
		if len(v.applied("receipt")) != 2 {
			t.Errorf("provider saw %v", v.applied("receipt"))
		}
		v.stop(v.d)
	})
}

type historyJSON struct {
	Channel      model.ID
	Conversation model.ID
	Since        string
	Limit        int
	Imported     int
	Skipped      int
	Gaps         int
	Gap          bool
	Complete     bool
	Next         string
}

// holdHistory hands the channel's provider the history it holds.
func (v *verbsSite) holdHistory(ch model.Channel, h adapterstest.HistoryHold) {
	v.t.Helper()
	body, err := json.Marshal(h)
	if err != nil {
		v.t.Fatal(err)
	}
	url := "http://" + v.hook + "/webhooks/" + string(ch.Ref.ID) + adapterstest.HistoryRoute
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:noctx,gosec // loopback test URL
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return
			}
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				v.t.Fatalf("hold history = %d", resp.StatusCode)
			}
		}
		if time.Now().After(deadline) {
			v.t.Fatalf("the channel's webhook never answered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// past is n provider messages of sender who, at fixed times in 2026.
func past(who, dest, prefix string, n int) []adapterstest.Inject {
	var out []adapterstest.Inject
	for i := range n {
		out = append(out, adapterstest.Inject{
			Event: fmt.Sprintf("%s-ev-%d", prefix, i), Message: fmt.Sprintf("%s-%d", prefix, i), From: who, Destination: dest,
			Text: fmt.Sprintf("%s past %d", prefix, i), OccurredAt: time.Date(2026, 1, 1+i, 9, 0, 0, 0, time.UTC),
		})
	}
	return out
}

func (v *verbsSite) texts(cv model.Conversation) []string {
	v.t.Helper()
	var msgs pageJSON[model.Message]
	v.json(&msgs, "message", "list", "--conversation", string(cv.Ref.ID), "--limit", "100")
	var out []string
	for _, m := range msgs.Items {
		out = append(out, desk.Text(m.Content))
	}
	return out
}

// TestHistoryFetchLifecycle: a bounded history import on a fake provider
// that declares history and holds scripted past messages. The preview
// reads nothing; the import records each message once by provider
// message ID, stops at its limit with a cursor that resumes it, and a
// second run imports nothing. A provider whose window is exhausted
// (older messages gone) is reported as a gap, never as a whole history,
// and says so in text. An adapter without history is refused before any
// read; roles decide who imports what: a conversation is the desk's, a
// channel the operator's, a viewer's nothing.
func TestHistoryFetchLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		v := newVerbsSite(t, "", adapterstest.Config{})
		cv := v.adaConv
		id := string(cv.Ref.ID)
		ada := past("ada", "vb-1", "ha", 5)
		cy := past("cy", "vb-1", "hc", 2)
		v.holdHistory(v.vb, adapterstest.HistoryHold{Items: append(slices.Clone(ada), cy...)})
		fetch := []string{"history", "fetch", "--conversation", id}

		// The preview reads nothing and writes nothing.
		var plan planJSON
		v.json(&plan, append(fetch, "--dry-run", "--limit", "3")...)
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if len(plan.Effects) != 1 || !strings.Contains(plan.Effects[0].Detail, "nothing is sent to the provider, and nothing is read until the real run") ||
			!strings.Contains(capLine, "history declared by "+string(adapterstest.VerbsType)) || string(plan.Args["limit"]) != "3" {
			t.Fatalf("preview = %+v capability %q", plan, capLine)
		}
		txt := v.heo(0, append(fetch, "--dry-run")...)
		for _, want := range []string{"capability", "since", "limit"} {
			if !strings.Contains(txt.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, txt.stdout)
			}
		}
		if len(v.applied("history")) != 0 || len(v.texts(cv)) != 1 {
			t.Fatalf("--dry-run read the provider (%v) or wrote (%v)", v.applied("history"), v.texts(cv))
		}

		// Bounded: three of ada's five, a cursor to resume. Route filtering:
		// cy's messages are not ada's conversation's.
		var first historyJSON
		v.json(&first, append(fetch, "--limit", "3")...)
		if first.Imported != 3 || first.Complete || first.Next == "" || first.Gap || first.Conversation != cv.Ref.ID || first.Channel != v.vb.Ref.ID || first.Limit != 3 {
			t.Fatalf("first import = %+v", first)
		}
		r := v.heo(0, append(fetch, "--limit", "1", "--since", "2026-01-04")...)
		for _, want := range []string{"imported", "skipped", "complete", "stopped at the limit of 1 messages read: resume with --cursor"} {
			if !strings.Contains(r.stdout+r.stderr, want) {
				t.Errorf("text import lacks %q:\nstdout: %s\nstderr: %s", want, r.stdout, r.stderr)
			}
		}
		var second historyJSON
		v.json(&second, append(fetch, "--cursor", first.Next)...)
		if second.Imported != 1 || second.Skipped != 1 || !second.Complete || second.Next != "" {
			t.Fatalf("resumed import = %+v, want the one message left imported and the one held skipped", second)
		}
		// Idempotent: everything again imports nothing.
		var third historyJSON
		v.json(&third, fetch...)
		if third.Imported != 0 || third.Skipped != 5 || !third.Complete || third.Gap {
			t.Fatalf("a second full run = %+v, want 0 imported and 5 skipped", third)
		}
		got := v.texts(cv)
		if len(got) != 6 {
			t.Fatalf("conversation holds %v, want hello + 5 past", got)
		}
		again := v.heo(0, fetch...)
		if !strings.Contains(again.stderr, "nothing new") || strings.Contains(again.stderr, "not whole") {
			t.Errorf("an empty rerun says:\n%s", again.stderr)
		}
		// Imported history is inbound: no operation, nothing unread, no
		// echo into the provider.
		if n := len(v.applied("receipt")) + len(v.applied("member")); n != 0 {
			t.Errorf("the import made the provider do %d things", n)
		}

		// Another conversation's messages stayed out; a channel import is
		// the operator's and takes what is left.
		if got := v.texts(v.cyConv); len(got) != 1 {
			t.Fatalf("cy's conversation holds %v before the channel import", got)
		}
		var ch historyJSON
		v.json(&ch, "history", "fetch", "--channel", "vb")
		if ch.Imported != 2 || ch.Skipped != 5 || ch.Conversation != "" || !ch.Complete {
			t.Fatalf("channel import = %+v, want cy's two imported and ada's five skipped", ch)
		}

		// A window the provider could not return whole is a gap.
		v.holdHistory(v.vb, adapterstest.HistoryHold{
			Items:   append(slices.Clone(ada), past("ada", "vb-1", "hn", 7)[5:]...),
			Horizon: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC),
		})
		var gap historyJSON
		v.json(&gap, fetch...)
		if gap.Gaps != 1 || !gap.Gap || !gap.Complete || gap.Imported != 2 {
			t.Fatalf("import past the provider's horizon = %+v, want a gap and the two newer messages", gap)
		}
		gr := v.heo(0, fetch...)
		if !strings.Contains(gr.stderr, "the history is not whole") || strings.Contains(gr.stderr, "read to its end") {
			t.Errorf("a gap is not said plainly:\n%s", gr.stderr)
		}

		// An adapter that declares no history: refused before any read.
		reads := len(v.applied("history"))
		before := len(v.texts(v.plConv))
		pf := []string{"history", "fetch", "--conversation", string(v.plConv.Ref.ID)}
		wantStderr(t, v.heo(2, append(pf, "--dry-run")...), "UNSUPPORTED", "does not declare history", "heo channel get "+string(v.plain.Ref.ID))
		wantStderr(t, v.heo(2, pf...), "UNSUPPORTED")
		wantStderr(t, v.heo(2, "history", "fetch", "--channel", "plain"), "UNSUPPORTED")
		if len(v.applied("history")) != reads || len(v.texts(v.plConv)) != before {
			t.Error("an undeclared import read the provider or wrote")
		}
		// Usage: exactly one scope, a bounded limit, a time.
		wantStderr(t, v.heo(2, "history", "fetch"), "USAGE", "exactly one of --channel and --conversation")
		wantStderr(t, v.heo(2, "history", "fetch", "--channel", "vb", "--conversation", id), "USAGE")
		wantStderr(t, v.heo(2, append(fetch, "--limit", "501")...), "USAGE", "1 to 500")
		wantStderr(t, v.heo(2, append(fetch, "--since", "yesterday")...), "USAGE", "--since")
		wantStderr(t, v.heo(3, "history", "fetch", "--conversation", "conv_404"), "NOT_FOUND")

		// Roles: a desk actor imports a conversation, not a channel; an
		// operator a channel, not a conversation; a viewer nothing.
		v.restart("authorization:\n  owner: [desk]\n")
		v.heo(0, append(fetch, "--dry-run")...)
		wantDenied(t, v.s.run(nil, "history", "fetch", "--channel", "vb", "--dry-run"), "heo.authz.not_granted")
		v.restart("authorization:\n  owner: [operator]\n")
		v.heo(0, "history", "fetch", "--channel", "vb", "--dry-run")
		wantDenied(t, v.s.run(nil, append(fetch, "--dry-run")...), "heo.authz.not_granted")
		v.restart("authorization:\n  owner: [viewer]\n")
		for _, args := range [][]string{append(slices.Clone(fetch), "--dry-run"), fetch, {"history", "fetch", "--channel", "vb"}} {
			wantDenied(t, v.s.run(nil, args...), "heo.authz.not_granted")
		}
		v.stop(v.d)
	})
}

// TestHistoryFetchPreviewsOnMaintainedAdapters: the maintained adapters
// that declare history (github.issues, twilio.sms, slack.chat, discord.chat) take a preview, which
// reads their real declaration; nothing is imported, nothing is read
// from a provider. The mail adapters, which declare none, are refused.
func TestHistoryFetchPreviewsOnMaintainedAdapters(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		c.json(&channelOutJSON{}, "channel", "create", "--type", "github.issues", "--key", "gh", "--configuration", `{}`, "--credential-ref", "gh-token")
		cfg, err := json.Marshal(sms.Config{AccountSID: xrrx.TwilioAccountSID, From: xrrx.TwilioSender, WebhookURL: "https://heo.invalid/twilio/sms"})
		if err != nil {
			t.Fatal(err)
		}
		c.json(&channelOutJSON{}, "channel", "create", "--type", string(model.TypeTwilioSms), "--key", "sms", "--configuration", string(cfg), "--credential-ref", "twilio")
		chat, err := json.Marshal(slack.Config{TeamID: "T0000000001", BotUserID: "U0000000001"})
		if err != nil {
			t.Fatal(err)
		}
		c.json(&channelOutJSON{}, "channel", "create", "--type", string(model.TypeSlackChat), "--key", "chat", "--configuration", string(chat), "--credential-ref", "slack")
		bot, err := json.Marshal(discord.Config{ApplicationID: "900000000000000001", BotUserID: "900000000000000002"})
		if err != nil {
			t.Fatal(err)
		}
		c.json(&channelOutJSON{}, "channel", "create", "--type", string(model.TypeDiscordChat), "--key", "bot", "--configuration", string(bot), "--credential-ref", "discord")
		for key, adapter := range map[string]string{"gh": "github.issues", "sms": "twilio.sms", "chat": "slack.chat", "bot": "discord.chat"} {
			var plan planJSON
			c.json(&plan, "history", "fetch", "--channel", key, "--dry-run")
			var capLine string
			_ = json.Unmarshal(plan.Args["capability"], &capLine)
			if !strings.Contains(capLine, "history declared by "+adapter) || len(plan.Effects) != 1 {
				t.Errorf("%s preview = %+v capability %q", key, plan, capLine)
			}
		}
		c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
		wantStderr(t, c.heo(2, "history", "fetch", "--channel", "plain", "--dry-run"), "UNSUPPORTED", "does not declare history")
		c.stop(d)
	})
}

// TestConversationGetShowsVerbs: a desk actor reading a conversation sees
// what its channel can do now, per provider-dependent verb, from uma's
// live capability status and without the right to read channels: a verb
// the adapter declares is available; one switched off by the channel's
// configuration is disabled (DISABLED, exit 4); one the provider account
// lacks the permission for is unauthorized (PROVIDER_PERMISSION, exit 5);
// one the adapter does not declare is unsupported. A viewer sees none.
func TestConversationGetShowsVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		v := newVerbsSite(t, "", adapterstest.Config{
			Disabled: []model.Capability{model.CapabilityHistory}, Denied: []model.Capability{model.CapabilityReceipt},
		})
		var cv struct {
			Ref   model.ConversationRef
			Verbs []verbLiveJSON
		}
		v.json(&cv, "conversation", "get", string(v.adaConv.Ref.ID))
		if cv.Ref.ID != v.adaConv.Ref.ID {
			t.Fatalf("conversation = %+v", cv)
		}
		for verb, w := range map[string]verbLiveJSON{
			"participant add":     {State: "available"},
			"participant remove":  {State: "available"},
			"message edit":        {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"receipt send":        {State: "unauthorized", Code: "PROVIDER_PERMISSION", Exit: 5},
			"history fetch":       {State: "disabled", Code: "DISABLED", Exit: 4},
			"conversation update": {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
			"interaction respond": {State: "unsupported", Code: "UNSUPPORTED", Exit: 2},
		} {
			got, ok := verbOf(cv.Verbs, verb)
			if !ok || got.State != w.State || got.Code != w.Code || got.Exit != w.Exit {
				t.Errorf("verb %s = %+v (listed %v), want %+v", verb, got, ok, w)
			}
		}
		txt := v.heo(0, "conversation", "get", string(v.adaConv.Ref.ID))
		for _, want := range []string{"verb participant add", "available", "verb receipt send", "unauthorized (PROVIDER_PERMISSION, exit 5)", "disabled (DISABLED, exit 4)"} {
			if !strings.Contains(txt.stdout, want) {
				t.Errorf("text conversation get lacks %q:\n%s", want, txt.stdout)
			}
		}
		// The plain adapter declares none of the new verbs.
		var pc struct{ Verbs []verbLiveJSON }
		v.json(&pc, "conversation", "get", string(v.plConv.Ref.ID))
		for _, verb := range []string{"participant add", "receipt send", "history fetch"} {
			if got, ok := verbOf(pc.Verbs, verb); !ok || got.State != "unsupported" {
				t.Errorf("plain %s = %+v (listed %v)", verb, got, ok)
			}
		}
		// A viewer reads the conversation and sees no verbs.
		v.restart("authorization:\n  owner: [viewer]\n")
		var vc struct {
			Ref   model.ConversationRef
			Verbs []verbLiveJSON
		}
		v.json(&vc, "conversation", "get", string(v.adaConv.Ref.ID))
		if vc.Ref.ID != v.adaConv.Ref.ID || len(vc.Verbs) != 0 {
			t.Errorf("viewer conversation get = %+v", vc)
		}
		v.stop(v.d)
	})
}
