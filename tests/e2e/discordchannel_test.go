package e2e

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestDiscordChannelConfigurationChecked: on the release build, a
// discord.chat channel needs its application and bot user IDs, refuses
// what the adapter refuses (an http API URL, an http gateway, a field it
// does not know, a receive mode it does not offer) and a credential in its configuration, each exit 2 with nothing
// written; a second channel on the same application is refused exit 4
// whatever its credential reference or its receive mode, since an
// application has one bot token and one Interactions Endpoint URL, and
// another application is not. The preview runs the same checks and writes
// nothing, with the flag in either position.
func TestDiscordChannelConfigurationChecked(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		create := func(key, ref, cfg string, extra ...string) []string {
			return append([]string{"channel", "create", "--type", string(model.TypeDiscordChat), "--key", key, "--credential-ref", ref, "--configuration", cfg}, extra...)
		}
		good := `{"application_id":"900000000000000001","bot_user_id":"900000000000000002"}`
		for name, cfg := range map[string]string{
			"no application":   `{"bot_user_id":"900000000000000002"}`,
			"no bot user":      `{"application_id":"900000000000000001"}`,
			"a name for an ID": `{"application_id":"heo","bot_user_id":"900000000000000002"}`,
			"an http API URL":  `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","api_url":"http://discord.com/api/v10/"}`,
			"an http gateway":  `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","gateway_url":"ws://gateway.discord.gg"}`,
			"an unknown field": `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","token":"x"}`,
			"a receive mode":   `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","receive":"socket"}`,
		} {
			wantStderr(t, c.heo(2, create("bad", "dc", cfg)...), "VALIDATION")
			wantStderr(t, c.heo(2, create("bad", "dc", cfg, "--dry-run")...), "VALIDATION")
			if got := c.channels(); len(got) != 0 {
				t.Fatalf("%s: wrote channels %+v", name, got)
			}
		}
		// The flag before the command's own.
		wantStderr(t, c.heo(2, append([]string{"--dry-run"}, create("bad", "dc", `{}`)...)...), "VALIDATION")

		c.heo(0, create("one", "dc-1", good, "--dry-run")...)
		if got := c.channels(); len(got) != 0 {
			t.Fatalf("a preview wrote channels %+v", got)
		}
		c.heo(0, create("one", "dc-1", good)...)
		wantStderr(t, c.heo(4, create("two", "dc-2", good)...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "dc-2", good, "--dry-run")...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "dc-2", `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","receive":"interactions"}`, "--dry-run")...), "CONFLICT", "provider account")
		c.heo(0, create("three", "dc-3", `{"application_id":"900000000000000003","bot_user_id":"900000000000000004"}`)...)
		if got := c.channels(); len(got) != 2 {
			t.Fatalf("channels = %d, want the two that were accepted", len(got))
		}
		if out := c.heo(0, "channel", "check", "one").stdout; strings.Contains(out, "error") {
			t.Errorf("channel check reports an error:\n%s", out)
		}
		c.stop(d)
	})
}

// TestDiscordRecipientsOnTheCLI: how `message send` names a Discord
// recipient, on the release build, with the flag in either position and
// nothing sent. A channel is written with its namespace, discord.chat/
// room:<id>; a user is user:<id> in a per-contact inbox, and is not a
// channel; a thread lives under a channel, so --to cannot name it and is
// answered with `message reply`. A preview names the route and writes
// nothing. (The bare room:<id> is read in the identity namespace, which
// holds users: the preview cannot tell, and the adapter refuses it when
// the send runs, see TestDiscordBareRoomIsRefusedWhenSent.)
func TestDiscordRecipientsOnTheCLI(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeDiscordChat), "--key", "bot", "--credential-ref", "dc",
			"--configuration", `{"application_id":"900000000000000001","bot_user_id":"900000000000000002"}`)
		c.heo(0, "inbox", "create", "--channel", "bot", "--key", "rooms", "--destination", "900000000000000001", "--mode", model.ModePerRoom)
		send := func(inbox, to string, flag ...string) []string {
			return append([]string{"message", "send", "--inbox", inbox, "--to", to, "--text", "hello"}, flag...)
		}
		const room = "900000000000000010"
		out := c.heo(0, send("rooms", "discord.chat/room:"+room, "--dry-run")...).stdout
		if !strings.Contains(out, "destination   room "+room) {
			t.Errorf("the preview does not name the channel:\n%s", out)
		}
		out = c.heo(0, append([]string{"--dry-run"}, send("rooms", "discord.chat/room:"+room)...)...).stdout
		if !strings.Contains(out, "destination   room "+room) {
			t.Errorf("the preview, flag first, does not name the channel:\n%s", out)
		}
		for _, to := range []string{"user:900000000000000003", "discord.chat.user/user:900000000000000003", "discord.chat/thread:900000000000000011"} {
			wantStderr(t, c.heo(2, send("rooms", to, "--dry-run")...), "VALIDATION")
			wantStderr(t, c.heo(2, append([]string{"--dry-run"}, send("rooms", to)...)...), "VALIDATION")
		}
		if got := c.conversationsOf("rooms"); len(got) != 0 {
			t.Fatalf("a refused or previewed send opened conversations %+v", got)
		}

		c.heo(0, "channel", "create", "--type", string(model.TypeDiscordChat), "--key", "dm", "--credential-ref", "dc-2",
			"--configuration", `{"application_id":"900000000000000005","bot_user_id":"900000000000000006"}`)
		c.heo(0, "inbox", "create", "--channel", "dm", "--key", "people", "--destination", "900000000000000005", "--mode", model.ModePerContact,
			"--lock-to-single", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		out = c.heo(0, send("people", "user:900000000000000003", "--dry-run")...).stdout
		if !strings.Contains(out, "900000000000000003") {
			t.Errorf("the preview does not name the user:\n%s", out)
		}
		wantStderr(t, c.heo(2, send("people", "discord.chat/room:"+room, "--dry-run")...), "VALIDATION")
		c.stop(d)
	})
}

