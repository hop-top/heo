package xrrx

import (
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// cleanDiscord is a Discord answer as a recording keeps it: placeholders
// only.
const cleanDiscord = `xrr: "1"
adapter: http
fingerprint: eeeeeeee
payload:
    status: 200
    headers:
        Content-Type: application/json
    body: '{"application_id":"900000000000000101","author":{"bot":true,"id":"900000000000000102","username":"heo-adapter-test-bot"},"channel_id":"900000000000000104","content":"heo adapter test 1","id":"1700000000000000042","mention_everyone":false,"mentions":[{"id":"900000000000000200"}],"timestamp":"2026-10-14T10:00:00.000000+00:00"}'
`

// TestAuditFlagsDiscordLeaks: every shape of Discord identity a recording
// must not keep is found by name, in a cassette and in a fixture, and the
// placeholders pass. The other providers' files, which hold usernames and
// numbers of their own, are not read for these.
func TestAuditFlagsDiscordLeaks(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-eeeeeeee.resp.yaml", cleanDiscord)
	write(t, d, "interaction.json", `{"application_id":"900000000000000101","channel_id":"900000000000000104","data":{"name":"triage","type":1},"guild_id":"900000000000000103","id":"1700000000000000500","member":{"user":{"id":"900000000000000105"}},"mention_everyone":false,"type":2}`)
	write(t, d, "http-ffffffff.resp.yaml", `xrr: "1"
adapter: http
fingerprint: ffffffff
payload:
    status: 200
    body: '{"title":"A GitHub issue","user":{"id":123456789012345678,"login":"sandbox-account","username":"someone"},"avatar":"x"}'
`)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("clean Discord files, and another provider's: %v, %v", fs, err)
	}
	for rule, mutate := range map[string]func(string) string{
		"discord bot token": func(s string) string {
			return strings.Replace(s, `"content"`, `"t":"MTIzNDU2Nzg5MDEyMzQ1Njc4.GabcDE.made_up_hmac_of_twenty_seven_chars","content"`, 1)
		},
		"discord name":     func(s string) string { return strings.Replace(s, `"content"`, `"global_name":"Acme","content"`, 1) },
		"discord username": func(s string) string { return strings.Replace(s, `"heo-adapter-test-bot"`, `"acme_support"`, 1) },
		"discord ID":       func(s string) string { return strings.Replace(s, `"900000000000000104"`, `"423456789012345678"`, 1) },
		"discord ID 2": func(s string) string {
			return strings.Replace(s, `"id":"900000000000000102"`, `"id":"123456789012345678"`, 1)
		},
		"discord ID 3": func(s string) string {
			return strings.Replace(s, `"content"`, `"guild_id":"323456789012345678","content"`, 1)
		},
		"discord ID 4": func(s string) string {
			return strings.Replace(s, `"id":"900000000000000200"`, `"id":"523456789012345678"`, 1)
		},
	} {
		for _, name := range []string{"http-eeeeeeee.resp.yaml", "fixture.json"} {
			body := cleanDiscord
			if name != "http-eeeeeeee.resp.yaml" {
				body = strings.ReplaceAll(cleanDiscord, "\n", " ")
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
	// The stand-in token is the one value allowed, in a request header
	// value a test pins.
	dd := t.TempDir()
	write(t, dd, "http-11111111.req.yaml", "xrr: \"1\"\nadapter: http\nfingerprint: 11111111\npayload:\n    method: POST\n    url: https://discord.com/api/v10/channels/900000000000000104/messages\n    note: "+DiscordStandInToken+"\n")
	if fs, _ := Audit(dd); len(fs) != 0 {
		t.Errorf("the stand-in token: %v", fs)
	}
	// What a recording used is denied by name.
	write(t, d, "leak.json", `{"x":"the real channel"}`)
	if fs, _ := AuditWith(d, "the real channel"); len(fs) != 1 || fs[0].Rule != "recorded identity" {
		t.Errorf("a denied literal: %v", fs)
	}
}

// TestFromEnvReadsADiscordSandbox: a recording process told the Discord
// sandbox gets it, and only in record mode with the live flag.
func TestFromEnvReadsADiscordSandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(xrr.EnvMode, "record")
	t.Setenv(xrr.EnvCassetteDir, dir)
	t.Setenv("CI", "")
	for k, v := range map[string]string{
		EnvLive: "1", EnvDiscordToken: dcToken, EnvDiscordApplicationID: dcApplication, EnvDiscordBotUserID: dcBot, EnvDiscordGuildID: dcGuild,
		EnvDiscordChannelID: dcChannel, EnvDiscordLedger: t.TempDir(), EnvDiscordMaxSends: "2", EnvDiscordRehearse: "1",
	} {
		t.Setenv(k, v)
	}
	e, err := FromEnv()
	if err != nil || e.Discord == nil || e.Discord.Max != 2 || !e.Discord.Rehearse || e.Discord.ChannelID != dcChannel || e.Discord.BotUserID != dcBot ||
		e.Slack != nil || e.Telegram != nil || e.Twilio != nil || e.GitHub != nil {
		t.Fatalf("record, live: %+v, %v", e, err)
	}
	t.Setenv(EnvDiscordChannelID, "#general")
	if _, err := FromEnv(); err == nil {
		t.Error("a channel's name was accepted as the channel")
	}
	t.Setenv(EnvDiscordChannelID, dcChannel)
	t.Setenv(xrr.EnvMode, "replay")
	if e, err := FromEnv(); err != nil || e.Discord != nil {
		t.Errorf("replay: %+v, %v; want no sandbox", e, err)
	}
}
