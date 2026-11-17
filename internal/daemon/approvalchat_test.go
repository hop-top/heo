package daemon_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/model"
)

const (
	chatMember model.ActorID = "uid:2000"
	chatPlugin model.ActorID = "plugin:triage"
	chatRoom                 = "room-1"
)

// testLog reports the runtime's logs in the test's.
type testLog struct{ t *testing.T }

func (l testLog) Info(msg any, kv ...any)  { l.t.Log(append([]any{"info:", msg}, kv...)...) }
func (l testLog) Error(msg any, kv ...any) { l.t.Log(append([]any{"error:", msg}, kv...)...) }

// chatRig is a daemon whose approval conversation is a per-room inbox
// on the fake channel: the members speak in it through the channel's
// webhook, the service answers through uma and the workers, and a
// plugin actor holds an edit of a message in another inbox.
type chatRig struct {
	*rig
	hooks, opsHook string
	opsInbox       model.InboxRef
	workInbox      model.InboxRef
	conv           model.ConversationRef
	workMsgs       []model.Message // one target for each operation held
	identity       string          // ada's identity record ID
	chat           *supervised
	n              int
}

func newChatRig(t *testing.T, opts ...rigOption) *chatRig {
	t.Helper()
	return buildChatRig(t, chatOpts{}, opts...)
}

// chatOpts shapes the approval conversation's channel and config.
type chatOpts struct {
	// slack makes the ops channel one of type slack.chat (the fake, typed
	// so); threadReplies sets slack_thread_replies.
	slack, threadReplies bool
}

func buildChatRig(t *testing.T, co chatOpts, opts ...rigOption) *chatRig {
	t.Helper()
	var slackFake *adapterstest.Factory
	if co.slack {
		slackFake = adapterstest.New()
		slackFake.As(model.TypeSlackChat)
		slackFake.DeclareEffects()
		opts = append(opts, func(r *rig) { r.extra = append(r.extra, slackFake.Adapter()) })
	}
	// The fake declares edit before the engines are built. It names every
	// message it sends to a route alike, so one worker keeps the sends
	// in the order they were queued: the announcement holds the name.
	declares := func(r *rig) { r.fake.DeclareEffects() }
	r := newRig(t, "acme", append([]rigOption{
		withSet(daemon.KeyWorkersHeldSweepEvery, "50ms"), withSet(daemon.KeyWorkersConcurrency, 1), declares}, opts...)...)
	r.deps.Logger = testLog{t}
	// The service keeps the announcements' references beside the
	// plugins' directory, so a restart still knows them.
	r.deps.PluginsDir = filepath.Join(t.TempDir(), "plugins")
	r.deps.Authorizer = authz.New(authz.Config{
		Owner: []authz.Role{authz.RoleOperator, authz.RoleDesk},
		Grants: []authz.Grant{
			{Actor: chatMember, Roles: []authz.Role{authz.RoleOperator, authz.RoleDesk}},
			{Actor: chatPlugin, Roles: []authz.Role{authz.RoleDesk}},
		},
	}, "acme", deskActor, daemon.ServiceActor)
	var ops model.ChannelRef
	if co.slack {
		ext := adapterstest.Config{Account: "acct-ops"}.Configuration()
		ext.Name = string(model.TypeSlackChat) + ".config"
		ops = r.channelOf(model.Channel{Key: "ops", Type: model.TypeSlackChat, Configuration: ext})
	} else {
		ops = r.channel("ops", adapterstest.Config{Account: "acct-ops"})
	}
	work := r.channel("work", adapterstest.Config{Account: "acct-work"})
	c := &chatRig{rig: r, workInbox: r.inbox("work", work, "work")}
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-ops"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: ops, Name: "ops", Destinations: []string{"ops"},
		Selection: model.Selection{Mode: model.ModePerRoom},
	}})
	if err != nil {
		t.Fatalf("configure ops inbox: %v", err)
	}
	if c.opsInbox, err = acc.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	// Workers deliver what the service posts; the webhook takes what
	// the members say.
	base := r.run(5*time.Second, daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook)
	base.ready(daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook)
	c.hooks = "http://" + base.svcs[daemon.NameWebhook].(serve.Addressed).Addr() + daemon.WebhookPrefix
	c.opsHook = c.hooks + string(ops.ID) + adapterstest.InjectRoute
	workHook := c.hooks + string(work.ID) + adapterstest.InjectRoute

	// The conversation exists, and ada's identity, before the service
	// reads configuration.
	c.say(c.opsHook, adapterstest.Inject{Event: "setup-ada", Message: "pm-setup", From: "ada", Destination: "ops", Room: chatRoom, Text: "hello"})
	var setup model.Message
	waitFor(t, func() bool {
		ms := c.messages(c.opsInbox)
		if len(ms) == 0 {
			return false
		}
		setup = ms[0]
		return true
	})
	c.conv, c.identity = setup.Ref.Conversation, string(setup.Author.Identity.ID)
	for i := range 3 {
		c.say(workHook, adapterstest.Inject{Event: "setup-bob-" + strconv.Itoa(i), Message: "pm-work-" + strconv.Itoa(i),
			From: "bob-" + strconv.Itoa(i), Destination: "work", Text: "please review"})
	}
	waitFor(t, func() bool {
		c.workMsgs = c.messages(c.workInbox)
		return len(c.workMsgs) == 3
	})
	cv := map[string]any{
		"inbox": string(c.opsInbox.ID), "conversation": string(c.conv.ID),
		"members": []any{map[string]any{"identity": c.identity, "actor": string(chatMember)}},
	}
	if co.threadReplies {
		cv["slack_thread_replies"] = true
	}
	r.v.Set("held_operations.chat", map[string]any{"enabled": true, "conversations": []any{cv}})
	return c
}

