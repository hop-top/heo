package daemon_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/model"
)

func teamsRoom(s *teamsSite, conv string) model.ExternalRef {
	return model.ExternalRef{Channel: s.ch, Namespace: teams.Namespace, Kind: model.RefKindRoom, ID: conv}
}

// TestTeamsProviderVerbs: through heo's workers, the real microsoft.teams
// adapter and heo's HTTP seam, against the Bot Connector in process: a
// first message to a conversation, a reply, an edit, a reaction set and
// cleared and a deletion each reach the Connector as the documented call
// with the bot's access token as the bearer, minted from the client secret in heo's backend;
// an edit of a person's message is refused by Teams and ends rejected,
// never as a new message; an emoji Teams has no reaction for is refused
// before any call; a card press cannot be answered with the respond verb,
// which the adapter does not declare.
func TestTeamsProviderVerbs(t *testing.T) {
	creds := teamsCreds(t, teamsSecret)
	s := newTeamsSite(t, creds, daemon.NameWebhook, daemon.NameWorkers)
	ctx := t.Context()
	c := s.r.client()
	text := func(v string) model.Content { return desk.TextContent(v) }

	// A first message to a conversation that never wrote in.
	acc, err := c.Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start"), Inbox: s.inbox, Recipient: teamsRoom(s, teamsGroup), Content: text("welcome to the chat")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op := s.settled(acc); op.State != model.ProviderAccepted {
		t.Fatalf("start = %s %+v", op.State, op.Failure)
	}
	sent := s.bot.called("send")
	if len(sent) != 1 || sent[0].Host != teamsConnectorHost || sent[0].Auth != "Bearer "+teamsToken || sent[0].Conversation != teamsGroup ||
		sent[0].Body["type"] != "message" || sent[0].Body["text"] != "welcome to the chat" || sent[0].Body["textFormat"] != "plain" {
		t.Fatalf("start sent %+v, want one plain message to the conversation with the bot's access token", sent)
	}
	if !creds.asked(teamsRef, composition.PurposeTeamsClientSecret) {
		t.Error("the bot's client secret was never asked of heo's backend")
	}
	if creds.asked(teamsRef, teams.CredentialPurposeConnector) {
		t.Error("the Bot Connector token was asked of heo's backend, which holds none: heo mints it")
	}

	// Someone writes in the channel; a reply answers the conversation.
	if code, body := s.activity(tmMessage("1760000300000", teamsPerson, teamsChannel, "channel", "hello team")); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	s.bot.person(teamsChannel, "1760000300000", "hello team")
	conv := s.roomConversation(teamsChannel)
	reply, err := c.Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: conv.Ref, Content: text("on it")})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if op := s.settled(reply); op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s %+v", op.State, op.Failure)
	}
	mine := s.byText(conv.Ref, "on it")
	id := providerID(mine)
	if !strings.HasPrefix(id, teamsChannel+"|") {
		t.Fatalf("the reply names provider message %q, want <conversation>|<activity>", id)
	}
	if got := s.bot.posted(id); got == nil || got.Text != "on it" {
		t.Fatalf("Teams holds %+v for %s", got, id)
	}

	// Edit, react, clear, delete: each its own Connector call, on the
	// activity the provider named.
	edited, err := c.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "edit-1", ExpectedRevision: mine.Revision},
		Message: mine.Ref, Content: text("on it, fixed"),
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if op := s.settled(edited); op.State != model.ProviderAccepted {
		t.Fatalf("edit = %s %+v", op.State, op.Failure)
	}
	activity := strings.TrimPrefix(id, teamsChannel+"|")
	if up := s.bot.called("update"); len(up) != 1 || up[0].Conversation != teamsChannel || up[0].Activity != activity || up[0].Body["text"] != "on it, fixed" {
		t.Fatalf("edit sent %+v", up)
	}
	react := func(op string, remove bool) {
		t.Helper()
		acc, err := c.Messages().React(ctx, model.Reaction{Command: s.r.cmd(op), Message: mine.Ref, Emoji: "\U0001F44D", Remove: remove})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got := s.settled(acc); got.State != model.ProviderAccepted {
			t.Fatalf("%s = %s %+v", op, got.State, got.Failure)
		}
	}
	react("react-add", false)
	if got := s.bot.posted(id); len(got.Reactions) != 1 || got.Reactions[0] != "like" {
		t.Fatalf("reactions after add = %+v, want Teams' like", got)
	}
	react("react-remove", true)
	if got := s.bot.posted(id); len(got.Reactions) != 0 {
		t.Fatalf("reactions after remove = %+v", got)
	}
	if rc := s.bot.called("react"); len(rc) != 1 || rc[0].Activity != activity || s.bot.called("unreact")[0].Reaction != "like" {
		t.Fatalf("reaction calls = %+v", rc)
	}

	// An emoji Teams names no reaction type for is refused before any call.
	calls := s.bot.total()
	bad, err := c.Messages().React(ctx, model.Reaction{Command: s.r.cmd("react-bad"), Message: mine.Ref, Emoji: "\U0001F389"})
	if err != nil {
		t.Fatalf("react with an unlisted emoji: %v", err)
	}
	if op := s.settled(bad); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unsupported {
		t.Fatalf("unlisted emoji = %s %+v, want rejected as unsupported", op.State, op.Failure)
	}
	if s.bot.total() != calls {
		t.Error("a refused emoji reached Teams")
	}

	mine = s.byText(conv.Ref, "on it, fixed")
	del, err := c.Messages().Delete(ctx, model.DeleteMessage{Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "delete-1", ExpectedRevision: mine.Revision}, Message: mine.Ref})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if op := s.settled(del); op.State != model.ProviderAccepted {
		t.Fatalf("delete = %s %+v", op.State, op.Failure)
	}
	if got := s.bot.posted(id); got == nil || !got.Deleted {
		t.Fatalf("Teams holds %+v after the deletion", got)
	}
	for _, op := range []string{"send", "update", "react", "unreact", "delete"} {
		for _, call := range s.bot.called(op) {
			if call.Host != teamsConnectorHost || call.Auth != "Bearer "+teamsToken {
				t.Errorf("%s went to %s with %q", op, call.Host, call.Auth)
			}
		}
	}

	// A person's message is not the bot's to change: Teams refuses and the
	// edit ends rejected; nothing is posted instead.
	theirs := s.byText(conv.Ref, "hello team")
	posts := len(s.bot.called("send"))
	refused, err := c.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "edit-2", ExpectedRevision: theirs.Revision},
		Message: theirs.Ref, Content: text("rewritten"),
	})
	if err != nil {
		t.Fatalf("edit of another's message: %v", err)
	}
	if op := s.settled(refused); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unauthorized {
		t.Fatalf("edit of a person's message = %s %+v, want rejected as unauthorized", op.State, op.Failure)
	}
	if len(s.bot.called("send")) != posts {
		t.Error("a refused edit posted a new message")
	}

	// A card press is recorded, and cannot be answered by the respond
	// verb: the adapter does not declare it.
	if code, _ := s.activity(tmPress("1760000300001", teamsPerson, teamsChannel, "channel", activity, "approve", "42")); code != http.StatusOK {
		t.Fatalf("press = %d", code)
	}
	ins := s.interactions()
	if len(ins) != 1 || ins[0].Action != "approve" || ins[0].Arguments != "42" {
		t.Fatalf("interactions = %+v, want the press, its string data as the arguments", ins)
	}
	calls = s.bot.total()
	_, err = c.Interactions().Respond(ctx, model.Respond{
		Command: s.r.cmd("answer"), Interaction: ins[0].Ref, Mode: model.ResponseModeMessage,
		Audience: model.Audience{Visibility: model.Public}, Content: text("approved"), ExpiresAt: time.Now().Add(time.Minute),
	})
	if !model.IsCode(err, model.Unsupported) {
		t.Errorf("respond to a card press = %v, want unsupported", err)
	}
	if s.bot.total() != calls {
		t.Error("a refused respond reached Teams")
	}
}

