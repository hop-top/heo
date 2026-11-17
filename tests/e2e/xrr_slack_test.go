package e2e

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// Cassettes of slack.chat through the real heo binary. In replay (the
// default) the daemon answers from committed cassettes and the incoming
// message and button press come from committed, unsigned fixtures the test
// signs with a stand-in secret, so nothing reaches Slack and no credential
// is needed. None are recorded yet: with no cassette the test skips, and
// `make record-slack` makes them. Recording is the one live step, and it is
// narrow: at most HEO_RECORD_SLACK_MAX_POSTS messages (three by default) to
// one throwaway channel of a workspace the maintainers own, behind
// HEO_RECORD_LIVE=1 and XRR_MODE=record, never in CI; see docs/DEVELOPING.md.
// The daemon is told a placeholder workspace, bot and channel and a
// stand-in token only; internal/xrrx translates between those and the real
// ones, counts every post against a cap, and refuses everything else.

const slFixtures = "testdata/slack"

// slLive is the recording's side of the workspace: nil in replay.
type slLive struct {
	s *xrrx.SlackSandbox
}

// slackCassettes counts the requests the test's daemon cassettes hold.
func slackCassettes(t *testing.T) int {
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

// openSlack returns the live sandbox when this test is recording, nil in
// replay, and skips a replay with nothing recorded and a recording that
// was not asked to go live.
func openSlack(t *testing.T) *slLive {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("slack.chat is replayed or recorded, never passed through to Slack")
	case xrr.ModeReplay:
		if slackCassettes(t) == 0 {
			t.Skip("no slack.chat cassettes are recorded yet: `make record-slack` against a workspace you own (docs/DEVELOPING.md)")
		}
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording slack.chat is live: set %s=1 and the workspace (docs/DEVELOPING.md)", xrrx.EnvLive)
	}
	if err := xrrx.CheckLiveSlack(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.SlackSandboxFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording slack.chat: at most %d posts (%d already made), rehearsal=%v", s.Max, s.Posted(), s.Rehearse)
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", s.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
	return &slLive{s: s}
}

// slSign is X-Slack-Signature: "v0=" and the hex HMAC-SHA256, under the
// signing secret, of "v0:", the timestamp, ":" and the body.
func slSign(secret, stamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("v0:" + stamp + ":"))
	m.Write(body)
	return "v0=" + hex.EncodeToString(m.Sum(nil))
}

// slSite is a daemon with a slack.chat channel on the placeholder
// workspace, fed fixtures and recording or replaying its calls to Slack.
type slSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *slLive
	channel model.Channel
}

func newSLSite(t *testing.T, live *slLive) *slSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	var env []string
	if live != nil {
		env = live.s.ChildEnv()
	}
	// Two entries, one per purpose: the bot token for the Web API and the
	// signing secret for Slack's requests. Both are stand-ins.
	for purpose, value := range map[string]string{
		slack.CredentialPurposeAPI: xrrx.SlackStandInToken, slack.CredentialPurposeSigning: xrrx.SlackStandInSigningSecret,
	} {
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": value})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/slack@"+purpose, string(b))
	}
	s := &slSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "receivers", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
		for _, lit := range []string{xrrx.SlackStandInToken, xrrx.SlackStandInSigningSecret} {
			if strings.Contains(s.d.log(), lit) {
				t.Error("the daemon log carries a stand-in credential")
			}
		}
		if live != nil {
			for _, lit := range live.s.Deny() {
				if lit != "" && strings.Contains(s.d.log(), lit) {
					t.Error("the daemon log carries something of the real workspace")
					break
				}
			}
		}
	})
	cfg, err := json.Marshal(slack.Config{TeamID: xrrx.SlackTeamID, BotUserID: xrrx.SlackBotUserID, BotID: xrrx.SlackBotID})
	if err != nil {
		t.Fatal(err)
	}
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeSlackChat), "--key", "chat", "--configuration", string(cfg), "--credential-ref", "slack")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "chat", "--key", "rooms", "--destination", xrrx.SlackTeamID, "--mode", model.ModePerRoom)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// fixture reads a committed fixture.
func slFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(slFixtures, name)) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatalf("no fixture %s: %v", name, err)
	}
	return bytes.TrimSpace(b)
}

