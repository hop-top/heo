package daemon_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// providerMessageID is the Telegram message ID ("<chat>:<id>") the
// adapter reported for m, "" until the provider accepted it.
func tgProviderID(m model.Message) string { return providerID(m) }

// TestTelegramProviderVerbs: through heo's workers, the real
// telegram.messages adapter and heo's HTTP seam, against the Bot API in
// process: a first message to a chat, a reply, an edit, a
// reaction set and cleared and a deletion each reach the Bot API as the
// documented method with the bot token in the path; an edit of someone
// else's message is refused by Telegram and ends rejected, never as a
// new message; a button press is answered as a message, as the notification
// only its presser sees and as an edit of the message the button sits on,
// each followed by the call that clears the client's progress bar.
func TestTelegramProviderVerbs(t *testing.T) {
	creds := fullTelegramCreds(t)
	s := newTelegramSite(t, creds, telegramConfig(), daemon.NameWebhook, daemon.NameWorkers)
	ctx := t.Context()
	c := s.r.client()
	text := func(v string) model.Content { return desk.TextContent(v) }
	room := func(chat int64) model.ExternalRef {
		return model.ExternalRef{Channel: s.ch, Namespace: telegram.Namespace, Kind: model.RefKindRoom, ID: strconv.FormatInt(chat, 10)}
	}

	// A first message to a chat that never wrote in.
	other := int64(-1009876543210)
	acc, err := c.Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start"), Inbox: s.inbox, Recipient: room(other), Content: text("welcome to the group")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op := s.settled(acc); op.State != model.ProviderAccepted {
		t.Fatalf("start = %s %+v", op.State, op.Failure)
	}
	sent := s.api.called("sendMessage")
	if len(sent) != 1 || sent[0].Host != "api.telegram.org" || sent[0].Token != telegramToken || sent[0].Body["chat_id"] != float64(other) ||
		sent[0].Body["text"] != "welcome to the group" || sent[0].Body["parse_mode"] != nil || sent[0].Body["message_thread_id"] != nil {
		t.Fatalf("start sent %+v, want one plain post to the chat with the bot token in the path", sent)
	}
	if !creds.asked(telegramRef, telegram.CredentialPurposeAPI) {
		t.Error("the bot token was never asked of heo's backend")
	}

	// Someone writes in the first chat; a reply answers the chat.
	s.api.person(telegramRoom, 100, "hello team")
	if code, body := s.update(tgMessage(30, telegramRoom, 100, telegramPerson, "hello team")); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	conv := s.roomConversation(telegramRoom)
	reply, err := c.Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: conv.Ref, Content: text("on it")})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if op := s.settled(reply); op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s %+v", op.State, op.Failure)
	}
	mine := s.byText(conv.Ref, "on it")
	id := tgProviderID(mine)
	if want := strconv.FormatInt(telegramRoom, 10) + ":"; !strings.HasPrefix(id, want) {
		t.Fatalf("the reply names provider message %q, want <chat>:<message_id>", id)
	}
	msgID, _ := strconv.ParseInt(strings.TrimPrefix(id, strconv.FormatInt(telegramRoom, 10)+":"), 10, 64)

	// Edit, react, clear, delete: each its own Bot API method, on the
	// message the provider named.
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
	if up := s.api.called("editMessageText"); len(up) != 1 || up[0].Body["chat_id"] != float64(telegramRoom) || up[0].Body["message_id"] != float64(msgID) || up[0].Body["text"] != "on it, fixed" {
		t.Fatalf("edit sent %+v", up)
	}
	if got := s.api.posted(id); got == nil || got.Text != "on it, fixed" {
		t.Fatalf("Telegram holds %+v after the edit", got)
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
	if got := s.api.posted(id); len(got.Reactions) != 1 || got.Reactions[0] != "\U0001F44D" {
		t.Fatalf("reactions after add = %+v, want the thumbs up", got)
	}
	react("react-remove", true)
	if got := s.api.posted(id); len(got.Reactions) != 0 {
		t.Fatalf("reactions after remove = %+v", got)
	}
	if rc := s.api.called("setMessageReaction"); len(rc) != 2 || rc[0].Body["message_id"] != float64(msgID) {
		t.Fatalf("setMessageReaction sent %+v", rc)
	}

	// An emoji Telegram does not list is refused before any call.
	calls := s.api.total()
	bad, err := c.Messages().React(ctx, model.Reaction{Command: s.r.cmd("react-bad"), Message: mine.Ref, Emoji: "\U0001F602"})
	if err != nil {
		t.Fatalf("react with an unlisted emoji: %v", err)
	}
	if op := s.settled(bad); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unsupported {
		t.Fatalf("unlisted emoji = %s %+v, want rejected as unsupported", op.State, op.Failure)
	}
	if s.api.total() != calls {
		t.Error("a refused emoji reached Telegram")
	}

	mine = s.byText(conv.Ref, "on it, fixed")
	del, err := c.Messages().Delete(ctx, model.DeleteMessage{Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "delete-1", ExpectedRevision: mine.Revision}, Message: mine.Ref})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if op := s.settled(del); op.State != model.ProviderAccepted {
		t.Fatalf("delete = %s %+v", op.State, op.Failure)
	}
	if got := s.api.posted(id); got == nil || !got.Deleted {
		t.Fatalf("Telegram holds %+v after the deletion", got)
	}
	for _, m := range []string{"sendMessage", "editMessageText", "setMessageReaction", "deleteMessage"} {
		for _, call := range s.api.called(m) {
			if call.Host != "api.telegram.org" || call.Token != telegramToken {
				t.Errorf("%s went to %s with token %q", m, call.Host, call.Token)
			}
		}
	}

	// Someone else's message is not the bot's to change: Telegram refuses
	// and the edit ends rejected; nothing is posted instead.
	theirs := s.byText(conv.Ref, "hello team")
	posts := len(s.api.called("sendMessage"))
	refused, err := c.Messages().Edit(ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "edit-2", ExpectedRevision: theirs.Revision},
		Message: theirs.Ref, Content: text("rewritten"),
	})
	if err != nil {
		t.Fatalf("edit of another's message: %v", err)
	}
	if op := s.settled(refused); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unauthorized {
		t.Fatalf("edit of another's message = %s %+v, want rejected as unauthorized", op.State, op.Failure)
	}
	if len(s.api.called("sendMessage")) != posts {
		t.Error("a refused edit posted a new message")
	}

	// Interactions: a press on a message of the bot is answered three
	// ways, each followed by answerCallbackQuery.
	live, err := c.Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply-2"), Conversation: conv.Ref, Content: text("choose")})
	if err != nil {
		t.Fatal(err)
	}
	s.settled(live)
	liveMsg := s.byText(conv.Ref, "choose")
	liveID, _ := strconv.ParseInt(strings.TrimPrefix(tgProviderID(liveMsg), strconv.FormatInt(telegramRoom, 10)+":"), 10, 64)
	press := func(update int64, id, data string) model.Interaction {
		t.Helper()
		if code, body := s.update(tgPress(update, telegramRoom, liveID, telegramPerson, id, data)); code != http.StatusOK || body != "" {
			t.Fatalf("press %s = %d %q", data, code, body)
		}
		for _, in := range s.interactions() {
			if in.Action == data {
				return in
			}
		}
		t.Fatalf("no interaction for %s in %+v", data, s.interactions())
		return model.Interaction{}
	}
	respond := func(in model.Interaction, op, mode string, vis model.Visibility, say string) model.Operation {
		t.Helper()
		expires := in.RespondUntil // a command has no window
		if expires.IsZero() {
			expires = time.Now().Add(10 * time.Minute)
		}
		acc, err := c.Interactions().Respond(ctx, model.Respond{
			Command: s.r.cmd(op), Interaction: in.Ref, Mode: mode, Audience: model.Audience{Visibility: vis},
			Content: text(say), ExpiresAt: expires,
		})
		if err != nil {
			t.Fatalf("respond %s: %v", op, err)
		}
		return s.settled(acc)
	}

	// As a message: a post to the chat, then the answer that clears the bar.
	a := press(31, "cb-77", "approve")
	posts = len(s.api.called("sendMessage"))
	if op := respond(a, "answer", model.ResponseModeMessage, model.Public, "approved"); op.State != model.ProviderAccepted {
		t.Fatalf("respond as a message = %s %+v", op.State, op.Failure)
	}
	if got := s.api.called("sendMessage"); len(got) != posts+1 || got[posts].Body["text"] != "approved" || got[posts].Body["chat_id"] != float64(telegramRoom) {
		t.Errorf("respond as a message sent %+v", got[posts:])
	}
	if ack := s.api.called("answerCallbackQuery"); len(ack) != 1 || ack[0].Body["callback_query_id"] != "cb-77" || ack[0].Body["text"] != nil {
		t.Errorf("answerCallbackQuery after a message = %+v, want one bare answer to cb-77", ack)
	}

	// As the notification only the presser sees: a text on the query.
	b := press(32, "cb-78", "notify")
	if op := respond(b, "notify", model.ResponseModeEphemeral, model.Restricted, "only you"); op.State != model.ProviderAccepted {
		t.Fatalf("respond as a notification = %s %+v", op.State, op.Failure)
	}
	if ack := s.api.called("answerCallbackQuery"); len(ack) != 2 || ack[1].Body["callback_query_id"] != "cb-78" || ack[1].Body["text"] != "only you" {
		t.Errorf("answerCallbackQuery for a notification = %+v", ack)
	}
	if len(s.api.called("sendMessage")) != posts+1 {
		t.Error("a notification posted a message")
	}

	// As an edit of the message the button sits on.
	e := press(33, "cb-79", "edit")
	if op := respond(e, "edit", model.ResponseModeUpdate, model.Public, "decided"); op.State != model.ProviderAccepted {
		t.Fatalf("respond as an update = %s %+v", op.State, op.Failure)
	}
	if got := s.api.posted(tgProviderID(liveMsg)); got == nil || got.Text != "decided" {
		t.Errorf("Telegram holds %+v after the update, want the message edited", got)
	}
	if ack := s.api.called("answerCallbackQuery"); len(ack) != 3 || ack[2].Body["callback_query_id"] != "cb-79" {
		t.Errorf("answerCallbackQuery after an update = %+v", ack)
	}

	// A command has no query: only a message answers it.
	command := func(update, id int64, name string) model.Interaction {
		t.Helper()
		if code, _ := s.update(tgCommand(update, telegramRoom, id, telegramPerson, name, "")); code != http.StatusOK {
			t.Fatalf("command %s = %d", name, code)
		}
		for _, in := range s.interactions() {
			if in.Command == name {
				return in
			}
		}
		t.Fatalf("no interaction for /%s", name)
		return model.Interaction{}
	}
	if op := respond(command(34, 110, "status"), "cmd-ephemeral", model.ResponseModeEphemeral, model.Restricted, "psst"); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unsupported {
		t.Errorf("a command answered as a notification = %s %+v, want rejected as unsupported", op.State, op.Failure)
	}
	if op := respond(command(35, 111, "help"), "cmd-message", model.ResponseModeMessage, model.Public, "status: fine"); op.State != model.ProviderAccepted {
		t.Errorf("a command answered as a message = %s %+v", op.State, op.Failure)
	}
	if len(s.api.called("answerCallbackQuery")) != 3 {
		t.Error("a command's answer called answerCallbackQuery")
	}
}