// TestTeamsConnectorCredential: heo mints the Bot Connector token from the
// bot's client secret, which is all the operator stores. Sends share the
// one token minted for the first, asked of the tenant's token endpoint
// with the app, the secret and the Bot Framework scope. A token the
// service refuses (401) is replaced by a new mint, once, and the send
// goes through with it; a secret Entra refuses fails the send as
// unauthorized naming the provider's code and never the secret, and the
// store remembers it was rejected; the corrected secret is used by the next
// send, with no restart and without waiting out the backoff. No secret
// stored fails the send as not found before any request leaves. Receiving
// needs none of it.
func TestTeamsConnectorCredential(t *testing.T) {
	var n int
	send := func(s *teamsSite) model.Operation {
		t.Helper()
		n++
		acc, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + strconv.Itoa(n)), Inbox: s.inbox, Recipient: teamsRoom(s, teamsGroup), Content: desk.TextContent("hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return s.settled(acc)
	}

	ok := newTeamsSite(t, teamsCreds(t, teamsSecret), daemon.NameWebhook, daemon.NameWorkers)
	for range 2 {
		if op := send(ok); op.State != model.ProviderAccepted {
			t.Fatalf("send = %s %+v", op.State, op.Failure)
		}
	}
	if sent := ok.bot.called("send"); len(sent) != 2 || sent[0].Auth != "Bearer "+teamsToken || sent[1].Auth != "Bearer "+teamsToken {
		t.Errorf("sends carried %+v, want the minted token", sent)
	}
	mints := ok.bot.minted()
	if len(mints) != 1 {
		t.Fatalf("%d mints for two sends, want 1: the token is cached", len(mints))
	}
	if m := mints[0]; m.Path != "/"+teamsTenantID+"/oauth2/v2.0/token" || m.Method != http.MethodPost || m.Grant != "client_credentials" ||
		m.Scope != "https://api.botframework.com/.default" || m.App != teamsAppID || m.Secret != teamsSecret || m.Authorization != "" {
		t.Errorf("mint = %+v, want the documented client credentials request for the channel's tenant and app", m)
	}

	// The service stops accepting the token it was given (revoked, or the
	// service rotated its keys): the 401 makes heo mint a new one.
	ok.bot.setConnectorToken("a-newer-connector-token")
	ok.clock.advance(time.Minute)
	if op := send(ok); op.State != model.ProviderAccepted {
		t.Fatalf("send after the service moved on = %s %+v", op.State, op.Failure)
	}
	if sent := ok.bot.called("send"); len(sent) != 4 || sent[2].Auth != "Bearer "+teamsToken || sent[3].Auth != "Bearer a-newer-connector-token" {
		t.Errorf("sends carried %+v, want the refused token and then the new one", sent)
	}
	if got := ok.bot.mintCount(); got != 2 {
		t.Errorf("%d mints, want 2", got)
	}

	// A secret Entra refuses.
	wrong, store := teamsRotatingCreds(t, "a-secret-entra-refuses")
	bad := newTeamsSite(t, wrong, daemon.NameWebhook, daemon.NameWorkers)
	op := send(bad)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unauthorized {
		t.Fatalf("a refused secret = %s %+v, want failed as unauthorized", op.State, op.Failure)
	}
	for _, want := range []string{"invalid_client", "AADSTS7000215"} {
		if !strings.Contains(op.Failure.SafeMessage, want) {
			t.Errorf("the failure says %q, want the provider's %s", op.Failure.SafeMessage, want)
		}
	}
	if text := op.Failure.SafeMessage; strings.Contains(text, "a-secret-entra-refuses") || strings.Contains(text, teamsToken) {
		t.Errorf("a secret or a token leaked: %q", text)
	}
	if n := bad.bot.total(); n != 0 {
		t.Errorf("a refused secret reached the Bot Connector %d times", n)
	}
	if !wrong.askedFresh(teamsRef, composition.PurposeTeamsClientSecret) {
		t.Error("the store was not told the secret was rejected")
	}
	// The operator stores the right one: the next send mints, restart-free
	// and with no wait for the failed mint's backoff.
	store(teamsSecret)
	if op := send(bad); op.State != model.ProviderAccepted {
		t.Errorf("after the rotation = %s %+v, want the new secret used", op.State, op.Failure)
	}
	if mints := bad.bot.minted(); len(mints) < 2 || mints[len(mints)-1].Secret != teamsSecret {
		t.Errorf("mints %+v, want the last made with the rotated secret", mints)
	}

	none := newTeamsSite(t, teamsCreds(t, ""), daemon.NameWebhook, daemon.NameWorkers)
	if op := send(none); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.NotFound {
		t.Fatalf("no secret stored = %s %+v, want failed as not found", op.State, op.Failure)
	}
	if n := none.bot.total() + none.bot.mintCount(); n != 0 {
		t.Errorf("a send without a secret reached Microsoft %d times", n)
	}
	if code, body := none.activity(tmMessage("1760000400001", teamsPerson, teamsChannel, "channel", "received")); code != http.StatusOK {
		t.Errorf("receiving with no secret stored = %d %q, want 200", code, body)
	}
}

