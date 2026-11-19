package cmd

import (
	"os"
	"strings"
	"testing"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

// TestStateVerbsAreSharedWrites: the nine provider state verbs are shared
// writes, idempotent under their key, each gated by the capability that
// carries it, and each takes an optional --revision.
func TestStateVerbsAreSharedWrites(t *testing.T) {
	a := newApp()
	for _, v := range desk.StateVerbs {
		path := "conversation " + string(v)
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Fatalf("%s: not in the tree", path)
		}
		if got, _ := cli.GetSideEffect(c); got != cli.SideEffectWriteShared {
			t.Errorf("%s side effect = %v, want write-shared", path, got)
		}
		if got, _ := cli.GetIdempotency(c); got != cli.IdempotencyConditional {
			t.Errorf("%s idempotency = %v, want conditional", path, got)
		}
		want, _ := v.Capability()
		if got := c.Annotations[annotationCapability]; got != string(want) {
			t.Errorf("%s capability = %q, want %s", path, got, want)
		}
		if f := c.Flags().Lookup(flagRevision); f == nil || strings.Contains(f.Usage, "(required)") {
			t.Errorf("%s --revision = %+v, want an optional flag", path, f)
		}
		if c.Flags().Lookup("url") != nil {
			t.Errorf("%s takes a --url", path)
		}
	}
}

// TestStateFlagsMapToTheChange: the flags map to the change and its usage
// rules; the flag verbs default to the star, a move names one way.
func TestStateFlagsMapToTheChange(t *testing.T) {
	for _, tc := range []struct {
		verb  model.StateVerb
		args  string
		asked string
	}{
		{model.StateVerbArchive, "", "archive"},
		{model.StateVerbFlag, "", "flag star"},
		{model.StateVerbUnflag, "--kind important", "unflag important"},
		{model.StateVerbMove, "--role junk", "move to the junk role"},
		{model.StateVerbMove, "--to Label_9", "move to Label_9"},
		{model.StateVerbMove, "--add Label_12,Label_13 --remove INBOX", "move add Label_12, Label_13; remove INBOX"},
		{model.StateVerbFlag, "--kind pin", "!"},
		{model.StateVerbMove, "", "!"},
		{model.StateVerbMove, "--to Label_9 --add Label_12", "!"},
		{model.StateVerbMove, "--role spam", "!"},
	} {
		c := find(newApp().r.Cmd, "conversation "+string(tc.verb))
		if err := c.ParseFlags(strings.Fields(tc.args)); err != nil {
			t.Fatalf("%s %q: %v", tc.verb, tc.args, err)
		}
		ch, err := stateChangeOf(c, tc.verb)
		switch {
		case tc.asked == "!" && err == nil:
			t.Errorf("%s %q accepted", tc.verb, tc.args)
		case tc.asked != "!" && (err != nil || ch.Asked() != tc.asked):
			t.Errorf("%s %q = %q, %v, want %q", tc.verb, tc.args, ch.Asked(), err, tc.asked)
		}
	}
}

// TestConversationStatePlanNamesTheExternalEffect: the plan says it is an
// external change at the provider, queued and not applied, that uma's
// status, labels and read marker are never touched, and, from a preview,
// the scope the channel reports; a shared scope says everyone sees it.
func TestConversationStatePlanNamesTheExternalEffect(t *testing.T) {
	conv := testTagConversation()
	for scope, wantShared := range map[model.StateScope]bool{model.ScopeAccount: false, model.ScopeShared: true} {
		p := conversationStatePlan(desk.StatePlan{
			Command:      model.Command{ExpectedRevision: "rev_1"},
			Change:       desk.StateChange{Verb: model.StateVerbTrash},
			Conversation: conv,
			Limits:       &model.StateLimits{Scope: scope},
		}, "key-1")
		if len(p.Effects) != 1 || p.Effects[0].Kind != "update" || !p.Effects[0].Reversible || p.Effects[0].Target != "provider_state:conv_1" {
			t.Fatalf("%s: effects = %+v", scope, p.Effects)
		}
		d := p.Effects[0].Detail
		for _, want := range []string{"external", "trash", "uma records it once applied"} {
			if !strings.Contains(d, want) {
				t.Errorf("%s: effect detail lacks %q: %s", scope, want, d)
			}
		}
		if strings.Contains(d, "shared") != wantShared {
			t.Errorf("%s: detail = %s", scope, d)
		}
		delivery, _ := p.Args["delivery"].(string)
		for _, want := range []string{"queued, not applied", "your read marker are never touched"} {
			if !strings.Contains(delivery, want) {
				t.Errorf("%s: delivery lacks %q: %s", scope, want, delivery)
			}
		}
		if p.Args["scope"] != string(scope) || p.Args["expected_revision"] != model.Revision("rev_1") || p.Args["operation_key"] != "key-1" {
			t.Errorf("%s: args = %v", scope, p.Args)
		}
	}
	// A submission's plan, made for the hold point, has no scope when none was read.
	p := conversationStatePlan(desk.StatePlan{Change: desk.StateChange{Verb: model.StateVerbArchive}, Conversation: conv}, "")
	if _, ok := p.Args["scope"]; ok {
		t.Errorf("a plan with no limits read names a scope: %v", p.Args)
	}
}