// startChat runs the approval chat service, as `service start` does.
func (c *chatRig) startChat() {
	c.t.Helper()
	c.chat = c.run(5*time.Second, daemon.NameApprovalChat)
	c.chat.ready(daemon.NameApprovalChat)
}

func (c *chatRig) say(url string, in adapterstest.Inject) {
	c.t.Helper()
	body, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec,noctx // loopback test URL
	if err != nil {
		c.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("inject %s = %d", in.Event, resp.StatusCode)
	}
}

// member says text in the approval conversation as a sender of the
// channel; each call is a new provider event.
func (c *chatRig) member(from, text string) {
	c.t.Helper()
	c.n++
	c.say(c.opsHook, adapterstest.Inject{Event: "ev-" + from + "-" + strconv.Itoa(c.n), Message: "pm-" + from + "-" + strconv.Itoa(c.n),
		From: from, Destination: "ops", Room: chatRoom, Text: text})
}

// reply says text in the approval conversation as a reply to the
// provider message to (a thread or quote reply, as a channel reports it)
// under the provider message ID msg.
func (c *chatRig) reply(from, msg, to, text string) {
	c.t.Helper()
	c.n++
	c.say(c.opsHook, adapterstest.Inject{Event: "ev-" + msg, Message: msg, InReplyTo: to,
		From: from, Destination: "ops", Room: chatRoom, Text: text})
}

// annProvider is the provider ID the fake gives the announcement, the
// first message it sends to the room.
const annProvider = "fake-" + chatRoom

// delivered waits for the workers to have sent the announcement, which
// is when uma knows the provider's ID for it.
func (c *chatRig) delivered() {
	c.t.Helper()
	waitFor(c.t, func() bool {
		for _, m := range c.messages(c.opsInbox) {
			if m.Origin != nil && len(m.External) > 0 {
				return true
			}
		}
		return false
	})
}

func (c *chatRig) react(from, emoji string) {
	c.t.Helper()
	c.n++
	c.say(c.opsHook, adapterstest.Inject{Event: "ev-react-" + strconv.Itoa(c.n), Message: "fake-" + chatRoom, Kind: model.EnvelopeReactionAdded,
		From: from, Destination: "ops", Room: chatRoom, Text: emoji})
}

func (c *chatRig) messages(in model.InboxRef) []model.Message {
	c.t.Helper()
	p, err := c.client().Messages().List(c.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "test"}, Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
	if err != nil {
		c.t.Fatal(err)
	}
	return p.Items
}

// said returns the text of the service's messages in the approval
// conversation: the outbound ones.
func (c *chatRig) said() []string {
	var out []string
	for _, m := range c.messages(c.opsInbox) {
		if m.Origin != nil {
			out = append(out, desk.Text(m.Content))
		}
	}
	return out
}

