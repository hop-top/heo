package desk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The mail channels' conversations: a thread the peer wrote in, whose
// reply route names the provider message it wrote, and one the peer has
// written no message in.
const (
	peerAddress = "peer@example.com"
	peerMessage = "18c0ffee"
)

// mailInbox is a per-contact inbox on a mail channel.
func (w *ticketWorld) mailInbox(key string, ch model.ChannelRef, dest string) model.InboxRef {
	w.t.Helper()
	lock := true
	acc, err := w.client(operator).Configuration().ConfigureInbox(w.t.Context(), model.ConfigureInbox{Command: w.cmd(), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: ch, Key: key, Name: key, Destinations: []string{dest},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		w.t.Fatalf("inbox %s: %v", key, err)
	}
	ref, _ := acc.Operation.Result.AsInbox()
	return ref
}

// seedMail writes one open conversation of a mail inbox, as ingress
// records it, whose reply route is the peer's address with parent the
// message the peer wrote (none when empty).
func (w *ticketWorld) seedMail(in model.InboxRef, ch model.ChannelRef, namespace, parent string) model.ConversationRef {
	w.t.Helper()
	w.n++
	conv := model.ConversationRef{Inbox: in, ID: model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-0000000002%02x", w.n))}
	route := model.ExternalRef{Channel: ch, Namespace: namespace, Kind: "address", ID: peerAddress, ParentID: parent}
	if err := w.st.Transact(w.t.Context(), tenant, func(tx store.Tx) error {
		return tx.PutConversation(w.t.Context(), model.Conversation{
			Ref: conv, Mode: model.ModePerContact, Status: model.ConversationOpen,
			External: []model.ExternalRef{route}, ReplyRoute: route,
			CreatedAt: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC), Revision: model.Revision(fmt.Sprintf("00000ad0-0000-7000-8000-0000000003%02x", w.n)),
		}, "")
	}); err != nil {
		w.t.Fatalf("seed mail: %v", err)
	}
	return conv
}

// gmailThread and outlookThread are conversations the peer wrote in.
func (w *ticketWorld) gmailThread(parent string) model.ConversationRef {
	return w.seedMail(w.gmIn, w.gm, google.IdentityNamespace, parent)
}

func (w *ticketWorld) outlookThread(parent string) model.ConversationRef {
	return w.seedMail(w.msIn, w.ms, microsoft.IdentityNamespace, parent)
}

// effects is the runtime's view of the effects each channel's adapter
// ships, as heo's composition registers them.
func (w *ticketWorld) effects(_ context.Context, ref model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
	switch ref {
	case w.gh:
		return github.NewFactory(github.Options{}).Extensions(), nil
	case w.zd:
		return zendesk.NewFactory().Extensions(), nil
	case w.gl:
		return nil, nil // gitlab.issues ships no effect
	case w.gm:
		return google.NewFactory(google.Options{}).Extensions(), nil
	case w.ms:
		return microsoft.NewFactory(microsoft.Options{}).Extensions(), nil
	}
	return nil, model.NewError(model.NotFound, "channel not found")
}

// validate asks the channel's real adapter factory, as the runtime
// does, whether it accepts an action's fields.
func (w *ticketWorld) validate(_ context.Context, ref model.ChannelRef, aud model.Audience, a model.Extension) ([]model.ValidationIssue, error) {
	var f adapter.Factory
	switch ref {
	case w.gh:
		f = github.NewFactory(github.Options{})
	case w.gm:
		f = google.NewFactory(google.Options{})
	case w.ms:
		f = microsoft.NewFactory(microsoft.Options{})
	case w.zd:
		f = zendesk.NewFactory()
	}
	if v, ok := f.(adapter.ActionValidator); ok {
		return v.ValidateAction(aud, a.Clone()), nil
	}
	return nil, nil
}

func strs(s ...string) *[]string { return &s }

func dataOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("action data %s: %v", raw, err)
	}
	return m
}

