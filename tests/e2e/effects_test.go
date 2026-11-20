package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

type conversationTagJSON struct {
	Conversation model.Conversation
	Message      string
	Effect       string
	Add          []string
	Remove       []string
	Set          *[]string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
	Notes        []string
}

// effectSite is a daemon with fake channels on Postgres whose adapters
// ship the extension effects of GitHub, Gmail, Outlook and Zendesk
// (each declares extension_execute and runs its one effect, recording
// it), and the plain fake, which declares nothing. Each channel's inbox
// holds one issue or one mail conversation reported through ingress.
type effectSite struct {
	*cliSite
	d       *daemon
	effects string
	up      model.Channel // ships github.issues.update
	lb      model.Channel // ships google.mail.labels
	cat     model.Channel // ships microsoft.mail.categories
	dr      model.Channel // ships google.mail.reply_draft
	un      model.Channel // ships google.mail.unsubscribe
	fw      model.Channel // ships google.mail.forward
	st      model.Channel // declares every provider conversation state capability: account scope, labels
	sf      model.Channel // declares archive, move and flag: shared scope, folder moves, the star flag
	tk      model.Channel // declares extension_execute, ships zendesk's update only
	mb      model.Channel // declares raw_read and lists and creates provider mailboxes
	plain   model.Channel // declares none
}

const (
	upRepo      = "example/updates"
	plainIssues = "example/plain-issues"
	tkRepo      = "example/ticket-effect"
	peerMail    = "peer@example.com"
)

