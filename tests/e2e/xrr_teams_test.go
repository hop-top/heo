package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/xrrx"
	"hop.top/uma/adapter/chat/teams"
	"hop.top/uma/model"
)

// Cassettes of microsoft.teams through the real heo binary. In replay (the
// default) the daemon answers from committed cassettes, so nothing reaches
// Microsoft and no credential is needed. None are recorded yet: with no
// cassette the test skips, and `make record-teams` makes them. Recording
// is the one live step, and it is narrow: at most
// HEO_RECORD_TEAMS_MAX_SENDS messages (three by default) to the bot's
// personal chat with one user of a tenant the maintainers own, behind
// HEO_RECORD_LIVE=1 and XRR_MODE=record, never in CI; see
// docs/DEVELOPING.md. The daemon is told a placeholder bot, tenant, service
// and user and a stand-in client secret (or, with HEO_RECORD_TEAMS_CREDENTIAL=
// certificate, a stand-in certificate and key) only, and mints a stand-in token
// with it from the sandbox; internal/xrrx translates between
// those and the real ones, counts every send against a cap, and refuses
// everything else. Inbound is not recorded: Teams signs every activity it
// posts with a token no replay can produce, so the route is checked for
// what it refuses, and the activities are covered in process
// (TestTeamsWebhook).

// openTeams returns the live sandbox when this test is recording, nil in
// replay, and skips a replay with nothing recorded and a recording that was
// not asked to go live.
func openTeams(t *testing.T) *xrrx.TeamsSandbox {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("microsoft.teams is replayed or recorded, never passed through to Microsoft")
	case xrr.ModeReplay:
		if teamsCassettes(t) == 0 {
			t.Skip("no microsoft.teams cassettes are recorded yet: `make record-teams` with a tenant you own (docs/DEVELOPING.md)")
		}
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording microsoft.teams is live: set %s=1 and the bot (docs/DEVELOPING.md)", xrrx.EnvLive)
	}
	if err := xrrx.CheckLiveTeams(os.Getenv); err != nil {
		t.Fatal(err)
	}
	s, err := xrrx.TeamsSandboxFromEnv(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("recording microsoft.teams: at most %d sends (%d already made), rehearsal=%v", s.Max, s.Sent(), s.Rehearse)
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", s.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
	return s
}

// teamsCassettes counts the requests the test's daemon cassettes hold.
func teamsCassettes(t *testing.T) int {
	t.Helper()
	n := 0
	ents, _ := os.ReadDir(filepath.Join("testdata", "cassettes", t.Name(), "daemon"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".req.yaml") {
			n++
		}
	}
	return n
}

// teamsSite is a daemon with a microsoft.teams channel on the placeholder
// bot, recording or replaying its calls to the Bot Connector.
type teamsSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *xrrx.TeamsSandbox
	channel model.Channel
}

func newTeamsSite(t *testing.T, live *xrrx.TeamsSandbox) *teamsSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	// One entry, for the one purpose the operator supplies: the stand-in for
	// the bot's client secret, a secret credential, or, in a recording that
	// asks for it, the stand-in certificate and its key, a client_cert
	// credential. heo mints the Bot Connector token from it; the sandbox
	// answers that request itself.
	var env []string
	var keyPEM []byte
	if os.Getenv(xrrx.EnvTeamsCredential) == "certificate" {
		if live == nil {
			t.Skipf("%s=certificate is for a live recording or its rehearsal: replay is of the cassettes the stand-in secret recorded", xrrx.EnvTeamsCredential)
		}
		var certPEM []byte
		var err error
		if certPEM, keyPEM, err = xrrx.NewTeamsStandInCertificate(); err != nil {
			t.Fatal(err)
		}
		if err := live.UseStandInCertificate(certPEM); err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "client_cert", "certificate_pem": string(certPEM), "private_key_pem": string(keyPEM)})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/teams@"+composition.PurposeTeamsClientCertificate, string(b))
	} else {
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": xrrx.TeamsStandInSecret})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/teams@"+composition.PurposeTeamsClientSecret, string(b))
	}
	if live != nil {
		env = live.ChildEnv()
	}
	s := &teamsSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "-c", "services.receivers.enabled=false", "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
		lits := []string{xrrx.TeamsStandInToken, xrrx.TeamsStandInSecret}
		if len(keyPEM) > 0 {
			lits = append(lits, strings.Split(strings.TrimSpace(string(keyPEM)), "\n")[1])
		}
		for _, lit := range lits {
			if strings.Contains(s.d.log(), lit) {
				t.Error("the daemon log carries the stand-in credential")
			}
		}
		if live != nil {
			for _, lit := range live.Deny() {
				if lit != "" && strings.Contains(s.d.log(), lit) {
					t.Error("the daemon log carries something of the real bot")
					break
				}
			}
		}
	})
	cfg, err := json.Marshal(teams.Config{AppID: xrrx.TeamsAppID, TenantID: xrrx.TeamsTenantID, ServiceURL: xrrx.TeamsServiceURL})
	if err != nil {
		t.Fatal(err)
	}
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "teams", "--configuration", string(cfg), "--credential-ref", "teams")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "teams", "--key", "people", "--destination", xrrx.TeamsTenantID, "--mode", model.ModePerContact,
		"--lock-to-single", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// post sends body to the channel's webhook as the Bot Connector would, with
