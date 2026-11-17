package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// actsSite is a daemon with two fake channels on Postgres, each with a
// per-work-item inbox holding one open issue reported through ingress:
// "ac", whose adapter declares work_item_act with an issue tracker's
// open and closed acts and applies them, and "plain", whose adapter
// declares none.
type actsSite struct {
	*cliSite
	d     *daemon
	ac    model.Channel
	plain model.Channel
	acts  string
}

const (
	acRepo    = "example/acts"
	plainRepo = "example/plain"
)

func newActsSite(t *testing.T, profile string) *actsSite {
	t.Helper()
	// Rate limits off: the lifecycle sends writes back to back.
	c := newCLISite(t, noBridge+"  all:\n    rate_limit:\n      enabled: false\n")
	a := &actsSite{cliSite: c, acts: c.s.path("effects.log")}
	c.s.env["HEO_E2E_FAKE_EFFECT_LOG"] = a.acts
	if profile != "" {
		c.s.write("cfg/heo/profiles/default.yaml", profile)
	}
	a.d = c.start()
	a.ac = c.fakeOf(adapterstest.ActsType, "ac", adapterstest.Config{Account: "acct-ac"})
	a.plain = c.fakeOf(adapterstest.Type, "plain", adapterstest.Config{Account: "acct-plain"})
	c.heo(0, "inbox", "create", "--channel", "ac", "--key", "acin", "--destination", acRepo, "--mode", model.ModePerWorkItem)
	c.heo(0, "inbox", "create", "--channel", "plain", "--key", "plin", "--destination", plainRepo, "--mode", model.ModePerWorkItem)
	return a
}

// report has the provider report issue number of repo in status, as its
// webhook would.
func (a *actsSite) report(ch model.Channel, repo, event, number, status, raw string) {
	a.t.Helper()
	a.inject(ch, adapterstest.Inject{
		Event: event, Kind: model.EnvelopeWorkItem, Destination: repo,
		Issue: &adapterstest.InjectIssue{Number: number, Subject: "Crash on start", Status: status, RawStatus: raw},
	})
}

// itemIn is the work item reported for inbox, read through its
// conversation.
func (a *actsSite) itemIn(inbox string) model.WorkItem {
	a.t.Helper()
	var p pageJSON[conversationItemJSON]
	a.json(&p, "conversation", "list", "--inbox", inbox, "--work-item")
	if len(p.Items) != 1 || p.Items[0].WorkItem == nil {
		a.t.Fatalf("work items in %s = %+v", inbox, p.Items)
	}
	return *p.Items[0].WorkItem
}

