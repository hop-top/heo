package e2e

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestTelegramChannelConfigurationChecked: on the release build, a
// telegram.messages channel needs its bot ID and username, refuses what
// the adapter refuses (an http API URL, a field it does not know, a poll
// timeout out of range) and a credential in its configuration, each exit
// 2 with nothing written; a second channel on the same bot is refused
// exit 4 whatever its credential reference, since a bot is received one
// way only, and another bot is not. The preview runs the same checks and
// writes nothing, with the flag in either position.
func TestTelegramChannelConfigurationChecked(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		create := func(key, ref, cfg string, extra ...string) []string {
			return append([]string{"channel", "create", "--type", string(model.TypeTelegramMessages), "--key", key, "--credential-ref", ref, "--configuration", cfg}, extra...)
		}
		good := `{"bot_id":7000000001,"bot_username":"heo_test_bot"}`
		for name, cfg := range map[string]string{
			"no bot ID":        `{"bot_username":"heo_test_bot"}`,
			"no username":      `{"bot_id":7000000001}`,
			"a negative ID":    `{"bot_id":-7,"bot_username":"heo_test_bot"}`,
			"a user's name":    `{"bot_id":7000000001,"bot_username":"alex"}`,
			"an http API URL":  `{"bot_id":7000000001,"bot_username":"heo_test_bot","api_url":"http://api.telegram.org/"}`,
			"an unknown field": `{"bot_id":7000000001,"bot_username":"heo_test_bot","token":"x"}`,
			"a long poll":      `{"bot_id":7000000001,"bot_username":"heo_test_bot","poll_timeout_seconds":120}`,
			"a string ID":      `{"bot_id":"7000000001","bot_username":"heo_test_bot"}`,
		} {
			wantStderr(t, c.heo(2, create("bad", "tg", cfg)...), "VALIDATION")
			wantStderr(t, c.heo(2, create("bad", "tg", cfg, "--dry-run")...), "VALIDATION")
			if got := c.channels(); len(got) != 0 {
				t.Fatalf("%s: wrote channels %+v", name, got)
			}
		}
		// The flag before the command's own.
		wantStderr(t, c.heo(2, append([]string{"--dry-run"}, create("bad", "tg", `{}`)...)...), "VALIDATION")

		c.heo(0, create("one", "tg-1", good, "--dry-run")...)
		if got := c.channels(); len(got) != 0 {
			t.Fatalf("a preview wrote channels %+v", got)
		}
		c.heo(0, create("one", "tg-1", good)...)
		wantStderr(t, c.heo(4, create("two", "tg-2", good)...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "tg-2", good, "--dry-run")...), "CONFLICT", "provider account")
		c.heo(0, create("three", "tg-3", `{"bot_id":7000000002,"bot_username":"heo_other_bot"}`)...)
		if got := c.channels(); len(got) != 2 {
			t.Fatalf("channels = %d, want the two that were accepted", len(got))
		}
		if out := c.heo(0, "channel", "check", "one").stdout; strings.Contains(out, "error") {
			t.Errorf("channel check reports an error:\n%s", out)
		}
		c.stop(d)
	})
}

// TestTelegramRecipientsOnTheCLI: how `message send` names a Telegram
// recipient, on the release build, with the flag in either position and
// nothing sent. A chat is written with its namespace, telegram.messages/
// room:<id>, and a group's negative ID is an ordinary value; a user is
// user:<id> in a per-contact inbox, and is not a chat; a forum topic lives
// inside a chat, so --to cannot name it and is answered with `message
// reply`. A preview names the route and writes nothing. (The bare
// room:<id> is read in the identity namespace, which holds users: the
// preview cannot tell, and the adapter refuses it when the send runs, see
// TestTelegramBareRoomIsRefusedWhenSent.)
func TestTelegramRecipientsOnTheCLI(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeTelegramMessages), "--key", "bot", "--credential-ref", "tg",
			"--configuration", `{"bot_id":7000000001,"bot_username":"heo_test_bot"}`)
		c.heo(0, "inbox", "create", "--channel", "bot", "--key", "rooms", "--destination", "7000000001", "--mode", model.ModePerRoom)
		send := func(inbox, to string, flag ...string) []string {
			return append([]string{"message", "send", "--inbox", inbox, "--to", to, "--text", "hello"}, flag...)
		}
		const group = "-1001234567890"
		out := c.heo(0, send("rooms", "telegram.messages/room:"+group, "--dry-run")...).stdout
		if !strings.Contains(out, "destination   room "+group) {
			t.Errorf("the preview does not name the chat:\n%s", out)
		}
		out = c.heo(0, append([]string{"--dry-run"}, send("rooms", "telegram.messages/room:"+group)...)...).stdout
		if !strings.Contains(out, "destination   room "+group) {
			t.Errorf("the preview, flag first, does not name the chat:\n%s", out)
		}
		for _, to := range []string{"user:5000000002", "telegram.messages.user/user:5000000002", "telegram.messages/thread:7"} {
			wantStderr(t, c.heo(2, send("rooms", to, "--dry-run")...), "VALIDATION")
			wantStderr(t, c.heo(2, append([]string{"--dry-run"}, send("rooms", to)...)...), "VALIDATION")
		}
		if got := c.conversationsOf("rooms"); len(got) != 0 {
			t.Fatalf("a refused or previewed send opened conversations %+v", got)
		}

		c.heo(0, "channel", "create", "--type", string(model.TypeTelegramMessages), "--key", "dm", "--credential-ref", "tg-2",
			"--configuration", `{"bot_id":7000000002,"bot_username":"heo_other_bot"}`)
		c.heo(0, "inbox", "create", "--channel", "dm", "--key", "people", "--destination", "7000000002", "--mode", model.ModePerContact,
			"--lock-to-single", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		out = c.heo(0, send("people", "user:5000000002", "--dry-run")...).stdout
		if !strings.Contains(out, "5000000002") {
			t.Errorf("the preview does not name the user:\n%s", out)
		}
		wantStderr(t, c.heo(2, send("people", "telegram.messages/room:"+group, "--dry-run")...), "VALIDATION")
		c.stop(d)
	})
}
