package daemon_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/model"
)

// These tests run the discord.chat adapter in heo's runtime. The gateway
// it holds a WebSocket to, the REST API it answers through and the
// requests Discord signs to the channel's Interactions Endpoint URL are
// the test's own (discordfake_test.go): heo's receivers service holds the
// gateway through heo's HTTP client, the webhook listener takes the signed
// requests on the real listener, and no bot, token, guild or network is
// involved.

const (
	discordApp    = "900000000000000001"
	discordBot    = "900000000000000002"
	discordPerson = "900000000000000003"
	discordRoom   = "900000000000000010"
	discordThread = "900000000000000011"
	discordGuild  = "900000000000000020"
	discordDM     = "900000000000000030"
	discordToken  = "discord-bot-token-for-tests" // #nosec G101 -- test value
	discordRef    = "dc"
)

// discordKey is the application's Ed25519 key pair: Discord signs with
// the private half, the channel's credential holds the public half in hex.
var discordKey = func() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed([]byte("heo-discord-test-seed-0123456789"))
}()

func discordPublicKey() string { return hex.EncodeToString(discordKey.Public().(ed25519.PublicKey)) }

func discordConfig(api *discordAPI, mod ...func(*discord.Config)) discord.Config {
	cfg := discord.Config{
		ApplicationID: discordApp, BotUserID: discordBot, APIURL: api.apiURL(), GatewayURL: api.gatewayURL(),
	}
	for _, m := range mod {
		m(&cfg)
	}
	return cfg
}

func discordReceive(m discord.ReceiveMode) func(*discord.Config) {
	return func(c *discord.Config) { c.Receive = m }
}

func fullDiscordCreds(t *testing.T) *recordingCreds {
	return slackCreds(t, "acme", discordRef, map[string]string{
		discord.CredentialPurposeAPI: discordToken, discord.CredentialPurposeInteractions: discordPublicKey(),
	})
}

// The dispatches and requests Discord sends, as its references print them.

func dcID(n int) string { return strconv.Itoa(800000000000000000 + n) }

func dcUser(id string) map[string]any {
	return map[string]any{"id": id, "username": "user" + id[len(id)-2:]}
}

// dcMessage is a MESSAGE_CREATE in a guild channel.
func dcMessage(id int, channel, from, text string, mod ...func(map[string]any)) map[string]any {
	m := map[string]any{
		"id": dcID(id), "channel_id": channel, "guild_id": discordGuild, "author": dcUser(from), "content": text,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "edited_timestamp": nil, "type": 0,
		"mentions": []any{}, "attachments": []any{},
	}
	for _, f := range mod {
		f(m)
	}
	return m
}

// dcMentions makes m mention the bot.
func dcMentions(m map[string]any) {
	m["mentions"] = []any{map[string]any{"id": discordBot, "bot": true}}
	m["content"] = "<@" + discordBot + "> " + m["content"].(string)
}

// dcDirect makes m a direct message.
func dcDirect(m map[string]any) { delete(m, "guild_id") }

// dcInThread makes m a message of a thread channel.
func dcInThread(m map[string]any) { m["channel_id"] = discordThread }

// dcCommand is a slash command interaction.
func dcCommand(id int, channel, from, name string) map[string]any {
	return map[string]any{
		"id": dcID(id), "application_id": discordApp, "type": 2, "token": "tok-" + dcID(id), "guild_id": discordGuild,
		"channel_id": channel, "member": map[string]any{"user": dcUser(from)},
		"data": map[string]any{"name": name, "type": 1},
	}
}

// dcPress is a button press on a message.
func dcPress(id int, channel, from, customID string, onMessage string) map[string]any {
	return map[string]any{
		"id": dcID(id), "application_id": discordApp, "type": 3, "token": "tok-" + dcID(id), "guild_id": discordGuild,
		"channel_id": channel, "member": map[string]any{"user": dcUser(from)},
		"message": map[string]any{"id": onMessage, "channel_id": channel, "flags": 0},
		"data":    map[string]any{"custom_id": customID, "component_type": 2},
	}
}