// TestDiscordChannelCheckRemoteReportsReceiving: on the release build with
// no credential stored, `channel check --remote` reports each way the
// channel receives as the running daemon runs it: the gateway receiver
// failed (it cannot read the bot token, and nothing reaches Discord), the
// Interactions Endpoint URL's route ready at its path, the failed receiver
// an error finding and exit 2, with the flag in either position and both
// as a table and as JSON. Moved to receive interactions the gateway reads
// off and is not an error, whatever else the check still finds.
func TestDiscordChannelCheckRemoteReportsReceiving(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		var made channelOutJSON
		c.json(&made, "channel", "create", "--type", string(model.TypeDiscordChat), "--key", "bot", "--credential-ref", "dc",
			"--configuration", `{"application_id":"900000000000000001","bot_user_id":"900000000000000002"}`)
		c.heo(0, "inbox", "create", "--channel", "bot", "--key", "rooms", "--destination", "900000000000000001", "--mode", model.ModePerRoom)

		type receiving struct {
			Way, State, Reason, Path string
		}
		type report struct {
			OK     bool
			Checks []struct {
				Findings []struct{ Severity, Scope, Code, Message string }
				Remote   struct{ Receiving []receiving }
			}
		}
		check := func(args ...string) (report, result) {
			t.Helper()
			var res report
			r := c.heo(2, append([]string{"channel", "check"}, append(args, "--format", "json")...)...)
			if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || len(res.Checks) != 1 {
				t.Fatalf("%v\n%s", err, r.stdout)
			}
			return res, r
		}
		ways := func(res report) map[string]receiving {
			out := map[string]receiving{}
			for _, r := range res.Checks[0].Remote.Receiving {
				out[r.Way] = r
			}
			return out
		}
		var res report
		eventually(t, "the gateway receiver to fail", func() bool {
			res, _ = check("bot", "--remote")
			return ways(res)["receive"].State == "failed"
		})
		w := ways(res)
		if len(w) != 2 || w["receive"].Reason == "" || w["receive_http"].State != "ready" ||
			w["receive_http"].Path != "/webhooks/"+string(made.Channel.Ref.ID) {
			t.Errorf("receiving = %+v, want the gateway failed with a reason and the route ready at its path", res.Checks[0].Remote.Receiving)
		}
		failed := false
		for _, f := range res.Checks[0].Findings {
			failed = failed || (f.Scope == "receiving:receive" && f.Code == "receiver_failed" && f.Severity == "error")
		}
		if res.OK || !failed {
			t.Errorf("check = %+v, want an error finding for the failed receiver", res)
		}
		// The flag in the other position, and as a table.
		check("--remote", "bot")
		table := c.heo(2, "channel", "check", "--remote", "bot").stdout
		for _, want := range []string{`receiving\s+receive\s+failed\s+not_found: credential not found`,
			`receiving\s+receive_http\s+ready\s+/webhooks/` + string(made.Channel.Ref.ID)} {
			if !regexp.MustCompile(want).MatchString(table) {
				t.Errorf("table lacks %q:\n%s", want, table)
			}
		}

		// Receive interactions only: the gateway is off, not failed.
		c.heo(0, "channel", "update", "bot", "--revision", string(made.Channel.Revision),
			"--configuration", `{"application_id":"900000000000000001","bot_user_id":"900000000000000002","receive":"interactions"}`)
		res, _ = check("bot", "--remote")
		w = ways(res)
		if w["receive"].State != "off" || !strings.Contains(w["receive"].Reason, "configuration") || w["receive_http"].State != "ready" {
			t.Errorf("receiving after the move = %+v, want the gateway off by configuration", res.Checks[0].Remote.Receiving)
		}
		for _, f := range res.Checks[0].Findings {
			if strings.HasPrefix(f.Scope, "receiving:") {
				t.Errorf("a way switched off raised %+v", f)
			}
		}
		c.stop(d)
	})
}