// TestTagQueuesMailEffects: a tag change is Gmail's labels effect for
// --add and --remove and Outlook's categories effect for --set, on the
// conversation, authorized as a conversation label; it names the
// provider message it lands on; planning writes nothing; submitting
// queues one operation and changes nothing in uma, which records no
// provider tags; a retry replays.
func TestTagQueuesMailEffects(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()

	p, err := d.PlanTag(ctx, string(gm.ID), desk.MailTag{Add: []string{"Label_12", "STARRED"}, Remove: []string{"INBOX"}}, "tg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Target != gm.Ref() || p.Request.Action.Name != google.ExtensionLabels || p.Request.Action.Version != google.ExtensionVersion ||
		p.Request.Audience.Visibility != model.Internal || p.Message != peerMessage {
		t.Fatalf("labels plan = %+v", p)
	}
	data := dataOf(t, p.Request.Action.Data)
	if len(data) != 2 || fmt.Sprint(data["add"]) != "[Label_12 STARRED]" || fmt.Sprint(data["remove"]) != "[INBOX]" {
		t.Fatalf("labels data = %s", p.Request.Action.Data)
	}
	if n := w.queued(); n != before {
		t.Fatalf("planning queued %d provider work items", n-before)
	}
	conv0 := w.conversation(gm)
	c, acc, err := d.TagConversation(ctx, p)
	if err != nil || acc.Operation.State != model.Queued || acc.Replayed {
		t.Fatalf("tag = %+v, %v", acc, err)
	}
	if rec := w.record(acc.Operation.Ref); rec.Key.Kind != model.ActionConversationLabel || rec.Key.Target != gm.Ref() {
		t.Fatalf("operation key = %+v, want a conversation label on the conversation", rec.Key)
	}
	if c.Revision != conv0.Revision || len(c.Labels) != 0 {
		t.Errorf("the conversation after the tag = %+v: uma records no provider tags", c)
	}
	if n := w.queued(); n != before+1 {
		t.Fatalf("queued %d provider work items, want 1", n-before)
	}
	if _, again, err := d.TagConversation(ctx, p); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v, want %v replayed", again, err, acc.Operation.Ref)
	}
	// Remove alone, add alone: only what is named is sent.
	for _, tc := range []struct {
		tag  desk.MailTag
		want string
	}{{desk.MailTag{Add: []string{"Label_1"}}, `{"add":["Label_1"]}`}, {desk.MailTag{Remove: []string{"UNREAD"}}, `{"remove":["UNREAD"]}`}} {
		p, err := d.PlanTag(ctx, string(gm.ID), tc.tag, "tg-x", channels.Submit)
		if err != nil || string(p.Request.Action.Data) != tc.want {
			t.Errorf("data = %s, %v, want %s", p.Request.Action.Data, err, tc.want)
		}
	}

	p, err = d.PlanTag(ctx, string(ms.ID), desk.MailTag{Set: strs("Billing", "Urgent")}, "tg-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if p.Request.Action.Name != microsoft.ExtensionCategories || p.Request.Action.Version != microsoft.ExtensionVersion || p.Message != "AAMk-1" {
		t.Fatalf("categories plan = %+v", p)
	}
	if got := string(p.Request.Action.Data); got != `{"set":["Billing","Urgent"]}` {
		t.Fatalf("categories data = %s", got)
	}
	if _, acc, err = d.TagConversation(ctx, p); err != nil || acc.Operation.State != model.Queued {
		t.Fatalf("categories tag = %+v, %v", acc, err)
	}
	// An empty list clears the categories.
	if p, err = d.PlanTag(ctx, string(ms.ID), desk.MailTag{Set: strs()}, "tg-3", channels.Submit); err != nil || string(p.Request.Action.Data) != `{"set":[]}` {
		t.Fatalf("clearing data = %s, %v", p.Request.Action.Data, err)
	}
}

// TestTagRefusals: what is refused before anything is written. The
// channel's adapter must ship the effect the flags need, in a preview
// and on submission alike: an adapter that ships the other mail effect
// says which flags it takes, and one that declares extension_execute
// for an effect of its own (Zendesk) is refused although uma would have
// accepted the request.
func TestTagRefusals(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()
	labels, categories := desk.MailTag{Add: []string{"Label_1"}}, desk.MailTag{Set: strs("Billing")}

	// Usage: one way, and something named.
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err := d.PlanTag(ctx, string(gm.ID), desk.MailTag{}, "tg-1", mode)
		wantCode(t, err, model.Validation, "name labels to --add or --remove")
		_, err = d.PlanTag(ctx, string(gm.ID), desk.MailTag{Add: []string{"a"}, Set: strs("b")}, "tg-1", mode)
		wantCode(t, err, model.Validation, "--set replaces the whole list")
	}
	_, err := d.PlanTag(ctx, "no-such-conversation", labels, "tg-1", channels.Submit)
	wantCode(t, err, model.NotFound, "")

	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		// Outlook ships categories only; Gmail labels only.
		_, err = d.PlanTag(ctx, string(ms.ID), labels, "tg-1", mode)
		wantCode(t, err, model.Unsupported, "ships no effect "+google.ExtensionLabels)
		if !strings.Contains(err.Error(), "--set") {
			t.Errorf("the refusal does not say Outlook takes --set: %v", err)
		}
		_, err = d.PlanTag(ctx, string(gm.ID), categories, "tg-1", mode)
		wantCode(t, err, model.Unsupported, "ships no effect "+microsoft.ExtensionCategories)
		if !strings.Contains(err.Error(), "--add and --remove") {
			t.Errorf("the refusal does not say Gmail takes --add and --remove: %v", err)
		}
		// A Zendesk ticket's conversation: extension_execute declared, no mail effect.
		_, err = d.PlanTag(ctx, string(ticketConv), labels, "tg-1", mode)
		wantCode(t, err, model.Unsupported, "ships no effect "+google.ExtensionLabels)
		if strings.Contains(err.Error(), "--set") {
			t.Errorf("a refusal points at a flag the adapter does not take either: %v", err)
		}
	}
	// A conversation the peer has written no message in has nothing to tag.
	empty := w.gmailThread("")
	for _, mode := range []channels.Mode{channels.Preview, channels.Submit} {
		_, err = d.PlanTag(ctx, string(empty.ID), labels, "tg-1", mode)
		wantCode(t, err, model.Conflict, "names no provider message")
	}
	// Adapters unknown here refuse; so does one that does not declare
	// extension_execute.
	_, err = w.as(agent).PlanTag(ctx, string(gm.ID), labels, "tg-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "unknown here")
	d.Adapter = func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
		desc := google.NewFactory(google.Options{}).Describe()
		desc.Capabilities = slices.DeleteFunc(slices.Clone(desc.Capabilities), func(c model.Capability) bool {
			return c == model.CapabilityExtensionExecute
		})
		return desc, nil
	}
	_, err = d.PlanTag(ctx, string(gm.ID), labels, "tg-1", channels.Submit)
	wantCode(t, err, model.Unsupported, "does not declare extension_execute")
	if n := w.queued(); n != before {
		t.Fatalf("a refused tag queued %d provider work items", n-before)
	}
}