// discordSite is a daemon with one discord.chat channel and an inbox
// bound to the application, its provider calls answered by the in-process
// Discord.
type discordSite struct {
	t     *testing.T
	r     *rig
	api   *discordAPI
	creds *recordingCreds
	ch    model.ChannelRef
	inbox model.InboxRef
	route string
	s     *supervised
}

func newDiscordSite(t *testing.T, creds *recordingCreds, mod func(*discord.Config), services ...string) *discordSite {
	t.Helper()
	return newDiscordSiteMode(t, creds, mod, model.ModePerRoom, services...)
}

func newDiscordSiteMode(t *testing.T, creds *recordingCreds, mod func(*discord.Config), mode string, services ...string) *discordSite {
	t.Helper()
	return newDiscordSitePrepared(t, creds, mod, mode, nil, services...)
}

// newDiscordSitePrepared is newDiscordSiteMode with prep run on Discord
// before the daemon starts: what a test sets so the first connection meets
// it.
func newDiscordSitePrepared(t *testing.T, creds *recordingCreds, mod func(*discord.Config), mode string, prep func(*discordAPI), services ...string) *discordSite {
	t.Helper()
	api := newDiscordAPI(t, discordToken, discordBot, discordApp)
	if prep != nil {
		prep(api)
	}
	cfg := discordConfig(api)
	if mod != nil {
		mod(&cfg)
	}
	r := newRig(t, "acme", withComposition(), withCredentials(creds), withHTTP(api.client()))
	selection := model.Selection{Mode: mode}
	if mode == model.ModePerContact {
		lock := true
		selection.LockToSingle, selection.LifecycleRules = &lock, model.RuleSetContactLifecycleV1
	}
	site := &discordSite{t: t, r: r, api: api, creds: creds}
	site.ch = r.channelOf(model.Channel{
		Key: "bot", Type: model.TypeDiscordChat, CredentialRef: discordRef, Configuration: cfg.Extension(),
	})
	acc, err := r.client().Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{Command: r.cmd("inbox-rooms"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: site.ch, Name: "rooms", Destinations: []string{discordApp},
		Selection: selection,
	}})
	if err != nil {
		t.Fatalf("configure inbox: %v", err)
	}
	if site.inbox, err = acc.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	site.s = r.run(5*time.Second, services...)
	site.s.ready(services...)
	if wh, ok := site.s.svcs[daemon.NameWebhook]; ok {
		if a, ok := wh.(serve.Addressed); ok {
			site.route = "http://" + a.Addr() + daemon.WebhookPrefix + string(site.ch.ID)
		}
	}
	return site
}

// reconfigure stores cfg as the channel's configuration and reconciles
// the running daemon, as `channel update` does.
func (s *discordSite) reconfigure(cfg discord.Config) {
	s.t.Helper()
	ch := s.r.stored(s.ch)
	ch.Configuration = cfg.Extension()
	cmd := s.r.cmd("reconfigure-" + string(ch.Revision))
	cmd.ExpectedRevision = ch.Revision
	if _, err := s.r.client().Configuration().ConfigureChannel(s.t.Context(), model.ConfigureChannel{Command: cmd, Channel: ch}); err != nil {
		s.t.Fatalf("update channel: %v", err)
	}
	s.r.reconcile()
}

// receiverScope is the receivers service's scope for the channel.
func (s *discordSite) receiverScope() daemon.ScopeStatus {
	s.t.Helper()
	sc, _ := s.r.scope(daemon.NameReceivers, s.ch)
	return sc
}

func (s *discordSite) webhookScope() (daemon.ScopeStatus, bool) {
	s.t.Helper()
	return s.r.scope(daemon.NameWebhook, s.ch)
}

// healthy fails unless both inbound services read ready, not degraded.
func (s *discordSite) healthy() {
	s.t.Helper()
	st := s.r.host.Status()
	for _, name := range []string{daemon.NameReceivers, daemon.NameWebhook} {
		if got := service(st, name).State; got != daemon.StateReady {
			s.t.Errorf("%s = %s, want ready", name, got)
		}
	}
}