// applied is what the fake provider was asked, one line per act.
func (a *actsSite) applied() []string {
	b, _ := os.ReadFile(a.acts)
	s := strings.TrimSpace(string(b))
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// ledger counts the operations uma holds: what a refused or previewed
// verb must leave as it was.
func (a *actsSite) ledger() int {
	a.t.Helper()
	c := a.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(a.t.Context(), req)
		if err != nil {
			a.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}

// TestConversationStatusLifecycle drives the e2e build on Postgres
// through close and reopen on a fake provider that declares work_item_act:
// a preview names the external change, the reason and that it is queued
// and not applied, and writes nothing; the act is queued durably with the
// revision always sent, applied by the provider, and the work item stays
// as it was until the provider's own report arrives through ingress; the
// output never claims the change; the conversation is untouched
// throughout. A status or a reason the adapter declares no transition
// for, a status the work item is already in, a stale revision and an
// adapter that does not declare the act are refused before anything is
// written; the act is in the caller's history; a viewer is refused.
func TestConversationStatusLifecycle(t *testing.T) {
	withE2EBinary(t, func() {
		a := newActsSite(t, "")
		a.report(a.ac, acRepo, "ev-open", "7", model.WorkItemOpen, "open")
		a.report(a.plain, plainRepo, "ev-open-plain", "9", model.WorkItemOpen, "open")
		item := a.itemIn("acin")
		id, rev := string(item.Ref.ID), string(item.Revision)
		convID := string(item.Ref.Conversation.ID)
		var conv0 model.Conversation
		a.json(&conv0, "conversation", "get", convID)

		// The preview.
		closeIt := []string{"conversation", "status", convID, "--set", "closed", "--reason", "not_planned"}
		var plan planJSON
		a.json(&plan, append(closeIt, "--dry-run")...)
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "work_item:"+id {
			t.Fatalf("preview effects = %+v", plan.Effects)
		}
		for _, want := range []string{"external", "open (open) -> closed", "not_planned", "queued, not applied", "reports it", "stays " + conv0.Status} {
			if !strings.Contains(plan.Effects[0].Detail, want) {
				t.Errorf("effect detail lacks %q: %s", want, plan.Effects[0].Detail)
			}
		}
		for k, want := range map[string]string{
			"status": `"closed"`, "reason": `"not_planned"`, "from": `"open"`, "expected_revision": `"` + rev + `"`,
			"adapter": `"` + string(adapterstest.ActsType) + `"`, "work_item": `"` + id + `"`, "conversation": `"` + convID + `"`,
		} {
			if string(plan.Args[k]) != want {
				t.Errorf("preview arg %s = %s, want %s", k, plan.Args[k], want)
			}
		}
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		for _, want := range []string{"work_item_act declared by " + string(adapterstest.ActsType), "live availability not checked", "heo channel check " + string(a.ac.Ref.ID) + " --remote"} {
			if !strings.Contains(capLine, want) {
				t.Errorf("capability line %q lacks %q", capLine, want)
			}
		}
		r := a.heo(0, append(closeIt, "--dry-run")...)
		for _, want := range []string{"capability", "closed", "not_planned", "expected revision", "queued, not applied"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, r.stdout)
			}
		}
		start := a.ledger()
		a.heo(0, append(closeIt, "--dry-run")...)
		if n := a.ledger(); n != start || len(a.applied()) != 0 {
			t.Fatalf("--dry-run left %d operations, was %d; provider saw %v", n, start, a.applied())
		}

		// Usage errors come before the daemon is asked; what the adapter
		// declares comes after.
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "solved"), "USAGE", `status "solved"`)
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "Closed"), "USAGE", `status "Closed"`)
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "closed", "--dry-run", "--reason", "Completed"), "snake_case")
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "closed", "--reason", "duplicate", "--dry-run"), "UNSUPPORTED", `status reason "duplicate"`)
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "open", "--reason", "completed", "--dry-run"), "UNSUPPORTED")
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "pending", "--dry-run"), "UNSUPPORTED", "declares no work item transition")
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "resolved", "--idempotency-key", "res-1"), "UNSUPPORTED")
		if a.ledger() != start {
			t.Fatal("a usage error or a refusal queued an operation")
		}

		// The close: queued durably with the revision, never applied.
		var out, again statusJSON
		a.json(&out, append(closeIt, "--revision", rev, "--idempotency-key", "close-1")...)
		if out.Operation.State != model.Queued || !out.Operation.Durable || out.Operation.Replayed || out.OperationKey != "close-1" ||
			out.Status != "closed" || out.Reason != "not_planned" || out.WorkItem.Ref != item.Ref {
			t.Fatalf("close = %+v", out)
		}
		if out.WorkItem.Status != model.WorkItemOpen || out.WorkItem.RawStatus != "open" {
			t.Fatalf("the output claims the change: work item %s/%s", out.WorkItem.Status, out.WorkItem.RawStatus)
		}
		a.json(&again, append(closeIt, "--revision", rev, "--idempotency-key", "close-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replayed close = %+v, first %+v", again, out)
		}
		d := a.waitDelivery(out.Operation.ID, model.ProviderAccepted, model.Delivered)
		if d.State == model.Failed {
			t.Fatalf("close failed: %+v", d.Failure)
		}
		if lines := a.applied(); len(lines) != 1 || lines[0] != "act "+acRepo+"#7 closed not_planned" {
			t.Fatalf("provider was asked %q, want one close with its reason", lines)
		}
		// The provider's answer carried no state: the work item is as it
		// was until the provider reports.
		if v := a.workItem(convID); v.WorkItem.Status != model.WorkItemOpen || v.WorkItem.Revision != item.Revision {
			t.Fatalf("work item after the act was applied, before the provider reported = %+v", v.WorkItem)
		}
		// The key finds the operation.
		if byKey := a.operation("--key", "close-1"); byKey.ID != out.Operation.ID || byKey.Kind != model.ActionWorkItemAct {
			t.Errorf("operation get --key close-1 = %+v, want %s %s", byKey, out.Operation.ID, model.ActionWorkItemAct)
		}

		// The text form says queued and what was asked, never closed.
		txt := a.heo(0, "conversation", "status", convID, "--set", "closed", "--revision", rev, "--idempotency-key", "close-text")
		status := ""
		for _, line := range strings.Split(txt.stdout, "\n") {
			if strings.HasPrefix(line, "status") {
				status = line
			}
		}
		if !strings.HasPrefix(strings.Join(strings.Fields(status), " "), "status open (asked: closed; not applied until the provider reports it)") ||
			!strings.Contains(txt.stdout, "queued") || !strings.Contains(txt.stderr, "queued: accepted durably") ||
			!strings.Contains(txt.stderr, "provider reports") || !strings.Contains(txt.stderr, "heo conversation get "+convID) {
			t.Errorf("text close:\nstdout: %s\nstderr: %s", txt.stdout, txt.stderr)
		}
		eventuallyLines(t, a, 2)

		// The provider reports the close: now the work item is closed, and
		// its conversation is as it was.
		a.report(a.ac, acRepo, "ev-closed", "7", model.WorkItemClosed, "closed:not_planned")
		v := a.workItem(convID)
		if v.WorkItem.Status != model.WorkItemClosed || v.WorkItem.RawStatus != "closed:not_planned" {
			t.Fatalf("work item after the provider reported = %+v", v.WorkItem)
		}
		if v.Conversation.Status != conv0.Status || v.Conversation.Revision != conv0.Revision {
			t.Fatalf("closing the work item moved its conversation: %+v, was %+v", v.Conversation, conv0)
		}

		// A replay under the key and revision after the report is still the
		// same operation.
		a.json(&again, append(closeIt, "--revision", rev, "--idempotency-key", "close-1")...)
		if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
			t.Fatalf("replay after the report = %+v", again)
		}

		// Closed already: refused, nothing written. A stale revision: a
		// conflict naming the current one.
		before := a.ledger()
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "closed", "--dry-run"), "UNSUPPORTED", "already closed")
		wantStderr(t, a.heo(2, "conversation", "status", convID, "--set", "closed"), "UNSUPPORTED", "already closed")
		wantStderr(t, a.heo(4, "conversation", "status", convID, "--set", "open", "--revision", rev),
			"CONFLICT", "work item "+id+" is at revision "+string(v.WorkItem.Revision), "not "+rev)
		wantStderr(t, a.heo(4, "conversation", "status", convID, "--set", "open", "--revision", rev, "--dry-run"), "CONFLICT", "is at revision")
		if n := a.ledger(); n != before || len(a.applied()) != 2 {
			t.Errorf("a refused change wrote: operations %d, was %d; provider %v", n, before, a.applied())
		}

		// Reopening: no reason is sent, and the adapter chooses its own.
		var plan2 planJSON
		a.json(&plan2, "conversation", "status", convID, "--set", "open", "--dry-run")
		if _, has := plan2.Args["reason"]; has || !strings.Contains(plan2.Effects[0].Detail, "closed:not_planned) -> open") ||
			!strings.Contains(plan2.Effects[0].Detail, "the adapter's default") {
			t.Errorf("reopen preview: args %v, effect %+v", plan2.Args, plan2.Effects)
		}
		var re statusJSON
		a.json(&re, "conversation", "status", convID, "--set", "open", "--revision", string(v.WorkItem.Revision), "--idempotency-key", "reopen-1")
		if re.Status != "open" || re.Reason != "" || re.Operation.State != model.Queued || re.WorkItem.Status != model.WorkItemClosed {
			t.Fatalf("reopen = %+v", re)
		}
		a.waitDelivery(re.Operation.ID, model.ProviderAccepted, model.Delivered)
		eventuallyLines(t, a, 3)
		if lines := a.applied(); lines[2] != "act "+acRepo+"#7 open" {
			t.Errorf("provider was asked %q, want a reopen without a reason", lines[2])
		}
		a.report(a.ac, acRepo, "ev-reopened", "7", model.WorkItemOpen, "open:reopened")
		if v := a.workItem(convID); v.WorkItem.Status != model.WorkItemOpen || v.Conversation.Status != conv0.Status {
			t.Fatalf("work item after the reopen was reported = %s, conversation %s", v.WorkItem.Status, v.Conversation.Status)
		}

		// The conversation's own lifecycle is independent: resolving it
		// leaves the work item as the provider reported it.
		var cur conversationItemJSON
		a.json(&cur, "conversation", "get", convID)
		a.heo(0, "conversation", "resolve", convID, "--revision", string(cur.Revision))
		if v := a.workItem(convID); v.Conversation.Status != model.ConversationResolved || v.WorkItem.Status != model.WorkItemOpen {
			t.Fatalf("after resolve: conversation %s, work item %s", v.Conversation.Status, v.WorkItem.Status)
		}

		// An adapter that does not declare the act: refused at the preview
		// and on submission, nothing written, and the channel's verb table
		// says so.
		other := a.itemIn("plin")
		oconv := string(other.Ref.Conversation.ID)
		before = a.ledger()
		wantStderr(t, a.heo(2, "conversation", "status", oconv, "--set", "closed", "--dry-run"),
			"UNSUPPORTED", "does not declare work_item_act", "heo channel get "+string(a.plain.Ref.ID))
		wantStderr(t, a.heo(2, "conversation", "status", oconv, "--set", "closed", "--idempotency-key", "plain-1"), "UNSUPPORTED")
		if n := a.ledger(); n != before || len(a.applied()) != 3 {
			t.Errorf("an undeclared act wrote: operations %d, was %d; provider %v", n, before, a.applied())
		}
		var pv verbTableJSON
		a.json(&pv, "conversation", "get", oconv)
		if got := pv.verbState("conversation status"); got != "unsupported" {
			t.Errorf("the plain channel's conversation status = %q, want unsupported", got)
		}
		a.json(&pv, "conversation", "get", convID)
		if got := pv.verbState("conversation status"); got != "available" {
			t.Errorf("the acts channel's conversation status = %q, want available", got)
		}
		// An unknown conversation is not found.
		wantStderr(t, a.heo(3, "conversation", "status", "00000ad0-0000-7000-8000-0000000009ff", "--set", "closed"), "NOT_FOUND")

		// A viewer may not change a status, preview included.
		a.stop(a.d)
		a.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
		a.d = a.start()
		for _, extra := range [][]string{nil, {"--dry-run"}} {
			wantDenied(t, a.s.run(nil, append([]string{"conversation", "status", convID, "--set", "closed"}, extra...)...), "heo.authz.not_granted")
		}
		if len(a.applied()) != 3 {
			t.Errorf("provider saw %v", a.applied())
		}
		a.stop(a.d)
	})
}

