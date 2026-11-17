package xrrx

import "regexp"

// What a Slack recording must not carry, beyond what every cassette must
// not (see AuditWith). The workspace's own names are not listed here: that
// would publish them. Any ID that is not a placeholder is a finding, and a
// recording also denies the literals it used (SlackSandbox.Deny).
var (
	// slackTokenAnyRe is a Slack token of any kind (bot, user, app,
	// refresh, legacy). The stand-in the test profile holds is the one
	// allowed value.
	slackTokenAnyRe = regexp.MustCompile(`\bxox[a-z]-[A-Za-z0-9-]{6,}`)
	// slackSignatureRe is a request signature: the header's name or a
	// "v0=" and 64 hex. Fixtures are stored unsigned and signed when the
	// test runs.
	slackSignatureRe = regexp.MustCompile(`(?i)x-slack-signature|\bv0=[0-9a-f]{64}\b`)
	// slackHookRe is a response or webhook URL carrying a secret segment.
	slackHookRe = regexp.MustCompile(`hooks\.slack\.com/(?:services|actions|commands|workflows)/[A-Za-z0-9/_-]*[A-Za-z0-9_-]{20,}`)
)

// slackFindings is the rules above over one file's text.
func slackFindings(text string) [][2]string {
	var out [][2]string
	for _, m := range slackTokenAnyRe.FindAllString(text, -1) {
		if m != SlackStandInToken {
			out = append(out, [2]string{"slack token", m})
			break
		}
	}
	for _, loc := range slackIDInTextRe.FindAllStringIndex(text, -1) {
		m := text[loc[0]:loc[1]]
		// An escape of a character beyond the Basic Multilingual Plane in a
		// YAML string, such as an emoji's "\U0001F44D", is no ID.
		if loc[0] > 0 && text[loc[0]-1] == '\\' && len(m) == 9 && m[0] == 'U' {
			continue
		}
		if hasDigit(m) && !slackPlaceholderRe.MatchString(m) {
			out = append(out, [2]string{"slack ID", m})
			break
		}
	}
	for _, c := range []struct {
		rule string
		re   *regexp.Regexp
	}{
		{"slack signature", slackSignatureRe},
		{"slack webhook URL", slackHookRe},
	} {
		if m := c.re.FindString(text); m != "" {
			out = append(out, [2]string{c.rule, m})
		}
	}
	for _, m := range slackSubdomainRe.FindAllStringSubmatch(text, -1) {
		if sub := m[1]; sub != "hooks" && sub != "api" && sub != "www" {
			out = append(out, [2]string{"slack workspace host", m[0]})
			break
		}
	}
	return out
}