func (s *discordSite) conversations() []model.Conversation {
	s.t.Helper()
	p, err := s.r.client().Conversations().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *discordSite) messages(conv model.ConversationRef) []model.Message {
	s.t.Helper()
	p, err := s.r.client().Messages().List(s.t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "test"}, Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *discordSite) interactions() []model.Interaction {
	s.t.Helper()
	p, err := s.r.client().Interactions().List(s.t.Context(), model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 20}})
	if err != nil {
		s.t.Fatal(err)
	}
	return p.Items
}

func (s *discordSite) byText(conv model.ConversationRef, text string) model.Message {
	s.t.Helper()
	for _, m := range s.messages(conv) {
		if desk.Text(m.Content) == text {
			return m
		}
	}
	s.t.Fatalf("no message %q in %v", text, conv)
	return model.Message{}
}

// roomConversation is the conversation of channel, in the per-room inbox.
func (s *discordSite) roomConversation(channel string) model.Conversation {
	s.t.Helper()
	for _, cv := range s.conversations() {
		if cv.ReplyRoute.ID == channel {
			return cv
		}
	}
	s.t.Fatalf("no conversation for channel %s in %+v", channel, s.conversations())
	return model.Conversation{}
}

func (s *discordSite) settled(acc model.Acceptance) model.Operation {
	s.t.Helper()
	var op model.Operation
	waitFor(s.t, func() bool {
		op = s.r.operation(acc.Operation.Ref)
		switch op.State {
		case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected:
			return true
		}
		return false
	})
	return op
}

// hexSig is X-Signature-Ed25519: the hex signature over the timestamp
// immediately followed by the body.
func hexSig(key ed25519.PrivateKey, stamp, body string) string {
	return hex.EncodeToString(ed25519.Sign(key, []byte(stamp+body)))
}

// signed posts body to the channel's route signed with key at stamp.
func (s *discordSite) signed(key ed25519.PrivateKey, stamp, body string) (int, string) {
	s.t.Helper()
	sig := hexSig(key, stamp, body)
	return do(s.t, http.MethodPost, s.route, "application/json", body,
		discord.HeaderSignature, sig, discord.HeaderTimestamp, stamp)
}

// interact posts v to the route as Discord would: signed now.
func (s *discordSite) interact(v map[string]any) (int, string) {
	s.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.signed(discordKey, strconv.FormatInt(time.Now().Unix(), 10), string(b))
}

// recorded waits for the conversation of channel to hold n messages.
func (s *discordSite) recorded(channel string, n int) []model.Message {
	s.t.Helper()
	var got []model.Message
	waitFor(s.t, func() bool {
		for _, cv := range s.conversations() {
			if cv.ReplyRoute.ID == channel {
				got = s.messages(cv.Ref)
				return len(got) == n
			}
		}
		return false
	})
	return got
}

