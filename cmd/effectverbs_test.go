package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

// parseFlags parses args on the leaf at path of a fresh tree, as the
// command line does, and returns it.
func parseFlags(t *testing.T, path string, args ...string) *cobra.Command {
	t.Helper()
	c := find(newApp().r.Cmd, path)
	if c == nil {
		t.Fatalf("%s: not in the tree", path)
	}
	if err := c.ParseFlags(args); err != nil {
		t.Fatalf("%s %v: %v", path, args, err)
	}
	return c
}

// TestMailTagOfNamesOnlyWhatWasGiven: --set is given only when named,
// an empty value naming an empty list; --add and --remove may go
// together, --set goes alone.
func TestMailTagOfNamesOnlyWhatWasGiven(t *testing.T) {
	c := parseFlags(t, "conversation tag", "--add", "Label_1,STARRED", "--remove", "INBOX")
	tag := mailTagOf(c)
	if tag.Set != nil || strings.Join(tag.Add, ",") != "Label_1,STARRED" || strings.Join(tag.Remove, ",") != "INBOX" {
		t.Errorf("add and remove = %+v", tag)
	}
	if err := desk.CheckMailTag(tag); err != nil {
		t.Errorf("add with remove refused: %v", err)
	}
	for _, arg := range []string{"--set=", "--set"} {
		args := []string{arg}
		if arg == "--set" {
			args = append(args, "")
		}
		c = parseFlags(t, "conversation tag", args...)
		if tag = mailTagOf(c); tag.Set == nil || len(*tag.Set) != 0 {
			t.Errorf("%v = %+v, want an empty list to set", args, tag)
		}
		if err := desk.CheckMailTag(tag); err != nil {
			t.Errorf("an empty --set clears and is valid: %v", err)
		}
	}
	// The flag rules cobra enforces: one of the three, --set alone.
	for args, wantErr := range map[string]bool{
		"":                   true,
		"--add a --remove b": false,
		"--set a":            false,
		"--set a --add b":    true,
		"--set a --remove b": true,
		"--set= --add b":     true,
	} {
		c := find(newApp().r.Cmd, "conversation tag")
		if err := c.ParseFlags(strings.Fields(args)); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		if err := c.ValidateFlagGroups(); (err != nil) != wantErr {
			t.Errorf("flags %q: group check = %v, want an error: %v", args, err, wantErr)
		}
	}
}

func testTagConversation() model.Conversation {
	return model.Conversation{
		Ref:        model.ConversationRef{Inbox: model.InboxRef{ID: "ibx_1"}, ID: "conv_1"},
		Status:     model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Namespace: "google.mail.address", Kind: "address", ID: "peer@example.com", ParentID: "18c0ffee"},
	}
}

// TestConversationTagPlanNamesTheExternalEffect: the plan names the
// provider message the change lands on, the effect and what was asked,
// and says uma records none of it.
func TestConversationTagPlanNamesTheExternalEffect(t *testing.T) {
	conv := testTagConversation()
	for name, tc := range map[string]struct {
		tag    desk.MailTag
		effect string
		want   []string
	}{
		"gmail":   {desk.MailTag{Add: []string{"Label_1"}, Remove: []string{"INBOX"}}, "google.mail.labels", []string{"add Label_1", "remove INBOX"}},
		"outlook": {desk.MailTag{Set: &[]string{"Billing"}}, "microsoft.mail.categories", []string{"set categories to Billing"}},
		"clear":   {desk.MailTag{Set: &[]string{}}, "microsoft.mail.categories", []string{"set categories to (none)"}},
	} {
		data, _ := tc.tag.Data()
		p := conversationTagPlan(desk.TagPlan{
			Request:      model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal}, Action: model.Extension{Name: tc.effect, Version: 1, Data: data}},
			Conversation: conv, Tag: tc.tag, Message: "18c0ffee",
		}, "key-1")
		if len(p.Effects) != 1 || p.Effects[0].Target != "mail:message 18c0ffee" {
			t.Fatalf("%s: effects = %+v", name, p.Effects)
		}
		for _, want := range append(tc.want, "external", tc.effect, "uma records none") {
			if !strings.Contains(p.Effects[0].Detail, want) {
				t.Errorf("%s: effect detail lacks %q: %s", name, want, p.Effects[0].Detail)
			}
		}
		if p.Args["message"] != "18c0ffee" || !strings.Contains(p.Args["change"].(string), tc.effect) {
			t.Errorf("%s: args = %v", name, p.Args)
		}
	}
}

