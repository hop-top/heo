package desk_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

func convID(it model.WorkItem) string { return string(it.Ref.Conversation.ID) }

// TestWorkItemIsFoundThroughItsConversation: the work item of a
// per_work_item conversation is the one bound to it, for an issue and a
// ticket alike, and a conversation of another mode has none.
func TestWorkItemIsFoundThroughItsConversation(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	d := w.as(agent)
	got, err := d.WorkItemOf(t.Context(), issue.Ref.Conversation)
	if err != nil || got == nil || got.Ref != issue.Ref || got.External.ID != "7" || got.Subject == "" {
		t.Fatalf("issue work item = %+v, %v", got, err)
	}
	got, err = d.WorkItemOf(t.Context(), w.ticket)
	if err != nil || got == nil || got.Ref.ID != ticketItem || got.External != w.ticketRoute() {
		t.Fatalf("ticket work item = %+v, %v", got, err)
	}
	got, err = d.WorkItemOf(t.Context(), w.gmailThread(peerMessage))
	if err != nil || got != nil {
		t.Fatalf("a mail conversation has work item %+v, %v", got, err)
	}
	if it, err := d.FindWorkItem(t.Context(), string(ticketItem)); err != nil || it.Ref.ID != ticketItem {
		t.Errorf("FindWorkItem = %+v, %v", it, err)
	}
	if _, err := d.FindWorkItem(t.Context(), "00000ad0-0000-7000-8000-0000000009ff"); !model.IsCode(err, model.NotFound) {
		t.Errorf("unknown work item: %v", err)
	}
}

