package e2e

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

type labelOutJSON struct {
	Label        model.Label
	Operation    opJSON
	OperationKey string `json:"operation_key"`
}

type labelingOutJSON struct {
	Conversation model.Conversation
	Labels       []string
	Added        []string
	Removed      []string
	Operation    opJSON
	Notes        []string
}

// visitSite has a new widget visitor on site send a first message and
// returns the conversation the daemon filed it in.
func visitSite(t *testing.T, db *testDB, route, site string) model.ConversationRef {
	t.Helper()
	list := func() map[model.ConversationRef]bool {
		page, err := db.operator("default").Conversations().List(t.Context(), model.ListRequest{
			Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100},
		})
		if err != nil {
			t.Fatal(err)
		}
		out := map[model.ConversationRef]bool{}
		for _, c := range page.Items {
			out[c.Ref] = true
		}
		return out
	}
	before := list()
	code, body := postJSON(t, route+"/session?"+composition.WidgetSiteParam+"="+site, "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("session = %d %s", code, body)
	}
	if code, body := postJSON(t, route+"/messages", `{"id":"m-1","text":"hello"}`, "Authorization", "Bearer "+sess.Token); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d %s", code, body)
	}
	for ref := range list() {
		if !before[ref] {
			return ref
		}
	}
	t.Fatalf("visitor on %s opened no conversation", site)
	return model.ConversationRef{}
}

