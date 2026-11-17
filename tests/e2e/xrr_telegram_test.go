package e2e

import (
	"bytes"
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

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/model"
)

// Cassettes of telegram.messages through the real heo binary. In replay
// (the default) the daemon answers from committed cassettes and the
// incoming message, button press and command come from committed
// fixtures the test posts with a stand-in secret token, so nothing reaches
// Telegram and no credential is needed. None are recorded yet: with no
// cassette the test skips, and `make record-telegram` makes them.
// Recording is the one live step, and it is narrow: at most
// HEO_RECORD_TELEGRAM_MAX_SENDS messages (three by default) to one chat of
// a bot the maintainers own, behind HEO_RECORD_LIVE=1 and
// XRR_MODE=record, never in CI; see docs/DEVELOPING.md. The daemon is told
// a placeholder bot and chat and a stand-in token only; internal/xrrx
// translates between those and the real ones, counts every send against a
// cap, and refuses everything else. The daemon runs no receiver: updates
// are not recorded, and a getUpdates would take the bot's queue.

const tgFixtures = "testdata/telegram"

// tgLive is the recording's side of the bot: nil in replay.
type tgLive struct {
	s *xrrx.TelegramSandbox
}

// telegramCassettes counts the requests the test's daemon cassettes hold.
func telegramCassettes(t *testing.T) int {
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

// openTelegram returns the live sandbox when this test is recording, nil
// in replay, and skips a replay with nothing recorded and a recording
// that was not asked to go live.
func openTelegram(t *testing.T) *tgLive {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("telegram.messages is replayed or recorded, never passed through to Telegram")
	case xrr.ModeReplay:
		if telegramCassettes(t) == 0 {
			t.Skip("no telegram.messages cassettes are recorded yet: `make record-telegram` with a bot you own (docs/DEVELOPING.md)")
		}
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording telegram.messages is live: set %s=1 and the bot (docs/DEVELOPING.md)", xrrx.EnvLive)
	}
	if err := xrrx.CheckLiveTelegram(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.TelegramSandboxFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording telegram.messages: at most %d sends (%d already made), rehearsal=%v", s.Max, s.Sent(), s.Rehearse)
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", s.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
	return &tgLive{s: s}
}

// tgSite is a daemon with a telegram.messages channel on the placeholder
// bot, fed fixtures and recording or replaying its calls to Telegram.
type tgSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *tgLive
	channel model.Channel
}

func newTGSite(t *testing.T, live *tgLive) *tgSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	var env []string
	if live != nil {
		env = live.s.ChildEnv()
	}
	// Two entries, one per purpose: the bot token for the Bot API and the
	// secret token of Telegram's requests. Both are stand-ins.
	for purpose, value := range map[string]string{
		telegram.CredentialPurposeAPI: xrrx.TelegramStandInToken, telegram.CredentialPurposeWebhook: xrrx.TelegramStandInSecret,
	} {
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": value})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/telegram@"+purpose, string(b))
	}
	s := &tgSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "-c", "services.receivers.enabled=false", "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
		for _, lit := range []string{xrrx.TelegramStandInToken, xrrx.TelegramStandInSecret} {
			if strings.Contains(s.d.log(), lit) {
				t.Error("the daemon log carries a stand-in credential")
			}
		}
		if live != nil {
			for _, lit := range live.s.Deny() {
				if lit != "" && strings.Contains(s.d.log(), lit) {
					t.Error("the daemon log carries something of the real bot")
					break
				}
			}
		}
	})
	cfg, err := json.Marshal(telegram.Config{BotID: xrrx.TelegramBotID, BotUsername: xrrx.TelegramBotUsername})
	if err != nil {
		t.Fatal(err)
	}
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeTelegramMessages), "--key", "bot", "--configuration", string(cfg), "--credential-ref", "telegram")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "bot", "--key", "rooms", "--destination", strconv.FormatInt(xrrx.TelegramBotID, 10), "--mode", model.ModePerRoom)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// tgFixture reads a committed fixture.
func tgFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tgFixtures, name)) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatalf("no fixture %s: %v", name, err)
	}
	return bytes.TrimSpace(b)
}