// TestConversationTagRows: the rows say what was asked and the effect,
// and name only the lists the change carried.
func TestConversationTagRows(t *testing.T) {
	op := operationView{ID: "op_1", State: model.Queued}
	rows := conversationTagRows(conversationTagOut{Conversation: testTagConversation(), Message: "18c0ffee", Effect: "google.mail.labels",
		Add: []string{"Label_1"}, Operation: op, OperationKey: "k"})
	if rowOf(rows, "add asked") != "Label_1" || rowOf(rows, "effect") != "google.mail.labels" || rowOf(rows, "message") != "18c0ffee" {
		t.Errorf("rows = %v", rows)
	}
	if got := rowOf(rows, "remove asked"); !strings.HasPrefix(got, "<no row") {
		t.Errorf("a remove row for a list the change did not carry: %q", got)
	}
	rows = conversationTagRows(conversationTagOut{Conversation: testTagConversation(), Effect: "microsoft.mail.categories", Set: &[]string{}, Operation: op})
	if rowOf(rows, "set asked") != "(none)" {
		t.Errorf("clearing rows = %v", rows)
	}
}

// TestEffectVerbsAreSharedWrites: both verbs reach a provider and are
// idempotent under their key, like the other provider writes.
func TestEffectVerbsAreSharedWrites(t *testing.T) {
	a := newApp()
	for _, path := range []string{"conversation update", "conversation tag", "message reply-draft", "conversation unsubscribe"} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Fatalf("%s: not in the tree", path)
		}
		if got, _ := cli.GetSideEffect(c); got != cli.SideEffectWriteShared {
			t.Errorf("%s: side effect %q, want write-shared", path, got)
		}
		if got := c.Annotations[annotationCapability]; got != string(model.CapabilityExtensionExecute) {
			t.Errorf("%s: capability %q", path, got)
		}
	}
	// conversation label stays local: it is no provider verb.
	if _, gated := find(a.r.Cmd, "conversation label").Annotations[annotationCapability]; gated {
		t.Error("conversation label carries a capability: it is uma's local label")
	}
}

// TestReplyDraftPlanNamesTheExternalEffect: the plan names the one
// recipient (the reply route), that nothing is sent, and, for a delete,
// that it cannot be undone; the change kind follows the flags.
func TestReplyDraftPlanNamesTheExternalEffect(t *testing.T) {
	conv := testTagConversation()
	for name, tc := range map[string]struct {
		d          desk.MailDraft
		kind       string
		reversible bool
		want       []string
	}{
		"create": {desk.MailDraft{Text: "hi"}, "create", true, []string{"create a draft reply", "peer@example.com", "not sent", "google.mail.reply_draft"}},
		"update": {desk.MailDraft{Text: "hi", Draft: "r-1"}, "update", true, []string{"update a draft reply", "not sent"}},
		"delete": {desk.MailDraft{Draft: "r-1", Delete: true}, "delete", false, []string{"delete a draft reply", "gone for good"}},
	} {
		data, _ := tc.d.Data()
		p := replyDraftPlan(desk.DraftPlan{
			Request:      model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal}, Action: model.Extension{Name: "google.mail.reply_draft", Version: 1, Data: data}},
			Conversation: conv, Draft: tc.d, Message: "18c0ffee",
		}, "key-1")
		if len(p.Effects) != 1 || p.Effects[0].Kind != tc.kind || p.Effects[0].Reversible != tc.reversible {
			t.Fatalf("%s: effects = %+v", name, p.Effects)
		}
		for _, want := range tc.want {
			if !strings.Contains(p.Effects[0].Detail, want) {
				t.Errorf("%s: effect detail lacks %q: %s", name, want, p.Effects[0].Detail)
			}
		}
		if _, cc := p.Args["cc"]; cc || p.Args["change_kind"] != tc.kind {
			t.Errorf("%s: args = %v", name, p.Args)
		}
	}
}