// TestTeamsConnectorTokenEndpointFailureIsRetried: Entra answering 503 to
// the mint fails the attempt as transient, naming the token endpoint, before
// anything reaches the Bot Connector; the worker retries the same send, heo
// mints again once its backoff is over, and the message is posted once.
func TestTeamsConnectorTokenEndpointFailureIsRetried(t *testing.T) {
	s := newTeamsSite(t, teamsCreds(t, teamsSecret), daemon.NameWebhook, daemon.NameWorkers)
	s.bot.failEntra(http.StatusServiceUnavailable)
	acc, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
		Command: s.r.cmd("start-down"), Inbox: s.inbox, Recipient: teamsRoom(s, teamsGroup), Content: desk.TextContent("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var retried bool
	deadline := time.Now().Add(30 * time.Second)
	var op model.Operation
	for time.Now().Before(deadline) {
		op = s.r.operation(acc.Operation.Ref)
		if op.State == model.RetryPending && op.Failure != nil {
			retried = true
			if op.Failure.Code != model.Transient || !strings.Contains(op.Failure.SafeMessage, "token endpoint: HTTP 503") {
				t.Fatalf("the failed attempt = %+v, want transient, naming the token endpoint's 503", op.Failure)
			}
			if n := s.bot.total(); n != 0 {
				t.Fatalf("the Bot Connector was called %d times before a token was minted", n)
			}
		}
		if op.State == model.ProviderAccepted || op.State == model.Failed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if op.State != model.ProviderAccepted || !retried {
		t.Fatalf("send = %s %+v (retry seen: %v), want a retry and then accepted", op.State, op.Failure, retried)
	}
	if got, sent := s.bot.mintCount(), len(s.bot.called("send")); got != 2 || sent != 1 {
		t.Errorf("%d mints and %d sends, want 2 mints and the message posted once", got, sent)
	}
}

// TestTeamsSelectionRoutes: one bot serves each selection an inbox may
// use, and an answer goes where the mode says. Per thread, a channel
// thread is the conversation and the answer is posted into it, as
// <channel>;messageid=<root>. Per contact, one user is the conversation
// whichever chat they wrote in, the answer goes to the personal chat
// with them, which the Connector creates, and a user the app is not
// installed for cannot be written to first.
func TestTeamsSelectionRoutes(t *testing.T) {
	ctx := t.Context()
	text := func(v string) model.Content { return desk.TextContent(v) }

	t.Run("per_thread", func(t *testing.T) {
		s := newTeamsSiteMode(t, teamsCreds(t, teamsSecret), model.ModePerThread, newTeamsBot(t, teamsToken), daemon.NameWebhook, daemon.NameWorkers)
		root := tmMessage("1760000500000", teamsPerson, teamsChannel, "channel", "a new post")
		if code, body := s.activity(root); code != http.StatusOK {
			t.Fatalf("a root post = %d %q", code, body)
		}
		inThread := tmMessage("1760000500001", teamsPerson, teamsChannel+";messageid=1760000500000", "channel", "a reply in it")
		if code, body := s.activity(inThread); code != http.StatusOK {
			t.Fatalf("a reply in the thread = %d %q", code, body)
		}
		other := tmMessage("1760000500002", teamsPerson, teamsChannel, "channel", "another post")
		if code, body := s.activity(other); code != http.StatusOK {
			t.Fatalf("another root post = %d %q", code, body)
		}
		if code, _ := s.activity(tmMessage("1760000500003", teamsPerson, teamsGroup, "groupChat", "no threads in chats")); code != http.StatusUnprocessableEntity {
			t.Errorf("a group chat message in a per-thread inbox = %d, want 422", code)
		}
		convs := s.conversations()
		if len(convs) != 2 {
			t.Fatalf("conversations = %+v, want one per thread", convs)
		}
		var conv model.Conversation
		for _, cv := range convs {
			if cv.ReplyRoute.Kind != model.RefKindThread || cv.ReplyRoute.ParentID != teamsChannel {
				t.Fatalf("reply route %+v, want a thread of the channel", cv.ReplyRoute)
			}
			if cv.ReplyRoute.ID == "1760000500000" {
				conv = cv
			}
		}
		if got := s.messages(conv.Ref); len(got) != 2 {
			t.Fatalf("the thread holds %d messages, want its root and its reply", len(got))
		}
		acc, err := s.r.client().Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: conv.Ref, Content: text("answered in the thread")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("reply = %s %+v", op.State, op.Failure)
		}
		if sent := s.bot.called("send"); len(sent) != 1 || sent[0].Conversation != teamsChannel+";messageid=1760000500000" {
			t.Errorf("reply sent %+v, want the channel's thread of that root", sent)
		}
	})

	t.Run("per_contact", func(t *testing.T) {
		bot := newTeamsBot(t, teamsToken)
		bot.install(teamsPerson)
		s := newTeamsSiteMode(t, teamsCreds(t, teamsSecret), model.ModePerContact, bot, daemon.NameWebhook, daemon.NameWorkers)
		if code, body := s.activity(tmMessage("1760000600000", teamsPerson, teamsGroup, "groupChat", "from a group")); code != http.StatusOK {
			t.Fatalf("a message from a user in a group = %d %q", code, body)
		}
		convs := s.conversations()
		if len(convs) != 1 || convs[0].Mode != model.ModePerContact {
			t.Fatalf("conversations = %+v, want one for the user", convs)
		}
		acc, err := s.r.client().Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: convs[0].Ref, Content: text("privately")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("reply = %s %+v", op.State, op.Failure)
		}
		if sent := s.bot.called("send"); len(sent) != 1 || sent[0].Conversation != "a:personal-"+teamsPerson {
			t.Errorf("reply sent %+v, want the user's personal chat, not the group", sent)
		}
		create := s.bot.called("create")
		if len(create) != 1 || create[0].Body["isGroup"] != false || create[0].Body["tenantId"] != teamsTenantID {
			t.Errorf("personal chat created with %+v, want one non-group chat in the tenant", create)
		}

		// A user the app is not installed for: Teams forbids it, final.
		stranger := model.ExternalRef{Channel: s.ch, Namespace: teams.UserNamespace, Kind: teams.RefKindUser, ID: "29:never-installed"}
		acc, err = s.r.client().Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start-1"), Inbox: s.inbox, Recipient: stranger, Content: text("hello you")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unauthorized {
			t.Fatalf("start to a user the app is not installed for = %s %+v, want rejected as unauthorized", op.State, op.Failure)
		}
		s.bot.install(stranger.ID)
		acc, err = s.r.client().Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start-2"), Inbox: s.inbox, Recipient: stranger, Content: text("hello you")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("start to a user the app is since installed for = %s %+v", op.State, op.Failure)
		}
		if last := s.bot.called("send"); last[len(last)-1].Conversation != "a:personal-29:never-installed" {
			t.Errorf("a start to a user sent %+v, want the user's personal chat", last[len(last)-1])
		}
	})
}