func newEffectSite(t *testing.T, profile string) *effectSite {
	t.Helper()
	c := newCLISite(t, noBridge)
	s := &effectSite{cliSite: c, effects: c.s.path("effects.log")}
	c.s.env["HEO_E2E_FAKE_EFFECT_LOG"] = s.effects
	if profile != "" {
		c.s.write("cfg/heo/profiles/default.yaml", profile)
	}
	s.d = c.start()
	s.up = c.fakeOf(adapterstest.UpdatesType, "up", adapterstest.Config{Account: "acct-up"})
	s.lb = c.fakeOf(adapterstest.LabelsType, "lb", adapterstest.Config{Account: "acct-lb"})
	s.cat = c.fakeOf(adapterstest.CategoriesType, "cat", adapterstest.Config{Account: "acct-cat"})
	s.dr = c.fakeOf(adapterstest.DraftsType, "dr", adapterstest.Config{Account: "acct-dr"})
	s.un = c.fakeOf(adapterstest.UnsubscribeType, "un", adapterstest.Config{Account: "acct-un"})
	s.fw = c.fakeOf(adapterstest.ForwardType, "fw", adapterstest.Config{Account: "acct-fw"})
	s.st = c.fakeOf(adapterstest.StateType, "st", adapterstest.Config{Account: "acct-st"})
	s.sf = c.fakeOf(adapterstest.StateFolderType, "sf", adapterstest.Config{Account: "acct-sf"})
	s.tk = c.fakeOf(adapterstest.TicketEffectType, "tk", adapterstest.Config{Account: "acct-tk"})
	s.mb = c.fakeOf(adapterstest.MailboxType, "mb", adapterstest.Config{Account: "acct-mb"})
	s.plain = c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
	for _, in := range []struct{ ch, key, dest, mode string }{
		{"up", "upin", upRepo, model.ModePerWorkItem},
		{"plain", "plissues", plainIssues, model.ModePerWorkItem},
		{"tk", "tkissues", tkRepo, model.ModePerWorkItem},
		{"lb", "lbin", "lb-1", model.ModePerContact},
		{"cat", "catin", "cat-1", model.ModePerContact},
		{"dr", "drin", "dr-1", model.ModePerContact},
		{"un", "unin", "un-1", model.ModePerContact},
		{"fw", "fwin", "fw-1", model.ModePerContact},
		{"st", "stin", "st-1", model.ModePerContact},
		{"sf", "sfin", "sf-1", model.ModePerContact},
		{"mb", "mbin", "mb-1", model.ModePerContact},
		{"plain", "plmail", "pl-1", model.ModePerContact},
		{"tk", "tkmail", "tk-1", model.ModePerContact},
	} {
		args := []string{"inbox", "create", "--channel", in.ch, "--key", in.key, "--destination", in.dest, "--mode", in.mode}
		if in.mode == model.ModePerContact {
			args = append(args, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		}
		c.heo(0, args...)
	}
	return s
}

// report has the provider report issue number of repo, as its webhook
// would.
func (s *effectSite) report(ch model.Channel, repo, event, number string, issue adapterstest.InjectIssue) {
	s.t.Helper()
	issue.Number, issue.Status = number, model.WorkItemOpen
	if issue.Subject == "" {
		issue.Subject = "Crash on start"
	}
	s.inject(ch, adapterstest.Inject{Event: event, Kind: model.EnvelopeWorkItem, Destination: repo, Issue: &issue})
}

// itemIn is the work item reported for inbox, read through its
// conversation.
func (s *effectSite) itemIn(inbox string) model.WorkItem {
	s.t.Helper()
	var p pageJSON[conversationItemJSON]
	s.json(&p, "conversation", "list", "--inbox", inbox, "--work-item")
	if len(p.Items) != 1 || p.Items[0].WorkItem == nil {
		s.t.Fatalf("work items in %s = %+v", inbox, p.Items)
	}
	return *p.Items[0].WorkItem
}

// mail has the provider deliver a message the peer wrote, which names
// parent as its provider message, and returns the conversation (parent
// empty: a reply route that names no message).
func (s *effectSite) mail(ch model.Channel, inbox, dest, event, parent string) model.Conversation {
	s.t.Helper()
	s.inject(ch, adapterstest.Inject{Event: event, Message: event, From: peerMail, Destination: dest, Text: "hello", Parent: parent})
	var p pageJSON[model.Conversation]
	s.json(&p, "conversation", "list", "--inbox", inbox)
	if len(p.Items) != 1 {
		s.t.Fatalf("conversations in %s = %+v", inbox, p.Items)
	}
	return p.Items[0]
}

// applied is what the fake providers were asked, one line per effect.
func (s *effectSite) applied() []string {
	b, _ := os.ReadFile(s.effects)
	t := strings.TrimSpace(string(b))
	if t == "" {
		return nil
	}
	return strings.Split(t, "\n")
}

// ledger counts the operations uma holds: what a refused or previewed
// verb must leave as it was.
func (s *effectSite) ledger() int {
	s.t.Helper()
	c := s.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(s.t.Context(), req)
		if err != nil {
			s.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}

func (s *effectSite) wantApplied(n int) {
	s.t.Helper()
	eventually(s.t, "the provider applied the effects", func() bool { return len(s.applied()) == n })
}

func (s *effectSite) capabilityLine(plan planJSON) string {
	var line string
	_ = json.Unmarshal(plan.Args["capability"], &line)
	return line
}

// TestConversationUpdateLifecycle drives the e2e build on Postgres
// through a field update on a fake provider that ships GitHub's update
// effect, which its adapter authorizes as a work item act, and so is found
// by that declaration and not by name: a preview names the external
// change, the fields it sends, that it is queued and not applied, and what
// it checked of the adapter, and writes nothing; the update is queued
// durably under its key and replays, is applied by the provider with
// exactly the fields named, and the work item stays as it was until the
// provider's own report arrives through ingress; a null or an empty list
// clears a field and a field left out is never sent; both flag positions
// work. The status of a ticket goes through the update effect of an
// adapter that ships Zendesk's. A key with no value, a key twice, fields
// the adapter refuses, an adapter that declares no extension_execute, an
// unknown conversation and a viewer are all refused before anything is
// written, with the exit codes the grammar gives.
func TestConversationUpdateLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		s.report(s.up, upRepo, "ev-open", "7", adapterstest.InjectIssue{Labels: []string{"old"}})
		s.report(s.plain, plainIssues, "ev-open-plain", "9", adapterstest.InjectIssue{})
		s.report(s.tk, tkRepo, "ev-open-tk", "11", adapterstest.InjectIssue{})
		item := s.itemIn("upin")
		id := string(item.Ref.ID)
		convID := string(item.Ref.Conversation.ID)
		var conv0 model.Conversation
		s.json(&conv0, "conversation", "get", convID)

		// The preview.
		update := []string{"conversation", "update", convID, "--field", `labels=["bug","ui"]`, "--field", `assignees=["ada"]`, "--field", "milestone=3"}
		var plan planJSON
		s.json(&plan, append(update, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "work_item:"+id {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "github.issues.update", `labels=["bug","ui"]`, `assignees=["ada"]`, "milestone=3", "queued, not applied", "stays " + conv0.Status} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		for k, want := range map[string]string{
			"visibility": `"internal"`, "adapter": `"` + string(adapterstest.UpdatesType) + `"`, "work_item": `"` + id + `"`, "conversation": `"` + convID + `"`,
		} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		for _, want := range []string{"extension_execute declared by " + string(adapterstest.UpdatesType), "ships github.issues.update",
			"live availability not checked", "heo channel check " + string(s.up.Ref.ID) + " --remote"} {
			if line := s.capabilityLine(plan); !strings.Contains(line, want) {
				t.Errorf("capability line %q lacks %q", line, want)
			}
		}
		r := s.heo(0, append(update, "--dry-run")...)
		for _, want := range []string{"capability", "labels", "milestone", "queued, not applied"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, r.stdout)
			}
		}
		start := s.ledger()
		s.heo(0, append(update, "--dry-run")...)
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run left %d operations, was %d; provider saw %v", n, start, s.applied())
		}

		// Usage: before the daemon is asked.
		wantStderr(t, s.heo(2, "conversation", "update", convID), "USAGE", "field")
		wantStderr(t, s.heo(2, "conversation", "update", convID, "--field", "novalue"), "USAGE", "key=value")
		wantStderr(t, s.heo(2, "conversation", "update", convID, "--field", "milestone=1", "--field", "milestone=2"), "USAGE", "given twice")
		// What the adapter refuses offline: a validation error, at the
		// preview and, from uma, on submission.
		for name, f := range map[string][]string{
			"duplicate labels":    {"--field", `labels=["bug","Bug"]`},
			"duplicate assignees": {"--field", `assignees=["ada","ADA"]`},
			"bad login":           {"--field", `assignees=["not a login"]`},
			"labels not a list":   {"--field", "labels=bug"},
			"no state":            {"--field", "state=closed"},
		} {
			args := append([]string{"conversation", "update", convID}, f...)
			wantStderr(t, s.heo(2, append(args, "--dry-run")...), "VALIDATION", "action.data.")
			wantStderr(t, s.heo(2, append(args, "--idempotency-key", "bad-"+strings.ReplaceAll(name, " ", "-"))...), "VALIDATION")
		}
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("a usage or validation error wrote: operations %d, was %d; provider %v", n, start, s.applied())
		}

		// The update: queued durably, never applied by the command.
		var out, again updateJSON
		s.json(&out, append(update, "--idempotency-key", "upd-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "upd-1" || out.WorkItem.Ref != item.Ref {
			t.Fatalf("update = %+v", out)
		}
		if got := compactJSON(t, fieldsJSON(t, out.Fields)); got != `{"assignees":["ada"],"labels":["bug","ui"],"milestone":3}` {
			t.Fatalf("fields = %s", got)
		}
		if out.WorkItem.Revision != item.Revision || len(out.WorkItem.Fields) != len(item.Fields) {
			t.Fatalf("the output claims the change: work item %+v", out.WorkItem)
		}
		s.json(&again, append(update, "--idempotency-key", "upd-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed update = %+v, first %+v", again, out)
		}
		d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("update failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); lines[0] != `effect github.issues.update `+upRepo+` 7 {"assignees":["ada"],"labels":["bug","ui"],"milestone":3}` {
			t.Fatalf("provider was asked %q", lines)
		}
		if byKey := s.operation("--key", "upd-1"); byKey.ID != out.Operation.ID || byKey.Kind != model.ActionWorkItemAct {
			t.Errorf("operation get --key upd-1 = %+v, want %s %s", byKey, out.Operation.ID, model.ActionWorkItemAct)
		}
		v := s.workItem(convID)
		if v.WorkItem.Revision != item.Revision || v.Conversation.Status != conv0.Status || v.Conversation.Revision != conv0.Revision {
			t.Fatalf("the update moved the work item or its conversation before the provider reported: %+v", v)
		}

		// The text form says queued and what was asked, never that the
		// work item has them.
		txt := s.heo(0, "conversation", "update", convID, "--field", `labels=["bug"]`, "--idempotency-key", "upd-text")
		for _, want := range []string{`labels asked ["bug"]`, "queued"} {
			if !strings.Contains(strings.Join(strings.Fields(txt.stdout), " "), strings.Join(strings.Fields(want), " ")) {
				t.Errorf("text update lacks %q:\n%s", want, txt.stdout)
			}
		}
		if !strings.Contains(txt.stderr, "queued: accepted durably") || !strings.Contains(txt.stderr, "provider reports") || !strings.Contains(txt.stderr, "heo conversation get "+convID) {
			t.Errorf("text update notes:\n%s", txt.stderr)
		}
		s.wantApplied(2)

		// Both flag positions, and null or an empty list that clears a field;
		// a field left out is never sent.
		var cleared updateJSON
		r = s.s.run(nil, "--format", "json", "conversation", "update", "--field", "labels=[]", "--field", "milestone=null", "--idempotency-key", "upd-clear", convID)
		s.s.want(r, 0, "conversation", "update")
		if err := json.Unmarshal([]byte(r.stdout), &cleared); err != nil || compactJSON(t, fieldsJSON(t, cleared.Fields)) != `{"labels":[],"milestone":null}` {
			t.Fatalf("clearing = %s, %v", r.stdout, err)
		}
		s.wantApplied(3)
		s.heo(0, "conversation", "update", convID, "--field", "assignees=[]", "--idempotency-key", "upd-noassign")
		s.wantApplied(4)
		lines := s.applied()
		if lines[2] != `effect github.issues.update `+upRepo+` 7 {"labels":[],"milestone":null}` ||
			lines[3] != `effect github.issues.update `+upRepo+` 7 {"assignees":[]}` {
			t.Errorf("provider was asked %q", lines)
		}

		// The provider reports the change: now the work item shows it, and
		// its conversation is as it was.
		s.report(s.up, upRepo, "ev-labeled", "7", adapterstest.InjectIssue{Labels: []string{"bug", "ui"}, Milestone: &adapterstest.InjectMilestone{Number: 3, Title: "v1"}})
		r = s.heo(0, "conversation", "get", convID)
		for _, want := range []string{`work item field github.issues.labels ["bug","ui"]`, `work item field github.issues.milestone {"number":3,"title":"v1"}`} {
			if !tableHas(r.stdout, want) {
				t.Errorf("conversation get lacks %q after the provider reported:\n%s", want, r.stdout)
			}
		}
		if v := s.workItem(convID); v.Conversation.Status != conv0.Status {
			t.Errorf("conversation status = %s after the report", v.Conversation.Status)
		}

		// A ticket adapter's update effect is found the same way: the
		// status goes to the provider as the field it is there.
		tkConv := string(s.itemIn("tkissues").Ref.Conversation.ID)
		var tk updateJSON
		s.json(&tk, "conversation", "update", tkConv, "--field", "status=solved", "--idempotency-key", "tk-1")
		if tk.Operation.State != model.Queued || string(tk.Fields["status"]) != `"solved"` {
			t.Fatalf("ticket update = %+v", tk)
		}
		s.wantApplied(5)
		if lines := s.applied(); lines[4] != `effect zendesk.tickets.update `+tkRepo+` 11 {"status":"solved"}` {
			t.Errorf("provider was asked %q", lines)
		}

		// Refusals before anything is written: an adapter that declares no
		// extension_execute, an unknown conversation; at the preview and on
		// submission.
		before := s.ledger()
		plainConv := string(s.itemIn("plissues").Ref.Conversation.ID)
		wantStderr(t, s.heo(2, "conversation", "update", plainConv, "--field", `labels=["bug"]`, "--dry-run"),
			"UNSUPPORTED", "ingest-only", "heo channel get "+string(s.plain.Ref.ID))
		wantStderr(t, s.heo(2, "conversation", "update", plainConv, "--field", `labels=["bug"]`, "--idempotency-key", "plain-1"), "UNSUPPORTED", "ingest-only")
		wantStderr(t, s.heo(3, "conversation", "update", "00000ad0-0000-7000-8000-0000000009ff", "--field", "a=b"), "NOT_FOUND")
		var pv verbTableJSON
		s.json(&pv, "conversation", "get", plainConv)
		if got := pv.verbState("conversation update"); got != "unsupported" {
			t.Errorf("the plain channel's conversation update = %q, want unsupported", got)
		}
		s.json(&pv, "conversation", "get", convID)
		if got := pv.verbState("conversation update"); got != "available" {
			t.Errorf("the updates channel's conversation update = %q, want available", got)
		}
		if n := s.ledger(); n != before || len(s.applied()) != 5 {
			t.Errorf("a refused update wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		// A viewer may not update, preview included.
		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"conversation", "update", convID, "--field", `labels=["bug"]`}, extra...)...), "heo.authz.not_granted")
		}
		if len(s.applied()) != 5 {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}

func fieldsJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestConversationTagLifecycle drives the e2e build on Postgres through
// a tag change on fake providers that ship Gmail's label effect and
// Outlook's category effect: a preview names the provider message the
// change lands on, the effect and what was asked, and writes nothing;
// --add and --remove queue Gmail's labels and --set Outlook's
// categories, durably under their key, replayed, applied with exactly
// what was named, and the conversation uma holds is untouched (it
// records no provider tags, and it is not a `conversation label`); an
// empty --set clears. Each adapter is refused the flags of the other,
// saying what it takes; an adapter that declares no extension_execute,
// one that declares it for another effect, a conversation the peer has
// written no message in, an unknown conversation and a viewer are
// refused before anything is written, with the exit codes the grammar
// gives.
func TestConversationTagLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		s := newEffectSite(t, noRateLimit)
		gm := s.mail(s.lb, "lbin", "lb-1", "ev-gm", "gmail-msg-1")
		ou := s.mail(s.cat, "catin", "cat-1", "ev-ou", "outlook-msg-1")
		gid, oid := string(gm.Ref.ID), string(ou.Ref.ID)

		// The preview.
		tag := []string{"conversation", "tag", gid, "--add", "Label_12,STARRED", "--remove", "INBOX"}
		var plan planJSON
		s.json(&plan, append(tag, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "mail:message gmail-msg-1" {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "google.mail.labels", "add Label_12, STARRED", "remove INBOX", "uma records none"} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		for k, want := range map[string]string{
			"message": `"gmail-msg-1"`, "visibility": `"internal"`, "adapter": `"` + string(adapterstest.LabelsType) + `"`,
			"change": `"google.mail.labels {\"add\":[\"Label_12\",\"STARRED\"],\"remove\":[\"INBOX\"]}"`,
		} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		if line := s.capabilityLine(plan); !strings.Contains(line, "extension_execute declared by "+string(adapterstest.LabelsType)) ||
			!strings.Contains(line, "ships google.mail.labels") || !strings.Contains(line, "live availability not checked") {
			t.Errorf("capability line = %q", line)
		}
		start := s.ledger()
		s.heo(0, append(tag, "--dry-run")...)
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("--dry-run left %d operations, was %d; provider saw %v", n, start, s.applied())
		}

		// Usage: before the daemon is asked.
		wantStderr(t, s.heo(2, "conversation", "tag", gid), "at least one of")
		wantStderr(t, s.heo(2, "conversation", "tag", gid, "--set", "a", "--add", "b"), "none of the others can be")
		wantStderr(t, s.heo(2, "conversation", "tag", gid, "--set", "a", "--remove", "b"), "none of the others can be")
		// What the adapters refuse offline: a validation error at the
		// preview and, from uma, on submission.
		for name, args := range map[string][]string{
			"empty id":    {"conversation", "tag", gid, "--add", ","},
			"id twice":    {"conversation", "tag", gid, "--add", "Label_1", "--remove", "Label_1"},
			"category x2": {"conversation", "tag", oid, "--set", "Billing,Billing"},
		} {
			wantStderr(t, s.heo(2, append(args, "--dry-run")...), "VALIDATION", "action.data")
			wantStderr(t, s.heo(2, append(args, "--idempotency-key", "bad-"+strings.ReplaceAll(name, " ", "-"))...), "VALIDATION")
		}
		if n := s.ledger(); n != start || len(s.applied()) != 0 {
			t.Fatalf("a usage or validation error wrote: operations %d, was %d; provider %v", n, start, s.applied())
		}

		// Gmail's labels: queued durably, applied, replayed under the key.
		var out, again conversationTagJSON
		s.json(&out, append(tag, "--idempotency-key", "tag-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "tag-1" ||
			out.Effect != "google.mail.labels" || out.Message != "gmail-msg-1" || out.Conversation.Ref != gm.Ref {
			t.Fatalf("tag = %+v", out)
		}
		if strings.Join(out.Add, ",") != "Label_12,STARRED" || strings.Join(out.Remove, ",") != "INBOX" || out.Set != nil {
			t.Fatalf("what was asked = add %v remove %v set %v", out.Add, out.Remove, out.Set)
		}
		s.json(&again, append(tag, "--idempotency-key", "tag-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed tag = %+v, first %+v", again, out)
		}
		d := s.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("tag failed: %+v", d.Failure)
		}
		s.wantApplied(1)
		if lines := s.applied(); lines[0] != `effect google.mail.labels gmail-msg-1 `+peerMail+` {"add":["Label_12","STARRED"],"remove":["INBOX"]}` {
			t.Fatalf("provider was asked %q", lines)
		}
		if byKey := s.operation("--key", "tag-1"); byKey.ID != out.Operation.ID || byKey.Kind != model.ActionConversationLabel {
			t.Errorf("operation get --key tag-1 = %+v, want %s %s", byKey, out.Operation.ID, model.ActionConversationLabel)
		}
		var got model.Conversation
		s.json(&got, "conversation", "get", gid)
		if got.Revision != gm.Revision || len(got.Labels) != 0 {
			t.Errorf("the tag changed the conversation uma holds: %+v", got)
		}
		// Add alone and remove alone, flags before the verb's arguments.
		r := s.s.run(nil, "--format", "json", "conversation", "tag", "--remove", "UNREAD", "--idempotency-key", "tag-rm", gid)
		s.s.want(r, 0, "conversation", "tag")
		s.wantApplied(2)
		s.heo(0, "conversation", "tag", gid, "--add=Label_9", "--idempotency-key", "tag-add")
		s.wantApplied(3)
		if lines := s.applied(); lines[1] != `effect google.mail.labels gmail-msg-1 `+peerMail+` {"remove":["UNREAD"]}` ||
			lines[2] != `effect google.mail.labels gmail-msg-1 `+peerMail+` {"add":["Label_9"]}` {
			t.Errorf("provider was asked %q", lines)
		}

		// Outlook's categories: --set replaces the whole list; empty clears.
		var cat conversationTagJSON
		s.json(&cat, "conversation", "tag", oid, "--set", "Billing,Urgent", "--idempotency-key", "cat-1")
		if cat.Effect != "microsoft.mail.categories" || cat.Message != "outlook-msg-1" || cat.Set == nil || strings.Join(*cat.Set, ",") != "Billing,Urgent" {
			t.Fatalf("categories = %+v", cat)
		}
		s.heo(0, "conversation", "tag", oid, "--set=", "--idempotency-key", "cat-clear")
		s.wantApplied(5)
		if lines := s.applied(); lines[3] != `effect microsoft.mail.categories outlook-msg-1 `+peerMail+` {"set":["Billing","Urgent"]}` ||
			lines[4] != `effect microsoft.mail.categories outlook-msg-1 `+peerMail+` {"set":[]}` {
			t.Errorf("provider was asked %q", lines)
		}
		txt := s.heo(0, "conversation", "tag", oid, "--set", "Billing", "--idempotency-key", "cat-text")
		for _, want := range []string{"set asked  Billing", "effect  microsoft.mail.categories", "queued"} {
			if !strings.Contains(strings.Join(strings.Fields(txt.stdout), " "), strings.Join(strings.Fields(want), " ")) {
				t.Errorf("text tag lacks %q:\n%s", want, txt.stdout)
			}
		}
		if !strings.Contains(txt.stderr, "queued: accepted durably") || !strings.Contains(txt.stderr, "uma records no provider tags") {
			t.Errorf("text tag notes:\n%s", txt.stderr)
		}
		s.wantApplied(6)

		// Refusals before anything is written.
		before := s.ledger()
		wantStderr(t, s.heo(2, "conversation", "tag", oid, "--add", "Label_1", "--dry-run"),
			"UNSUPPORTED", "ships no effect google.mail.labels", "--set")
		wantStderr(t, s.heo(2, "conversation", "tag", oid, "--add", "Label_1", "--idempotency-key", "x-1"), "UNSUPPORTED", "--set")
		wantStderr(t, s.heo(2, "conversation", "tag", gid, "--set", "Billing", "--dry-run"),
			"UNSUPPORTED", "ships no effect microsoft.mail.categories", "--add and --remove")
		wantStderr(t, s.heo(2, "conversation", "tag", gid, "--set", "Billing", "--idempotency-key", "x-2"), "UNSUPPORTED", "--add and --remove")
		pl := s.mail(s.plain, "plmail", "pl-1", "ev-pl", "plain-msg-1")
		wantStderr(t, s.heo(2, "conversation", "tag", string(pl.Ref.ID), "--add", "Label_1", "--dry-run"),
			"UNSUPPORTED", "does not declare extension_execute", "heo channel get "+string(s.plain.Ref.ID))
		wantStderr(t, s.heo(2, "conversation", "tag", string(pl.Ref.ID), "--set", "a", "--idempotency-key", "x-3"), "UNSUPPORTED", "does not declare extension_execute")
		tkc := s.mail(s.tk, "tkmail", "tk-1", "ev-tk", "tk-msg-1")
		for _, flags := range [][]string{{"--add", "Label_1"}, {"--set", "a"}} {
			wantStderr(t, s.heo(2, append([]string{"conversation", "tag", string(tkc.Ref.ID)}, append(flags, "--dry-run")...)...),
				"UNSUPPORTED", "declares extension_execute and ships no effect")
			wantStderr(t, s.heo(2, append([]string{"conversation", "tag", string(tkc.Ref.ID)}, append(flags, "--idempotency-key", "x-4")...)...),
				"UNSUPPORTED", "declares extension_execute and ships no effect")
		}
		// No message of the peer's to tag.
		s.inject(s.lb, adapterstest.Inject{Event: "ev-gm-2", Message: "ev-gm-2", From: "other@example.com", Destination: "lb-1", Text: "hello"})
		var all pageJSON[model.Conversation]
		s.json(&all, "conversation", "list", "--inbox", "lbin")
		var bare model.Conversation
		for _, c := range all.Items {
			if c.Ref.ID != gm.Ref.ID {
				bare = c
			}
		}
		if bare.Ref.ID == "" {
			t.Fatalf("no second conversation among %+v", all.Items)
		}
		wantStderr(t, s.heo(4, "conversation", "tag", string(bare.Ref.ID), "--add", "Label_1", "--dry-run"), "CONFLICT", "names no provider message")
		wantStderr(t, s.heo(4, "conversation", "tag", string(bare.Ref.ID), "--add", "Label_1", "--idempotency-key", "x-5"), "CONFLICT", "names no provider message")
		wantStderr(t, s.heo(3, "conversation", "tag", "00000ad0-0000-7000-8000-0000000009ff", "--add", "Label_1"), "NOT_FOUND")
		if n := s.ledger(); n != before || len(s.applied()) != 6 {
			t.Errorf("a refused tag wrote: operations %d, was %d; provider %v", n, before, s.applied())
		}

		// `conversation label` is uma's own and local: it neither needs
		// nor reaches the provider, on any adapter.
		s.heo(0, "label", "create", "billing")
		s.heo(0, "conversation", "label", string(pl.Ref.ID), "--add", "billing")
		if len(s.applied()) != 6 {
			t.Errorf("a local label reached the provider: %v", s.applied())
		}

		// A viewer may not tag, preview included.
		s.stop(s.d)
		s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		s.d = s.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, s.s.run(nil, append([]string{"conversation", "tag", gid, "--add", "Label_1"}, extra...)...), "heo.authz.not_granted")
		}
		if len(s.applied()) != 6 {
			t.Errorf("provider saw %v", s.applied())
		}
		s.stop(s.d)
	})
}
