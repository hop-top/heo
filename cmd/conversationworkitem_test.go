package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

func rowOf(rows [][2]string, name string) string {
	for _, r := range rows {
		if r[0] == name {
			return r[1]
		}
	}
	return "<no row " + name + ">"
}

func testWorkItem(status, raw string) model.WorkItem {
	conv := model.ConversationRef{Inbox: model.InboxRef{ID: "ibx_1"}, ID: "conv_1"}
	return model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: conv, ID: "wi_456"},
		External: model.ExternalRef{Namespace: "tracker.issues", Kind: model.RefKindWorkItem, ID: "7", ParentID: "example/repo"},
		Status:   status, RawStatus: raw, Revision: "rev_3", Binding: model.WorkItemBindingActive,
	}
}

func testStatusPlan(status, reason string, local bool) desk.StatusPlan {
	w := testWorkItem(model.WorkItemOpen, "opened")
	return desk.StatusPlan{
		Request: model.WorkItemAction{Command: model.Command{ExpectedRevision: "rev_3"}, Target: w.Ref},
		Item:    w, Conversation: model.Conversation{Ref: w.Ref.Conversation, Status: model.ConversationOpen},
		Status: status, Reason: reason, Local: local,
	}
}

// TestIssueAndTicketGroupsAreGone: the provider-named groups are removed
// outright; a tracker is reached through the conversation verbs, each a
// provider-dependent write that carries its capability.
func TestIssueAndTicketGroupsAreGone(t *testing.T) {
	a := newApp()
	for _, path := range []string{"issue", "ticket", "issue transition", "issue update", "ticket transition", "issue get", "ticket list"} {
		if find(a.r.Cmd, path) != nil {
			t.Errorf("%s is still in the tree", path)
		}
	}
	for path, capability := range map[string]model.Capability{
		"conversation status": model.CapabilityWorkItemAct,
		"conversation update": model.CapabilityExtensionExecute,
	} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Fatalf("%s: not in the tree", path)
		}
		if got := c.Annotations[annotationCapability]; got != string(capability) {
			t.Errorf("%s: capability %q, want %s", path, got, capability)
		}
		if got, _ := cli.GetSideEffect(c); got != cli.SideEffectWriteShared {
			t.Errorf("%s: side effect %q", path, got)
		}
	}
	// The conversation's own lifecycle verbs stay, and are not the work
	// item's.
	for _, path := range []string{"conversation resolve", "conversation reopen", "conversation tag"} {
		if find(a.r.Cmd, path) == nil {
			t.Errorf("%s is gone", path)
		}
	}
}

// TestStatusPlanNamesWhereTheChangeGoes: for a status the provider owns
// the plan says the change is external, which status to which, the
// reason, that it is queued and not applied, and that the conversation
// stays as it is; for one uma owns it says it is local.
func TestStatusPlanNamesWhereTheChangeGoes(t *testing.T) {
	p := statusPlan(testStatusPlan("closed", "not_planned", false), "key-1")
	if len(p.Effects) != 1 || p.Effects[0].Target != "work_item:wi_456" || !p.Effects[0].Reversible {
		t.Fatalf("effects = %+v", p.Effects)
	}
	for _, want := range []string{"external", "tracker.issues example/repo#7", "open (opened) -> closed", "not_planned",
		"queued, not applied", "conversation conv_1 stays open"} {
		if !strings.Contains(p.Effects[0].Detail, want) {
			t.Errorf("effect detail lacks %q: %s", want, p.Effects[0].Detail)
		}
	}
	for k, want := range map[string]string{"status": `"closed"`, "reason": `"not_planned"`, "from": `"open"`,
		"expected_revision": `"rev_3"`, "operation_key": `"key-1"`, "work_item": `"wi_456"`} {
		b, _ := json.Marshal(p.Args[k])
		if string(b) != want {
			t.Errorf("arg %s = %s, want %s", k, b, want)
		}
	}
	if d, _ := p.Args["delivery"].(string); !strings.Contains(d, "queued, not applied") {
		t.Errorf("delivery arg = %q", d)
	}
	if d := statusPlan(testStatusPlan("closed", "", false), "").Effects[0].Detail; !strings.Contains(d, "the adapter's default") {
		t.Errorf("default reason: %s", d)
	}
	local := statusPlan(testStatusPlan("resolved", "", true), "")
	if d := local.Effects[0].Detail; !strings.Contains(d, "local to uma") || strings.Contains(d, "external") {
		t.Errorf("local plan: %s", d)
	}
	if _, has := local.Args["delivery"]; has {
		t.Error("a local status change names a delivery")
	}
}