// TestTeamsOneReceiverPerBot: the Bot Connector posts every activity of a
// bot registration to its one messaging endpoint, so two microsoft.teams
// channels of one App ID in one tenant would be handed one stream. heo
// refuses to start them, within a profile and across the profiles of a
// host. Another App ID is another account.
func TestTeamsOneReceiverPerBot(t *testing.T) {
	channel := func(key, ref, app string) model.Channel {
		return model.Channel{
			Key: key, Type: model.TypeMicrosoftTeams, CredentialRef: ref,
			Configuration: teamsConfig(func(c *teams.Config) { c.AppID = app }).Extension(),
		}
	}
	r := newRig(t, "acme", withComposition())
	r.channelOf(channel("one", "teams-1", teamsAppID))
	r.channelOf(channel("two", "teams-2", strings.ToUpper(teamsAppID)))
	res := r.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
		!strings.Contains(res.Err.Message, `"one"`) || !strings.Contains(res.Err.Message, `"two"`) {
		t.Fatalf("one profile, two channels on one bot: %s %v", res.Outcome, res.Err)
	}

	ok := newRig(t, "acme", withComposition())
	ok.channelOf(channel("one", "teams-1", teamsAppID))
	ok.channelOf(channel("two", "teams-2", "33333333-3333-4333-8333-333333333333"))
	ok.run(5*time.Second, daemon.NameWebhook).ready(daemon.NameWebhook)

	dir := t.TempDir()
	first := newRig(t, "first", withComposition(), withAccounts(dir))
	second := newRig(t, "second", withComposition(), withAccounts(dir))
	first.channelOf(channel("one", "teams-1", teamsAppID))
	second.channelOf(channel("two", "teams-1", teamsAppID))
	s1 := first.run(5*time.Second, daemon.NameWebhook)
	s1.ready(daemon.NameWebhook)
	res = second.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's bot: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}

// TestTeamsBareRoomIsRefusedWhenSent: `message send --to room:<id>`
// resolves the room in the adapter's identity namespace, which holds
// users, so the plan cannot tell it from a conversation; the adapter
// refuses the route when the send runs, final and before any call to the
// Bot Connector, and the operation fails as a validation error; so does a
// thread named without its channel. The conversation written with its own
// namespace is sent.
func TestTeamsBareRoomIsRefusedWhenSent(t *testing.T) {
	s := newTeamsSite(t, teamsCreds(t, teamsSecret), daemon.NameWorkers)
	start := func(name, namespace string) model.Operation {
		t.Helper()
		acc, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + name), Inbox: s.inbox, Content: desk.TextContent("hello"),
			Recipient: model.ExternalRef{Channel: s.ch, Namespace: namespace, Kind: model.RefKindRoom, ID: teamsGroup},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return s.settled(acc)
	}
	op := start("bare", teams.UserNamespace)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Validation {
		t.Errorf("the bare room = %s %+v, want failed as validation", op.State, op.Failure)
	}
	if n := s.bot.total(); n != 0 {
		t.Errorf("a route that is not a conversation reached the Bot Connector %d times", n)
	}
	if op := start("namespaced", teams.Namespace); op.State != model.ProviderAccepted {
		t.Errorf("the namespaced room = %s %+v", op.State, op.Failure)
	}

	// A thread named by its root alone, without the channel it is in, is no
	// route either: answer a thread with a reply.
	th := newTeamsSiteMode(t, teamsCreds(t, teamsSecret), model.ModePerThread, newTeamsBot(t, teamsToken), daemon.NameWorkers)
	acc, err := th.r.client().Conversations().Start(t.Context(), model.StartConversation{
		Command: th.r.cmd("start-thread"), Inbox: th.inbox, Content: desk.TextContent("hello"),
		Recipient: model.ExternalRef{Channel: th.ch, Namespace: teams.Namespace, Kind: model.RefKindThread, ID: "1760000000000"},
	})
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if op := th.settled(acc); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Validation {
		t.Errorf("a thread without its channel = %s %+v, want failed as validation", op.State, op.Failure)
	}
	if n := th.bot.total(); n != 0 {
		t.Errorf("a thread route that names no channel reached the Bot Connector %d times", n)
	}
}

