package xrrx

import "regexp"

// What a Twilio recording must not carry, beyond what every cassette must
// not (see AuditWith). The owner's own number is not named here: that
// would publish it. Any phone number that is not a placeholder is a
// finding, as is the national form of one, and a recording also denies the
// literals it used (TwilioSandbox.Deny).
var (
	// twilioAuditSIDRe is a Twilio SID of any resource, whatever the case
	// of its hex.
	twilioAuditSIDRe = regexp.MustCompile(`\b[A-Z]{2}[0-9a-fA-F]{32}\b`)
	// hex32Re is a bare 32-hex value: an auth token or API secret.
	hex32Re = regexp.MustCompile(`\b[0-9a-fA-F]{32}\b`)
	// e164AuditRe is an E.164 number, with its plus as written or
	// percent-encoded in a form.
	e164AuditRe = regexp.MustCompile(`(?:\+|%2[bB])[1-9][0-9]{6,14}\b`)
	// nationalRe is a North American number written the way a person
	// writes it, with parentheses or separators; ten bare digits are not
	// matched, since record and comment IDs have that shape (a recording
	// denies the digits it used instead). 555-01xx is the fictional range.
	nationalRe = regexp.MustCompile(`\(\d{3}\)\s?[2-9]\d{2}[ .-]?\d{4}\b|\b[2-9]\d{2}[ .-][2-9]\d{2}[ .-]?\d{4}\b`)
	// twilioNameRe is the account's own names.
	twilioNameRe = regexp.MustCompile(`(?i)idea\s*crafters|my first twilio account`)
	// basicBlobRe is a base64 Basic credential for an account SID: the
	// encoding of "AC" starts "QUM".
	basicBlobRe = regexp.MustCompile(`\bQUM[A-Za-z0-9+/]{30,}={0,2}`)
	// twilioSignatureRe is a webhook signature: the header's name, or a
	// base64 HMAC-SHA1 (28 characters ending in an equals sign) in a form
	// fixture. A fixture is stored unsigned and signed when the test runs.
	twilioSignatureRe = regexp.MustCompile(`(?i)x-twilio-signature|\b[A-Za-z0-9+/]{27}=`)
)

// twilioFindings is the rules above over one file's text.
func twilioFindings(text string) [][2]string {
	var out [][2]string
	for _, m := range twilioAuditSIDRe.FindAllString(text, -1) {
		if !placeholderSIDRe.MatchString(m) && m != TwilioAccountSID {
			out = append(out, [2]string{"twilio SID", m})
			break
		}
	}
	for _, m := range hex32Re.FindAllString(text, -1) {
		if m != "00000000000000000000000000000000" {
			out = append(out, [2]string{"32-hex token", m})
			break
		}
	}
	for _, m := range e164AuditRe.FindAllString(text, -1) {
		n := m
		if len(n) > 0 && n[0] == '%' {
			n = "+" + n[3:]
		}
		if !IsTwilioPlaceholder(n) {
			out = append(out, [2]string{"phone number", m})
			break
		}
	}
	for _, m := range nationalRe.FindAllString(text, -1) {
		if !fictionalNational(m) {
			out = append(out, [2]string{"phone number", m})
			break
		}
	}
	for _, c := range []struct {
		rule string
		re   *regexp.Regexp
	}{
		{"twilio account name", twilioNameRe},
		{"basic auth blob", basicBlobRe},
		{"twilio signature", twilioSignatureRe},
	} {
		if m := c.re.FindString(text); m != "" {
			out = append(out, [2]string{c.rule, m})
		}
	}
	return out
}

// fictionalNational reports whether n is in the 555-0100..0199 range,
// which no subscriber has.
func fictionalNational(n string) bool {
	d := ""
	for _, r := range n {
		if r >= '0' && r <= '9' {
			d += string(r)
		}
	}
	return len(d) == 10 && d[3:6] == "555" && d[6:8] == "01"
}
