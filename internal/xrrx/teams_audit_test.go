package xrrx

import (
	"strconv"
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// cleanTeams is a Bot Connector answer as a recording keeps it:
// placeholders only.
const cleanTeams = `xrr: "1"
adapter: http
fingerprint: eeeeeeee
payload:
    status: 200
    headers:
        Content-Type: application/json
    body: '{"activityId":null,"id":"a:heo-adapter-test-chat","members":[{"id":"29:heo-adapter-test-user"},{"id":"28:00000000-0000-4000-8000-0000000000a1"},{"id":"29:heo-adapter-test-other-1"}],"serviceUrl":"https://smba.heo-adapter-test.invalid/teams/","tenantId":"00000000-0000-4000-8000-0000000000a2"}'
`

// TestAuditFlagsTeamsLeaks: every shape of Teams identity a recording must
// not keep is found by name, in a cassette and in a fixture, and the
// placeholders pass. The other providers' files, which hold GUIDs and names
// of their own, are not read for these.
func TestAuditFlagsTeamsLeaks(t *testing.T) {
	d := t.TempDir()
	write(t, d, "http-eeeeeeee.resp.yaml", cleanTeams)
	write(t, d, "http-ffffffff.resp.yaml", `xrr: "1"
adapter: http
fingerprint: ffffffff
payload:
    status: 200
    body: '{"title":"A GitHub issue","user":{"id":123456789,"login":"sandbox-account","name":"someone","node":"4f9c2d3a-1111-4222-8333-444455556666"}}'
`)
	if fs, err := Audit(d); err != nil || len(fs) != 0 {
		t.Fatalf("clean Teams files, and another provider's: %v, %v", fs, err)
	}
	for rule, mutate := range map[string]func(string) string{
		"teams connector token": func(s string) string {
			return strings.Replace(s, `"id"`, `"t":"eyJhbGciOiJSUzI1NiJ9.eyJhdWQiOiJtYWRlLXVwIn0.c2lnbmF0dXJlLW1hZGUtdXA","id"`, 1)
		},
		"teams service host": func(s string) string {
			return strings.Replace(s, `smba.heo-adapter-test.invalid/teams/`, `smba.trafficmanager.net/emea/`, 1)
		},
		"teams service host 2": func(s string) string {
			return strings.Replace(s, `"id"`, `"u":"https://smba.infra.gcc.teams.microsoft.com/teams","id"`, 1)
		},
		"teams name": func(s string) string {
			return strings.Replace(s, `{"id":"29:heo-adapter-test-user"`, `{"name":"Alex","id":"29:heo-adapter-test-user"`, 1)
		},
		"teams name 2": func(s string) string { return strings.Replace(s, `"id"`, `"aadObjectId":"x","id"`, 1) },
		"teams account ID": func(s string) string {
			return strings.Replace(s, `29:heo-adapter-test-user`, `29:1AbCdEfGhIjKlMnOpQrSt`, 1)
		},
		"teams account ID 2": func(s string) string {
			return strings.Replace(s, `28:00000000-0000-4000-8000-0000000000a1`, `28:abcdefgh-ijkl`, 1)
		},
		"teams conversation ID": func(s string) string {
			return strings.Replace(s, `a:heo-adapter-test-chat`, `a:1ZyXwVuTsRqPoNmLkJiHg`, 1)
		},
		"teams GUID": func(s string) string {
			return strings.Replace(s, `00000000-0000-4000-8000-0000000000a2`, `8b2c3d4e-2222-4333-8444-555566667777`, 1)
		},
	} {
		for _, name := range []string{"http-eeeeeeee.resp.yaml", "fixture.json"} {
			body := cleanTeams
			if name != "http-eeeeeeee.resp.yaml" {
				body = strings.ReplaceAll(cleanTeams, "\n", " ")
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
	write(t, d, "leak.json", `{"x":"the real chat"}`)
	if fs, _ := AuditWith(d, "the real chat"); len(fs) != 1 || fs[0].Rule != "recorded identity" {
		t.Errorf("a denied literal: %v", fs)
	}
}

// TestFromEnvReadsATeamsSandbox: a recording process told the Teams
// sandbox gets it, and only in record mode with the live flag.
func TestFromEnvReadsATeamsSandbox(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(xrr.EnvMode, "record")
	t.Setenv(xrr.EnvCassetteDir, dir)
	t.Setenv("CI", "")
	for k, v := range map[string]string{
		EnvLive: "1", EnvTeamsConnectorToken: tmToken, EnvTeamsAppID: tmApp, EnvTeamsTenantID: tmTenant, EnvTeamsServiceURL: tmService,
		EnvTeamsUserID: tmUser, EnvTeamsConversationID: tmChat, EnvTeamsLedger: t.TempDir(), EnvTeamsMaxSends: "2", EnvTeamsRehearse: "1",
	} {
		t.Setenv(k, v)
	}
	e, err := FromEnv()
	if err != nil || e.Teams == nil || e.Teams.Max != 2 || !e.Teams.Rehearse || e.Teams.ConversationID != tmChat || e.Teams.AppID != tmApp ||
		e.Teams.ServiceURL != tmService || e.Slack != nil || e.Twilio != nil || e.GitHub != nil || e.Telegram != nil {
		t.Fatalf("record, live: %+v, %v", e, err)
	}
	t.Setenv(EnvTeamsConversationID, "19:abc123456789@thread.tacv2")
	if _, err := FromEnv(); err == nil {
		t.Error("a channel's ID was accepted as the personal chat")
	}
	t.Setenv(EnvTeamsConversationID, tmChat)
	t.Setenv(xrr.EnvMode, "replay")
	if e, err := FromEnv(); err != nil || e.Teams != nil {
		t.Errorf("replay: %+v, %v; want no sandbox", e, err)
	}
}

// TestTeamsChildEnvRoundTrips: what the recording's parent tells its child
// is what the child reads back, so the two translate the same names.
func TestTeamsChildEnvRoundTrips(t *testing.T) {
	s := newTeams(t, 2)
	s.Rehearse = true
	env := map[string]string{xrr.EnvMode: "record", "CI": ""}
	for _, kv := range s.ChildEnv() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	got, err := TeamsSandboxFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != s.Token || got.AppID != s.AppID || got.TenantID != s.TenantID || got.ServiceURL != s.ServiceURL || got.UserID != s.UserID ||
		got.ConversationID != s.ConversationID || got.Max != 2 || got.Ledger != s.Ledger || !got.Rehearse {
		t.Errorf("child reads %+v, parent holds %+v", got, s)
	}
	if strconv.Itoa(got.Max) != "2" {
		t.Error("cap lost")
	}
}
