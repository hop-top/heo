package xrrx

import (
	"strings"
	"testing"
)

// cleanTwilio is a Twilio answer as a recording keeps it: placeholders
// only.
const cleanTwilio = `xrr: "1"
adapter: http
fingerprint: cccccccc
payload:
    status: 201
    headers:
        Content-Type: application/json
    body: '{"account_sid":"AC00000000000000000000000000000001","from":"+15550100001","sid":"SM00000000000000000000000000000001","to":"+15550100002","uri":"/2010-04-01/Accounts/AC00000000000000000000000000000001/Messages/SM00000000000000000000000000000001.json"}'
`

// TestAuditFlagsTwilioLeaks: every shape of Twilio identity a recording
// must not keep is found by name, in a cassette and in a fixture, and the
// placeholders and fictional numbers pass.
func TestAuditFlagsTwilioLeaks(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-cccccccc.resp.yaml", cleanTwilio)
	write(t, d, "inbound.form", "AccountSid=AC00000000000000000000000000000001&From=%2B15550100002&To=%2B15550100001&MessageSid=SM00000000000000000000000000000002&Body=hi")
	write(t, d, "numbers.json", `{"a":"+15550100001","b":"%2B15550100002","c":"(555) 010-0199","d":"555-0142","e":"+1555"}`)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("clean Twilio files: %v, %v", fs, err)
	}
	realSID := "0123456789abcdef0123456789abcdef"
	for rule, mutate := range map[string]func(string) string{
		"twilio SID": func(s string) string {
			return strings.Replace(s, "AC00000000000000000000000000000001", "AC"+realSID, 1)
		},
		"twilio SID (message)": func(s string) string {
			return strings.Replace(s, "SM00000000000000000000000000000001", "SM"+realSID, 1)
		},
		"32-hex token": func(s string) string { return strings.Replace(s, `"to"`, `"token":"`+realSID+`","to"`, 1) },
		"phone number": func(s string) string { return strings.Replace(s, "+15550100002", "+14155550142", 1) },
		"twilio account name": func(s string) string {
			return strings.Replace(s, `"to"`, `"friendly_name":"My First Twilio Account","to"`, 1)
		},
		"basic auth blob": func(s string) string {
			return strings.Replace(s, `"to"`, `"x":"QUMwMTIzNDU2Nzg5YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZjpzZWNyZXQ=","to"`, 1)
		},
		"twilio signature": func(s string) string { return strings.Replace(s, `"to"`, `"x":"X-Twilio-Signature","to"`, 1) },
	} {
		for _, name := range []string{"http-cccccccc.resp.yaml", "fixture.form", "fixture.json"} {
			body := cleanTwilio
			if name != "http-cccccccc.resp.yaml" {
				body = strings.ReplaceAll(cleanTwilio, "\n", " ")
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
				t.Errorf("%s not found in %s: %v", rule, name, fs)
			}
		}
	}
	for what, body := range map[string]string{
		"national, parentheses": `{"x":"(514) 555-0223"}`,
		"national, dashes":      `{"x":"514-555-0223"}`,
		"national, dots":        `{"x":"514.555.0223"}`,
		"e164 in a form":        "To=%2B14155550142",
		"e164, other country":   `{"x":"+33612345678"}`,
		"signature as a field":  "Signature=" + strings.Repeat("A", 27) + "=",
	} {
		dd := t.TempDir()
		write(t, dd, "x.json", body)
		if fs, _ := Audit(dd); len(fs) == 0 {
			t.Errorf("%s was not found: %s", what, body)
		}
	}
	// Ten bare digits are an ID as often as a number: the audit leaves
	// them to the recording's own deny list.
	dd := t.TempDir()
	write(t, dd, "x.json", `{"id":6133211007}`)
	if fs, _ := Audit(dd); len(fs) != 0 {
		t.Errorf("a ten-digit ID was flagged: %v", fs)
	}
}
