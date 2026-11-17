package daemon_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/model"
)

// TestDiscordInteractionsEndpoint: the channel's route is the
// application's Interactions Endpoint URL. Every request is verified
// against the application's Ed25519 public key (the channel's credential)
// before anything else, and every failure is 401, because Discord probes
// the URL with bad signatures and removes one that accepts them: no
// signature, a malformed one, another key's, a stale timestamp, a body
// other than the signed one. PING is answered, a command and a button
// press are recorded once however often they are delivered and answered
// with the deferral inside Discord's three seconds, and another
// application's interaction is refused.
func TestDiscordInteractionsEndpoint(t *testing.T) {
	creds := fullDiscordCreds(t)
	s := newDiscordSite(t, creds, discordReceive(discord.ReceiveInteractions), daemon.NameWebhook)
	body := func(v map[string]any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	ping := body(map[string]any{"type": 1, "application_id": discordApp})
	stranger := ed25519.NewKeyFromSeed([]byte("not-the-applications-key-0123456"))
	sig := func(key ed25519.PrivateKey, stamp, b string) string { return hexSig(key, stamp, b) }

	for name, c := range map[string]struct {
		sig, stamp, body string
	}{
		"no signature":       {"", now, ping},
		"malformed":          {"not-hex", now, ping},
		"another key":        {sig(stranger, now, ping), now, ping},
		"a stale timestamp":  {sig(discordKey, "1600000000", ping), "1600000000", ping},
		"another body":       {sig(discordKey, now, ping), now, body(map[string]any{"type": 2})},
		"no timestamp":       {sig(discordKey, now, ping), "", ping},
		"a signed timestamp": {sig(discordKey, "1"+now, ping), now, ping},
	} {
		var header []string
		if c.sig != "" {
			header = append(header, discord.HeaderSignature, c.sig)
		}
		if c.stamp != "" {
			header = append(header, discord.HeaderTimestamp, c.stamp)
		}
		if code, _ := do(t, http.MethodPost, s.route, "application/json", c.body, header...); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	if code, _ := do(t, http.MethodGet, s.route, "", ""); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	if !creds.asked(discordRef, discord.CredentialPurposeInteractions) {
		t.Error("the application's public key was never asked of heo's backend")
	}
	if n := len(s.interactions()); n != 0 {
		t.Fatalf("refused requests recorded %d interactions", n)
	}

	if code, out := s.interact(map[string]any{"type": 1, "application_id": discordApp}); code != http.StatusOK || strings.TrimSpace(out) != `{"type":1}` {
		t.Errorf("PING = %d %q, want 200 {\"type\":1}", code, out)
	}
	for i := 0; i < 2; i++ {
		code, out := s.interact(dcCommand(1, discordRoom, discordPerson, "triage"))
		if code != http.StatusOK || !strings.Contains(out, `"type":5`) {
			t.Fatalf("command delivery %d = %d %q, want 200 with the deferral", i+1, code, out)
		}
	}
	if code, out := s.interact(dcPress(2, discordRoom, discordPerson, "approve:42", dcID(50))); code != http.StatusOK || !strings.Contains(out, `"type":6`) {
		t.Fatalf("button press = %d %q, want 200 with the update deferral", code, out)
	}
	byKind := map[string]model.Interaction{}
	for _, in := range s.interactions() {
		byKind[in.Kind] = in
	}
	if len(s.interactions()) != 2 || byKind[model.InteractionCommand].Command != "triage" ||
		byKind[model.InteractionAction].Action != "approve:42" {
		t.Errorf("interactions = %+v, want the command once and the press", s.interactions())
	}
	if in := byKind[model.InteractionCommand]; in.RespondUntil.IsZero() {
		t.Errorf("command = %+v, want a respond window", in)
	}
	other := dcCommand(3, discordRoom, discordPerson, "triage")
	other["application_id"] = "900000000000000099"
	if code, _ := s.interact(other); code != http.StatusBadRequest {
		t.Errorf("another application's interaction = %d, want 400", code)
	}
	if n := s.api.total(); n != 0 {
		t.Errorf("receiving at the endpoint made %d calls to Discord", n)
	}
}

// TestDiscordInteractionsKeyMissing: with no public key stored the route
// answers 503, not 401 (a missing credential is not a forged request), so
// the fault is the operator's to see; a key that is not an Ed25519 key is
// refused the same way.
func TestDiscordInteractionsKeyMissing(t *testing.T) {
	for name, stored := range map[string]map[string]string{
		"none":     {discord.CredentialPurposeAPI: discordToken},
		"not hex":  {discord.CredentialPurposeAPI: discordToken, discord.CredentialPurposeInteractions: "not-a-key"},
		"too long": {discord.CredentialPurposeAPI: discordToken, discord.CredentialPurposeInteractions: discordPublicKey() + "00"},
	} {
		creds := slackCreds(t, "acme", discordRef, stored)
		s := newDiscordSite(t, creds, discordReceive(discord.ReceiveInteractions), daemon.NameWebhook)
		code, _ := s.interact(map[string]any{"type": 1, "application_id": discordApp})
		if code != http.StatusServiceUnavailable && code != http.StatusUnauthorized {
			t.Errorf("%s: PING = %d, want 503 (or a refusal)", name, code)
		}
		if name == "none" && code != http.StatusServiceUnavailable {
			t.Errorf("no key stored = %d, want 503", code)
		}
		if n := len(s.interactions()); n != 0 {
			t.Errorf("%s: %d interactions recorded", name, n)
		}
	}
}

// TestDiscordProviderVerbs: through heo's workers, the real discord.chat
// adapter and heo's HTTP seam, against Discord in process: a first message
// to a channel, a reply, an edit, a reaction set and cleared and a
// deletion each reach Discord as the documented REST route with the bot
// token and Discord's User-Agent form, and with allowed_mentions that
// ping nobody; an edit of someone else's message is refused by Discord
// and ends rejected, never as a new message; an interaction is answered
// through its webhook, which takes no bot token.
func TestDiscordProviderVerbs(t *testing.T) {
	creds := fullDiscordCreds(t)
	s := newDiscordSite(t, creds, nil, daemon.NameReceivers, daemon.NameWorkers)
	conn := s.api.next()
	ctx := t.Context()
	c := s.r.client()
	text := func(v string) model.Content { return desk.TextContent(v) }
	room := func(channel string) model.ExternalRef {
		return model.ExternalRef{Channel: s.ch, Namespace: discord.Namespace, Kind: model.RefKindRoom, ID: channel}
	}

	// A first message to a channel that never wrote in.
	other := "900000000000000040"
	acc, err := c.Conversations().Start(ctx, model.StartConversation{Command: s.r.cmd("start"), Inbox: s.inbox, Recipient: room(other), Content: text("welcome to the server")})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if op := s.settled(acc); op.State != model.ProviderAccepted {
		t.Fatalf("start = %s %+v", op.State, op.Failure)
	}
	sent := s.api.called(http.MethodPost, "channels/"+other+"/messages")
	if len(sent) != 1 || sent[0].Host != discordFakeHost || sent[0].Auth != "Bot "+discordToken || !strings.HasPrefix(sent[0].Agent, "DiscordBot (") ||
		sent[0].Body["content"] != "welcome to the server" {
		t.Fatalf("start sent %+v, want one post to the channel with the bot token", sent)
	}
	if am, _ := sent[0].Body["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
		t.Errorf("allowed_mentions = %v, want none parsed", sent[0].Body["allowed_mentions"])
	}
	if !creds.asked(discordRef, discord.CredentialPurposeAPI) {
		t.Error("the bot token was never asked of heo's backend")
	}

	// Someone writes in the room; a reply answers the room.
	s.api.person(discordRoom, dcID(100), "hello team")
	conn.dispatch("MESSAGE_CREATE", dcMessage(100, discordRoom, discordPerson, "hello team", dcMentions))
	s.recorded(discordRoom, 1)
	conv := s.roomConversation(discordRoom)
	reply, err := c.Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: conv.Ref, Content: text("on it")})
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if op := s.settled(reply); op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s %+v", op.State, op.Failure)
	}
	mine := s.byText(conv.Ref, "on it")
	id := providerID(mine)
	if !strings.HasPrefix(id, discordRoom+":") {
		t.Fatalf("the reply names provider message %q, want <channel>:<message>", id)
	}
	msg := strings.TrimPrefix(id, discordRoom+":")

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
	if up := s.api.called(http.MethodPatch, "channels/"+discordRoom+"/messages/"+msg); len(up) != 1 || up[0].Body["content"] != "on it, fixed" {
		t.Fatalf("edit sent %+v", up)
	}
	if got := s.api.posted(id); got == nil || got.Text != "on it, fixed" {
		t.Fatalf("Discord holds %+v after the edit", got)
	}
	thumbs := "\U0001F44D"
	react := func(op string, remove bool) {
		t.Helper()
		acc, err := c.Messages().React(ctx, model.Reaction{Command: s.r.cmd(op), Message: mine.Ref, Emoji: thumbs, Remove: remove})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		if got := s.settled(acc); got.State != model.ProviderAccepted {
			t.Fatalf("%s = %s %+v", op, got.State, got.Failure)
		}
	}
	react("react-add", false)
	path := "channels/" + discordRoom + "/messages/" + msg + "/reactions/" + thumbs + "/@me" // the server sees the path decoded
	if put := s.api.called(http.MethodPut, "channels/"); len(put) != 1 || put[0].Path != path {
		t.Fatalf("reaction sent %+v, want PUT %s", put, path)
	}
	if got := s.api.posted(id); len(got.Reactions) != 1 {
		t.Fatalf("reactions after add = %+v", got.Reactions)
	}
	react("react-remove", true)
	if got := s.api.posted(id); len(got.Reactions) != 0 {
		t.Fatalf("reactions after remove = %+v", got.Reactions)
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
		t.Fatalf("Discord holds %+v after the deletion", got)
	}

	// Someone else's message is not the bot's to edit: Discord refuses
	// (50005) and the edit ends rejected; nothing is posted instead.
	theirs := s.byText(conv.Ref, "<@"+discordBot+"> hello team")
	posts := len(s.api.called(http.MethodPost, "channels/"+discordRoom+"/messages"))
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
	if len(s.api.called(http.MethodPost, "channels/"+discordRoom+"/messages")) != posts {
		t.Error("a refused edit posted a new message")
	}

	// A command is answered through the interaction's webhook: the token
	// in the path stands for the bot's, and none is sent.
	conn.dispatch("INTERACTION_CREATE", dcCommand(200, discordRoom, discordPerson, "status"))
	waitFor(t, func() bool { return len(s.interactions()) == 1 })
	in := s.interactions()[0]
	respond := func(in model.Interaction, op, mode string, vis model.Visibility, say string) model.Operation {
		t.Helper()
		acc, err := c.Interactions().Respond(ctx, model.Respond{
			Command: s.r.cmd(op), Interaction: in.Ref, Mode: mode, Audience: model.Audience{Visibility: vis},
			Content: text(say), ExpiresAt: in.RespondUntil,
		})
		if err != nil {
			t.Fatalf("respond %s: %v", op, err)
		}
		return s.settled(acc)
	}
	if op := respond(in, "answer", model.ResponseModeMessage, model.Public, "status: fine"); op.State != model.ProviderAccepted {
		t.Fatalf("respond to a command = %s %+v", op.State, op.Failure)
	}
	orig := s.api.called(http.MethodPatch, "webhooks/"+discordApp+"/tok-"+dcID(200)+"/messages/@original")
	if len(orig) != 1 || orig[0].Auth != "" || orig[0].Body["content"] != "status: fine" {
		t.Errorf("the answer to a command = %+v, want the original response edited with no bot token", orig)
	}
	// The deferral fixed the visibility of a command's answer.
	conn.dispatch("INTERACTION_CREATE", dcCommand(202, discordRoom, discordPerson, "help"))
	waitFor(t, func() bool { return len(s.interactions()) == 2 })
	for _, got := range s.interactions() {
		if got.Command == "help" {
			in = got
		}
	}
	if op := respond(in, "answer-ephemeral", model.ResponseModeEphemeral, model.Restricted, "psst"); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.Unsupported {
		t.Errorf("a command answered as ephemeral = %s %+v, want rejected as unsupported", op.State, op.Failure)
	}

	// A button press can be answered with a message only its user sees.
	conn.dispatch("INTERACTION_CREATE", dcPress(201, discordRoom, discordPerson, "approve:7", dcID(300)))
	waitFor(t, func() bool { return len(s.interactions()) == 3 })
	for _, got := range s.interactions() {
		if got.Action == "approve:7" {
			in = got
		}
	}
	if op := respond(in, "notify", model.ResponseModeEphemeral, model.Restricted, "only you"); op.State != model.ProviderAccepted {
		t.Fatalf("respond to a press as ephemeral = %s %+v", op.State, op.Failure)
	}
	follow := s.api.called(http.MethodPost, "webhooks/"+discordApp+"/tok-"+dcID(201))
	if len(follow) != 1 || follow[0].Body["content"] != "only you" || follow[0].Body["flags"] != float64(64) || follow[0].Auth != "" {
		t.Errorf("ephemeral answer = %+v, want a follow-up with the ephemeral flag and no bot token", follow)
	}
}

