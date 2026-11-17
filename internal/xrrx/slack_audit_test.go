package xrrx

import (
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// cleanSlack is a Slack answer as a recording keeps it: placeholders only.
const cleanSlack = `xrr: "1"
adapter: http
fingerprint: dddddddd
payload:
    status: 200
    headers:
        Content-Type: application/json; charset=utf-8
    body: '{"channel":"C0000000001","message":{"app_id":"A0000000001","bot_id":"B0000000001","text":"heo adapter test 1","ts":"1760000000.000100","user":"U0000000001"},"ok":true,"ts":"1760000000.000100"}'
`

// TestAuditFlagsSlackLeaks: every shape of Slack identity a recording must
// not keep is found by name, in a cassette and in a fixture, and the
// placeholders pass. Twilio's and GitHub's placeholders are not Slack IDs.
func TestAuditFlagsSlackLeaks(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-dddddddd.resp.yaml", cleanSlack)
	write(t, d, "event.json", `{"team_id":"T0000000001","event":{"user":"U0000000002","channel":"C0000000001","ts":"1760000100.000100"},`+
		`"response_url":"https://hooks.slack.com/actions/T0000000001/1/placeholder","token":"`+SlackStandInToken+`","note":"AC00000000000000000000000000000001 SM00000000000000000000000000000001"}`)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("clean Slack files: %v, %v", fs, err)
	}
	for rule, mutate := range map[string]func(string) string{
		"slack ID":   func(s string) string { return strings.Replace(s, "U0000000001", "U01ABCDEFGH", 1) },
		"slack ID 2": func(s string) string { return strings.Replace(s, "C0000000001", "C024BE91L", 1) },
		"slack token": func(s string) string {
			return strings.Replace(s, `"ok"`, `"t":"xoxb-123456789012-abcdefghijkl","ok"`, 1)
		},
		"slack signature": func(s string) string { return strings.Replace(s, `"ok"`, `"h":"X-Slack-Signature","ok"`, 1) },
		"slack webhook URL": func(s string) string {
			return strings.Replace(s, `"ok"`, `"u":"https://hooks.slack.com/services/`+`T0000000001/B0000000001/abcdefghijklmnopqrstuvwx","ok"`, 1)
		},
		"slack workspace host": func(s string) string {
			return strings.Replace(s, `"ok"`, `"u":"https://acme.slack.com/archives/C1","ok"`, 1)
		},
	} {
		for _, name := range []string{"http-dddddddd.resp.yaml", "fixture.json"} {
			body := cleanSlack
			if name != "http-dddddddd.resp.yaml" {
				body = strings.ReplaceAll(cleanSlack, "\n", " ")
			}
			dd := t.TempDir()
			write(t, dd, name, mutate(body))
			fs, err := Audit(dd)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range fs {
				found = found || strings.HasPrefix(rule, f.Rule)
			}
			if !found {
				t.Errorf("%s in %s: not found (%v)", rule, name, fs)
			}
		}
	}
	// What a recording used is denied by name.
	write(t, d, "leak.json", `{"x":"the real channel"}`)
	if fs, _ := AuditWith(d, "the real channel"); len(fs) != 1 || fs[0].Rule != "recorded identity" {
		t.Errorf("a denied literal: %v", fs)
	}
}

// TestFromEnvReadsASlackSandbox: a recording process told the Slack
// sandbox gets it, and only in record mode with the live flag.
func TestFromEnvReadsASlackSandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(xrr.EnvMode, "record")
	t.Setenv(xrr.EnvCassetteDir, dir)
	t.Setenv("CI", "")
	for k, v := range map[string]string{
		EnvLive: "1", EnvSlackToken: slToken, EnvSlackTeamID: slTeam, EnvSlackBotUserID: slBotUser,
		EnvSlackChannelID: slChannel, EnvSlackLedger: t.TempDir(), EnvSlackMaxPosts: "2", EnvSlackRehearse: "1",
	} {
		t.Setenv(k, v)
	}
	e, err := FromEnv()
	if err != nil || e.Slack == nil || e.Slack.Max != 2 || !e.Slack.Rehearse || e.Slack.ChannelID != slChannel || e.Twilio != nil || e.GitHub != nil {
		t.Fatalf("record, live: %+v, %v", e, err)
	}
	t.Setenv(EnvSlackChannelID, "D01ABCDEFGH")
	if _, err := FromEnv(); err == nil {
		t.Error("a direct message was accepted as the channel")
	}
	t.Setenv(EnvSlackChannelID, slChannel)
	t.Setenv(xrr.EnvMode, "replay")
	if e, err := FromEnv(); err != nil || e.Slack != nil {
		t.Errorf("replay: %+v, %v; want no sandbox", e, err)
	}
}

// TestAuditIgnoresEmojiEscapes: a YAML string spells an emoji as "\U" and
// eight hex digits, which is shaped like a Slack user ID and is none. Only
// that escape is skipped: an ID after a backslash of another length is
// still one.
func TestAuditIgnoresEmojiEscapes(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-aaaaaaaa.req.yaml", "payload:\n    body: \"{\\\"emoji\\\":\\\"\\U0001F44D\\\"}\"\n")
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("an emoji escape: %v, %v", fs, err)
	}
	dd := t.TempDir()
	write(t, dd, "http-bbbbbbbb.req.yaml", "payload:\n    body: \"{\\\"user\\\":\\\"\\U01ABCDEFGH\\\"}\"\n")
	fs, _ := Audit(dd)
	found := false
	for _, f := range fs {
		found = found || f.Rule == "slack ID"
	}
	if !found {
		t.Errorf("an ID after a backslash: %v", fs)
	}
}