// TestStatusOutputNeverClaimsTheChange: uma may return no state for the
// act, and a provider's status changes only when the provider reports
// it, so the output says what was asked and that it is not applied while
// the work item is as it was, and never says the status is set.
func TestStatusOutputNeverClaimsTheChange(t *testing.T) {
	op := operationView{ID: "op_1", State: model.Queued, Durable: true}
	conv := model.Conversation{Ref: model.ConversationRef{ID: "conv_1"}}
	o := workItemStatusOut{Conversation: conv, WorkItem: testWorkItem(model.WorkItemOpen, "opened"), Status: "closed", Reason: "completed", Operation: op, OperationKey: "k"}
	o.Notes = statusNotes(o)
	rows := statusRows(o)
	st := rowOf(rows, "status")
	if !strings.HasPrefix(st, "opened ") || !strings.Contains(st, "asked: closed") || !strings.Contains(st, "not applied") {
		t.Errorf("status row = %q, want the provider's status, what was asked and not applied", st)
	}
	joined := strings.Join(o.Notes, "\n")
	for _, want := range []string{"queued", "provider reports", "heo conversation get conv_1", "heo delivery get op_1", "conversation's own status is not affected"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q:\n%s", want, joined)
		}
	}
	for _, line := range append(o.Notes, st) {
		if strings.Contains(line, "is closed") || strings.Contains(line, "was closed") || strings.Contains(line, "has been closed") {
			t.Errorf("output claims the change: %q", line)
		}
	}
	// After the provider reported it, nothing says it is pending.
	o.WorkItem = testWorkItem(model.WorkItemClosed, "closed")
	if st := rowOf(statusRows(o), "status"); strings.Contains(st, "not applied") {
		t.Errorf("status row after the report = %q", st)
	}
	// A local status is set in uma, and says so.
	o = workItemStatusOut{Conversation: conv, WorkItem: testWorkItem(model.WorkItemResolved, "opened"), Status: "resolved", Local: true, Operation: op}
	if st := rowOf(statusRows(o), "status"); !strings.Contains(st, "set in uma") || len(statusNotes(o)) != 0 {
		t.Errorf("local status row = %q, notes %v", st, statusNotes(o))
	}
}

func testUpdatePlan(local bool, args ...string) desk.UpdatePlan {
	w := testWorkItem(model.WorkItemOpen, "opened")
	f, _ := desk.ParseFields(args)
	p := desk.UpdatePlan{Fields: f, Item: w, Conversation: model.Conversation{Ref: w.Ref.Conversation, Status: model.ConversationOpen}}
	if local {
		p.Act = &model.WorkItemAction{Command: model.Command{ExpectedRevision: "rev_3"}, Target: w.Ref}
		return p
	}
	p.Effect = &model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal},
		Action: model.Extension{Name: "tracker.issues.update", Version: 1}}
	return p
}

// TestUpdatePlanNamesWhereTheChangeGoes: fields the provider owns go
// through the adapter's effect, named in the plan, queued and not applied;
// fields uma owns are local.
func TestUpdatePlanNamesWhereTheChangeGoes(t *testing.T) {
	p := updatePlan(testUpdatePlan(false, "milestone=3", `labels=["bug"]`), "key-1")
	d := p.Effects[0].Detail
	for _, want := range []string{"external", "tracker.issues example/repo#7", "tracker.issues.update", "milestone=3", `labels=["bug"]`,
		"queued, not applied", "conversation conv_1 stays open"} {
		if !strings.Contains(d, want) {
			t.Errorf("effect detail lacks %q: %s", want, d)
		}
	}
	for k, want := range map[string]string{"operation_key": `"key-1"`, "work_item": `"wi_456"`, "visibility": `"internal"`} {
		b, _ := json.Marshal(p.Args[k])
		if string(b) != want {
			t.Errorf("arg %s = %s, want %s", k, b, want)
		}
	}
	l := updatePlan(testUpdatePlan(true, "subject=x"), "")
	if d := l.Effects[0].Detail; !strings.Contains(d, "local to uma") || strings.Contains(d, "external") {
		t.Errorf("local plan: %s", d)
	}
	if l.Args["expected_revision"] != model.Revision("rev_3") {
		t.Errorf("local plan revision = %v", l.Args["expected_revision"])
	}
}