// TestDiscordSelectionRoutes: one bot serves each selection an inbox may
// use, and an answer goes where the mode says. Per thread, a thread
// channel is the conversation and the answer is posted into it; per
// contact, one user is the conversation whichever channel they wrote in
// and the answer goes to their direct message, which Create DM opens.
func TestDiscordSelectionRoutes(t *testing.T) {
	ctx := t.Context()
	text := func(v string) model.Content { return desk.TextContent(v) }

	t.Run("per_thread", func(t *testing.T) {
		s := newDiscordSiteMode(t, fullDiscordCreds(t), nil, model.ModePerThread, daemon.NameReceivers, daemon.NameWorkers)
		s.api.setThread(discordThread, discordRoom)
		conn := s.api.next()
		conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "outside a thread", dcMentions))
		conn.dispatch("MESSAGE_CREATE", dcMessage(2, discordThread, discordPerson, "inside a thread", dcMentions, dcInThread))
		waitFor(t, func() bool { return len(s.conversations()) == 1 })
		time.Sleep(200 * time.Millisecond)
		convs := s.conversations()
		if len(convs) != 1 || convs[0].Mode != model.ModePerThread || convs[0].ReplyRoute.Kind != model.RefKindThread ||
			convs[0].ReplyRoute.ID != discordThread || convs[0].ReplyRoute.ParentID != discordRoom {
			t.Fatalf("conversations = %+v, want the thread under its channel and nothing for the message outside it", convs)
		}
		acc, err := s.r.client().Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: convs[0].Ref, Content: text("answered in the thread")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("reply = %s %+v", op.State, op.Failure)
		}
		if sent := s.api.called(http.MethodPost, "channels/"+discordThread+"/messages"); len(sent) != 1 || sent[0].Body["content"] != "answered in the thread" {
			t.Errorf("reply sent %+v, want a post into the thread channel", sent)
		}
		if sent := s.api.called(http.MethodPost, "channels/"+discordRoom+"/messages"); len(sent) != 0 {
			t.Errorf("the thread's answer was posted to its parent: %+v", sent)
		}

		// Membership of a thread: a participant is removed from it and
		// added again, each a call on the thread's members.
		const guest = "900000000000000004"
		conn.dispatch("MESSAGE_CREATE", dcMessage(3, discordThread, guest, "me too", dcMentions, dcInThread))
		var who model.Identity
		waitFor(t, func() bool {
			p, err := s.r.client().Contacts().Identities(ctx, model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 50}})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range p.Items {
				if id.SourceID == guest {
					who = id
				}
			}
			return who.SourceID == guest
		})
		change := func(op string, add bool) model.Operation {
			t.Helper()
			ch := model.ParticipantChange{Command: s.r.cmd(op), Conversation: convs[0].Ref, Identity: who.Ref}
			submit := s.r.client().Conversations().RemoveParticipant
			if add {
				submit = s.r.client().Conversations().AddParticipant
			}
			acc, err := submit(ctx, ch)
			if err != nil {
				t.Fatalf("%s: %v", op, err)
			}
			return s.settled(acc)
		}
		member := "channels/" + discordThread + "/thread-members/" + guest
		if op := change("member-remove", false); op.State != model.ProviderAccepted {
			t.Fatalf("remove = %s %+v", op.State, op.Failure)
		}
		if got := s.api.called(http.MethodDelete, member); len(got) != 1 || got[0].Auth != "Bot "+discordToken {
			t.Errorf("remove sent %+v, want one DELETE on the thread's member", got)
		}
		if op := change("member-add", true); op.State != model.ProviderAccepted {
			t.Fatalf("add = %s %+v", op.State, op.Failure)
		}
		if got := s.api.called(http.MethodPut, member); len(got) != 1 {
			t.Errorf("add sent %+v, want one PUT on the thread's member", got)
		}
	})

	t.Run("per_contact", func(t *testing.T) {
		s := newDiscordSiteMode(t, fullDiscordCreds(t), nil, model.ModePerContact, daemon.NameReceivers, daemon.NameWorkers)
		conn := s.api.next()
		conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "from a guild channel", dcMentions))
		waitFor(t, func() bool { return len(s.conversations()) == 1 })
		convs := s.conversations()
		if convs[0].Mode != model.ModePerContact {
			t.Fatalf("conversations = %+v, want one for the user", convs)
		}
		acc, err := s.r.client().Messages().Reply(ctx, model.Reply{Command: s.r.cmd("reply"), Conversation: convs[0].Ref, Content: text("privately")})
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("reply = %s %+v", op.State, op.Failure)
		}
		opened := s.api.called(http.MethodPost, "users/@me/channels")
		if len(opened) != 1 || opened[0].Body["recipient_id"] != discordPerson {
			t.Fatalf("Create DM = %+v, want one for the user", opened)
		}
		dm := s.api.dmOf(discordPerson)
		if sent := s.api.called(http.MethodPost, "channels/"+dm+"/messages"); len(sent) != 1 || sent[0].Body["content"] != "privately" {
			t.Errorf("reply sent %+v, want the user's direct message channel", sent)
		}
		if sent := s.api.called(http.MethodPost, "channels/"+discordRoom+"/messages"); len(sent) != 0 {
			t.Errorf("a private answer was posted in the guild channel: %+v", sent)
		}
	})
}