// verbTableJSON is `conversation get` as far as the verb table.
type verbTableJSON struct {
	Verbs []struct{ Verb, State string }
}

func (c verbTableJSON) verbState(verb string) string {
	for _, v := range c.Verbs {
		if v.Verb == verb {
			return v.State
		}
	}
	return ""
}

func eventuallyLines(t *testing.T, a *actsSite, n int) {
	t.Helper()
	eventually(t, "the provider applied the acts", func() bool { return len(a.applied()) == n })
}

// TestConversationStatusGitHubAdapter runs the status change against the
// release binary's github.issues adapter, which declares work_item_act
// with GitHub's close and reopen. Workers stay off, so nothing reaches
// GitHub: the preview passes on the adapter's real declarations, refuses a
// status and a reason GitHub declares no transition for, queues one act
// under the key and replays it, never moves the conversation, and leaves
// the work item as GitHub last reported it until GitHub's own webhook
// arrives.
func TestConversationStatusGitHubAdapter(t *testing.T) {
	s := newWorkItemSite(t)
	s.deliver("issues", "issues-opened", "d-opened")
	s.deliver("issue_comment", "issue_comment-created", "d-comment")
	var items pageJSON[conversationItemJSON]
	s.json(&items, "conversation", "list", "--work-item", "--inbox", "issues")
	issue := *items.Items[0].WorkItem
	id := string(issue.Ref.ID)
	convID := string(issue.Ref.Conversation.ID)
	var conv0 model.Conversation
	s.json(&conv0, "conversation", "get", convID)
	before := s.queued()

	closeIt := []string{"conversation", "status", convID, "--set", "closed", "--reason", "completed"}
	var plan planJSON
	s.json(&plan, append(closeIt, "--dry-run")...)
	var capLine string
	_ = json.Unmarshal(plan.Args["capability"], &capLine)
	if !strings.Contains(capLine, "work_item_act declared by "+string(github.Type)) || len(plan.Effects) != 1 ||
		plan.Effects[0].Target != "work_item:"+id || !strings.Contains(plan.Effects[0].Detail, "example/uma-sandbox#7") {
		t.Fatalf("preview = %+v, capability %q", plan, capLine)
	}
	if n := s.queued(); n != before {
		t.Fatalf("--dry-run queued %d provider work items", n-before)
	}

	var out, again statusJSON
	s.json(&out, append(closeIt, "--idempotency-key", "gh-close")...)
	if out.Operation.State != model.Queued || out.WorkItem.Status != model.WorkItemOpen || out.WorkItem.RawStatus != "open" {
		t.Fatalf("close = %+v", out)
	}
	s.json(&again, append(closeIt, "--revision", string(issue.Revision), "--idempotency-key", "gh-close")...)
	if !again.Operation.Replayed || again.Operation.ID != out.Operation.ID {
		t.Fatalf("replay = %+v, first %+v", again, out)
	}
	if n := s.queued(); n != before+1 {
		t.Fatalf("queued %d provider work items, want 1", n-before)
	}
	var conv model.Conversation
	s.json(&conv, "conversation", "get", convID)
	if conv.Status != conv0.Status || conv.Revision != conv0.Revision {
		t.Fatalf("the status change moved the conversation: %+v", conv)
	}
	// GitHub's webhook of the close: the work item is closed, the
	// conversation as it was, nothing more queued.
	s.deliver("issues", "issues-closed", "d-closed")
	v := s.workItem(convID)
	if v.WorkItem.Status != model.WorkItemClosed || v.Conversation.Status != conv0.Status {
		t.Fatalf("work item after GitHub reported = %s, conversation %s", v.WorkItem.Status, v.Conversation.Status)
	}
	if n := s.queued(); n != before+1 {
		t.Errorf("GitHub's report queued %d more provider work items", n-before-1)
	}
	// GitHub declares no transition to pending, and no such reason.
	wantStderr(t, s.heo(2, "conversation", "status", convID, "--set", "pending", "--dry-run"), "UNSUPPORTED", "declares no work item transition")
	wantStderr(t, s.heo(2, "conversation", "status", convID, "--set", "open", "--reason", "duplicate", "--dry-run"), "UNSUPPORTED", `status reason "duplicate"`)
	s.stop(s.d)
}