// TestDiscordGateway: heo's receiver holds the bot's gateway connection
// through heo's HTTP client. It identifies with the bot token and, under
// the default mentions_only, without the privileged MESSAGE_CONTENT
// intent; the channel reads ready. A guild message that mentions the bot
// and a direct message open their conversations once however often
// Discord delivers them; a guild message that does not mention the bot and
// the bot's own are dropped. An interaction over the gateway is recorded
// and acknowledged through the callback endpoint, which takes no bot
// token.
func TestDiscordGateway(t *testing.T) {
	creds := fullDiscordCreds(t)
	s := newDiscordSite(t, creds, nil, daemon.NameReceivers)
	conn := s.api.next()
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })

	ids, _, _ := s.api.counts()
	if ids != 1 {
		t.Fatalf("identifies = %d, want 1", ids)
	}
	s.api.mu.Lock()
	id := s.api.identifies[0]
	s.api.mu.Unlock()
	intents, _ := id["intents"].(float64)
	if id["token"] != discordToken {
		t.Error("Identify did not carry the bot token")
	}
	const guildMessages, messageContent = 1 << 9, 1 << 15
	if int(intents)&guildMessages == 0 || int(intents)&messageContent != 0 {
		t.Errorf("intents = %d: want guild messages without MESSAGE_CONTENT (mentions_only is on)", int(intents))
	}
	if !creds.asked(discordRef, discord.CredentialPurposeAPI) {
		t.Error("the bot token was never asked of heo's backend")
	}
	if bots := s.api.called(http.MethodGet, "gateway/bot"); len(bots) != 1 || bots[0].Auth != "Bot "+discordToken ||
		!strings.HasPrefix(bots[0].Agent, "DiscordBot (") {
		t.Errorf("Get Gateway Bot = %+v, want one call with the bot token and Discord's User-Agent form", bots)
	}

	for i := 0; i < 2; i++ {
		conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "is anyone there?", dcMentions))
	}
	conn.dispatch("MESSAGE_CREATE", dcMessage(2, discordRoom, discordPerson, "just chatting"))
	conn.dispatch("MESSAGE_CREATE", dcMessage(3, discordRoom, discordBot, "my own echo", dcMentions))
	conn.dispatch("MESSAGE_CREATE", dcMessage(4, discordDM, discordPerson, "hello in private", dcDirect))
	waitFor(t, func() bool { return len(s.conversations()) == 2 })
	room := s.recorded(discordRoom, 1)
	if got := desk.Text(room[0].Content); !strings.HasSuffix(got, "is anyone there?") {
		t.Errorf("room message = %q, want the mention", got)
	}
	dm := s.recorded(discordDM, 1)
	if desk.Text(dm[0].Content) != "hello in private" {
		t.Errorf("direct message = %q", desk.Text(dm[0].Content))
	}
	if cv := s.roomConversation(discordRoom); cv.Mode != model.ModePerRoom || cv.ReplyRoute.Kind != model.RefKindRoom || cv.ReplyRoute.Namespace != discord.Namespace {
		t.Errorf("conversation = %+v, want a room of discord.chat", cv)
	}

	// An interaction over the gateway is answered at the callback.
	conn.dispatch("INTERACTION_CREATE", dcCommand(10, discordRoom, discordPerson, "triage"))
	waitFor(t, func() bool { return len(s.interactions()) == 1 })
	if in := s.interactions()[0]; in.Kind != model.InteractionCommand || in.Command != "triage" {
		t.Errorf("interaction = %+v", in)
	}
	waitFor(t, func() bool { return len(s.api.called(http.MethodPost, "interactions/")) == 1 })
	cb := s.api.called(http.MethodPost, "interactions/")[0]
	if cb.Path != "interactions/"+dcID(10)+"/tok-"+dcID(10)+"/callback" || cb.Body["type"] != float64(5) || cb.Auth != "" {
		t.Errorf("callback = %+v, want a deferral at the interaction's token with no bot token", cb)
	}
	if sc := s.receiverScope(); sc.State != daemon.ScopeReady || strings.Contains(sc.Reason, discordToken) {
		t.Errorf("scope = %+v", sc)
	}
}

// TestDiscordGatewayAsksForMessageContent: with mentions_only off the
// receiver asks for the privileged intent and reports guild messages that
// do not mention the bot.
func TestDiscordGatewayAsksForMessageContent(t *testing.T) {
	off := false
	s := newDiscordSite(t, fullDiscordCreds(t), func(c *discord.Config) { c.MentionsOnly = &off }, daemon.NameReceivers)
	conn := s.api.next()
	s.api.mu.Lock()
	intents, _ := s.api.identifies[0]["intents"].(float64)
	s.api.mu.Unlock()
	if int(intents)&(1<<15) == 0 {
		t.Errorf("intents = %d, want MESSAGE_CONTENT", int(intents))
	}
	conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "no mention at all"))
	if got := s.recorded(discordRoom, 1); desk.Text(got[0].Content) != "no mention at all" {
		t.Errorf("message = %q", desk.Text(got[0].Content))
	}
}