// TestTeamsConnectorRefusals: what the Bot Connector answers a send is
// the operation's outcome: a conversation it does not know ends rejected
// as not found, a bot outside the conversation's roster rejected as
// unauthorized, each final and sent once; a server error after the
// request left is not sent again, as nothing says the message was not
// posted.
func TestTeamsConnectorRefusals(t *testing.T) {
	s := newTeamsSite(t, teamsCreds(t, teamsSecret), daemon.NameWorkers)
	start := func(name string) model.Operation {
		t.Helper()
		acc, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + name), Inbox: s.inbox, Recipient: teamsRoom(s, teamsGroup), Content: desk.TextContent("hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		var op model.Operation
		waitFor(t, func() bool {
			op = s.r.operation(acc.Operation.Ref)
			switch op.State {
			case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected, model.Unknown:
				return true
			}
			return false
		})
		return op
	}
	for _, tc := range []struct {
		name   string
		status int
		code   string
		state  model.DeliveryState
		fail   model.ErrorCode
	}{
		{"gone", http.StatusNotFound, "ConversationNotFound", model.Rejected, model.NotFound},
		{"roster", http.StatusForbidden, "BotNotInConversationRoster", model.Rejected, model.Unauthorized},
		{"broken", http.StatusInternalServerError, "ServiceError", model.Unknown, model.OutcomeUnknown},
	} {
		s.bot.failWith("send", tc.status, tc.code)
		before := len(s.bot.called("send"))
		op := start(tc.name)
		if op.State != tc.state || op.Failure == nil || op.Failure.Code != tc.fail {
			t.Errorf("%s: %s %+v, want %s %s", tc.name, op.State, op.Failure, tc.state, tc.fail)
		}
		if got := len(s.bot.called("send")) - before; got != 1 {
			t.Errorf("%s: the Bot Connector was asked %d times, want once", tc.name, got)
		}
	}
	s.bot.failWith("send", 0, "")
	if op := start("fine"); op.State != model.ProviderAccepted {
		t.Errorf("after the refusals = %s %+v", op.State, op.Failure)
	}
}