// authorization as the Authorization header (none when empty), retried
// while the daemon has not mounted the channel's route yet. It returns the
// status of the last answer.
func (s *teamsSite) post(body, authorization string) int {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, strings.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback webhook of the daemon the test started
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				return resp.StatusCode
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("webhook: the channel's route never answered (%v)", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// mustApply waits for the operation to be accepted by the Bot Connector.
func (s *teamsSite) mustApply(t *testing.T, op model.ID, what string) {
	t.Helper()
	if d := s.waitDelivery(op, model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected); d.State == model.Failed || d.State == model.Rejected {
		t.Fatalf("%s = %s %+v", what, d.State, d.Failure)
	}
}

// TestXRRTeamsProviderVerbs: microsoft.teams through the real heo binary.
// A first message to a user starts their conversation, through the personal
// chat the Bot Connector creates; a reply joins it; an edit, a reaction set
// and cleared and two deletions are each a request the daemon's workers
// make to the Bot Connector and the cassettes hold. The messaging endpoint
// refuses what carries no valid Bot Connector token on the real listener.
// In replay nothing reaches Microsoft. With no cassette recorded the test
// skips.
func TestXRRTeamsProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openTeams(t)
		s := newTeamsSite(t, live)

		// The verb table, offline: microsoft.teams declares edit, delete and
		// react, and not respond, history or the membership, receipt or work
		// item verbs.
		table := s.heo(0, "channel", "get", "teams").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)",
			"verb reaction add         declared (react)",
			"verb interaction respond  unsupported: not_declared (respond)",
			"verb history fetch        unsupported: not_declared (history)",
			"verb participant add      unsupported: not_declared (membership)", "verb receipt send         unsupported: not_declared (receipt)",
			"verb conversation status unsupported: not_declared (work_item_act)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// The messaging endpoint, on the real listener: no token, or one that
		// is none, is refused before anything is read from Microsoft.
		activity := `{"type":"message","id":"1","channelId":"msteams","serviceUrl":"` + xrrx.TeamsServiceURL + `"}`
		for name, auth := range map[string]string{"no token": "", "garbage": "Bearer not.a.token", "not a bearer": "Basic eA=="} { // #nosec G101 -- made-up values
			if code := s.post(activity, auth); code != http.StatusUnauthorized {
				t.Errorf("%s = %d, want 401", name, code)
			}
		}

		// A preview sends nothing.
		held := teamsCassettes(t)
		send := []string{"message", "send", "--inbox", "people", "--to", "user:" + xrrx.TeamsUserID}
		s.heo(0, append(append(cloneArgs(send), "--text", xrrx.TeamsTextPrefix+" 1"), "--dry-run")...)
		if teamsCassettes(t) != held {
			t.Fatalf("a preview made a request")
		}

		// Send 1: opens the personal chat and starts the user's conversation.
		var first sendJSON
		s.json(&first, append(cloneArgs(send), "--text", xrrx.TeamsTextPrefix+" 1", "--idempotency-key", "teams-send-1")...)
		s.mustApply(t, first.Operation.ID, "send 1")
		conv := string(first.Conversation)

		// A reply, answering the first: a second send, to the same chat.
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", xrrx.TeamsTextPrefix+" 2", "--idempotency-key", "teams-reply")
		s.mustApply(t, reply.Operation.ID, "the reply")
		if got := s.conversationsOf("people"); len(got) != 1 {
			t.Fatalf("conversations = %+v, want the one user", got)
		}

		// Edit and react to the first message; clear the reaction.
		var mine model.Message
		s.json(&mine, "message", "get", string(first.Message))
		var edit providerOpJSON
		s.json(&edit, "message", "edit", string(mine.Ref.ID), "--revision", string(mine.Revision), "--text", xrrx.TeamsTextPrefix+" 1 edited", "--idempotency-key", "teams-edit")
		s.mustApply(t, edit.Operation.ID, "the edit")
		var react reactJSON
		s.json(&react, "reaction", "add", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "teams-react")
		s.mustApply(t, react.Operation.ID, "the reaction")
		s.json(&react, "reaction", "remove", "--message", string(mine.Ref.ID), "👍", "--idempotency-key", "teams-unreact")
		s.mustApply(t, react.Operation.ID, "the removal of the reaction")

		// Delete both of the recording's messages.
		for i, id := range []model.ID{first.Message, reply.Message} {
			var m model.Message
			s.json(&m, "message", "get", string(id))
			var del providerOpJSON
			s.json(&del, "message", "delete", string(id), "--revision", string(m.Revision), "--confirm=yes", "--idempotency-key", fmt.Sprintf("teams-delete-%d", i+1))
			s.mustApply(t, del.Operation.ID, "the deletion")
		}
		if live != nil {
			t.Logf("sends made so far, across runs: %d of at most %d", live.Sent(), live.Max)
		}
	})
}