// TestDiscordGatewayReconnects: a connection Discord ends with a resumable
// close, or asks to reconnect (opcode 7), is resumed inside the adapter's
// receiver with the session and the last sequence number, with no Identify
// and no change to the channel's scope; what Discord dispatches on the
// resumed connection is recorded.
func TestDiscordGatewayReconnects(t *testing.T) {
	s := newDiscordSite(t, fullDiscordCreds(t), nil, daemon.NameReceivers)
	conn := s.api.next()
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "before", dcMentions))
	s.recorded(discordRoom, 1)

	conn.closeWith(4000)
	conn = s.api.next()
	conn.dispatch("MESSAGE_CREATE", dcMessage(2, discordRoom, discordPerson, "after the close", dcMentions))
	s.recorded(discordRoom, 2)
	if ids, resumes, _ := s.api.counts(); ids != 1 || resumes != 1 {
		t.Fatalf("after a close 4000: %d identifies, %d resumes, want 1 and 1", ids, resumes)
	}
	s.api.mu.Lock()
	r := s.api.resumes[0]
	s.api.mu.Unlock()
	if r["session_id"] != "session-1" || r["token"] != discordToken || r["seq"] != float64(2) {
		t.Errorf("Resume = %+v, want session-1 at sequence 2 (READY, then the first message)", r)
	}

	if err := conn.send(7, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	conn = s.api.next()
	conn.dispatch("MESSAGE_CREATE", dcMessage(3, discordRoom, discordPerson, "after a reconnect", dcMentions))
	s.recorded(discordRoom, 3)
	if ids, resumes, _ := s.api.counts(); ids != 1 || resumes != 2 {
		t.Fatalf("after a Reconnect: %d identifies, %d resumes, want 1 and 2", ids, resumes)
	}
	if sc := s.receiverScope(); sc.State != daemon.ScopeReady || sc.Reason != "" {
		t.Errorf("scope = %+v, want ready throughout", sc)
	}
	if st := service(s.r.host.Status(), daemon.NameReceivers); st.State != daemon.StateReady {
		t.Errorf("receivers = %s, want ready", st.State)
	}
}

// TestDiscordGatewayRestartResumes: stopping the daemon drops the
// connection without a close frame of 1000 or 1001 (which would end the
// session at Discord), and the channel's cursor is kept in the store: a
// restarted daemon resumes the same session at the last sequence number
// it handled instead of identifying again, Discord replays what was
// dispatched while it was down, and a message already recorded is not
// recorded twice.
func TestDiscordGatewayRestartResumes(t *testing.T) {
	s := newDiscordSite(t, fullDiscordCreds(t), nil, daemon.NameReceivers)
	conn := s.api.next()
	conn.dispatch("MESSAGE_CREATE", dcMessage(1, discordRoom, discordPerson, "before the stop", dcMentions))
	s.recorded(discordRoom, 1)
	if res := s.s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("stop: %s %v", res.Outcome, res.Err)
	}
	if !conn.ended(5 * time.Second) {
		t.Fatal("the gateway connection outlived the daemon")
	}
	if code := conn.how(); code == 1000 || code == 1001 {
		t.Errorf("the connection was closed with %d, which ends the session at Discord", code)
	}

	s.api.dispatchOffline("MESSAGE_CREATE", dcMessage(2, discordRoom, discordPerson, "while down", dcMentions))
	again := s.r.run(5*time.Second, daemon.NameReceivers)
	again.ready(daemon.NameReceivers)
	s.api.next()
	s.recorded(discordRoom, 2)
	if ids, resumes, _ := s.api.counts(); ids != 1 || resumes != 1 {
		t.Fatalf("after the restart: %d identifies, %d resumes, want 1 and 1", ids, resumes)
	}
	s.api.mu.Lock()
	r := s.api.resumes[0]
	s.api.mu.Unlock()
	if r["session_id"] != "session-1" || r["seq"] != float64(2) {
		t.Errorf("Resume after the restart = %+v, want session-1 at sequence 2", r)
	}
}