// TestDiscordBareRoomIsRefusedWhenSent: `message send --to room:<id>`
// resolves the room in the adapter's identity namespace, which holds
// users, so the plan cannot tell it from a channel; the adapter refuses
// the route when the send runs, final and before any call to Discord, and
// the operation fails as a validation error. The channel written with its
// own namespace is sent.
func TestDiscordBareRoomIsRefusedWhenSent(t *testing.T) {
	s := newDiscordSite(t, fullDiscordCreds(t), discordReceive(discord.ReceiveInteractions), daemon.NameWorkers)
	c := s.r.client()
	start := func(name, namespace string) model.Operation {
		t.Helper()
		acc, err := c.Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-" + name), Inbox: s.inbox, Content: desk.TextContent("hello"),
			Recipient: model.ExternalRef{Channel: s.ch, Namespace: namespace, Kind: model.RefKindRoom, ID: discordRoom},
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return s.settled(acc)
	}
	op := start("bare", discord.UserNamespace)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Validation {
		t.Errorf("the bare room = %s %+v, want failed as validation", op.State, op.Failure)
	}
	if n := s.api.total(); n != 0 {
		t.Errorf("a route that is not a channel reached Discord %d times", n)
	}
	if op := start("namespaced", discord.Namespace); op.State != model.ProviderAccepted {
		t.Errorf("the namespaced room = %s %+v", op.State, op.Failure)
	}
}