// TestUpdateOutputNeverClaimsTheChange: the work item shown is the one
// uma holds, so a provider's update says what was asked and that the
// provider has to report it.
func TestUpdateOutputNeverClaimsTheChange(t *testing.T) {
	op := operationView{ID: "op_1", State: model.Queued, Durable: true}
	f, _ := desk.ParseFields([]string{`labels=["bug"]`, "milestone=null"})
	o := updateOut{Conversation: model.Conversation{Ref: model.ConversationRef{ID: "conv_1"}}, WorkItem: testWorkItem(model.WorkItemOpen, "opened"),
		Fields: fieldMap(f), Operation: op, OperationKey: "k"}
	o.Notes = updateNotes(o)
	rows := updateRows(o)
	if rowOf(rows, "labels asked") != `["bug"]` || rowOf(rows, "milestone asked") != "null" {
		t.Errorf("rows = %v", rows)
	}
	joined := strings.Join(o.Notes, "\n")
	for _, want := range []string{"queued", "provider reports", "heo conversation get conv_1", "heo delivery get op_1", "not affected"} {
		if !strings.Contains(joined, want) {
			t.Errorf("notes lack %q:\n%s", want, joined)
		}
	}
	o.Local = true
	if rowOf(updateRows(o), "labels set") != `["bug"]` || len(updateNotes(o)) != 0 {
		t.Errorf("local rows = %v, notes %v", updateRows(o), updateNotes(o))
	}
}

// TestWorkItemRowsShowWhatTheProviderReported: `conversation get` shows
// the status beside the provider's own, the binding, the assignees, the
// URL and each field by its name, with no provider's field named by heo.
func TestWorkItemRowsShowWhatTheProviderReported(t *testing.T) {
	w := testWorkItem(model.WorkItemPending, "waiting")
	w.URL = "https://example.test/7"
	w.Assignees = []model.ExternalRef{{ID: "ada"}, {ID: "grace"}}
	w.LocalFields = []string{model.WorkItemFieldSubject}
	w.Fields = []model.Extension{{Name: "tracker.issues.labels", Version: 1, Data: json.RawMessage(`["bug","ui"]`)}}
	rows := workItemRows(w, []model.ConversationRef{{ID: "conv_9"}})
	for name, want := range map[string]string{
		"work item": "wi_456", "work item reference": "example/repo#7", "work item status": "pending", "work item provider status": "waiting",
		"work item binding": "active", "work item assignees": "ada, grace", "work item url": "https://example.test/7",
		"work item uma-owned fields": "subject", "work item field tracker.issues.labels": `["bug","ui"]`,
		"work item linked from": "conversation conv_9", "work item revision": "rev_3",
	} {
		if got := rowOf(rows, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if workItemCell(nil) != "-" || workItemCell(&w) != "example/repo#7 pending" {
		t.Errorf("cell = %q / %q", workItemCell(nil), workItemCell(&w))
	}
}

// TestConversationListTakesTheWorkItemFilters: --work-item and
// --work-item-status read as asked: --work-item alone keeps conversations
// that have a work item, =false those that have none, and unasked sets
// nothing.
func TestConversationListTakesTheWorkItemFilters(t *testing.T) {
	for name, tc := range map[string]struct {
		args     []string
		only     *bool
		statuses string
	}{
		"none":     {args: nil},
		"has":      {args: []string{"--work-item"}, only: boolp(true)},
		"has not":  {args: []string{"--work-item=false"}, only: boolp(false)},
		"statuses": {args: []string{"--work-item-status", "open,pending", "--work-item-status", "closed"}, statuses: "open,pending,closed"},
	} {
		c := parseFlags(t, "conversation list", tc.args...)
		f := workItemFilterOf(c)
		if (f.Only == nil) != (tc.only == nil) || (f.Only != nil && *f.Only != *tc.only) || strings.Join(f.Statuses, ",") != tc.statuses {
			t.Errorf("%s: filter = %+v", name, f)
		}
	}
}

func boolp(b bool) *bool { return &b }
