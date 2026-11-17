package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/store"
	umapg "hop.top/uma/store/postgres"
)

// ghHookSecret is the GitHub channel's webhook secret.
const ghHookSecret = "e2e-github-webhook-secret" // #nosec G101 -- test value, not a credential

// conversationItemJSON is `conversation get` for a work item's
// conversation: the conversation's own fields, flat, and its work item.
type conversationItemJSON struct {
	model.Conversation
	WorkItem   *model.WorkItem         `json:"work_item"`
	LinkedFrom []model.ConversationRef `json:"linked_from"`
}

// workItemJSON is a work item read through its conversation: the work
// item, the conversation and the conversation's messages.
type workItemJSON struct {
	WorkItem     model.WorkItem
	Conversation model.Conversation
	Messages     []model.Message
}

// statusJSON is `conversation status`.
type statusJSON struct {
	Conversation model.Conversation
	WorkItem     model.WorkItem `json:"work_item"`
	Status       string
	Reason       string
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// updateJSON is `conversation update`.
type updateJSON struct {
	Conversation model.Conversation
	WorkItem     model.WorkItem `json:"work_item"`
	Fields       map[string]json.RawMessage
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

// workItem reads the work item of conversation id as the caller sees it:
// `conversation get` for the work item and the conversation, `message
// list` for the messages.
func (s *cliSite) workItem(id string) workItemJSON {
	s.t.Helper()
	var c conversationItemJSON
	s.json(&c, "conversation", "get", id)
	if c.WorkItem == nil {
		s.t.Fatalf("conversation %s has no work item: %+v", id, c)
	}
	var ms pageJSON[model.Message]
	s.json(&ms, "message", "list", "--conversation", id, "--limit", "100")
	return workItemJSON{WorkItem: *c.WorkItem, Conversation: c.Conversation, Messages: ms.Items}
}

// workItemSite is a cliSite with a github.issues and a zendesk.tickets
// channel configured through heo's commands. The GitHub channel is fed
// uma's webhook fixtures through the daemon's webhook listener; the
// Zendesk ticket is seeded in the store, since uma holds no recorded
// Zendesk exchanges. Workers stay off: nothing reaches a provider.
type workItemSite struct {
	*cliSite
	d      *daemon
	ghHook string
	zdIn   model.InboxRef
	zdCh   model.ChannelRef
}

func newWorkItemSite(t *testing.T) *workItemSite {
	t.Helper()
	c := newCLISite(t, "")
	key, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": ghHookSecret})
	if err != nil {
		t.Fatal(err)
	}
	c.s.write("secrets/uma/default/gh", string(key))
	d := c.start("workers")
	var gh, zd channelOutJSON
	c.json(&gh, "channel", "create", "--type", string(github.Type), "--key", "gh", "--configuration", `{}`, "--credential-ref", "gh")
	c.heo(0, "inbox", "create", "--channel", "gh", "--key", "issues", "--destination", "example/uma-sandbox", "--mode", model.ModePerWorkItem)
	c.json(&zd, "channel", "create", "--type", string(zendesk.Type), "--key", "zd", "--credential-ref", "zd",
		"--configuration", `{"subdomain":"acme","auth":"api_token","email":"agent@example.com","webhook_tolerance_seconds":300}`)
	var in struct{ Inbox model.Inbox }
	c.json(&in, "inbox", "create", "--channel", "zd", "--key", "support", "--destination", "360000001", "--mode", model.ModePerWorkItem)
	c.stop(d)
	s := &workItemSite{cliSite: c, ghHook: "http://" + c.hook + "/webhooks/" + string(gh.Channel.Ref.ID),
		zdIn: in.Inbox.Ref, zdCh: zd.Channel.Ref}
	s.d = c.start("workers")
	return s
}

// deliver posts uma's GitHub webhook fixture name as event, signed
// under the channel's webhook secret, to the daemon's webhook listener.
func (s *workItemSite) deliver(event, name, delivery string) {
	s.t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "hop.top/uma/adapter/ticket/github").Output()
	if err != nil {
		s.t.Fatalf("go list github adapter module: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "testdata", "webhooks", name+".json")) // #nosec G304 -- fixture
	if err != nil {
		s.t.Fatal(err)
	}
	m := hmac.New(sha256.New, []byte(ghHookSecret))
	m.Write(body)
	req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.ghHook, bytes.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.HeaderEvent, event)
	req.Header.Set(github.HeaderDelivery, delivery)
	req.Header.Set(github.HeaderSignature, "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("%s %s = %d", event, name, resp.StatusCode)
	}
}

func (s *workItemSite) store() store.Store {
	s.t.Helper()
	st, err := umapg.Open(s.t.Context(), s.db.pool(), umapg.Options{})
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

// workItems lists every work item in the store.
func (s *workItemSite) workItems() []model.WorkItem {
	s.t.Helper()
	var out []model.WorkItem
	if err := s.store().View(s.t.Context(), "default", func(r store.Reader) error {
		p, err := r.WorkItems(s.t.Context(), model.ListRequest{Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return out
}

// queued counts the provider work waiting for a worker.
func (s *workItemSite) queued() int {
	s.t.Helper()
	var n int
	if err := s.store().View(s.t.Context(), "default", func(r store.Reader) error {
		d, err := r.WorkDepth(s.t.Context(), "operation.execute")
		n = d.Pending + d.Claimed + d.Quarantined
		return err
	}); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// seedTicket writes one open Zendesk ticket, number 100, and its
// conversation, as ingress records them, and returns the work item.
func (s *workItemSite) seedTicket() model.WorkItem {
	s.t.Helper()
	ctx := s.t.Context()
	route := model.ExternalRef{Channel: s.zdCh, Namespace: zendesk.Namespace, Kind: model.RefKindWorkItem, ID: "100"}
	conv := model.ConversationRef{Inbox: s.zdIn, ID: "00000ad0-0000-7000-8000-0000000001c1"}
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	item := model.WorkItem{
		Ref: model.WorkItemRef{Conversation: conv, ID: "00000ad0-0000-7000-8000-0000000001c2"}, External: route,
		Subject: "My printer is on fire", Status: model.WorkItemOpen, RawStatus: "open",
		Requester: model.ExternalRef{Channel: s.zdCh, Namespace: zendesk.Namespace, Kind: zendesk.RefKindUser, ID: "42"},
		Binding:   model.WorkItemBindingActive, Source: model.WorkItemSource{Revision: "1", OccurredAt: at},
		Revision: "00000ad0-0000-7000-8000-0000000001d2",
	}
	if err := s.store().Transact(ctx, "default", func(tx store.Tx) error {
		if err := tx.PutConversation(ctx, model.Conversation{
			Ref: conv, Mode: model.ModePerWorkItem, Status: model.ConversationOpen,
			External: []model.ExternalRef{route}, ReplyRoute: route, CreatedAt: at,
			Revision: "00000ad0-0000-7000-8000-0000000001d1",
		}, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(ctx, item, "")
	}); err != nil {
		s.t.Fatalf("seed ticket: %v", err)
	}
	return item
}

// TestWorkItemsThroughConversations drives the release binary on
// Postgres: an issue and a ticket are conversations. GitHub's issue
// webhooks (uma's fixtures) reach the registered github.issues adapter
// through the daemon's webhook listener, and `conversation list` and
// `conversation get` show each conversation with its work item, its
// public comments through `message list`; the work item filters browse
// the tracker; `conversation status` previews the external change at
// GitHub, and is refused on a ticket, whose adapter declares no act;
// `conversation update` reaches the ticket through the adapter's own
// update effect, queues one Zendesk update under its key and replays it,
// and never moves the conversation; GitHub closing the issue leaves its
// conversation open and queues nothing; a viewer reads the work items
// without internal notes and changes nothing.
func TestWorkItemsThroughConversations(t *testing.T) {
	s := newWorkItemSite(t)
	s.deliver("issues", "issues-opened", "d-opened")
	s.deliver("issue_comment", "issue_comment-created", "d-comment")
	ticket := s.seedTicket()
	if n := len(s.workItems()); n != 2 {
		t.Fatalf("work items = %d, want the issue and the ticket", n)
	}

	// Browsing: every conversation of the two inboxes has a work item, each
	// narrowed by inbox and by the work item's status.
	var all, byStatus pageJSON[desk.ConversationItem]
	s.json(&all, "conversation", "list", "--work-item")
	if len(all.Items) != 2 {
		t.Fatalf("conversation list --work-item = %+v", all.Items)
	}
	var issueConv, ticketConv desk.ConversationItem
	for _, c := range all.Items {
		switch c.WorkItem.External.Namespace {
		case github.Namespace:
			issueConv = c
		case zendesk.Namespace:
			ticketConv = c
		}
	}
	if issueConv.WorkItem == nil || ticketConv.WorkItem == nil || ticketConv.WorkItem.Ref != ticket.Ref {
		t.Fatalf("work items listed = %+v", all.Items)
	}
	s.json(&byStatus, "conversation", "list", "--work-item-status", "open", "--inbox", "issues")
	if len(byStatus.Items) != 1 || byStatus.Items[0].Ref != issueConv.Ref {
		t.Fatalf("open work items of the issues inbox = %+v", byStatus.Items)
	}
	s.json(&byStatus, "conversation", "list", "--work-item-status", "resolved,closed")
	if len(byStatus.Items) != 0 {
		t.Fatalf("resolved or closed work items = %+v", byStatus.Items)
	}
	s.json(&byStatus, "conversation", "list", "--work-item=false")
	if len(byStatus.Items) != 0 {
		t.Fatalf("conversations without a work item = %+v", byStatus.Items)
	}
	wantStderr(t, s.heo(2, "conversation", "list", "--work-item-status", "solved"), "VALIDATION", `work item status "solved"`)
	issueID, ticketID := string(issueConv.WorkItem.Ref.ID), string(ticket.Ref.ID)
	issueConvID, ticketConvID := string(issueConv.Ref.ID), string(ticket.Ref.Conversation.ID)
	r := s.heo(0, "conversation", "list", "--work-item")
	for _, want := range []string{issueConvID, "example/uma-sandbox#7 open", "per_work_item"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("conversation list lacks %q:\n%s", want, r.stdout)
		}
	}

	// conversation get: the issue as GitHub reported it, beside its
	// conversation, with its comments through the messages.
	iv := s.workItem(issueConvID)
	if iv.WorkItem.External.ID != "7" || iv.WorkItem.External.ParentID != "example/uma-sandbox" ||
		iv.WorkItem.RawStatus != "open" || iv.Conversation.Status != model.ConversationOpen || len(iv.Messages) != 2 {
		t.Fatalf("issue conversation = %+v", iv)
	}
	r = s.heo(0, "conversation", "get", issueConvID)
	for _, row := range [][2]string{{"work item reference", "example/uma-sandbox#7"}, {"work item status", "open"},
		{"work item provider status", "open"}, {"work item binding", "active"}, {"work item", issueID}} {
		if !hasRow(r.stdout, row[0], row[1]) {
			t.Errorf("conversation get lacks row %v:\n%s", row, r.stdout)
		}
	}
	wantStderr(t, s.heo(3, "conversation", "get", "00000ad0-0000-7000-8000-0000000009ff"), "NOT_FOUND")

	// A desk note on the issue's conversation, for the viewer below.
	s.heo(0, "note", "add", issueConvID, "--text", "internal secret", "--placement", "local")

	// The ticket: its adapter declares no work item act, so the status is
	// refused where the adapter declares nothing, and verb discovery shows
	// it. Its status goes through the adapter's own update effect.
	tv := s.workItem(ticketConvID)
	if tv.WorkItem.External.ID != "100" || tv.WorkItem.RawStatus != "open" || tv.Conversation.Status != model.ConversationOpen {
		t.Fatalf("ticket conversation = %+v", tv)
	}
	var cv struct {
		Verbs []struct{ Verb, State string } `json:"verbs"`
	}
	s.json(&cv, "conversation", "get", ticketConvID)
	state := map[string]string{}
	for _, v := range cv.Verbs {
		state[v.Verb] = v.State
	}
	if state["conversation status"] != "unsupported" || state["conversation update"] != "available" {
		t.Errorf("ticket verbs = %v, want status unsupported and update available", state)
	}
	before := s.queued()
	wantStderr(t, s.heo(2, "conversation", "status", ticketConvID, "--set", "resolved", "--dry-run"), "UNSUPPORTED", "does not declare work_item_act")
	wantStderr(t, s.heo(2, "conversation", "status", ticketConvID, "--set", "resolved", "--idempotency-key", "ts-1"), "UNSUPPORTED")
	wantStderr(t, s.heo(2, "conversation", "status", ticketConvID, "--set", "solved"), "USAGE", "status")
	update := []string{"conversation", "update", ticketConvID, "--field", "status=solved"}
	var plan planJSON
	s.json(&plan, append(update, "--dry-run")...)
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Effects[0].Target != "work_item:"+ticketID ||
		!strings.Contains(plan.Effects[0].Detail, "external") || !strings.Contains(plan.Effects[0].Detail, "stays open") ||
		!strings.Contains(plan.Effects[0].Detail, zendesk.ExtensionUpdate) ||
		string(plan.Args["change"]) != `"status=\"solved\""` || string(plan.Args["visibility"]) != `"internal"` {
		t.Fatalf("update preview = %+v", plan)
	}
	// The preview says what it checked of the adapter, and what not.
	var capLine string
	_ = json.Unmarshal(plan.Args["capability"], &capLine)
	if !strings.Contains(capLine, "extension_execute declared by zendesk.tickets") || !strings.Contains(capLine, zendesk.ExtensionUpdate) ||
		!strings.Contains(capLine, "live availability not checked") {
		t.Errorf("update preview capability line = %q", capLine)
	}
	r = s.heo(0, append(update, "--dry-run")...)
	for _, row := range []string{"capability", "extension_execute declared by zendesk.tickets", ticketID, "status=\"solved\"", "work_item 100"} {
		if !strings.Contains(r.stdout, row) {
			t.Errorf("text preview lacks %q:\n%s", row, r.stdout)
		}
	}
	if n := s.queued(); n != before {
		t.Fatalf("--dry-run queued %d provider work items", n-before)
	}
	wantStderr(t, s.heo(2, "conversation", "update", ticketConvID, "--field", "novalue"), "USAGE", "key=value")
	wantStderr(t, s.heo(2, "conversation", "update", ticketConvID, "--field", "status=solved", "--field", "status=open"), "USAGE", "given twice")
	wantStderr(t, s.heo(3, "conversation", "update", "00000ad0-0000-7000-8000-0000000009ff", "--field", "status=solved"), "NOT_FOUND")

	var first, again updateJSON
	s.json(&first, append(update, "--idempotency-key", "tu-1")...)
	if first.Operation.State != model.Queued || first.Operation.Replayed || first.OperationKey != "tu-1" ||
		first.WorkItem.RawStatus != "open" || string(first.Fields["status"]) != `"solved"` {
		t.Fatalf("update = %+v", first)
	}
	s.json(&again, append(update, "--idempotency-key", "tu-1")...)
	if !again.Operation.Replayed || again.Operation.ID != first.Operation.ID {
		t.Fatalf("retry = %+v, want %s replayed", again, first.Operation.ID)
	}
	if n := s.queued(); n != before+1 {
		t.Fatalf("queued %d provider work items, want 1", n-before)
	}
	var conv model.Conversation
	s.json(&conv, "conversation", "get", ticketConvID)
	if conv.Status != model.ConversationOpen || conv.Revision != tv.Conversation.Revision {
		t.Fatalf("a ticket update moved its conversation: %+v", conv)
	}

	// The issue: its adapter declares the close and the reopen, so the
	// status is previewed; a status it declares no transition to is
	// refused. Nothing is queued by a preview or a refusal.
	var sp planJSON
	s.json(&sp, "conversation", "status", issueConvID, "--set", "closed", "--reason", "not_planned", "--dry-run")
	if len(sp.Effects) != 1 || sp.Effects[0].Target != "work_item:"+issueID || !strings.Contains(sp.Effects[0].Detail, "external") ||
		!strings.Contains(sp.Effects[0].Detail, "not_planned") || string(sp.Args["status"]) != `"closed"` || string(sp.Args["from"]) != `"open"` {
		t.Fatalf("status preview = %+v", sp)
	}
	wantStderr(t, s.heo(2, "conversation", "status", issueConvID, "--set", "pending", "--dry-run"), "UNSUPPORTED", "declares no work item transition")
	wantStderr(t, s.heo(2, "conversation", "status", issueConvID, "--set", "open", "--dry-run"), "UNSUPPORTED", "already open")
	wantStderr(t, s.heo(2, "conversation", "status", issueConvID, "--set", "closed", "--reason", "duplicate", "--dry-run"), "UNSUPPORTED", "duplicate")
	wantStderr(t, s.heo(2, "conversation", "status", issueConvID, "--set", "closed", "--reason", "Not Planned"), "USAGE", "snake_case")
	if n := s.queued(); n != before+1 {
		t.Fatalf("previews and refusals queued %d provider work items", n-before-1)
	}
	// GitHub closes the issue: closed at GitHub, its conversation open,
	// nothing queued back.
	s.deliver("issues", "issues-closed", "d-closed")
	iv = s.workItem(issueConvID)
	if iv.WorkItem.Status != model.WorkItemClosed || iv.WorkItem.RawStatus != "closed:completed" || iv.Conversation.Status != model.ConversationOpen {
		t.Fatalf("issue after GitHub closed it = %+v / conversation %s", iv.WorkItem, iv.Conversation.Status)
	}
	s.json(&byStatus, "conversation", "list", "--work-item-status", "closed")
	if len(byStatus.Items) != 1 || byStatus.Items[0].Ref != issueConv.Ref {
		t.Fatalf("closed work items = %+v", byStatus.Items)
	}
	if n := s.queued(); n != before+1 {
		t.Fatalf("GitHub's report queued %d provider work items", n-before-1)
	}
	s.stop(s.d)

	// A viewer reads the work items, public content only, and changes
	// nothing.
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	s.d = s.start("workers")
	iv = s.workItem(issueConvID)
	if len(iv.Messages) != 2 {
		t.Fatalf("viewer issue messages = %+v", iv.Messages)
	}
	if r := s.heo(0, "conversation", "get", issueConvID); strings.Contains(r.stdout, "internal secret") || strings.Contains(r.stdout, "internal") {
		t.Fatalf("viewer conversation get shows the note:\n%s", r.stdout)
	}
	s.json(&all, "conversation", "list", "--work-item")
	if len(all.Items) != 2 {
		t.Fatalf("viewer conversation list --work-item = %+v", all.Items)
	}
	wantStderr(t, s.heo(5, append(update, "--idempotency-key", "tu-2")...), "UNAUTHORIZED")
	wantStderr(t, s.heo(5, "conversation", "status", issueConvID, "--set", "open", "--idempotency-key", "ts-2"), "UNAUTHORIZED")
	if n := s.queued(); n != before+1 {
		t.Fatalf("viewer queued %d provider work items", n-before-1)
	}
	s.stop(s.d)
}