// TestDiscordGatewayDisabledIntent: Discord closes the gateway with 4014
// when MESSAGE_CONTENT was asked for and the application was not granted
// it. That ends the receiver for good: the channel reads failed with the
// way out, the rest of the daemon is unaffected, and no further connection
// is made.
func TestDiscordGatewayDisabledIntent(t *testing.T) {
	off := false
	s := newDiscordSitePrepared(t, fullDiscordCreds(t), func(c *discord.Config) { c.MentionsOnly = &off }, model.ModePerRoom,
		func(d *discordAPI) { d.closeOnReady = 4014 }, daemon.NameReceivers)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeFailed })
	sc := s.receiverScope()
	if !strings.Contains(sc.Reason, "mentions_only") || !strings.Contains(sc.Reason, "4014") {
		t.Errorf("reason = %q, want the close code and the way out", sc.Reason)
	}
	_, _, conns := s.api.counts()
	time.Sleep(2500 * time.Millisecond)
	if _, _, n := s.api.counts(); n != conns {
		t.Errorf("a disallowed intent was connected with again: %d connections, then %d", conns, n)
	}
	if strings.Contains(sc.Reason, discordToken) {
		t.Error("the bot token reached the scope status")
	}
}

// TestDiscordGatewayRefusedToken: a token Discord refuses, at Get Gateway
// Bot or by closing 4004, ends the receiver without retries: the
// credential will not mend itself.
func TestDiscordGatewayRefusedToken(t *testing.T) {
	creds := slackCreds(t, "acme", discordRef, map[string]string{
		discord.CredentialPurposeAPI: "discord-revoked-token-for-tests", discord.CredentialPurposeInteractions: discordPublicKey(), // #nosec G101 -- test value
	})
	s := newDiscordSite(t, creds, nil, daemon.NameReceivers)
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeFailed })
	reads := len(s.api.called(http.MethodGet, "gateway/bot"))
	time.Sleep(2500 * time.Millisecond)
	if n := len(s.api.called(http.MethodGet, "gateway/bot")); n != reads {
		t.Errorf("a refused token was tried again: %d reads, then %d", reads, n)
	}
	if ids, _, _ := s.api.counts(); ids != 0 {
		t.Errorf("a refused token identified %d times", ids)
	}
	if st := service(s.r.host.Status(), daemon.NameReceivers); st.State != daemon.StateDegraded {
		t.Errorf("receivers = %s, want degraded", st.State)
	}
}

// TestDiscordReceiveModes: the channel's receive key picks the way the
// bot receives, and heo runs exactly that. gateway: a receiver and no
// route (a request there is 404, the webhook service holds no scope for
// the channel). interactions: a route and no receiver (no gateway
// connection, no receivers scope, neither service degraded). Unset: both.
func TestDiscordReceiveModes(t *testing.T) {
	both := []string{daemon.NameReceivers, daemon.NameWebhook}
	cmd := dcCommand(1, discordRoom, discordPerson, "ping")

	t.Run("gateway", func(t *testing.T) {
		s := newDiscordSite(t, fullDiscordCreds(t), discordReceive(discord.ReceiveGateway), both...)
		s.api.next()
		waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
		if code, _ := s.interact(cmd); code != http.StatusNotFound {
			t.Errorf("route of a gateway channel = %d, want 404: nothing is mounted", code)
		}
		if sc, ok := s.webhookScope(); ok {
			t.Errorf("webhook scope for a gateway channel: %+v", sc)
		}
		s.healthy()
	})

	t.Run("interactions", func(t *testing.T) {
		s := newDiscordSite(t, fullDiscordCreds(t), discordReceive(discord.ReceiveInteractions), both...)
		if code, body := s.interact(cmd); code != http.StatusOK {
			t.Fatalf("interaction = %d %q", code, body)
		}
		if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
			t.Errorf("webhook scope = %+v, %v, want ready", sc, ok)
		}
		time.Sleep(300 * time.Millisecond)
		if sc, ok := s.r.scope(daemon.NameReceivers, s.ch); ok {
			t.Errorf("receivers scope for an interactions channel: %+v", sc)
		}
		if _, _, n := s.api.counts(); n != 0 || len(s.api.called(http.MethodGet, "gateway/bot")) != 0 {
			t.Errorf("heo reached the gateway for an interactions channel: %d connections", n)
		}
		s.healthy()
	})

	t.Run("both", func(t *testing.T) {
		s := newDiscordSite(t, fullDiscordCreds(t), nil, both...)
		s.api.next()
		waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
		if code, body := s.interact(cmd); code != http.StatusOK {
			t.Errorf("interaction = %d %q", code, body)
		}
		if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
			t.Errorf("webhook scope = %+v, %v, want ready", sc, ok)
		}
		s.healthy()
	})
}

