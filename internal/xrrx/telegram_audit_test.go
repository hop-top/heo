package xrrx

import (
	"strconv"
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// cleanTelegram is a Bot API answer as a recording keeps it: placeholders
// only.
const cleanTelegram = `xrr: "1"
adapter: http
fingerprint: eeeeeeee
payload:
    status: 200
    headers:
        Content-Type: application/json
    body: '{"ok":true,"result":{"chat":{"id":-1000000000001,"type":"supergroup"},"date":1760000000,"from":{"id":7000000001,"is_bot":true,"username":"heo_adapter_test_bot"},"message_id":42,"text":"heo adapter test 1"}}'
`

// TestAuditFlagsTelegramLeaks: every shape of Telegram identity a
// recording must not keep is found by name, in a cassette and in a
// fixture, and the placeholders pass. The other providers' files, which
// hold titles and numbers of their own, are not read for these.
func TestAuditFlagsTelegramLeaks(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-eeeeeeee.resp.yaml", cleanTelegram)
	write(t, d, "update.json", `{"update_id":1,"message":{"message_id":7,"from":{"id":5000000002,"is_bot":false},"chat":{"id":-1000000000001,"type":"supergroup"},"date":1760000500,"text":"heo adapter test inbound"}}`)
	write(t, d, "http-ffffffff.resp.yaml", `xrr: "1"
adapter: http
fingerprint: ffffffff
payload:
    status: 200
    body: '{"title":"A GitHub issue","user":{"id":123456789,"login":"sandbox-account","username":"someone"}}'
`)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("clean Telegram files, and another provider's: %v, %v", fs, err)
	}
	for rule, mutate := range map[string]func(string) string{
		"telegram bot token": func(s string) string {
			return strings.Replace(s, `"ok"`, `"t":"123456789:AAEhBP0av28hBzfzuhw2Xv6Y1Gj3vZ_made_up","ok"`, 1)
		},
		"telegram secret token header": func(s string) string {
			return strings.Replace(s, `"ok"`, `"h":"X-Telegram-Bot-Api-Secret-Token","ok"`, 1)
		},
		"telegram name":     func(s string) string { return strings.Replace(s, `"ok"`, `"first_name":"Acme","ok"`, 1) },
		"telegram username": func(s string) string { return strings.Replace(s, `"heo_adapter_test_bot"`, `"acme_support_bot"`, 1) },
		"telegram ID":       func(s string) string { return strings.Replace(s, `-1000000000001`, `-1001234567890`, 1) },
		"telegram ID 2":     func(s string) string { return strings.Replace(s, `"id":7000000001`, `"id":123456789`, 1) },
		"telegram ID 3":     func(s string) string { return strings.Replace(s, `"ok"`, `"chat_id":-1001234567890,"ok"`, 1) },
	} {
		for _, name := range []string{"http-eeeeeeee.resp.yaml", "fixture.json"} {
			body := cleanTelegram
			if name != "http-eeeeeeee.resp.yaml" {
				body = strings.ReplaceAll(cleanTelegram, "\n", " ")
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
	// The stand-in token is the one value allowed, in a request path.
	dd := t.TempDir()
	write(t, dd, "http-11111111.req.yaml", "xrr: \"1\"\nadapter: http\nfingerprint: 11111111\npayload:\n    method: POST\n    url: https://api.telegram.org/bot"+TelegramStandInToken+"/sendMessage\n")
	if fs, _ := Audit(dd); len(fs) != 0 {
		t.Errorf("the stand-in token in a request path: %v", fs)
	}
	// What a recording used is denied by name.
	write(t, d, "leak.json", `{"x":"the real chat"}`)
	if fs, _ := AuditWith(d, "the real chat"); len(fs) != 1 || fs[0].Rule != "recorded identity" {
		t.Errorf("a denied literal: %v", fs)
	}
}

// TestFromEnvReadsATelegramSandbox: a recording process told the Telegram
// sandbox gets it, and only in record mode with the live flag.
func TestFromEnvReadsATelegramSandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(xrr.EnvMode, "record")
	t.Setenv(xrr.EnvCassetteDir, dir)
	t.Setenv("CI", "")
	for k, v := range map[string]string{
		EnvLive: "1", EnvTelegramToken: tgToken, EnvTelegramBotUsername: tgUsername, EnvTelegramChatID: strconv.FormatInt(tgChat, 10),
		EnvTelegramLedger: t.TempDir(), EnvTelegramMaxSends: "2", EnvTelegramRehearse: "1",
	} {
		t.Setenv(k, v)
	}
	e, err := FromEnv()
	if err != nil || e.Telegram == nil || e.Telegram.Max != 2 || !e.Telegram.Rehearse || e.Telegram.ChatID != tgChat || e.Telegram.BotID != tgBot ||
		e.Slack != nil || e.Twilio != nil || e.GitHub != nil {
		t.Fatalf("record, live: %+v, %v", e, err)
	}
	t.Setenv(EnvTelegramChatID, "@acme_channel")
	if _, err := FromEnv(); err == nil {
		t.Error("a channel's name was accepted as the chat")
	}
	t.Setenv(EnvTelegramChatID, strconv.FormatInt(tgChat, 10))
	t.Setenv(xrr.EnvMode, "replay")
	if e, err := FromEnv(); err != nil || e.Telegram != nil {
		t.Errorf("replay: %+v, %v; want no sandbox", e, err)
	}
}
