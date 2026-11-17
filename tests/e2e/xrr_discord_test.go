package e2e

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/model"
)

// Cassettes of discord.chat through the real heo binary. In replay (the
// default) the daemon answers from committed cassettes and the command and
// the button press come from committed fixtures the test signs with a
// stand-in key, so nothing reaches Discord and no credential is needed.
// None are recorded yet: with no cassette the test skips, and `make
// record-discord` makes them. Recording is the one live step, and it is
// narrow: at most HEO_RECORD_DISCORD_MAX_SENDS messages (three by default)
// to one channel of a server the maintainers own, behind HEO_RECORD_LIVE=1
// and XRR_MODE=record, never in CI; see docs/DEVELOPING.md. The daemon is
// told a placeholder application, bot, server and channel and a stand-in
// token only; internal/xrrx translates between those and the real ones,
// counts every send against a cap, and refuses everything else. Only REST
// is recorded: the daemon runs no receiver, because a gateway session is
// no HTTP exchange and nothing in it can be replayed from a cassette, and
// an answer to an interaction needs a token only a person's interaction
// makes.

const dcFixtures = "testdata/discord"

// dcStandInKey is the application's Ed25519 key pair in every recording and
// replay: the fixtures are signed with the private half, the channel's
// credential holds the public half. A stand-in, never Discord's.
var dcStandInKey = ed25519.NewKeyFromSeed([]byte("heo-adapter-test-ed25519-seed-01")) //nolint:gosec // G101: a stand-in key

// dcLive is the recording's side of the server: nil in replay.
type dcLive struct {
	s *xrrx.DiscordSandbox
}

// discordCassettes counts the requests the test's daemon cassettes hold.
func discordCassettes(t *testing.T) int {
	t.Helper()
	n := 0
	ents, _ := os.ReadDir(filepath.Join("testdata", "cassettes", t.Name(), "daemon"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".req.yaml") {
			n++
		}
	}
	return n
}

// openDiscord returns the live sandbox when this test is recording, nil in
// replay, and skips a replay with nothing recorded and a recording that was
// not asked to go live.
func openDiscord(t *testing.T) *dcLive {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("discord.chat is replayed or recorded, never passed through to Discord")
	case xrr.ModeReplay:
		if discordCassettes(t) == 0 {
			t.Skip("no discord.chat cassettes are recorded yet: `make record-discord` with an application and a server you own (docs/DEVELOPING.md)")
		}
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording discord.chat is live: set %s=1 and the application (docs/DEVELOPING.md)", xrrx.EnvLive)
	}
	if os.Getenv(xrrx.EnvDiscordToken) == "" {
		t.Skipf("recording discord.chat needs the application's credentials, none given: set %s and the IDs (docs/DEVELOPING.md)", xrrx.EnvDiscordToken)
	}
	if err := xrrx.CheckLiveDiscord(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.DiscordSandboxFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording discord.chat: at most %d sends (%d already made), rehearsal=%v", s.Max, s.Sent(), s.Rehearse)
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", s.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
	return &dcLive{s: s}
}

// dcSite is a daemon with a discord.chat channel on the placeholder
// application, fed fixtures and recording or replaying its calls to Discord.
type dcSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *dcLive
	channel model.Channel
}