// deliver posts body to the channel's webhook as Slack would: signed under
// secret now, retried while the daemon has not mounted the channel's route
// yet. It returns the status and body of the last answer.
func (s *slSite) deliver(contentType string, body []byte, secret string) (int, string) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		stamp := strconv.FormatInt(time.Now().Unix(), 10)
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, bytes.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("X-Slack-Request-Timestamp", stamp)
		req.Header.Set("X-Slack-Signature", slSign(secret, stamp, body))
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
func (s *slSite) cassettes() int { return slackCassettes(s.t) }

// TestXRRSlackChatProviderVerbs: slack.chat through the real heo binary. A
// first message to a room starts its conversation; a signed message in the
// room joins it, a signed button press is recorded as an interaction; a
// reply, an edit, a reaction added and removed and two deletions are each
// a request the daemon's workers make to Slack's Web API and the cassettes
// hold. Slack's request signatures are checked on the real listener. An
// answer to the button press is previewed only: its response URL answers
// once, to the person who pressed, so no recording can make it. In record
// mode the messages are read back from the channel after each change. In
// replay nothing reaches Slack. With no cassette recorded the test skips.
func TestXRRSlackChatProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openSlack(t)
		s := newSLSite(t, live)

		// The verb table, offline: slack.chat declares edit, delete, react,
		// history and respond, and not the membership, receipt or work item
		// verbs.
		table := s.heo(0, "channel", "get", "chat").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)",
			"verb reaction add         declared (react)", "verb history fetch        declared (history)",
			"verb interaction respond  declared (respond)",
			"verb participant add      unsupported: not_declared (membership)", "verb receipt send         unsupported: not_declared (receipt)",
			"verb conversation status unsupported: not_declared (work_item_act)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// Slack's handshake and signature checks, on the real listener.
		verify := slFixture(t, "url-verification.json")
		if code, body := s.deliver("application/json", verify, xrrx.SlackStandInSigningSecret); code != http.StatusOK || strings.TrimSpace(body) != "heo-adapter-test-challenge" {
			t.Fatalf("url_verification = %d %q, want 200 and the challenge", code, body)
		}
		if code, body := s.deliver("application/json", verify, "another-secret"); code != http.StatusUnauthorized || strings.Contains(body, "heo-adapter-test-challenge") {
			t.Errorf("url_verification signed with another secret = %d %q, want 401 and no challenge", code, body)
		}

		// A preview sends nothing.
		held := s.cassettes()
		send := []string{"message", "send", "--inbox", "rooms", "--to", "slack.chat/room:" + xrrx.SlackChannelID}
		s.heo(0, append(append(cloneArgs(send), "--text", xrrx.SlackTextPrefix+" 1"), "--dry-run")...)
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Send 1: starts the room's conversation.
		var first sendJSON
		s.json(&first, append(cloneArgs(send), "--text", xrrx.SlackTextPrefix+" 1", "--idempotency-key", "sl-send-1")...)
		if d := s.waitDelivery(first.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
			t.Fatalf("send 1 = %s %+v", d.State, d.Failure)
		}
		conv := string(first.Conversation)
		s.expectChannel(t, "after the send", []string{xrrx.SlackTextPrefix + " 1"})

		// Reply, while the only message of the conversation is the
		// recording's own: a second post, into the thread of the first. (After
		// the fixture message arrives the reply would name its ts, which no
		// recording posted, and the sandbox refuses it.)
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", xrrx.SlackTextPrefix+" 2", "--idempotency-key", "sl-reply")
		if d := s.waitDelivery(reply.Operation.ID, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
			t.Fatalf("the reply = %s %+v", d.State, d.Failure)
		}

		s.expectChannel(t, "after the reply", []string{xrrx.SlackTextPrefix + " 1", xrrx.SlackTextPrefix + " 2"})

		// A signed message in the room joins the conversation; one signed
		// with another secret is refused and files nowhere.
		event := slFixture(t, "event-message.json")
		if code, _ := s.deliver("application/json", event, "another-secret"); code != http.StatusUnauthorized {
			t.Errorf("message signed with another secret = %d, want 401", code)
		}
		if code, body := s.deliver("application/json", event, xrrx.SlackStandInSigningSecret); code != http.StatusOK {
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
			t.Fatalf("conversations = %+v, want the one room", got)
		}

		// A signed button press is an interaction, recorded and answered
		// 200 with an empty body; its answer is previewed, never sent.
		press := url.Values{"payload": {string(slFixture(t, "interaction-press.json"))}}.Encode()
		if code, body := s.deliver("application/x-www-form-urlencoded", []byte(press), xrrx.SlackStandInSigningSecret); code != http.StatusOK || body != "" {
			t.Fatalf("button press = %d %q, want 200 and an empty body", code, body)
		}
		var ix model.Interaction
		eventually(t, "the interaction", func() bool {
			got := interactionsOf(s.cliSite, "--conversation", conv)
			if len(got) == 1 {
				ix = got[0]
			}
			return len(got) == 1
		})
		if ix.Kind != model.InteractionAction || ix.Action != "approve" || ix.Arguments != "heo adapter test approve" {
			t.Fatalf("interaction = %+v", ix)
		}
		held = s.cassettes()
		var plan planJSON
		s.json(&plan, "interaction", "respond", string(ix.Ref.ID), "--mode", model.ResponseModeMessage, "--text", xrrx.SlackTextPrefix+" answer", "--dry-run")
		var capLine string
		_ = json.Unmarshal(plan.Args["capability"], &capLine)
		if !strings.Contains(capLine, "respond declared by slack.chat") {
			t.Errorf("respond preview capability = %q", capLine)
		}
		if s.cassettes() != held {
			t.Fatalf("a preview made a request")
		}

		// Edit and react to the first message; unreact.
		var mine model.Message
		s.json(&mine, "message", "get", string(first.Message))
		var edit providerOpJSON
		s.json(&edit, "message", "edit", string(mine.Ref.ID), "--revision", string(mine.Revision), "--text", xrrx.SlackTextPrefix+" 1 edited", "--idempotency-key", "sl-edit")
		s.mustApply(t, edit.Operation.ID, "the edit")
		var react reactJSON
		s.json(&react, "reaction", "add", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "sl-react")
		s.mustApply(t, react.Operation.ID, "the reaction")
		s.expectChannel(t, "after the edit and the reaction", []string{xrrx.SlackTextPrefix + " 1 edited", xrrx.SlackTextPrefix + " 2"}, "thumbsup")
		s.json(&react, "reaction", "remove", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "sl-unreact")
		s.mustApply(t, react.Operation.ID, "the removal of the reaction")

		// Delete both of the recording's messages.
		for i, id := range []model.ID{first.Message, reply.Message} {
			var m model.Message
			s.json(&m, "message", "get", string(id))
			var del providerOpJSON
			s.json(&del, "message", "delete", string(id), "--revision", string(m.Revision), "--confirm=yes", "--idempotency-key", fmt.Sprintf("sl-delete-%d", i+1))
			s.mustApply(t, del.Operation.ID, "the deletion")
		}
		s.expectChannel(t, "after the deletions", nil)
		if live != nil {
			t.Logf("posts made so far, across runs: %d of at most %d", live.s.Posted(), live.s.Max)
		}
	})
}