// TestReplyDraftFlags: the flags map to the change; a delete needs its
// draft and takes no text; no change without text.
func TestReplyDraftFlags(t *testing.T) {
	for args, want := range map[string]string{
		"--text hi":                     "create",
		"--text hi --draft r-1":         "update",
		"--draft r-1 --delete":          "delete",
		"--delete":                      "!",
		"":                              "!",
		"--draft r-1 --text x --delete": "!",
	} {
		c := find(newApp().r.Cmd, "message reply-draft")
		if err := c.ParseFlags(strings.Fields(args)); err != nil {
			t.Fatalf("%q: %v", args, err)
		}
		var d desk.MailDraft
		d.Text, _ = c.Flags().GetString(flagText)
		d.Draft, _ = c.Flags().GetString(flagDraft)
		d.Delete, _ = c.Flags().GetBool(flagDelete)
		err := desk.CheckMailDraft(d)
		switch {
		case want == "!" && err == nil:
			t.Errorf("%q accepted", args)
		case want != "!" && (err != nil || d.Verb() != want):
			t.Errorf("%q = %s, %v, want %s", args, d.Verb(), err, want)
		}
	}
}

// TestConversationUnsubscribePlanSaysItIsIrreversible: the preview says
// the request cannot be recalled, names the method and the rules the
// adapter applies (one-click conditions, no GET, no private host), and
// takes no URL.
func TestConversationUnsubscribePlanSaysItIsIrreversible(t *testing.T) {
	data, _ := json.Marshal(map[string]string{"method": "auto"})
	p := conversationUnsubscribePlan(desk.UnsubscribePlan{
		Request:      model.ExecuteExtension{Audience: model.Audience{Visibility: model.Internal}, Action: model.Extension{Name: "google.mail.unsubscribe", Version: 1, Data: data}},
		Conversation: testTagConversation(), Method: "auto", Message: "18c0ffee",
	}, "key-1")
	if len(p.Effects) != 1 || p.Effects[0].Reversible || p.Effects[0].Target != "mail:message 18c0ffee" {
		t.Fatalf("effects = %+v", p.Effects)
	}
	for _, want := range []string{"irreversible", "third party", "RFC 8058", "DKIM", "no GET", "private, loopback, link-local", "follows no redirect", "sender's own organization", "method auto"} {
		if !strings.Contains(p.Effects[0].Detail, want) {
			t.Errorf("effect detail lacks %q: %s", want, p.Effects[0].Detail)
		}
	}
	if p.Args["method"] != "auto" || !strings.Contains(p.Args["change"].(string), "google.mail.unsubscribe") {
		t.Errorf("args = %v", p.Args)
	}
	if _, takes := find(newApp().r.Cmd, "conversation unsubscribe").Flags().GetString("url"); takes == nil {
		t.Error("conversation unsubscribe takes a --url: the sender's headers alone name the target")
	}
}

// TestUnsubscribeHoldsWhileOtherExtensionVerbsStayRefused: while holding
// is on, a plugin actor's other extension.execute verbs (issue update,
// the modal verbs) have no hold path and stay refused by holdGuard; the
// unsubscribe goes through deskHeld, which answers for the hold itself.
func TestUnsubscribeHoldsWhileOtherExtensionVerbsStayRefused(t *testing.T) {
	a := newApp()
	a.r.Viper.Set(settings.KeyHeldEnabled, true)
	plugin := control.Caller{Actor: "plugin:triage"}
	if !model.IsCode(a.holdGuard(plugin, model.ActionExtensionExecute), model.Unsupported) {
		t.Fatal("extension.execute is hold-aware as an action: issue update and the modal verbs would skip review")
	}
	if holdAware[model.ActionExtensionExecute] {
		t.Fatal("extension.execute listed hold-aware")
	}
	src, err := os.ReadFile("conversationunsubscribe.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "a.deskHeld(ctx, c, model.ActionExtensionExecute") || !strings.Contains(string(src), "s.Hold(ctx, &p.Request.Command") {
		t.Error("conversation unsubscribe does not run through deskHeld and offer its write to the hold point")
	}
}
