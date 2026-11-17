package e2e

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

const (
	teamsTestTenant = "1111aaaa-1111-4111-8111-11111111bbbb"
	teamsTestApp    = "2222cccc-2222-4222-8222-22222222dddd"
)

var teamsTestConfig = teamsTestBot(teamsTestApp, teamsTestTenant)

// teamsTestBot is the configuration of the bot app in tenant.
func teamsTestBot(app, tenant string) string {
	return `{"app_id":"` + app + `","tenant_id":"` + tenant + `","service_url":"https://smba.trafficmanager.net/teams/"}`
}

// TestTeamsChannelConfigurationChecked: on the release build, a
// microsoft.teams channel needs its App ID, tenant ID and service URL,
// refuses what the adapter refuses (a value that is no GUID, an http
// service URL, one with user info, a field it does not know, so a client
// secret put there by mistake) and a credential in its configuration,
// each exit 2 with nothing written; a second channel of the same bot in
// the same tenant is refused exit 4 whatever its credential reference or
// the case of its GUIDs, since the Bot Connector posts a bot's activities
// to one endpoint, and another App ID is not. The preview runs the same
// checks and writes nothing, with the flag in either position.
func TestTeamsChannelConfigurationChecked(t *testing.T) {
	withE2EBinary(t, func() {
		// Many refused writes in a row: the daemon's own rate limit is not
		// what is under test.
		c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
		d := c.start()
		create := func(key, ref, cfg string, extra ...string) []string {
			return append([]string{"channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", key, "--credential-ref", ref, "--configuration", cfg}, extra...)
		}
		for name, cfg := range map[string]string{
			"no app ID":           `{"tenant_id":"` + teamsTestTenant + `","service_url":"https://smba.trafficmanager.net/teams/"}`,
			"no tenant ID":        `{"app_id":"` + teamsTestApp + `","service_url":"https://smba.trafficmanager.net/teams/"}`,
			"no service URL":      `{"app_id":"` + teamsTestApp + `","tenant_id":"` + teamsTestTenant + `"}`,
			"an app that is none": `{"app_id":"my-bot","tenant_id":"` + teamsTestTenant + `","service_url":"https://smba.trafficmanager.net/teams/"}`,
			"a tenant by name":    `{"app_id":"` + teamsTestApp + `","tenant_id":"contoso.onmicrosoft.com","service_url":"https://smba.trafficmanager.net/teams/"}`,
			"an http service":     `{"app_id":"` + teamsTestApp + `","tenant_id":"` + teamsTestTenant + `","service_url":"http://smba.trafficmanager.net/teams/"}`,
			"a service with user": `{"app_id":"` + teamsTestApp + `","tenant_id":"` + teamsTestTenant + `","service_url":"https://u:p@smba.trafficmanager.net/teams/"}`,
			"an unknown field":    `{"app_id":"` + teamsTestApp + `","tenant_id":"` + teamsTestTenant + `","service_url":"https://smba.trafficmanager.net/teams/","client_secret":"x"}`,
		} {
			wantStderr(t, c.heo(2, create("bad", "teams", cfg)...), "VALIDATION")
			wantStderr(t, c.heo(2, create("bad", "teams", cfg, "--dry-run")...), "VALIDATION")
			if got := c.channels(); len(got) != 0 {
				t.Fatalf("%s: wrote channels %+v", name, got)
			}
		}
		// The flag before the command's own.
		wantStderr(t, c.heo(2, append([]string{"--dry-run"}, create("bad", "teams", `{}`)...)...), "VALIDATION")

		c.heo(0, create("one", "teams-1", teamsTestConfig, "--dry-run")...)
		if got := c.channels(); len(got) != 0 {
			t.Fatalf("a preview wrote channels %+v", got)
		}
		c.heo(0, create("one", "teams-1", teamsTestConfig)...)
		wantStderr(t, c.heo(4, create("two", "teams-2", teamsTestConfig)...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "teams-2", teamsTestConfig, "--dry-run")...), "CONFLICT", "provider account")
		wantStderr(t, c.heo(4, create("two", "teams-2", teamsTestBot(strings.ToUpper(teamsTestApp), strings.ToUpper(teamsTestTenant)))...), "CONFLICT", "provider account")
		c.heo(0, create("three", "teams-3", teamsTestBot("3333dddd-3333-4333-8333-33333333eeee", teamsTestTenant))...)
		if got := c.channels(); len(got) != 2 {
			t.Fatalf("channels = %d, want the two that were accepted", len(got))
		}
		if out := c.heo(0, "channel", "check", "one").stdout; strings.Contains(out, "error") {
			t.Errorf("channel check reports an error:\n%s", out)
		}
		c.stop(d)
	})
}