// TestDiscordReceiveModeReconciled: a channel moved from the gateway to
// its Interactions Endpoint URL while the daemon runs drops its gateway
// connection and gets its route, and moved back, loses the route and
// connects again; neither service is left degraded by the way the channel
// no longer uses.
func TestDiscordReceiveModeReconciled(t *testing.T) {
	both := []string{daemon.NameReceivers, daemon.NameWebhook}
	s := newDiscordSite(t, fullDiscordCreds(t), discordReceive(discord.ReceiveGateway), both...)
	conn := s.api.next()
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })

	s.reconfigure(discordConfig(s.api, discordReceive(discord.ReceiveInteractions)))
	waitFor(t, func() bool { _, ok := s.r.scope(daemon.NameReceivers, s.ch); return !ok })
	if !conn.ended(5 * time.Second) {
		t.Fatal("the gateway connection outlived the move to interactions")
	}
	if code, body := s.interact(dcCommand(1, discordRoom, discordPerson, "ping")); code != http.StatusOK {
		t.Fatalf("interaction after the move = %d %q", code, body)
	}
	if sc, ok := s.webhookScope(); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("webhook scope after the move = %+v, %v", sc, ok)
	}
	s.healthy()

	s.reconfigure(discordConfig(s.api, discordReceive(discord.ReceiveGateway)))
	s.api.next()
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	if code, _ := s.interact(dcCommand(2, discordRoom, discordPerson, "ping")); code != http.StatusNotFound {
		t.Errorf("route after the move back to the gateway = %d, want 404", code)
	}
	if sc, ok := s.webhookScope(); ok {
		t.Errorf("webhook scope after the move back: %+v", sc)
	}
	s.healthy()
}

// TestDiscordOneReceiverPerApplication: an application has one bot token,
// so one gateway session, and one Interactions Endpoint URL, so two
// discord.chat channels of one application would be handed one stream or
// would take the session from each other. heo refuses to start them,
// within a profile and across the profiles of a host, whichever way each
// receives. Another application is another account.
func TestDiscordOneReceiverPerApplication(t *testing.T) {
	channel := func(key, ref, app string, receive discord.ReceiveMode) model.Channel {
		cfg := discord.Config{ApplicationID: app, BotUserID: discordBot, Receive: receive}
		return model.Channel{Key: key, Type: model.TypeDiscordChat, CredentialRef: ref, Configuration: cfg.Extension()}
	}
	for _, services := range [][]string{{daemon.NameReceivers}, {daemon.NameWebhook}, {daemon.NameReceivers, daemon.NameWebhook}} {
		r := newRig(t, "acme", withComposition())
		r.channelOf(channel("gateway", "dc-1", discordApp, discord.ReceiveGateway))
		r.channelOf(channel("endpoint", "dc-2", discordApp, discord.ReceiveInteractions))
		res := r.run(5*time.Second, services...).result()
		if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "receive the same provider account") ||
			!strings.Contains(res.Err.Message, `"gateway"`) || !strings.Contains(res.Err.Message, `"endpoint"`) {
			t.Fatalf("%v: one profile, two channels on one application: %s %v", services, res.Outcome, res.Err)
		}
	}

	ok := newRig(t, "acme", withComposition())
	ok.channelOf(channel("one", "dc-1", discordApp, discord.ReceiveInteractions))
	ok.channelOf(channel("two", "dc-2", "900000000000000099", discord.ReceiveInteractions))
	ok.run(5*time.Second, daemon.NameReceivers, daemon.NameWebhook).ready(daemon.NameReceivers, daemon.NameWebhook)

	// Across profiles: the first receives the application's interactions,
	// the second would take its gateway, and is refused.
	dir := t.TempDir()
	first := newRig(t, "first", withComposition(), withAccounts(dir))
	second := newRig(t, "second", withComposition(), withAccounts(dir))
	first.channelOf(channel("one", "dc-1", discordApp, discord.ReceiveInteractions))
	second.channelOf(channel("two", "dc-1", discordApp, discord.ReceiveGateway))
	s1 := first.run(5*time.Second, daemon.NameWebhook)
	s1.ready(daemon.NameWebhook)
	res := second.run(5*time.Second, daemon.NameReceivers).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/first (pid ") {
		t.Fatalf("second profile on the first's application: %s %v", res.Outcome, res.Err)
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
}

