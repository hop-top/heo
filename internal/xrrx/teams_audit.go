package xrrx

import "regexp"

// What a Teams recording must not carry, beyond what every cassette must
// not (see AuditWith). The bot's, the tenant's and the user's own values
// are not listed here: that would publish them. A Bot Connector token and
// a Microsoft service host are rules for every file; an account, a chat,
// a GUID and a name key are read only in a file that looks like the Bot
// Connector's, since other providers' cassettes hold GUIDs and names of
// their own. A recording also denies the literals it used
// (TeamsSandbox.Deny).
var (
	// teamsShapeRe is what marks a file as the Bot Connector's: the
	// placeholder service in a request, or the path or field only its
	// objects carry.
	teamsShapeRe = regexp.MustCompile(`(?i)smba\.|/v3/conversations|\bconversationType\\?"|\bserviceUrl\\?"|\bactivityId\\?"|\bmsteams\b`)
	// teamsRealServiceRe is a Microsoft Bot Connector service host, which
	// only the placeholder may replace.
	teamsRealServiceRe = regexp.MustCompile(`(?i)[a-z0-9.-]+\.(?:trafficmanager\.net|teams\.microsoft\.com|teams\.microsoft\.us)`)
	// teamsOwnedGUIDs and the accounts and chats a placeholder names.
	teamsAuditAccountRe = regexp.MustCompile(`\b2[89]:[A-Za-z0-9_-]{8,}`)
	teamsAuditChatRe    = regexp.MustCompile(`\ba:[A-Za-z0-9_-]{8,}`)
)

// teamsPlaceholderAccount reports whether m is an account a recording
// leaves: the bot's, the user's and the numbered others.
func teamsPlaceholderAccount(m string) bool {
	return m == TeamsUserID || m == "28:"+TeamsAppID || teamsHasPrefix(m[3:], teamsOtherUserBase)
}

func teamsHasPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

// teamsPlaceholderGUID reports whether g is a GUID a recording leaves.
func teamsPlaceholderGUID(g string) bool {
	if g == TeamsAppID || g == TeamsTenantID {
		return true
	}
	return teamsHasPrefix(g, "00000000-0000-4000-8000-0000000")
}

// teamsFindings is the rules above over one file's text.
func teamsFindings(text string) [][2]string {
	var out [][2]string
	if m := teamsJWTAnyRe.FindString(text); m != "" {
		out = append(out, [2]string{"teams connector token", m})
	}
	if !teamsShapeRe.MatchString(text) {
		return out
	}
	for _, m := range teamsRealServiceRe.FindAllString(text, -1) {
		if m != TeamsServiceHost {
			out = append(out, [2]string{"teams service host", m})
			break
		}
	}
	if m := teamsNameKeyRe.FindString(text); m != "" {
		out = append(out, [2]string{"teams name", m})
	}
	for _, m := range teamsAuditAccountRe.FindAllString(text, -1) {
		if !teamsPlaceholderAccount(m) {
			out = append(out, [2]string{"teams account ID", m})
			break
		}
	}
	for _, m := range teamsAuditChatRe.FindAllString(text, -1) {
		if m != TeamsConversationID && !teamsHasPrefix(m, "a:"+teamsOtherUserBase) {
			out = append(out, [2]string{"teams conversation ID", m})
			break
		}
	}
	for _, m := range teamsGUIDAnyRe.FindAllString(text, -1) {
		if !teamsPlaceholderGUID(m) {
			out = append(out, [2]string{"teams GUID", m})
			break
		}
	}
	return out
}