// TestTeamsRecipientsOnTheCLI: how `message send` names a Teams recipient,
// on the release build, with the flag in either position and nothing
// sent. A conversation (a channel, a group chat or a personal chat) is
// written with its namespace, microsoft.teams/room:<conversation ID>,
// whose colon and @ are ordinary characters of the ID; a user is
// user:<Teams ID> in a per-contact inbox, and is not a conversation; a
// channel thread lives inside a channel, so --to cannot name it and is
// answered with `message reply`. A preview names the route and writes
// nothing. (The bare room:<id> is read in the identity namespace, which
// holds users: the preview cannot tell, and the adapter refuses it when
// the send runs, see TestTeamsBareRoomIsRefusedWhenSent.)
func TestTeamsRecipientsOnTheCLI(t *testing.T) {
	withE2EBinary(t, func() {
		// Many refused writes in a row: the daemon's own rate limit is not
		// what is under test.
		c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "teams", "--credential-ref", "teams",
			"--configuration", teamsTestConfig)
		c.heo(0, "inbox", "create", "--channel", "teams", "--key", "rooms", "--destination", teamsTestTenant, "--mode", model.ModePerRoom)

		// The verb table, offline: microsoft.teams declares edit, delete and
		// react, and not respond (an Action.Execute is answered by a reply or
		// an edit), history or the membership, receipt or work item verbs.
		table := c.heo(0, "channel", "get", "teams").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)",
			"verb reaction add         declared (react)", "verb reaction remove      declared (react)",
			"verb interaction respond  unsupported: not_declared (respond)",
			"verb history fetch        unsupported: not_declared (history)",
			"verb participant add      unsupported: not_declared (membership)", "verb receipt send         unsupported: not_declared (receipt)",
			"verb conversation status unsupported: not_declared (work_item_act)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		send := func(inbox, to string, flag ...string) []string {
			return append([]string{"message", "send", "--inbox", inbox, "--to", to, "--text", "hello"}, flag...)
		}
		const group = "19:group456@thread.v2"
		out := c.heo(0, send("rooms", "microsoft.teams/room:"+group, "--dry-run")...).stdout
		if !strings.Contains(out, "destination   room "+group) {
			t.Errorf("the preview does not name the conversation:\n%s", out)
		}
		out = c.heo(0, append([]string{"--dry-run"}, send("rooms", "microsoft.teams/room:"+group)...)...).stdout
		if !strings.Contains(out, "destination   room "+group) {
			t.Errorf("the preview, flag first, does not name the conversation:\n%s", out)
		}
		for _, to := range []string{"user:29:alice", "microsoft.teams.user/user:29:alice", "microsoft.teams/thread:1760000000000"} {
			wantStderr(t, c.heo(2, send("rooms", to, "--dry-run")...), "VALIDATION")
			wantStderr(t, c.heo(2, append([]string{"--dry-run"}, send("rooms", to)...)...), "VALIDATION")
		}
		if got := c.conversationsOf("rooms"); len(got) != 0 {
			t.Fatalf("a refused or previewed send opened conversations %+v", got)
		}
		// A per-thread inbox takes a thread by its root message, and the
		// preview names it; the thread's channel cannot be written with it,
		// so the adapter refuses the route when the send runs (see
		// TestTeamsBareRoomIsRefusedWhenSent): a thread is answered with
		// `message reply`.
		c.heo(0, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "threads", "--credential-ref", "teams-3",
			"--configuration", teamsTestBot("4444eeee-4444-4444-8444-44444444ffff", teamsTestTenant))
		c.heo(0, "inbox", "create", "--channel", "threads", "--key", "posts", "--destination", teamsTestTenant, "--mode", model.ModePerThread)
		out = c.heo(0, send("posts", "microsoft.teams/thread:1760000000000", "--dry-run")...).stdout
		if !strings.Contains(out, "destination   thread 1760000000000") {
			t.Errorf("the preview does not name the thread:\n%s", out)
		}
		wantStderr(t, c.heo(2, send("posts", "microsoft.teams/room:"+group, "--dry-run")...), "VALIDATION")
		if got := c.conversationsOf("posts"); len(got) != 0 {
			t.Fatalf("a previewed send opened conversations %+v", got)
		}

		c.heo(0, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "dm", "--credential-ref", "teams-2",
			"--configuration", teamsTestBot("3333dddd-3333-4333-8333-33333333eeee", teamsTestTenant))
		c.heo(0, "inbox", "create", "--channel", "dm", "--key", "people", "--destination", teamsTestTenant, "--mode", model.ModePerContact,
			"--lock-to-single", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		out = c.heo(0, send("people", "user:29:alice", "--dry-run")...).stdout
		if !strings.Contains(out, "29:alice") {
			t.Errorf("the preview does not name the user:\n%s", out)
		}
		wantStderr(t, c.heo(2, send("people", "microsoft.teams/room:"+group, "--dry-run")...), "VALIDATION")
		c.stop(d)
	})
}