func newDCSite(t *testing.T, live *dcLive) *dcSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	var env []string
	if live != nil {
		env = live.s.ChildEnv()
	}
	// Two entries, one per purpose: the bot token for the REST API and the
	// application's public key that verifies Discord's requests. Both are
	// stand-ins.
	pub := hex.EncodeToString(dcStandInKey.Public().(ed25519.PublicKey))
	for purpose, value := range map[string]string{
		discord.CredentialPurposeAPI: xrrx.DiscordStandInToken, discord.CredentialPurposeInteractions: pub,
	} {
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": value})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/discord@"+purpose, string(b))
	}
	s := &dcSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "-c", "services.receivers.enabled=false", "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
		for _, lit := range []string{xrrx.DiscordStandInToken, pub} {
			if strings.Contains(s.d.log(), lit) {
				t.Error("the daemon log carries a stand-in credential")
			}
		}
		if live != nil {
			for _, lit := range live.s.Deny() {
				if lit != "" && strings.Contains(s.d.log(), lit) {
					t.Error("the daemon log carries something of the real application")
					break
				}
			}
		}
	})
	cfg, err := json.Marshal(discord.Config{
		ApplicationID: xrrx.DiscordApplicationID, BotUserID: xrrx.DiscordBotUserID, Receive: discord.ReceiveInteractions,
	})
	if err != nil {
		t.Fatal(err)
	}
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeDiscordChat), "--key", "bot", "--configuration", string(cfg), "--credential-ref", "discord")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "bot", "--key", "rooms", "--destination", xrrx.DiscordApplicationID, "--mode", model.ModePerRoom)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// dcFixture reads a committed fixture.
func dcFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dcFixtures, name)) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatalf("no fixture %s: %v", name, err)
	}
	return bytes.TrimSpace(b)
}

