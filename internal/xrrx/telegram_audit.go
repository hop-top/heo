package xrrx

import (
	"regexp"
	"strconv"
)

// What a Telegram recording must not carry, beyond what every cassette
// must not (see AuditWith). The bot's and the chat's own names are not
// listed here: that would publish them. A token, the secret header and the
// keys that name a person are rules for every file; a username and a
// number are read only in a file that looks like the Bot API's, since
// other providers' cassettes hold usernames and numbers of their own. A
// recording also denies the literals it used (TelegramSandbox.Deny).
var (
	// telegramSecretHeaderRe is the webhook secret token's header. Fixtures
	// are stored without it and sent with it when the test runs.
	telegramSecretHeaderRe = regexp.MustCompile(`(?i)x-telegram-bot-api-secret-token`)
	// telegramNameRe is a key that names a person or a group.
	telegramNameRe = regexp.MustCompile(`\b(?:first_name|last_name|language_code)\\?"\s*:`)
	// telegramUsernameRe captures the value of a username field.
	telegramUsernameRe = regexp.MustCompile(`\busername\\?"\s*:\s*\\?"([^"\\]*)`)
	// telegramIDRe captures the number of an id or chat_id field.
	telegramIDRe = regexp.MustCompile(`\b(?:id|chat_id|migrate_to_chat_id)\\?"\s*:\s*(-?[0-9]{6,})`)
	// telegramShapeRe is what marks a file as the Bot API's: its host in a
	// request, or a field only its objects carry.
	telegramShapeRe = regexp.MustCompile(`api\.telegram\.org|\bis_bot\\?"|\bupdate_id\\?"|\bcallback_query\\?"|\bmessage_id\\?"`)
)

// telegramPlaceholderID reports whether n is an ID a recording leaves:
// the bot's, the chat's, the fixtures' person and the numbered others.
func telegramPlaceholderID(n int64) bool {
	switch {
	case n == TelegramBotID || n == TelegramChatID || n == TelegramPerson:
		return true
	case n >= telegramOtherBase && n < telegramOtherBase+1000:
		return true
	case n <= -1000000000100 && n > -1000000001100:
		return true
	}
	return false
}

// telegramFindings is the rules above over one file's text.
func telegramFindings(text string) [][2]string {
	var out [][2]string
	for _, m := range telegramTokenAnyRe.FindAllString(text, -1) {
		if m != TelegramStandInToken {
			out = append(out, [2]string{"telegram bot token", m})
			break
		}
	}
	if m := telegramSecretHeaderRe.FindString(text); m != "" {
		out = append(out, [2]string{"telegram secret token header", m})
	}
	if m := telegramNameRe.FindString(text); m != "" {
		out = append(out, [2]string{"telegram name", m})
	}
	if !telegramShapeRe.MatchString(text) {
		return out
	}
	for _, m := range telegramUsernameRe.FindAllStringSubmatch(text, -1) {
		if m[1] != TelegramBotUsername {
			out = append(out, [2]string{"telegram username", m[0]})
			break
		}
	}
	for _, m := range telegramIDRe.FindAllStringSubmatch(text, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && !telegramPlaceholderID(n) {
			out = append(out, [2]string{"telegram ID", m[0]})
			break
		}
	}
	return out
}