// deliver posts body to the channel's webhook as Telegram would, with
// secret as the secret token (none when empty), retried while the daemon
// has not mounted the channel's route yet. It returns the status and body
// of the last answer.
func (s *tgSite) deliver(body []byte, secret string) (int, string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, bytes.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if secret != "" {
			req.Header.Set(xrrx.TelegramSecretHeader, secret)
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
			s.t.Fatalf("webhook: the channel's route never answered (%v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cassettes counts the requests the daemon's cassette directory holds.
func (s *tgSite) cassettes() int { return telegramCassettes(s.t) }

// mustApply waits for the operation to be accepted by Telegram.
func (s *tgSite) mustApply(t *testing.T, op model.ID, what string) {
	t.Helper()
	if d := s.waitDelivery(op, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
		t.Fatalf("%s = %s %+v", what, d.State, d.Failure)
	}
}

// TestXRRTelegramMessagesProviderVerbs: telegram.messages through the real
// heo binary. A first message to a chat starts its conversation; a reply, a
// signed message in the chat, a button press and a command join it; an
// edit, a reaction set and cleared and two deletions are each a request the
// daemon's workers make to the Bot API and the cassettes hold. Telegram's
// secret token is checked on the real listener. An answer to the button
// press is previewed only: Telegram takes an answer to a callback query
// only from a press made by a person, so no recording can make it. In
// replay nothing reaches Telegram. With no cassette recorded the test
// skips.
func TestXRRTelegramMessagesProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openTelegram(t)
		s := newTGSite(t, live)

		// The verb table, offline: telegram.messages declares edit, delete,
		// react and respond, and not history or the membership, receipt or
		// work item verbs.
		table := s.heo(0, "channel", "get", "bot").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)",
			"verb reaction add         declared (react)", "verb interaction respond  declared (respond)",
			"verb history fetch        unsupported: not_declared (history)",
			"verb participant add      unsupported: not_declared (membership)", "verb receipt send         unsupported: not_declared (receipt)",
			"verb conversation status unsupported: not_declared (work_item_act)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// Telegram's secret token, on the real listener.
		message := tgFixture(t, "update-message.json")
		if code, _ := s.deliver(message, "another-secret-token-of-16-or-more"); code != http.StatusUnauthorized {
			t.Errorf("a message with another secret token = %d, want 401", code)
		}
		if code, _ := s.deliver(message, ""); code != http.StatusUnauthorized {
			t.Errorf("a message with no secret token = %d, want 401", code)
		}

		// A preview sends nothing.
		held := s.cassettes()
		send := []string{"message", "send", "--inbox", "rooms", "--to", "telegram.messages/room:" + strconv.FormatInt(xrrx.TelegramChatID, 10)}
		s.heo(0, append(append(cloneArgs(send), "--text", xrrx.TelegramTextPrefix+" 1"), "--dry-run")...)
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Send 1: starts the chat's conversation.
		var first sendJSON
		s.json(&first, append(cloneArgs(send), "--text", xrrx.TelegramTextPrefix+" 1", "--idempotency-key", "tg-send-1")...)
		s.mustApply(t, first.Operation.ID, "send 1")
		conv := string(first.Conversation)

		// Reply, while the only message of the conversation is the
		// recording's own: a second send, answering the first. (After the
		// fixture message arrives the reply would name its ID, which no
		// recording sent, and the sandbox refuses it.)
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", xrrx.TelegramTextPrefix+" 2", "--idempotency-key", "tg-reply")
		s.mustApply(t, reply.Operation.ID, "the reply")

		// A message with the right secret token joins the conversation.
		if code, body := s.deliver(message, xrrx.TelegramStandInSecret); code != http.StatusOK {
			t.Fatalf("message = %d %q, want 200", code, body)
		}
		var msgs pageJSON[model.Message]
		eventually(t, "the incoming message", func() bool {
			s.json(&msgs, "message", "list", "--conversation", conv)
			for _, m := range msgs.Items {
				if desk.Text(m.Content) == "heo adapter test inbound" {
					return true
				}
			}
			return false
		})
		if got := s.conversationsOf("rooms"); len(got) != 1 {
			t.Fatalf("conversations = %+v, want the one chat", got)
		}

		// A button press and a command are interactions, recorded and
		// answered 200 with an empty body; the press's answer is previewed,
		// never sent.
		if code, body := s.deliver(tgFixture(t, "update-callback.json"), xrrx.TelegramStandInSecret); code != http.StatusOK || body != "" {
			t.Fatalf("button press = %d %q, want 200 and an empty body", code, body)
		}
		if code, body := s.deliver(tgFixture(t, "update-command.json"), xrrx.TelegramStandInSecret); code != http.StatusOK || body != "" {
			t.Fatalf("command = %d %q, want 200 and an empty body", code, body)
		}
		var press model.Interaction
		eventually(t, "the interactions", func() bool {
			got := interactionsOf(s.cliSite, "--conversation", conv)
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
		s.json(&plan, "interaction", "respond", string(press.Ref.ID), "--mode", model.ResponseModeMessage, "--text", xrrx.TelegramTextPrefix+" answer", "--dry-run")
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if !strings.Contains(capLine, "respond declared by telegram.messages") {
			t.Errorf("respond preview capability = %q", capLine)
		}
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Edit and react to the first message; clear the reaction.
		var mine model.Message
		s.json(&mine, "message", "get", string(first.Message))
		var edit providerOpJSON
		s.json(&edit, "message", "edit", string(mine.Ref.ID), "--revision", string(mine.Revision), "--text", xrrx.TelegramTextPrefix+" 1 edited", "--idempotency-key", "tg-edit")
		s.mustApply(t, edit.Operation.ID, "the edit")
		var react reactJSON
		s.json(&react, "reaction", "add", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "tg-react")
		s.mustApply(t, react.Operation.ID, "the reaction")
		s.json(&react, "reaction", "remove", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "tg-unreact")
		s.mustApply(t, react.Operation.ID, "the removal of the reaction")

		// Delete both of the recording's messages.
		for i, id := range []model.ID{first.Message, reply.Message} {
			var m model.Message
			s.json(&m, "message", "get", string(id))
			var del providerOpJSON
			s.json(&del, "message", "delete", string(id), "--revision", string(m.Revision), "--confirm=yes", "--idempotency-key", fmt.Sprintf("tg-delete-%d", i+1))
			s.mustApply(t, del.Operation.ID, "the deletion")
		}
		if live != nil {
			t.Logf("sends made so far, across runs: %d of at most %d", live.s.Sent(), live.s.Max)
		}
	})
}