func cloneArgs(a []string) []string { return append([]string{}, a...) }

// mustApply waits for the operation to be accepted by Slack.
func (s *slSite) mustApply(t *testing.T, op model.ID, what string) {
	t.Helper()
	if d := s.waitDelivery(op, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
		t.Fatalf("%s = %s %+v", what, d.State, d.Failure)
	}
}

// expectChannel reads back, in record mode, what the throwaway channel
// holds of the recording's own: exactly texts, oldest first, the first
// carrying the reaction named. In replay it does nothing.
func (s *slSite) expectChannel(t *testing.T, when string, texts []string, reaction ...string) {
	t.Helper()
	if s.live == nil || s.live.s.Rehearse {
		return
	}
	got, err := s.live.s.ReadBack(t.Context(), nil)
	if err != nil {
		t.Fatalf("read back %s: %v", when, err)
	}
	if len(got) != len(texts) {
		t.Fatalf("the channel holds %d of the recording's messages %s, want %d: %v", len(got), when, len(texts), got)
	}
	for i, want := range texts {
		if got[i]["text"] != want {
			t.Errorf("message %d %s = %v, want %q", i+1, when, got[i]["text"], want)
		}
	}
	if len(reaction) > 0 && len(got) > 0 {
		if b, _ := json.Marshal(got[0]["reactions"]); !strings.Contains(string(b), reaction[0]) {
			t.Errorf("the first message %s carries reactions %s, want %s", when, b, reaction[0])
		}
	}
}