// deliver posts body to the channel's Interactions Endpoint URL as Discord
// would, signed with key at stamp (unsigned when key is nil), retried while
// the daemon has not mounted the channel's route yet. It returns the status
// and body of the last answer.
func (s *dcSite) deliver(body []byte, key ed25519.PrivateKey, stamp string) (int, string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, bytes.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if key != nil {
			req.Header.Set(discord.HeaderTimestamp, stamp)
			req.Header.Set(discord.HeaderSignature, hex.EncodeToString(ed25519.Sign(key, append([]byte(stamp), body...))))
		}
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback webhook of the daemon the test started
		if err == nil {
			var got bytes.Buffer
			_, _ = got.ReadFrom(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				return resp.StatusCode, got.String()
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("interactions endpoint: the channel's route never answered (%v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cassettes counts the requests the daemon's cassette directory holds.
func (s *dcSite) cassettes() int { return discordCassettes(s.t) }

// mustApply waits for the operation to be accepted by Discord.
func (s *dcSite) mustApply(t *testing.T, op model.ID, what string) {
	t.Helper()
	if d := s.waitDelivery(op, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
		t.Fatalf("%s = %s %+v", what, d.State, d.Failure)
	}
}

// TestXRRDiscordChatProviderVerbs: discord.chat through the real heo
// binary. A first message to a channel starts its conversation; a reply, a
// command and a button press (signed fixtures) join it; an edit, a reaction
// set and cleared and two deletions are each a request the daemon's workers
// make to the REST API and the cassettes hold. The Interactions Endpoint
// URL's Ed25519 check runs on the real listener. An answer to the button
// press is previewed only: Discord takes an answer to an interaction only
// with the token of one a person made, so no recording can make it. The
// gateway is not recorded. In replay nothing reaches Discord. With no
// cassette recorded the test skips.
func TestXRRDiscordChatProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openDiscord(t)
		s := newDCSite(t, live)

		// The verb table, offline: discord.chat declares edit, delete, react,
		// membership, history and respond, and not receipt or the work item
		// verbs.
		table := s.heo(0, "channel", "get", "bot").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)",
			"verb reaction add         declared (react)", "verb interaction respond  declared (respond)",
			"verb history fetch        declared (history)", "verb participant add      declared (membership)",
			"verb receipt send         unsupported: not_declared (receipt)",
			"verb conversation status unsupported: not_declared (work_item_act)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// Discord's signature check, on the real listener: every failure is
		// 401, and a PING is answered.
		now := strconv.FormatInt(time.Now().Unix(), 10)
		command := dcFixture(t, "interaction-command.json")
		other := ed25519.NewKeyFromSeed([]byte("not-the-applications-key-0123456"))
		if code, _ := s.deliver(command, other, now); code != http.StatusUnauthorized {
			t.Errorf("a command signed by another key = %d, want 401", code)
		}
		if code, _ := s.deliver(command, nil, ""); code != http.StatusUnauthorized {
			t.Errorf("an unsigned command = %d, want 401", code)
		}
		if code, _ := s.deliver(command, dcStandInKey, "1600000000"); code != http.StatusUnauthorized {
			t.Errorf("a command signed long ago = %d, want 401", code)
		}
		if code, body := s.deliver([]byte(`{"application_id":"`+xrrx.DiscordApplicationID+`","type":1}`), dcStandInKey, now); code != http.StatusOK || strings.TrimSpace(body) != `{"type":1}` {
			t.Errorf("PING = %d %q, want 200 {\"type\":1}", code, body)
		}

		// A preview sends nothing.
		held := s.cassettes()
		send := []string{"message", "send", "--inbox", "rooms", "--to", "discord.chat/room:" + xrrx.DiscordChannelID}
		s.heo(0, append(append(cloneArgs(send), "--text", xrrx.DiscordTextPrefix+" 1"), "--dry-run")...)
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Send 1: starts the channel's conversation.
		var first sendJSON
		s.json(&first, append(cloneArgs(send), "--text", xrrx.DiscordTextPrefix+" 1", "--idempotency-key", "dc-send-1")...)
		s.mustApply(t, first.Operation.ID, "send 1")
		conv := string(first.Conversation)

		// Reply, answering the first message: a second send.
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", xrrx.DiscordTextPrefix+" 2", "--idempotency-key", "dc-reply")
		s.mustApply(t, reply.Operation.ID, "the reply")

		// A command and a button press, signed, are interactions: recorded
		// and answered 200 with the deferral inside Discord's three seconds.
		if code, body := s.deliver(command, dcStandInKey, now); code != http.StatusOK || !strings.Contains(body, `"type":5`) {
			t.Fatalf("command = %d %q, want 200 with the deferral", code, body)
		}
		if code, body := s.deliver(dcFixture(t, "interaction-component.json"), dcStandInKey, now); code != http.StatusOK || !strings.Contains(body, `"type":6`) {
			t.Fatalf("button press = %d %q, want 200 with the update deferral", code, body)
		}
		var press model.Interaction
		eventually(t, "the interactions", func() bool {
			got := interactionsOf(s.cliSite)
			for _, ix := range got {
				if ix.Kind == model.InteractionAction {
					press = ix
				}
			}
			return len(got) == 2
		})
		if press.Action != "approve" {
			t.Fatalf("interaction = %+v", press)
		}
		held = s.cassettes()
		var plan planJSON
		s.json(&plan, "interaction", "respond", string(press.Ref.ID), "--mode", model.ResponseModeEphemeral, "--text", xrrx.DiscordTextPrefix+" answer", "--dry-run")
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if !strings.Contains(capLine, "respond declared by discord.chat") {
			t.Errorf("respond preview capability = %q", capLine)
		}
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Edit and react to the first message; clear the reaction.
		var mine model.Message
		s.json(&mine, "message", "get", string(first.Message))
		var edit providerOpJSON
		s.json(&edit, "message", "edit", string(mine.Ref.ID), "--revision", string(mine.Revision), "--text", xrrx.DiscordTextPrefix+" 1 edited", "--idempotency-key", "dc-edit")
		s.mustApply(t, edit.Operation.ID, "the edit")
		var react reactJSON
		s.json(&react, "reaction", "add", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "dc-react")
		s.mustApply(t, react.Operation.ID, "the reaction")
		s.json(&react, "reaction", "remove", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "dc-unreact")
		s.mustApply(t, react.Operation.ID, "the removal of the reaction")

		// Delete both of the recording's messages.
		for i, id := range []model.ID{first.Message, reply.Message} {
			var m model.Message
			s.json(&m, "message", "get", string(id))
			var del providerOpJSON
			s.json(&del, "message", "delete", string(id), "--revision", string(m.Revision), "--confirm=yes", "--idempotency-key", fmt.Sprintf("dc-delete-%d", i+1))
			s.mustApply(t, del.Operation.ID, "the deletion")
		}
		if live != nil {
			t.Logf("sends made so far, across runs: %d of at most %d", live.s.Sent(), live.s.Max)
		}
	})
}