// TestStateVerbsHoldThroughDeskHeld: while holding is on, a plugin
// actor's provider state change is not refused by holdGuard, because its
// verbs go through deskHeld and offer their write to the hold point
// themselves; both scopes it may be authorized as stay unlisted as
// hold-aware actions, as extension.execute is.
func TestStateVerbsHoldThroughDeskHeld(t *testing.T) {
	a := newApp()
	a.r.Viper.Set(settings.KeyHeldEnabled, true)
	plugin := control.Caller{Actor: "plugin:triage"}
	for _, action := range []string{model.ActionConversationStateAccount, model.ActionConversationStateShared} {
		if !model.IsCode(a.holdGuard(plugin, action), model.Unsupported) {
			t.Errorf("%s is hold-aware as an action: a verb that skipped deskHeld would skip review", action)
		}
	}
	src, err := os.ReadFile("conversationstate.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "a.deskHeld(ctx, c, model.ActionConversationStateAccount") || !strings.Contains(string(src), "s.Hold(ctx, &p.Command") {
		t.Error("the provider state verbs do not run through deskHeld and offer their write to the hold point")
	}
}

// TestStateVerbsTakeRepeatableMessages: every provider state verb takes
// --message, repeatable, into the change; an empty one is a usage error.
func TestStateVerbsTakeRepeatableMessages(t *testing.T) {
	for _, v := range desk.StateVerbs {
		c := find(newApp().r.Cmd, "conversation "+string(v))
		args := []string{"--message", "msg_1", "--message", "msg_2"}
		switch v {
		case model.StateVerbMove:
			args = append(args, "--role", "junk")
		}
		if err := c.ParseFlags(args); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		ch, err := stateChangeOf(c, v)
		if err != nil || strings.Join(ch.Messages, ",") != "msg_1,msg_2" || !strings.Contains(ch.Asked(), "on message msg_1, msg_2") {
			t.Errorf("%s: change = %+v, %v; want both messages", v, ch, err)
		}
		c = find(newApp().r.Cmd, "conversation "+string(v))
		if err := c.ParseFlags(append(args, "--message", "")); err != nil {
			t.Fatal(err)
		}
		if _, err := stateChangeOf(c, v); err == nil {
			t.Errorf("%s: an empty --message was accepted", v)
		}
		// Alone, an empty value reads back as no value: it must not widen the change.
		c = find(newApp().r.Cmd, "conversation "+string(v))
		if err := c.ParseFlags(append([]string{"--message", ""}, args[4:]...)); err != nil {
			t.Fatal(err)
		}
		if _, err := stateChangeOf(c, v); err == nil {
			t.Errorf("%s: a lone empty --message was accepted as the whole conversation", v)
		}
	}
}

// TestConversationStatePlanListsItems: a preview lists the messages the
// change resolves to, and says a Gmail change without --message is
// thread-wide.
func TestConversationStatePlanListsItems(t *testing.T) {
	conv := testTagConversation()
	items := []desk.MessageItem{{Message: "msg_1", Provider: "gm1"}, {Message: "msg_2", Provider: "gm2"}}
	p := conversationStatePlan(desk.StatePlan{
		Change: desk.StateChange{Verb: model.StateVerbArchive}, Conversation: conv, Items: items, ThreadWide: true,
	}, "")
	d := p.Effects[0].Detail
	if got, ok := p.Args["items"].([]desk.MessageItem); !ok || len(got) != 2 || p.Args["thread_wide"] != true ||
		!strings.Contains(d, "all 2 provider message") || !strings.Contains(d, "thread-wide") {
		t.Errorf("unscoped plan: args = %v, detail = %s", p.Args, d)
	}
	p = conversationStatePlan(desk.StatePlan{
		Change: desk.StateChange{Verb: model.StateVerbArchive, Messages: []string{"msg_2"}}, Conversation: conv, Items: items[1:],
	}, "")
	d = p.Effects[0].Detail
	if _, ok := p.Args["thread_wide"]; ok || !strings.Contains(d, "limited to the 1 message(s) named with --message") || strings.Contains(d, "thread-wide") {
		t.Errorf("targeted plan: args = %v, detail = %s", p.Args, d)
	}
}

// TestTagAndUnsubscribeTakeAMessage: both take one --message, and their
// help says the default target is the first message, not the latest.
func TestTagAndUnsubscribeTakeAMessage(t *testing.T) {
	for _, path := range []string{"conversation tag", "conversation unsubscribe"} {
		c := find(newApp().r.Cmd, path)
		if f := c.Flags().Lookup(flagMessage); f == nil || !strings.Contains(f.Usage, "first") {
			t.Errorf("%s --message = %+v, want a flag naming the default as the first message", path, f)
		}
		if strings.Contains(c.Long, "the latest the peer wrote") || !strings.Contains(c.Long, "first message") {
			t.Errorf("%s help does not state the default target truthfully:\n%s", path, c.Long)
		}
	}
	c := find(newApp().r.Cmd, "conversation tag")
	if err := c.ParseFlags([]string{"--add", "X", "--message", "msg_9"}); err != nil {
		t.Fatal(err)
	}
	if got := mailTagOf(c); got.Message != "msg_9" {
		t.Errorf("tag message = %q, want msg_9", got.Message)
	}
	for _, path := range []string{"conversation tag", "conversation unsubscribe"} {
		c := find(newApp().r.Cmd, path)
		if err := c.ParseFlags([]string{"--message", ""}); err != nil {
			t.Fatal(err)
		}
		if _, err := singleMessageOf(c); err == nil {
			t.Errorf("%s: an empty --message was read as the default target", path)
		}
	}
}