// TestTelegramSelectionRoutes: one bot serves each selection an inbox
// may use, and an answer goes where the mode says. Per thread, a forum
// topic is the conversation and the answer is posted into it (a message
// outside any topic cannot be placed and is refused 422). Per contact,
// one user is the conversation whichever chat they wrote in, the answer
// goes to the private chat with them, and a user who never started the
// bot cannot be written to first.
func TestTelegramSelectionRoutes(t *testing.T) {
	ctx := t.Context()
	text := func(v string) model.Content { return desk.TextContent(v) }
	topic := func(id int64) func(map[string]any) {
		return func(m map[string]any) { m["message_thread_id"], m["is_topic_message"] = id, true }
	}

	t.Run("per_thread", func(t *testing.T) {
		s := newTelegramSiteMode(t, fullTelegramCreds(t), telegramConfig(), model.ModePerThread, daemon.NameWebhook, daemon.NameWorkers)
		if code, _ := s.update(tgMessage(40, telegramRoom, 300, telegramPerson, "no topic")); code != http.StatusUnprocessableEntity {
			t.Errorf("a message outside a topic = %d, want 422", code)
		}
		if code, body := s.update(tgMessage(41, telegramRoom, 301, telegramPerson, "in a topic", topic(7))); code != http.StatusOK {
			t.Fatalf("a message in topic 7 = %d %q", code, body)
		}
		if code, body := s.update(tgMessage(42, telegramRoom, 302, telegramPerson, "in another", topic(8))); code != http.StatusOK {
			t.Fatalf("a message in topic 8 = %d %q", code, body)
		}
		convs := s.conversations()
		if len(convs) != 2 {
			t.Fatalf("conversations = %+v, want one per topic", convs)
		}
		var conv model.Conversation
		for _, cv := range convs {
			if cv.ReplyRoute.Kind != model.RefKindThread || cv.ReplyRoute.ParentID != strconv.FormatInt(telegramRoom, 10) {
				t.Fatalf("reply route %+v, want a thread of the chat", cv.ReplyRoute)
			}
			if cv.ReplyRoute.ID == "7" {
				conv = cv
			}
		}
		acc, err := s.r.client().Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: conv.Ref, Content: text("answered in the topic")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("reply = %s %+v", op.State, op.Failure)
		}
		sent := s.api.called("sendMessage")
		if len(sent) != 1 || sent[0].Body["chat_id"] != float64(telegramRoom) || sent[0].Body["message_thread_id"] != float64(7) {
			t.Errorf("reply sent %+v, want the chat with message_thread_id 7", sent)
		}
	})

	t.Run("per_contact", func(t *testing.T) {
		s := newTelegramSiteMode(t, fullTelegramCreds(t), telegramConfig(), model.ModePerContact, daemon.NameWebhook, daemon.NameWorkers)
		if code, body := s.update(tgMessage(50, telegramRoom, 400, telegramPerson, "from a group")); code != http.StatusOK {
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
		if sent := s.api.called("sendMessage"); len(sent) != 1 || sent[0].Body["chat_id"] != float64(telegramPerson) {
			t.Errorf("reply sent %+v, want the user's private chat, not the group", sent)
		}

		// A user who never started the bot: Telegram forbids it, final.
		stranger := model.ExternalRef{Channel: s.ch, Namespace: telegram.UserNamespace, Kind: telegram.RefKindUser, ID: "5000000009"}
		s.api.failWith("sendMessage", telegramFailure{Code: http.StatusForbidden, Description: "Forbidden: bot can't initiate conversation with a user"})
		acc, err = s.r.client().Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start-1"), Inbox: s.inbox, Recipient: stranger, Content: text("hello you")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unauthorized ||
			!strings.Contains(op.Failure.Error(), "can't initiate conversation") {
			t.Fatalf("start to a user who never started the bot = %s %+v, want rejected as unauthorized with Telegram's words", op.State, op.Failure)
		}
		s.api.failWith("sendMessage", telegramFailure{})
		acc, err = s.r.client().Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start-2"), Inbox: s.inbox, Recipient: stranger, Content: text("hello you")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("start to a user who since started the bot = %s %+v", op.State, op.Failure)
		}
		if last := s.api.called("sendMessage"); last[len(last)-1].Body["chat_id"] != float64(5000000009) {
			t.Errorf("a start to a user sent %+v, want the user's private chat", last[len(last)-1].Body)
		}
	})
}