// TestHostReceiving: the daemon reports how it runs each way a channel
// receives, for `channel check --remote`: the receiver's scope state and
// reason, and who holds its receiver lease (the one owner per
// application), the route's scope with its path. A way no service runs
// reads not_running with the reason; a receiver that ended for good
// reads failed with Discord's reason.
func TestHostReceiving(t *testing.T) {
	both := []string{daemon.NameReceivers, daemon.NameWebhook}
	s := newDiscordSite(t, fullDiscordCreds(t), nil, both...)
	s.api.next()
	waitFor(t, func() bool { return s.receiverScope().State == daemon.ScopeReady })
	rx := s.r.host.Receiving(t.Context(), s.ch, model.CapabilityReceive)
	if rx.State != "ready" || rx.Owner != "heo/acme/receiver" || !rx.Until.After(time.Now()) || rx.Since.IsZero() {
		t.Errorf("receive = %+v, want ready, held by heo/acme/receiver until later", rx)
	}
	wh := s.r.host.Receiving(t.Context(), s.ch, model.CapabilityReceiveHTTP)
	if wh.State != "ready" || wh.Path != daemon.WebhookPrefix+string(s.ch.ID) || wh.Owner != "" {
		t.Errorf("receive_http = %+v, want ready at the channel's route", wh)
	}

	// Only the webhook runs: no receiver, no lease.
	hook := newDiscordSite(t, fullDiscordCreds(t), nil, daemon.NameWebhook)
	if rx := hook.r.host.Receiving(t.Context(), hook.ch, model.CapabilityReceive); rx.State != "not_running" || !strings.Contains(rx.Reason, "receivers service is not running") || rx.Owner != "" {
		t.Errorf("receive without the receivers service = %+v, want not_running, the service named", rx)
	}
	// The service runs, and has nothing for a channel that takes the
	// other way.
	gw := newDiscordSite(t, fullDiscordCreds(t), discordReceive(discord.ReceiveGateway), both...)
	gw.api.next()
	if rh := gw.r.host.Receiving(t.Context(), gw.ch, model.CapabilityReceiveHTTP); rh.State != "not_running" || !strings.Contains(rh.Reason, "runs nothing for this channel") {
		t.Errorf("receive_http of a gateway channel = %+v, want not_running, nothing run for it", rh)
	}

	// A receiver Discord ended for good.
	off := false
	dead := newDiscordSitePrepared(t, fullDiscordCreds(t), func(c *discord.Config) { c.MentionsOnly = &off }, model.ModePerRoom,
		func(d *discordAPI) { d.closeOnReady = 4014 }, daemon.NameReceivers)
	waitFor(t, func() bool { return dead.receiverScope().State == daemon.ScopeFailed })
	if rx := dead.r.host.Receiving(t.Context(), dead.ch, model.CapabilityReceive); rx.State != "failed" || !strings.Contains(rx.Reason, "4014") {
		t.Errorf("receive after 4014 = %+v, want failed with Discord's reason", rx)
	}
}
