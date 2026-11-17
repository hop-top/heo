package xrrx

import "regexp"

// What a Discord recording must not carry, beyond what every cassette must
// not (see AuditWith). The application's, the bot's, the server's and the
// channel's own IDs are not listed here: that would publish them. A bot
// token and the keys that name a person are rules for every file; an ID or
// a username is read only in a file that looks like Discord's, since other
// providers' cassettes hold usernames and numbers of their own. A
// recording also denies the literals it used (DiscordSandbox.Deny).
var (
	// discordNameRe is a key that names a person.
	discordNameRe = regexp.MustCompile(`\b(?:global_name|display_name|discriminator)\\?"\s*:`)
	// discordUsernameRe captures the value of a username field.
	discordUsernameRe = regexp.MustCompile(`\busername\\?"\s*:\s*\\?"([^"\\]*)`)
	// discordAccountIDRe captures the ID an account, server or channel key
	// holds.
	discordAccountIDRe = regexp.MustCompile(`\b(?:application_id|guild_id|channel_id|webhook_id|parent_id|owner_id|user_id|bot_user_id|author_id|recipient_id)\\?"\s*:\s*\\?"([0-9]{17,20})`)
	// discordUserIDRe captures the ID of a user object: the id field right
	// after a key naming one.
	discordUserIDRe = regexp.MustCompile(`\b(?:author|user|member|mentions)\\?"\s*:\s*\[?\s*\{[^{}]*?\\?"id\\?"\s*:\s*\\?"([0-9]{17,20})`)
	// discordShapeRe is what marks a file as Discord's: its host in a
	// request, or a field only its messages carry.
	discordShapeRe = regexp.MustCompile(`discord\.com/api|\bmention_everyone\\?"|\benforce_nonce\\?"|\bmention_roles\\?"|\bguild_id\\?"`)
)

// discordPlaceholderID reports whether id is an ID a recording leaves: the
// sandbox's, the fixtures' person and the numbered others.
func discordPlaceholderID(id string) bool {
	switch id {
	case DiscordApplicationID, DiscordBotUserID, DiscordGuildID, DiscordChannelID, DiscordPerson:
		return true
	}
	return len(id) == len(DiscordPerson) && id >= "900000000000000200" && id < "900000000000001200"
}

// discordFindings is the rules above over one file's text.
func discordFindings(text string) [][2]string {
	var out [][2]string
	for _, m := range discordTokenAnyRe.FindAllString(text, -1) {
		if m != DiscordStandInToken {
			out = append(out, [2]string{"discord bot token", m})
			break
		}
	}
	if m := discordNameRe.FindString(text); m != "" {
		out = append(out, [2]string{"discord name", m})
	}
	if !discordShapeRe.MatchString(text) {
		return out
	}
	for _, m := range discordUsernameRe.FindAllStringSubmatch(text, -1) {
		if m[1] != DiscordBotUsername {
			out = append(out, [2]string{"discord username", m[0]})
			break
		}
	}
	for _, re := range []*regexp.Regexp{discordAccountIDRe, discordUserIDRe} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if !discordPlaceholderID(m[1]) {
				out = append(out, [2]string{"discord ID", m[0]})
				break
			}
		}
	}
	return out
}