// TestTelegramOneReceiverPerBot: a bot takes its updates one way, from
// its webhook or from getUpdates, so two telegram.messages channels of
// one bot would be handed one stream, or would take it from each other
// and answer a webhook Telegram will not send. heo refuses to start them,
// within a profile and across the profiles of a host, whichever way each
// receives: two channels on one token under two references, one by
// polling and one by webhook, are one account. Another bot is another
// account.
func TestTelegramOneReceiverPerBot(t *testing.T) {
	channel := func(key, ref string, bot int64) model.Channel {
		return model.Channel{
			Key: key, Type: model.TypeTelegramMessages, CredentialRef: ref,
			Configuration: telegramConfig(func(c *telegram.Config) { c.BotID = bot }).Extension(),
		}
	}
	for _, services := range [][]string{{daemon.NameReceivers}, {daemon.NameWebhook}, {daemon.NameReceivers, daemon.NameWebhook}} {
		r := newRig(t, "acme", withComposition())
		r.channelOf(channel("poll", "tg-1", telegramBotID))
		r.channelOf(channel("hook", "tg-2", telegramBotID))
		res := r.run(5*time.Second, services...).result()
		if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
			!strings.Contains(res.Err.Message, `"poll"`) || !strings.Contains(res.Err.Message, `"hook"`) {
			t.Fatalf("%v: one profile, two channels on one bot: %s %v", services, res.Outcome, res.Err)
		}
	}

	// No provider is reachable here: a receiver that cannot connect
	// settles waiting at the settle timeout, which this test is not about.
	ok := newRig(t, "acme", withComposition(), withSet(daemon.KeyReceiversSettleTimeout, "50ms"))
	ok.channelOf(channel("one", "tg-1", telegramBotID))
	ok.channelOf(channel("two", "tg-2", telegramBotID+1))
	ok.run(5*time.Second, daemon.NameReceivers, daemon.NameWebhook).ready(daemon.NameReceivers, daemon.NameWebhook)

	// Across profiles: the first receives the bot by polling, the second
	// would take its webhook, and is refused.
	dir := t.TempDir()
	first := newRig(t, "first", withComposition(), withAccounts(dir), withSet(daemon.KeyReceiversSettleTimeout, "50ms"))
	second := newRig(t, "second", withComposition(), withAccounts(dir))
	first.channelOf(channel("one", "tg-1", telegramBotID))
	second.channelOf(channel("two", "tg-1", telegramBotID))
	s1 := first.run(5*time.Second, daemon.NameReceivers)
	s1.ready(daemon.NameReceivers)
	res := second.run(5*time.Second, daemon.NameWebhook).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's bot: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}

// TestTelegramBareRoomIsRefusedWhenSent: `message send --to room:<id>`
// resolves the room in the adapter's identity namespace, which holds
// users, so the plan cannot tell it from a chat; the adapter refuses the
// route when the send runs, final and before any call to Telegram, and the
// operation fails as a validation error. The chat written with its own namespace is sent.
func TestTelegramBareRoomIsRefusedWhenSent(t *testing.T) {
	s := newTelegramSite(t, fullTelegramCreds(t), telegramConfig(), daemon.NameWorkers)
	c := s.r.client()
	start := func(name, namespace string) model.Operation {
		t.Helper()
		acc, err := c.Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + name), Inbox: s.inbox, Content: desk.TextContent("hello"),
			Recipient: model.ExternalRef{Channel: s.ch, Namespace: namespace, Kind: model.RefKindRoom, ID: strconv.FormatInt(telegramRoom, 10)},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return s.settled(acc)
	}
	op := start("bare", telegram.UserNamespace)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Validation {
		t.Errorf("the bare room = %s %+v, want failed as validation", op.State, op.Failure)
	}
	if n := len(s.api.called("sendMessage")); n != 0 {
		t.Errorf("a route that is not a chat reached Telegram %d times", n)
	}
	if op := start("namespaced", telegram.Namespace); op.State != model.ProviderAccepted {
		t.Errorf("the namespaced room = %s %+v", op.State, op.Failure)
	}
}