func (c *chatRig) sawText(sub string) bool {
	for _, s := range c.said() {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// hold has the plugin actor propose an edit of the work message, held
// for expiry.
func (c *chatRig) hold(key string, expiry time.Duration) string {
	c.t.Helper()
	ctx := c.t.Context()
	client, err := c.seed.For(model.Principal{Tenant: c.tenant, Actor: chatPlugin})
	if err != nil {
		c.t.Fatal(err)
	}
	s := desk.Service{Client: client, Tenant: c.tenant, Actor: chatPlugin,
		Holds: &desk.Holds{Expiry: expiry, OnBehalfOf: deskActor}}
	m := c.workMsgs[0]
	c.workMsgs = c.workMsgs[1:]
	ep, err := s.PlanEdit(ctx, string(m.Ref.ID), string(m.Ref.Conversation.ID), m.Revision, "edited by the plugin", model.OperationID(key), channels.Submit)
	if err != nil {
		c.t.Fatal(err)
	}
	h, err := s.Hold(ctx, &ep.Request.Command, desk.HoldInput{Action: model.ActionMessageEdit, Verb: "edit", Class: "message",
		Channel: string(c.workInbox.ID), Target: m.Ref.Ref(), Preview: map[string]any{"text": "edited by the plugin"}})
	if err != nil || h == nil {
		c.t.Fatalf("hold = %v, %v", h, err)
	}
	acc, err := s.Edit(ctx, ep)
	acc, err = h.Settle(ctx, acc, err)
	if err != nil || acc.Operation.State != model.Held {
		c.t.Fatalf("propose %s = %v, %v", key, acc.Operation.State, err)
	}
	return string(acc.Operation.Ref.ID)
}

// operation reads an operation as uma holds it.
func (c *chatRig) operation(id string) model.Operation {
	c.t.Helper()
	op, err := c.client().Operations().Get(c.t.Context(), model.Lookup[model.OperationRef]{
		Call: model.Call{Correlation: "test"}, Ref: model.OperationRef{Tenant: c.tenant, ID: model.ID(id)}})
	if err != nil {
		c.t.Fatal(err)
	}
	return op
}

func (c *chatRig) announced(id string) bool {
	return c.sawText("Held operation " + id + " needs a decision")
}

// TestApprovalChatAnnouncesHeldOperation: a plugin actor's operation
// held in another inbox is announced once to the configured
// conversation, with its preview and the way to decide, by the daemon
// service reading uma's event log as its own actor.
func TestApprovalChatAnnouncesHeldOperation(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	var found string
	for _, s := range c.said() {
		if strings.Contains(s, id) {
			found = s
		}
	}
	for _, want := range []string{"kind: message.edit", "proposer: " + string(chatPlugin), "edited by the plugin", "approve #" + id} {
		if !strings.Contains(found, want) {
			t.Errorf("announcement lacks %q: %s", want, found)
		}
	}
	// Once: the held event and any redelivery announce it one time.
	time.Sleep(200 * time.Millisecond)
	n := 0
	for _, s := range c.said() {
		if strings.Contains(s, "needs a decision") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("announcements = %d, want 1", n)
	}
	if got := service(c.host.Status(), daemon.NameApprovalChat); got.State != daemon.StateReady {
		t.Errorf("service state = %q, want ready", got.State)
	}
	sc := service(c.host.Status(), daemon.NameApprovalChat).Scopes
	if len(sc) != 1 || sc[0].Scope != daemon.ApprovalChatScope || sc[0].State != daemon.ScopeReady {
		t.Errorf("scopes = %+v, want the ready subscription", sc)
	}
}

// TestApprovalChatMappedReplyApproves: a reply naming the operation from
// a member whose identity is mapped decides it as the mapped actor, and
// the announcement is closed to the outcome.
func TestApprovalChatMappedReplyApproves(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })

	c.member("ada", "approve #"+id+" looks fine")
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	op := c.operation(id)
	if op.Held == nil || op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want approved by %s", op.Held, chatMember)
	}
	waitFor(t, func() bool { return c.sawText("Held operation " + id + ": approved by " + string(chatMember)) })
	waitFor(t, func() bool { return !c.sawText("needs a decision") })
}

// TestApprovalChatReactionRejects: a thumbs-down on the announcement
// from a mapped member rejects the operation.
func TestApprovalChatReactionRejects(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	// The reaction names the announcement by the provider's ID, which
	// uma knows once the worker delivered it.
	waitFor(t, func() bool {
		for _, m := range c.messages(c.opsInbox) {
			if m.Origin != nil && len(m.External) > 0 {
				return true
			}
		}
		return false
	})
	c.react("ada", "👎")
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	op := c.operation(id)
	if op.Held == nil || op.Held.Decision != model.HeldRejected || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want rejected by %s", op.Held, chatMember)
	}
}