// TestTagPreviewChecksTheFields: a preview asks the real mail adapter
// factory, offline, whether it accepts the fields, and refuses what
// Gmail or Graph would (an empty ID, an ID twice, more than 100, a
// category twice or blank); a submission leaves that to uma, which asks
// the same factory and refuses the same, with nothing stored.
func TestTagPreviewChecksTheFields(t *testing.T) {
	w := newTicketWorld(t)
	ctx := t.Context()
	d := w.asKnowing(agent)
	gm, ms := w.gmailThread(peerMessage), w.outlookThread("AAMk-1")
	before := w.queued()
	many := make([]string, 101)
	for i := range many {
		many[i] = fmt.Sprintf("Label_%d", i)
	}
	for name, tc := range map[string]struct {
		conv model.ConversationRef
		tag  desk.MailTag
	}{
		"empty label ID":            {gm, desk.MailTag{Add: []string{""}}},
		"label in both lists":       {gm, desk.MailTag{Add: []string{"Label_1"}, Remove: []string{"Label_1"}}},
		"label twice":               {gm, desk.MailTag{Add: []string{"Label_1", "Label_1"}}},
		"more than 100":             {gm, desk.MailTag{Add: many}},
		"category twice":            {ms, desk.MailTag{Set: strs("Billing", "Billing")}},
		"blank category":            {ms, desk.MailTag{Set: strs("")}},
		"bare empty add and remove": {gm, desk.MailTag{Add: []string{}, Remove: []string{}}},
	} {
		_, err := d.PlanTag(ctx, string(tc.conv.ID), tc.tag, "tg-1", channels.Preview)
		if name == "bare empty add and remove" {
			wantCode(t, err, model.Validation, "name labels to --add or --remove")
			continue
		}
		wantCode(t, err, model.Validation, "action.data")
		p, err := d.PlanTag(ctx, string(tc.conv.ID), tc.tag, "tg-1", channels.Submit)
		if err != nil {
			t.Fatalf("%s: a submission plans without the adapter's check: %v", name, err)
		}
		if _, _, err := d.TagConversation(ctx, p); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: uma accepted %v, want a validation error", name, err)
		}
	}
	if n := w.queued(); n != before {
		t.Fatalf("a refused tag queued %d provider work items", n-before)
	}
	p, err := d.PlanTag(ctx, string(gm.ID), desk.MailTag{Add: []string{"Label_1"}}, "tg-1", channels.Preview)
	if err != nil || p.Declaration == nil || p.Declaration.Adapter != google.Type || p.Declaration.Effect != google.ExtensionLabels ||
		!strings.Contains(p.Declaration.Line(), "live availability not checked") {
		t.Fatalf("preview = %+v, %v", p.Declaration, err)
	}
}

// TestTagNeedsTheConversationLabelRight: uma authorizes the change as
// a conversation label, as the caller: a viewer is refused.
func TestTagNeedsTheConversationLabelRight(t *testing.T) {
	w := newTicketWorld(t)
	gm := w.gmailThread(peerMessage)
	p, err := w.asKnowing(viewer).PlanTag(t.Context(), string(gm.ID), desk.MailTag{Add: []string{"Label_1"}}, "tg-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	before := w.queued()
	if _, _, err := w.asKnowing(viewer).TagConversation(t.Context(), p); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("a viewer's tag = %v, want unauthorized", err)
	}
	if n := w.queued(); n != before {
		t.Fatalf("a refused tag queued %d provider work items", n-before)
	}
}