// TestStatusQueuesTheAdapterDeclaredAct: setting a work item's status is
// uma's work item act with the status and the reason, at the work item's
// revision; planning writes nothing; submitting queues one outbound
// operation and changes nothing in uma — not the work item, which changes
// when the provider reports it, and not the conversation, whose
// lifecycle is uma's own.
func TestStatusQueuesTheAdapterDeclaredAct(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	conv := w.conversation(issue.Ref.Conversation)
	before := w.queued()

	p, err := d.PlanStatus(ctx, convID(issue), "closed", "not_planned", "", "st-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	req := p.Request
	if req.Target != issue.Ref || req.Action.Name != model.SchemaWorkItemUpdate || req.Action.Version != 1 ||
		req.Operation != "st-1" || req.ExpectedRevision != issue.Revision {
		t.Fatalf("request = %+v, want the work item's act at revision %s", req, issue.Revision)
	}
	var data map[string]any
	if err := json.Unmarshal(req.Action.Data, &data); err != nil || len(data) != 2 || data["status"] != "closed" || data["status_reason"] != "not_planned" {
		t.Fatalf("action data = %s", req.Action.Data)
	}
	if p.Item.Ref != issue.Ref || p.Conversation.Ref != issue.Ref.Conversation || p.Local {
		t.Fatalf("plan = %+v", p)
	}
	if n := w.queued(); n != before {
		t.Fatalf("planning queued %d provider work items", n-before)
	}

	it, acc, err := d.SetStatus(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Operation.State != model.Queued || acc.Replayed || w.queued() != before+1 {
		t.Fatalf("acceptance = %+v, queued %d", acc, w.queued()-before)
	}
	if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionWorkItemAct || rec.Key.Target != issue.Ref.Ref() {
		t.Fatalf("operation key = %+v", rec.Key)
	}
	if it.Status != model.WorkItemOpen || it.RawStatus != issue.RawStatus {
		t.Errorf("work item after the act was queued = %s/%s, want open until the provider reports", it.RawStatus, it.Status)
	}
	if got := w.conversation(issue.Ref.Conversation); got.Status != conv.Status || got.Revision != conv.Revision {
		t.Errorf("conversation moved by a status change: %+v", got)
	}

	// The provider reports the close: the work item is closed, the
	// conversation as it was, and nothing more is queued. The same
	// request is then a replay, not a refusal.
	w.deliver("issues", "issues-closed", "d-closed")
	got, err := d.WorkItemOf(ctx, issue.Ref.Conversation)
	if err != nil || got.Status != model.WorkItemClosed {
		t.Fatalf("work item after the provider reported = %+v, %v", got, err)
	}
	if c := w.conversation(issue.Ref.Conversation); c.Status != conv.Status || w.queued() != before+1 {
		t.Errorf("the report moved the conversation to %s or queued %d", c.Status, w.queued()-before-1)
	}
	again, err := d.PlanStatus(ctx, convID(issue), "closed", "not_planned", issue.Revision, "st-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, rep, err := d.SetStatus(ctx, again); err != nil || !rep.Replayed || rep.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v, want %v replayed", rep, err, acc.Operation.Ref)
	}

	// Reopening is the transition back; no reason is sent.
	p, err = d.PlanStatus(ctx, convID(issue), "open", "", "", "st-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	data = nil
	if err := json.Unmarshal(p.Request.Action.Data, &data); err != nil || len(data) != 1 || data["status"] != "open" || p.Request.ExpectedRevision != got.Revision {
		t.Fatalf("reopen = %s at %s", p.Request.Action.Data, p.Request.ExpectedRevision)
	}
	if _, _, err := d.SetStatus(ctx, p); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

// TestStatusRefusedWhereTheAdapterDeclaresNoTransition: a status the
// adapter declares no transition to, a reason it does not declare, and a
// channel whose adapter declares no work_item_act are refused as
// unsupported — in a preview from the declarations, and on submission by
// uma — before anything is queued.
func TestStatusRefusedWhereTheAdapterDeclaresNoTransition(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	gl := w.seedGitLabIssue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()

	for _, tc := range []struct {
		name, conv, status, reason, want string
	}{
		{"github has no pending", convID(issue), "pending", "", "declares no work item transition"},
		{"github has no resolved", convID(issue), "resolved", "", "declares no work item transition"},
		{"github reason", convID(issue), "closed", "duplicate", `status reason "duplicate"`},
		{"zendesk declares no act", string(ticketConv), "resolved", "", "does not declare work_item_act"},
		{"gitlab declares no reasons", convID(gl), "closed", "completed", "status reason"},
	} {
		_, err := d.PlanStatus(ctx, tc.conv, tc.status, tc.reason, "", "st-x", channels.Preview)
		if !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "heo channel get") {
			t.Errorf("%s: preview %v, want unsupported containing %q and a pointer to channel get", tc.name, err, tc.want)
		}
		p, err := d.PlanStatus(ctx, tc.conv, tc.status, tc.reason, "", "st-x", channels.Submit)
		if err != nil {
			t.Errorf("%s: submit plan: %v", tc.name, err)
			continue
		}
		if _, _, err := d.SetStatus(ctx, p); !model.IsCode(err, model.Unsupported) {
			t.Errorf("%s: uma accepted it: %v", tc.name, err)
		}
	}
	if n := w.queued(); n != before {
		t.Errorf("refusals queued %d provider work items", n-before)
	}
	s := d
	s.Adapter = nil
	_, err := s.PlanStatus(ctx, convID(issue), "closed", "", "", "st-x", channels.Preview)
	wantCode(t, err, model.Unsupported, "adapter is unknown")
}

// TestStatusClosesAndReopensGitLabIssues: gitlab.issues declares the same
// close and reopen as GitHub; the verb works on it with no provider named
// by heo, queuing one act each way.
func TestStatusClosesAndReopensGitLabIssues(t *testing.T) {
	w := newTicketWorld(t)
	gl := w.seedGitLabIssue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()

	p, err := d.PlanStatus(ctx, convID(gl), "closed", "", "", "gl-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if p.Declaration == nil || p.Declaration.Adapter != "gitlab.issues" || p.Declaration.Capability != model.CapabilityWorkItemAct {
		t.Errorf("declaration = %+v", p.Declaration)
	}
	if _, acc, err := d.SetStatus(ctx, p); err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("close = %+v, %v", acc, err)
	}
	if w.queued() != before+1 {
		t.Fatalf("close queued %d", w.queued()-before)
	}
	// The provider reports the close; reopening is declared too.
	w.providerSets(gl, model.WorkItemClosed, "closed")
	if _, err := d.PlanStatus(ctx, convID(gl), "closed", "", "", "gl-2", channels.Preview); !model.IsCode(err, model.Unsupported) {
		t.Errorf("closing a closed issue: %v, want unsupported", err)
	}
	p, err = d.PlanStatus(ctx, convID(gl), "open", "", "", "gl-3", channels.Preview)
	if err != nil {
		t.Fatalf("reopen preview: %v", err)
	}
	if _, acc, err := d.SetStatus(ctx, p); err != nil || acc.Operation.State != model.Queued || w.queued() != before+2 {
		t.Fatalf("reopen = %+v, %v, queued %d", acc, err, w.queued()-before)
	}
}

// TestStatusOfALocalStatusCommitsInUma: a work item whose status uma owns
// on its channel (local_fields) changes in uma at once, reaching no
// provider, and takes no provider reason.
func TestStatusOfALocalStatusCommitsInUma(t *testing.T) {
	w := newTicketWorld(t)
	gl := w.seedGitLabIssue(model.WorkItemFieldStatus)
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()

	_, err := d.PlanStatus(ctx, convID(gl), "resolved", "fixed", "", "ls-1", channels.Preview)
	wantCode(t, err, model.Validation, "uma's own")
	p, err := d.PlanStatus(ctx, convID(gl), "resolved", "", "", "ls-1", channels.Preview)
	if err != nil {
		t.Fatalf("local status preview: %v", err)
	}
	if !p.Local || p.Declaration != nil {
		t.Errorf("plan = %+v, want local with no adapter check", p)
	}
	it, acc, err := d.SetStatus(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != model.WorkItemResolved || acc.Operation.State == model.Queued || w.queued() != before {
		t.Errorf("local status = %s, operation %s, queued %d", it.Status, acc.Operation.State, w.queued()-before)
	}
}

// TestStatusRefusals: usage errors, a conversation without a work item, a
// binding the provider dropped, a stale revision, an item already in the
// status and a caller without the desk role are refused before anything
// is queued.
func TestStatusRefusals(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()
	for _, tc := range []struct{ status, reason, want string }{
		{"", "", "status: required"},
		{"solved", "", `status "solved"`},
		{"Closed", "", `status "Closed"`},
		{"closed", "Not Planned", "snake_case"},
	} {
		_, err := d.PlanStatus(ctx, convID(issue), tc.status, tc.reason, "", "st-x", channels.Submit)
		wantCode(t, err, model.Validation, tc.want)
	}
	_, err := d.PlanStatus(ctx, string(w.gmailThread(peerMessage).ID), "closed", "", "", "st-x", channels.Submit)
	wantCode(t, err, model.Unsupported, "has no work item")
	wantCode(t, mustErr(d.PlanStatus(ctx, "00000ad0-0000-7000-8000-0000000009ff", "closed", "", "", "st-x", channels.Submit)), model.NotFound, "not found")

	_, err = d.PlanStatus(ctx, convID(issue), "open", "", "", "st-x", channels.Preview)
	wantCode(t, err, model.Unsupported, "already open")
	const stale model.Revision = "00000ad0-0000-7000-8000-0000000001ee"
	_, err = d.PlanStatus(ctx, convID(issue), "closed", "", stale, "st-x", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision "+string(issue.Revision))
	p, err := d.PlanStatus(ctx, convID(issue), "closed", "", stale, "st-x", channels.Submit)
	if err != nil {
		t.Fatalf("submit plan leaves the revision to uma: %v", err)
	}
	_, _, err = d.SetStatus(ctx, p)
	wantCode(t, err, model.Conflict, "is at revision "+string(issue.Revision))

	p, err = d.PlanStatus(ctx, convID(issue), "closed", "", "", "st-y", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.asKnowing(viewer).SetStatus(ctx, p); !model.IsCode(err, model.Unauthorized) {
		t.Errorf("viewer: %v, want unauthorized", err)
	}
	w.providerBinds(issue, model.WorkItemBindingDeleted)
	_, err = d.PlanStatus(ctx, convID(issue), "closed", "", "", "st-z", channels.Submit)
	wantCode(t, err, model.Conflict, "deleted")
	if n := w.queued(); n != before {
		t.Errorf("refusals queued %d provider work items", n-before)
	}
}

func mustErr[T any](_ T, err error) error { return err }

// TestUpdateRoutesByWhatTheAdapterDeclares: the provider's fields go
// through the update effect the channel's adapter ships authorized as a
// work item act — GitHub's and Zendesk's are found the same way, by that
// declaration, not by name — carrying the fields as given; planning
// writes nothing; submitting queues one operation and changes nothing in
// uma; a retry under the key replays.
func TestUpdateRoutesByWhatTheAdapterDeclares(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()

	for _, tc := range []struct {
		name   string
		conv   model.ConversationRef
		args   []string
		effect string
		want   map[string]any
	}{
		{"github", issue.Ref.Conversation, []string{`labels=["bug","ui"]`, "milestone=3", "assignees=[]"}, github.ExtensionUpdate,
			map[string]any{"labels": []any{"bug", "ui"}, "milestone": float64(3), "assignees": []any{}}},
		{"zendesk", w.ticket, []string{"status=solved", `tags=["vip"]`}, zendesk.ExtensionUpdate,
			map[string]any{"status": "solved", "tags": []any{"vip"}}},
	} {
		fields, err := desk.ParseFields(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		p, err := d.PlanUpdate(ctx, string(tc.conv.ID), fields, "", model.OperationID("up-"+tc.name), channels.Preview)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if p.Local() || p.Effect == nil || p.Effect.Action.Name != tc.effect || p.Effect.Target != tc.conv.Ref() ||
			p.Effect.Audience.Visibility != model.Internal || p.Declaration == nil || p.Declaration.Effect != tc.effect {
			t.Fatalf("%s: plan = %+v", tc.name, p)
		}
		if got := dataOf(t, p.Effect.Action.Data); !equalJSON(got, tc.want) {
			t.Errorf("%s: action data = %v, want %v", tc.name, got, tc.want)
		}
		if w.queued() != before {
			t.Fatalf("%s: planning queued work", tc.name)
		}
		sp, err := d.PlanUpdate(ctx, string(tc.conv.ID), fields, "", model.OperationID("up-"+tc.name), channels.Submit)
		if err != nil || sp.Declaration != nil {
			t.Fatalf("%s: submit plan = %+v, %v", tc.name, sp.Declaration, err)
		}
		_, acc, err := d.Update(ctx, sp)
		if err != nil || acc.Operation.State != model.Queued {
			t.Fatalf("%s: submit = %+v, %v", tc.name, acc, err)
		}
		if rec := w.record(acc.Operation.Ref); rec.Key.Target != tc.conv.Ref() {
			t.Errorf("%s: operation key = %+v", tc.name, rec.Key)
		}
		if _, again, err := d.Update(ctx, sp); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
			t.Errorf("%s: retry = %+v, %v", tc.name, again, err)
		}
		before++
	}
	if w.queued() != before {
		t.Errorf("queued %d, want one per update", w.queued()-(before-2))
	}
	if it := w.item(); it.RawStatus != "open" {
		t.Errorf("ticket changed in uma by a queued update: %s", it.RawStatus)
	}
}

func equalJSON(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// TestUpdateOfFieldsUmaOwnsCommitsLocally: the fields a channel's
// configuration makes uma's (local_fields) change in uma, through uma's
// work item act at the work item's revision, on any adapter, whether or
// not it ships an update effect; the other entries of fields are kept.
func TestUpdateOfFieldsUmaOwnsCommitsLocally(t *testing.T) {
	w := newTicketWorld(t)
	gl := w.seedGitLabIssue(model.WorkItemFieldSubject, model.WorkItemFieldAssignees, model.WorkItemFieldFields)
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()

	parse := func(args ...string) []desk.Field {
		f, err := desk.ParseFields(args)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	p, err := d.PlanUpdate(ctx, convID(gl), parse("subject=Build is green", "assignees=ada,grace", "team=infra"), "", "lu-1", channels.Preview)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Local() || p.Effect != nil || p.Act.ExpectedRevision != gl.Revision || p.Declaration != nil {
		t.Fatalf("plan = %+v", p)
	}
	it, acc, err := d.Update(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Operation.State == model.Queued || w.queued() != before {
		t.Errorf("a local update was queued to a provider: %s, %d", acc.Operation.State, w.queued()-before)
	}
	ids := []string{}
	for _, a := range it.Assignees {
		ids = append(ids, a.ID)
	}
	if it.Subject != "Build is green" || !slices.Equal(ids, []string{"ada", "grace"}) || len(it.Fields) != 1 || it.Fields[0].Name != "gitlab.issues.team" {
		t.Fatalf("work item = %+v", it)
	}
	// A second entry joins the first; one of the same name replaces it.
	p, err = d.PlanUpdate(ctx, convID(gl), parse("env=prod", "team=data"), "", "lu-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if it, _, err = d.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range it.Fields {
		got[f.Name] = string(f.Data)
	}
	if len(got) != 2 || got["gitlab.issues.team"] != `"data"` || got["gitlab.issues.env"] != `"prod"` {
		t.Errorf("fields = %v", got)
	}
	// A stale revision is a conflict, from the plan in a preview.
	_, err = d.PlanUpdate(ctx, convID(gl), parse("subject=x"), "00000ad0-0000-7000-8000-0000000001ee", "lu-3", channels.Preview)
	wantCode(t, err, model.Conflict, "is at revision")
}

// TestUpdateOfProviderFieldsWithNoEffectIsIngestOnly: a provider's field
// on a channel whose adapter ships no update effect is refused as
// unsupported: uma's work item is ingest-only for it.
func TestUpdateOfProviderFieldsWithNoEffectIsIngestOnly(t *testing.T) {
	w := newTicketWorld(t)
	gl := w.seedGitLabIssue(model.WorkItemFieldSubject)
	ctx := t.Context()
	d := w.asKnowing(agent)
	before := w.queued()
	fields, _ := desk.ParseFields([]string{`labels=["bug"]`})
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := d.PlanUpdate(ctx, convID(gl), fields, "", "io-1", mode)
		wantCode(t, err, model.Unsupported, "ingest-only")
	}
	// Even a field uma owns is no excuse to send another to the provider.
	mixed, _ := desk.ParseFields([]string{"subject=x", `labels=["bug"]`})
	_, err := d.PlanUpdate(ctx, convID(gl), mixed, "", "io-2", channels.Submit)
	wantCode(t, err, model.Validation, "two updates")
	if w.queued() != before {
		t.Errorf("refusals queued work")
	}
}

// TestUpdateRefusals: the status has its own verb, a conversation without
// a work item has nothing to update, a viewer may not, and the adapter's
// own rules answer a preview.
func TestUpdateRefusals(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	d := w.asKnowing(agent)
	mk := func(args ...string) []desk.Field {
		f, err := desk.ParseFields(args)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	gl := w.seedGitLabIssue(model.WorkItemFieldStatus)
	_, err := d.PlanUpdate(ctx, convID(gl), mk("status=closed"), "", "ur-1", channels.Submit)
	wantCode(t, err, model.Validation, "conversation status")
	_, err = d.PlanUpdate(ctx, string(w.gmailThread(peerMessage).ID), mk("a=b"), "", "ur-2", channels.Submit)
	wantCode(t, err, model.Unsupported, "has no work item")
	_, err = d.PlanUpdate(ctx, convID(issue), mk("labels=notalist"), "", "ur-3", channels.Preview)
	wantCode(t, err, model.Validation, "")
	_, err = d.PlanUpdate(ctx, convID(issue), mk("labels=[]"), "rev", "ur-4", channels.Submit)
	wantCode(t, err, model.Validation, "revision")
	p, err := d.PlanUpdate(ctx, convID(issue), mk(`labels=["bug"]`), "", "ur-5", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.asKnowing(viewer).Update(ctx, p); !model.IsCode(err, model.Unauthorized) {
		t.Errorf("viewer: %v, want unauthorized", err)
	}
	for _, bad := range [][]string{nil, {"novalue"}, {"=x"}, {"a=1", "a=2"}} {
		if _, err := desk.ParseFields(bad); !model.IsCode(err, model.Validation) {
			t.Errorf("ParseFields(%v) = %v, want validation", bad, err)
		}
	}
	f, _ := desk.ParseFields([]string{"n=3", "s=text", `q="3"`, "z=null", "e="})
	var vals []string
	for _, x := range f {
		vals = append(vals, string(x.Value))
	}
	if !slices.Equal(vals, []string{"3", `"text"`, `"3"`, "null", `""`}) {
		t.Errorf("values = %v", vals)
	}
}

// TestUpdateEffectNeedsExactlyOne: an adapter that ships two effects
// authorized as a work item act leaves heo no way to choose, and is
// refused; the discovery reads the declaration only.
func TestUpdateEffectNeedsExactlyOne(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	s := w.asKnowing(agent)
	one := github.NewFactory(github.Options{}).Extensions()[0]
	two := one
	two.Name = "other.effect"
	s.Effects = func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
		return []adapter.ExtensionDefinition{one, two}, nil
	}
	fields, _ := desk.ParseFields([]string{"milestone=1"})
	_, err := s.PlanUpdate(ctx, convID(issue), fields, "", "ue-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "more than one work item update effect")

	// An effect authorized as anything else is no update.
	other := one
	other.AuthorizeAs = model.ActionConversationLabel
	s.Effects = func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
		return []adapter.ExtensionDefinition{other}, nil
	}
	_, err = s.PlanUpdate(ctx, convID(issue), fields, "", "ue-2", channels.Submit)
	wantCode(t, err, model.Unsupported, "ingest-only")
}

// TestLifecyclesIndependent: resolving a work item's conversation leaves
// the work item as the provider last reported it, and the provider
// closing an issue (its webhook) leaves the conversation open and
// queues nothing back.
func TestLifecyclesIndependent(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	d := w.as(agent)
	for _, conv := range []model.ConversationRef{w.ticket, issue.Ref.Conversation} {
		cur := w.conversation(conv)
		p, err := d.PlanTransition(ctx, string(conv.ID), cur.Revision, model.ConversationResolved, model.OperationID("resolve-"+string(conv.ID)), channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.Transition(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if it := w.item(); it.Status != model.WorkItemOpen || it.RawStatus != "open" {
		t.Errorf("ticket after its conversation resolved = %s/%s", it.Status, it.RawStatus)
	}
	got, err := d.WorkItemOf(ctx, issue.Ref.Conversation)
	if err != nil || got.Status != model.WorkItemOpen {
		t.Errorf("issue after its conversation resolved = %+v, %v", got, err)
	}
	cur := w.conversation(issue.Ref.Conversation)
	p, err := d.PlanTransition(ctx, string(cur.Ref.ID), cur.Revision, model.ConversationOpen, "reopen-issue", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Transition(ctx, p); err != nil {
		t.Fatal(err)
	}
	before := w.queued()
	w.deliver("issues", "issues-closed", "d-closed")
	got, err = d.WorkItemOf(ctx, issue.Ref.Conversation)
	if err != nil || got.Status != model.WorkItemClosed || got.RawStatus != "closed:completed" {
		t.Fatalf("issue after the provider closed it = %+v, %v", got, err)
	}
	if c := w.conversation(issue.Ref.Conversation); c.Status != model.ConversationOpen {
		t.Errorf("closed issue's conversation = %s, want open", c.Status)
	}
	if n := w.queued(); n != before {
		t.Errorf("the provider's report queued %d provider work items", n-before)
	}
}

// TestListConversationItemsFiltersByWorkItem: a listing carries each
// conversation's work item, and narrows by whether it has one and by its
// status, a page at a time, scanning past conversations that do not match.
func TestListConversationItemsFiltersByWorkItem(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	mail := w.gmailThread(peerMessage)
	ctx := t.Context()
	d := w.as(viewer)
	all := model.PageRequest{Limit: 20}
	yes, no := true, false

	p, err := d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{}, all)
	if err != nil || len(p.Items) != 3 {
		t.Fatalf("all = %+v, %v", p, err)
	}
	byID := map[model.ID]desk.ConversationItem{}
	for _, c := range p.Items {
		byID[c.Ref.ID] = c
	}
	if c := byID[issue.Ref.Conversation.ID]; c.WorkItem == nil || c.WorkItem.External.ID != "7" || c.WorkItem.Status != model.WorkItemOpen {
		t.Errorf("issue conversation = %+v", c)
	}
	if c := byID[ticketConv]; c.WorkItem == nil || c.WorkItem.RawStatus != "open" {
		t.Errorf("ticket conversation = %+v", c)
	}
	if c := byID[mail.ID]; c.WorkItem != nil {
		t.Errorf("mail conversation has work item %+v", c.WorkItem)
	}

	p, err = d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Only: &yes}, all)
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("with a work item = %+v, %v", p, err)
	}
	p, err = d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Only: &no}, all)
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref.ID != mail.ID {
		t.Fatalf("without a work item = %+v, %v", p, err)
	}
	p, err = d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Statuses: []string{model.WorkItemResolved}}, all)
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("resolved = %+v, %v", p, err)
	}
	w.providerReports("solved", model.WorkItemResolved)
	p, err = d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Statuses: []string{model.WorkItemResolved}}, all)
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref.ID != ticketConv {
		t.Fatalf("resolved after the provider solved it = %+v, %v", p, err)
	}
	// One per page: each page scans past the conversations that do not
	// match and none is skipped.
	var seen []model.ID
	req := model.PageRequest{Limit: 1}
	for {
		p, err := d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Only: &yes}, req)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Items {
			seen = append(seen, c.Ref.ID)
		}
		if p.Complete || p.Next == "" {
			break
		}
		req.After = p.Next
	}
	if len(seen) != 2 {
		t.Errorf("paged = %v", seen)
	}
	if _, err := d.ListConversationItems(ctx, desk.Filter{}, desk.WorkItemFilter{Statuses: []string{"solved"}}, all); !model.IsCode(err, model.Validation) {
		t.Errorf("provider status as filter: %v, want validation", err)
	}
}
