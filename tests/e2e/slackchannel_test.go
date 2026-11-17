package e2e

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestSlackChannelConfigurationChecked: on the release build, a slack.chat
// channel needs its workspace and bot user, refuses what the adapter
// refuses (an http API URL, a field it does not know, a mention-only flag
// of the wrong type) and a credential in its configuration, each exit 2
// with nothing written; a second channel on the same install is refused
// exit 4 whatever its credential reference, and another bot of the
// workspace is not. The preview runs the same checks and writes nothing,
// with the flag in either position.
func TestSlackChannelConfigurationChecked(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		create := func(key, ref, cfg string, extra ...string) []string {
			return append([]string{"channel", "create", "--type", string(model.TypeSlackChat), "--key", key, "--credential-ref", ref, "--configuration", cfg}, extra...)
		}
		good := `{"team_id":"T0000000001","bot_user_id":"U0000000001"}`
		for name, cfg := range map[string]string{
			"no workspace":      `{"bot_user_id":"U0000000001"}`,
			"no bot":            `{"team_id":"T0000000001"}`,
			"a lower-case ID":   `{"team_id":"t0000000001","bot_user_id":"U0000000001"}`,
			"an http API URL":   `{"team_id":"T0000000001","bot_user_id":"U0000000001","api_url":"http://slack.com/api/"}`,
			"an unknown field":  `{"team_id":"T0000000001","bot_user_id":"U0000000001","token":"x"}`,
			"a bad window":      `{"team_id":"T0000000001","bot_user_id":"U0000000001","webhook_tolerance_seconds":900}`,
			"too many channels": `{"team_id":"T0000000001","bot_user_id":"U0000000001","history_channels":["C01","C02","C03","C04","C05","C06","C07","C08","C09","C10","C11"]}`,
		} {
			wantStderr(t, c.heo(2, create("bad", "slack", cfg)...), "VALIDATION")
			wantStderr(t, c.heo(2, create("bad", "slack", cfg, "--dry-run")...), "VALIDATION")
			if got := c.channels(); len(got) != 0 {
				t.Fatalf("%s: wrote channels %+v", name, got)
			}
		}
		// The flag before the command's own.
		wantStderr(t, c.heo(2, append([]string{"--dry-run"}, create("bad", "slack", `{}`)...)...), "VALIDATION")

		c.heo(0, create("one", "slack-1", good, "--dry-run")...)
		if got := c.channels(); len(got) != 0 {
			t.Fatalf("a preview wrote channels %+v", got)
		}
		c.heo(0, create("one", "slack-1", good)...)
		r := c.heo(4, create("two", "slack-2", good)...)
		wantStderr(t, r, "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "slack-2", good, "--dry-run")...), "CONFLICT", "provider account")
		c.heo(0, create("three", "slack-3", `{"team_id":"T0000000001","bot_user_id":"U0000000002"}`)...)
		if got := c.channels(); len(got) != 2 {
			t.Fatalf("channels = %d, want the two that were accepted", len(got))
		}
		if out := c.heo(0, "channel", "check", "one").stdout; strings.Contains(out, "error") {
			t.Errorf("channel check reports an error:\n%s", out)
		}
		c.stop(d)
	})
}