// TestApprovalChatUnmappedIgnored: a sender whose identity is not mapped
// decides nothing, by reply or by reaction; the events are handled in
// order, so the later decision of a mapped member shows the earlier
// ones changed nothing.
func TestApprovalChatUnmappedIgnored(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	first, second := c.hold("held-1", time.Hour), c.hold("held-2", time.Hour)
	waitFor(t, func() bool { return c.announced(first) && c.announced(second) })

	c.member("mallory", "approve #"+first)
	c.member("mallory", "reject #"+second)
	c.member("ada", "approve #"+second)
	waitFor(t, func() bool { return c.operation(second).State != model.Held })
	if op := c.operation(first); op.State != model.Held {
		t.Errorf("the unmapped sender decided the first operation: %s %+v", op.State, op.Held)
	}
	if op := c.operation(second); op.Held == nil || op.Held.Decision != model.HeldApproved {
		t.Errorf("second = %+v, want approved by the mapped member alone", op.Held)
	}
}

// TestApprovalChatExpiryClosesAnnouncement: an operation that expires
// undecided has its announcement edited to say so.
func TestApprovalChatExpiryClosesAnnouncement(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", 1500*time.Millisecond)
	waitFor(t, func() bool { return c.announced(id) })
	waitFor(t, func() bool { return c.operation(id).Held != nil && c.operation(id).Held.Decision == model.HeldExpired })
	waitFor(t, func() bool { return c.sawText("Held operation " + id + ": expired.") })
	waitFor(t, func() bool { return !c.sawText("needs a decision") })
}

// TestApprovalChatApproveWorksAfterRestart: a restarted service resumes
// its subscription (it does not announce again) and still decides
// `approve #<operation>`; the announcement it made before is edited to
// the outcome.
func TestApprovalChatApproveWorksAfterRestart(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	c.delivered()
	if res := c.chat.stop(); res.Err != nil {
		t.Fatalf("stop: %v", res.Err)
	}
	if got := service(c.host.Status(), daemon.NameApprovalChat).State; got != daemon.StateStopped {
		t.Errorf("state after stop = %q", got)
	}
	c.startChat()
	c.member("ada", "approve #"+id)
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	if op := c.operation(id); op.Held == nil || op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want approved by %s", op.Held, chatMember)
	}
	// The pre-restart announcement is edited to the outcome, and the
	// answer is posted: two messages, nothing announced again.
	waitFor(t, func() bool { return !c.sawText("needs a decision") })
	waitFor(t, func() bool { return c.sawText("Held operation " + id + ": approved by " + string(chatMember)) })
	if got := c.said(); len(got) != 2 {
		t.Errorf("messages = %q, want the closed announcement and the answer only", got)
	}
}

// TestApprovalChatBareReplyApproves: `approve` alone, as a reply to the
// announcement, decides that operation as the mapped actor, once.
func TestApprovalChatBareReplyApproves(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	c.delivered()
	c.reply("ada", "pm-r1", annProvider, "approve looks fine")
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	op := c.operation(id)
	if op.Held == nil || op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want approved by %s", op.Held, chatMember)
	}
	waitFor(t, func() bool { return c.sawText("Held operation " + id + ": approved by " + string(chatMember)) })
	waitFor(t, func() bool { return !c.sawText("needs a decision") })
	// A second reply to the same announcement changes nothing.
	c.reply("ada", "pm-r2", annProvider, "reject")
	c.quiet("held-2")
	if op := c.operation(id); op.Held == nil || op.Held.Decision != model.HeldApproved {
		t.Errorf("a second reply changed the decision: %+v", op.Held)
	}
}

// quiet holds a further operation and waits for its announcement: the
// service reads events in order, so what was said before it is handled.
func (c *chatRig) quiet(key string) string {
	c.t.Helper()
	id := c.hold(key, time.Hour)
	waitFor(c.t, func() bool { return c.announced(id) })
	return id
}

// TestApprovalChatBareReplyGates: a bare reply decides only from a
// mapped identity, only on the announcement; one from an unmapped
// sender, one to any other message and an edit of a message into
// "approve" decide nothing.
func TestApprovalChatBareReplyGates(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	c.delivered()

	c.reply("mallory", "pm-m1", annProvider, "approve") // unmapped
	c.reply("ada", "pm-o1", "pm-setup", "approve")      // to another message
	c.reply("ada", "pm-e1", annProvider, "sounds good") // chatter, edited below
	c.say(c.opsHook, adapterstest.Inject{Event: "ev-pm-e1-edit", Message: "pm-e1", Kind: model.EnvelopeMessageEdited,
		From: "ada", Destination: "ops", Room: chatRoom, Text: "approve"})
	c.quiet("held-2")
	if op := c.operation(id); op.State != model.Held {
		t.Fatalf("a gated reply decided: %s %+v", op.State, op.Held)
	}
	// The mapped member's reply to the announcement still does.
	c.reply("ada", "pm-r1", annProvider, "approve")
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	if op := c.operation(id); op.Held == nil || op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want approved by %s", op.Held, chatMember)
	}
}