// labelEventCount counts the tenant's events of kind.
func labelEventCount(t *testing.T, db *testDB, kind string) []model.ConversationLabelPayload {
	t.Helper()
	page, err := db.operator("default").Events().List(t.Context(), model.ListEvents{
		Call: model.Call{Correlation: "e2e"}, Filter: model.Filter{Kinds: []string{kind}}, Start: model.StartBeginning, Limit: 500,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []model.ConversationLabelPayload
	for _, ev := range page.Items {
		var p model.ConversationLabelPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func refIDs(refs ...model.ConversationRef) []model.ID {
	out := make([]model.ID, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.ID)
	}
	slices.Sort(out)
	return out
}

// TestLabelCommands drives the release binary on Postgres through real
// widget visitors in two inboxes: labels are created (preview writes
// nothing, a retry under its key replays, a taken name conflicts) and
// listed; conversations are labeled and unlabeled, idempotently — adding
// a held label succeeds with no event — and an unknown label is not
// found with the create command as its fix; the any, all and none
// filters return the right conversations across inboxes; uma emits one
// event per label changed; a viewer lists and filters but may not
// create or label.
func TestLabelCommands(t *testing.T) {
	c := newCLISite(t, "")
	d := c.start()
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(widget.Type), "--key", "web",
		"--configuration", `{"sites":["site-1","site-2"]}`, "--credential-ref", "widget-key")
	for in, site := range map[string]string{"webin": "site-1", "webin2": "site-2"} {
		c.heo(0, "inbox", "create", "--channel", "web", "--key", in, "--destination", site,
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	}
	c.stop(d)
	d = c.start()
	route := "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	a, b := visitSite(t, c.db, route, "site-1"), visitSite(t, c.db, route, "site-1")
	x := visitSite(t, c.db, route, "site-2")
	if a.Inbox == x.Inbox || a.Inbox != b.Inbox {
		t.Fatalf("inboxes: a %s, b %s, x %s", a.Inbox.ID, b.Inbox.ID, x.Inbox.ID)
	}
	labels := func() []model.Label {
		t.Helper()
		var p pageJSON[model.Label]
		c.json(&p, "label", "list")
		return p.Items
	}
	get := func(ref model.ConversationRef) model.Conversation {
		t.Helper()
		var got model.Conversation
		c.json(&got, "conversation", "get", string(ref.ID))
		return got
	}

	// Labels: a preview writes nothing; a create replays under its key;
	// a name taken under case folding conflicts.
	var plan planJSON
	c.json(&plan, "label", "create", "billing", "--dry-run")
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" || plan.Effects[0].Target != "label:billing" || len(labels()) != 0 {
		t.Fatalf("create preview = %+v, or it wrote", plan)
	}
	var made, again labelOutJSON
	c.json(&made, "label", "create", "billing", "--description", "money", "--idempotency-key", "lbl-1")
	if made.Label.Name != "billing" || made.Label.Description != "money" || made.Label.Ref.ID == "" ||
		made.Operation.State != model.Committed || made.OperationKey != "lbl-1" {
		t.Fatalf("label create = %+v", made)
	}
	c.json(&again, "label", "create", "billing", "--description", "money", "--idempotency-key", "lbl-1")
	if !again.Operation.Replayed || again.Label.Ref != made.Label.Ref {
		t.Fatalf("replayed create = %+v", again)
	}
	wantStderr(t, c.heo(4, "label", "create", "Billing"), "CONFLICT", `label "Billing"`)
	wantStderr(t, c.heo(2, "label", "create", "x", "--color", "red"), "VALIDATION", "color")
	wantStderr(t, c.heo(2, "label", "create", "a,b"), "USAGE", "comma")
	r := c.heo(0, "label", "create", "urgent")
	if !strings.Contains(r.stdout, "urgent") || !strings.Contains(r.stdout, "operation key") {
		t.Errorf("text create:\n%s", r.stdout)
	}
	c.heo(0, "label", "create", "spam")
	if got := labels(); len(got) != 3 || got[0].Name != "billing" {
		t.Fatalf("label list = %+v", got)
	}
	r = c.heo(0, "label", "list")
	for _, want := range []string{"NAME", "billing", "money", "urgent", "spam"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("label list lacks %q:\n%s", want, r.stdout)
		}
	}

	// Labeling: unknown names are not found and point at label create.
	id := string(a.ID)
	wantStderr(t, c.heo(3, "conversation", "label", id, "--add", "billing", "--add", "nope"),
		"NOT_FOUND", `label "nope": not found`, "heo label create nope")
	wantStderr(t, c.heo(3, "conversation", "label", id, "--remove", "nope"), "NOT_FOUND", "heo label list")
	c.heo(2, "conversation", "label", id)
	c.heo(2, "conversation", "label", id, "--add", "billing", "--remove", "urgent")
	wantStderr(t, c.heo(3, "conversation", "label", "conv_404", "--add", "billing"), "NOT_FOUND")

	before := get(a)
	c.json(&plan, "conversation", "label", id, "--add", "billing", "--add", "urgent", "--dry-run")
	if len(plan.Effects) != 2 || plan.Effects[0].Target != "conversation:"+id ||
		!strings.Contains(plan.Effects[0].Detail, "add label billing") || string(plan.Args["labels"]) != `"billing, urgent"` {
		t.Fatalf("labeling preview = %+v", plan)
	}
	if got := get(a); len(got.Labels) != 0 || got.Revision != before.Revision {
		t.Fatalf("--dry-run wrote: %+v", got)
	}
	if n := len(labelEventCount(t, c.db, model.EventConversationLabelAdded)); n != 0 {
		t.Fatalf("--dry-run emitted %d events", n)
	}
	var out labelingOutJSON
	c.json(&out, "conversation", "label", id, "--add", "billing", "--add", "urgent", "--idempotency-key", "lab-1")
	if !slices.Equal(out.Added, []string{"billing", "urgent"}) || !slices.Equal(out.Labels, []string{"billing", "urgent"}) ||
		out.Operation.State != model.Committed || out.Operation.Replayed || out.Conversation.Revision == before.Revision {
		t.Fatalf("label add = %+v", out)
	}
	labeled := out.Conversation
	c.json(&out, "conversation", "label", id, "--add", "billing,urgent", "--idempotency-key", "lab-1")
	if !out.Operation.Replayed {
		t.Fatalf("replayed label add = %+v", out)
	}
	// Adding a held label: success, nothing changes, no event.
	c.json(&out, "conversation", "label", id, "--add", "BILLING")
	if len(out.Added) != 0 || out.Conversation.Revision != labeled.Revision || len(out.Notes) != 1 ||
		!strings.Contains(out.Notes[0], "already holds label billing") {
		t.Fatalf("re-add = %+v", out)
	}
	r = c.heo(0, "conversation", "label", id, "--add", "billing")
	if !strings.Contains(r.stderr, "already holds label billing") || !strings.Contains(r.stdout, "billing, urgent") {
		t.Errorf("text re-add:\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	c.heo(0, "conversation", "label", string(b.ID), "--add", "billing")
	c.heo(0, "conversation", "label", string(x.ID), "--add", "spam")
	if evs := labelEventCount(t, c.db, model.EventConversationLabelAdded); len(evs) != 4 {
		t.Fatalf("label-added events = %+v, want 4 (one per label changed)", evs)
	}

	// Filters, across inboxes, applied by uma.
	cases := []struct {
		args []string
		want []model.ID
	}{
		{[]string{"--label", "billing"}, refIDs(a, b)},
		{[]string{"--label", "billing,urgent"}, refIDs(a, b)},
		{[]string{"--label", "urgent", "--label", "spam"}, refIDs(a, x)},
		{[]string{"--label-all", "billing,urgent"}, refIDs(a)},
		{[]string{"--label-all", "billing", "--label-all", "spam"}, nil},
		{[]string{"--label-none", "billing"}, refIDs(x)},
		{[]string{"--label", "billing", "--label-none", "urgent"}, refIDs(b)},
		{[]string{"--label-all", "billing", "--inbox", "webin"}, refIDs(a, b)},
		{[]string{"--label-none", "spam", "--inbox", "webin2"}, nil},
	}
	for _, tc := range cases {
		var p pageJSON[model.Conversation]
		c.json(&p, append([]string{"conversation", "list"}, tc.args...)...)
		var got []model.ID
		for _, cv := range p.Items {
			got = append(got, cv.Ref.ID)
		}
		slices.Sort(got)
		if !slices.Equal(got, tc.want) {
			t.Errorf("conversation list %v = %v, want %v", tc.args, got, tc.want)
		}
	}
	wantStderr(t, c.heo(3, "conversation", "list", "--label-none", "nope"), "NOT_FOUND", "heo label list")
	wantStderr(t, c.heo(2, "conversation", "list", "--label", "billing", "--label-none", "billing"), "VALIDATION")

	// Removing: a held label goes, with one event; an absent one is a
	// no-op.
	c.json(&out, "conversation", "label", id, "--remove", "urgent")
	if !slices.Equal(out.Removed, []string{"urgent"}) || !slices.Equal(out.Labels, []string{"billing"}) {
		t.Fatalf("label remove = %+v", out)
	}
	c.json(&out, "conversation", "label", id, "--remove", "urgent")
	if len(out.Removed) != 0 || len(out.Notes) != 1 {
		t.Fatalf("remove absent = %+v", out)
	}
	if evs := labelEventCount(t, c.db, model.EventConversationLabelRemoved); len(evs) != 1 || evs[0].Conversation != a {
		t.Fatalf("label-removed events = %+v", evs)
	}
	c.stop(d)

	// A viewer lists labels and filters by them, and may neither create
	// a label nor label a conversation, previews included.
	c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	d = c.start()
	if got := labels(); len(got) != 3 {
		t.Fatalf("viewer label list = %+v", got)
	}
	var p pageJSON[model.Conversation]
	c.json(&p, "conversation", "list", "--label", "billing")
	if len(p.Items) != 2 {
		t.Fatalf("viewer filter = %+v", p.Items)
	}
	for _, args := range [][]string{
		{"label", "create", "vip"},
		{"label", "create", "vip", "--dry-run"},
		{"conversation", "label", id, "--add", "spam"},
		{"conversation", "label", id, "--remove", "billing", "--dry-run"},
	} {
		wantDenied(t, c.s.run(nil, args...), "heo.authz.not_granted")
	}
	if got := labels(); len(got) != 3 {
		t.Fatalf("viewer wrote a label: %+v", got)
	}
	c.stop(d)
}