// TestApprovalChatRefsSurviveRestart: after a restart a bare reply to
// the announcement made before it decides, and the announcement is
// edited to the outcome.
func TestApprovalChatRefsSurviveRestart(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	c.delivered()
	if res := c.chat.stop(); res.Err != nil {
		t.Fatalf("stop: %v", res.Err)
	}
	c.startChat()
	c.reply("ada", "pm-r1", annProvider, "approve")
	waitFor(t, func() bool { return c.operation(id).State != model.Held })
	if op := c.operation(id); op.Held == nil || op.Held.DecidedBy != chatMember {
		t.Errorf("held = %+v, want decided by %s", op.Held, chatMember)
	}
	waitFor(t, func() bool { return !c.sawText("needs a decision") })
	if got := c.said(); len(got) != 2 {
		t.Errorf("messages = %q, want the closed announcement and the answer only", got)
	}
}

// TestApprovalChatOffRunsNothing: without held_operations.chat.enabled
// the service is ready, idle, and holds no store.
func TestApprovalChatOffRunsNothing(t *testing.T) {
	r := newRig(t, "acme")
	s := r.run(5*time.Second, daemon.NameApprovalChat)
	s.ready(daemon.NameApprovalChat)
	st := r.host.Status()
	if got := service(st, daemon.NameApprovalChat); got.State != daemon.StateReady || len(got.Scopes) != 0 {
		t.Errorf("service = %+v, want ready with no scope", got)
	}
	if st.Storage.State != daemon.StorageClosed {
		t.Errorf("storage = %q: an idle service opened the store", st.Storage.State)
	}
}

// TestApprovalChatSlackThreadReplies: on Slack every reply in the
// announcement's thread arrives as a reply to it, so a bare one decides
// nothing unless slack_thread_replies is set; the explicit form decides
// either way.
func TestApprovalChatSlackThreadReplies(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		c := buildChatRig(t, chatOpts{slack: true})
		c.startChat()
		id := c.hold("held-1", time.Hour)
		waitFor(t, func() bool { return c.announced(id) })
		c.delivered()
		c.reply("ada", "pm-r1", annProvider, "approve")
		c.quiet("held-2")
		if op := c.operation(id); op.State != model.Held {
			t.Fatalf("a Slack thread reply decided: %s %+v", op.State, op.Held)
		}
		c.reply("ada", "pm-r2", annProvider, "approve #"+id)
		waitFor(t, func() bool { return c.operation(id).State != model.Held })
		if op := c.operation(id); op.Held == nil || op.Held.DecidedBy != chatMember {
			t.Errorf("held = %+v, want decided by %s", op.Held, chatMember)
		}
	})
	t.Run("on", func(t *testing.T) {
		c := buildChatRig(t, chatOpts{slack: true, threadReplies: true})
		c.startChat()
		id := c.hold("held-1", time.Hour)
		waitFor(t, func() bool { return c.announced(id) })
		c.delivered()
		c.reply("ada", "pm-r1", annProvider, "approve")
		waitFor(t, func() bool { return c.operation(id).State != model.Held })
		if op := c.operation(id); op.Held == nil || op.Held.DecidedBy != chatMember {
			t.Errorf("held = %+v, want decided by %s", op.Held, chatMember)
		}
	})
}

// TestApprovalChatReplyAfterCloseIsIgnored: a reply to the announcement
// of an operation that is decided posts nothing and decides nothing,
// also after a restart.
func TestApprovalChatReplyAfterCloseIsIgnored(t *testing.T) {
	c := newChatRig(t)
	c.startChat()
	id := c.hold("held-1", time.Hour)
	waitFor(t, func() bool { return c.announced(id) })
	c.delivered()
	c.reply("ada", "pm-r1", annProvider, "approve")
	waitFor(t, func() bool { return c.sawText("Held operation " + id + ": approved by " + string(chatMember)) })
	if res := c.chat.stop(); res.Err != nil {
		t.Fatalf("stop: %v", res.Err)
	}
	c.startChat()
	before := len(c.said())
	c.reply("ada", "pm-r2", annProvider, "reject")
	c.quiet("held-2")
	if got := c.said(); len(got) != before+1 { // the later announcement only
		t.Errorf("messages = %q, want none for the late reply", got)
	}
	if op := c.operation(id); op.Held == nil || op.Held.Decision != model.HeldApproved {
		t.Errorf("a late reply changed the decision: %+v", op.Held)
	}
}
