package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hop.top/heo/internal/composition"
	"hop.top/uma/model"
)

// entraStandIn is a Microsoft Entra token endpoint of the test's own, on
// loopback: it issues a Bot Connector token for one tenant, app and client
// secret, and answers any other secret as Entra does (401 invalid_client,
// AADSTS7000215), repeating the secret in its description as a careless
// server might.
type entraStandIn struct {
	srv *httptest.Server

	mu       sync.Mutex
	secret   string
	requests []map[string]string
}

func newEntraStandIn(t *testing.T, secret string) *entraStandIn {
	t.Helper()
	e := &entraStandIn{secret: secret}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		e.mu.Lock()
		e.requests = append(e.requests, map[string]string{
			"path": r.URL.Path, "client_id": r.PostForm.Get("client_id"), "client_secret": r.PostForm.Get("client_secret"),
			"scope": r.PostForm.Get("scope"), "grant_type": r.PostForm.Get("grant_type"),
		})
		want := e.secret
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		got := r.PostForm.Get("client_secret")
		if r.URL.Path != "/"+teamsTestTenant+"/oauth2/v2.0/token" || r.PostForm.Get("client_id") != teamsTestApp ||
			r.PostForm.Get("scope") != "https://api.botframework.com/.default" || got != want {
			w.WriteHeader(http.StatusUnauthorized)
			// #nosec G705 -- a JSON error for a client the test owns
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_codes": []int{7000215},
				"error_description": "AADSTS7000215: Invalid client secret provided. (sent " + got + ")",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "heo-e2e-minted-connector-token"}) //nolint:gosec // G101: a made-up token
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *entraStandIn) setSecret(s string) {
	e.mu.Lock()
	e.secret = s
	e.mu.Unlock()
}

func (e *entraStandIn) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func (e *entraStandIn) last() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) == 0 {
		return nil
	}
	return e.requests[len(e.requests)-1]
}

// TestTeamsChannelCheckRemoteMintsTheConnectorToken: on the built binary,
// `channel check --remote` of a microsoft.teams channel mints the bot's
// Bot Connector token from the stored client secret at the host's token
// endpoint (adapters.teams.authority, here a stand-in on loopback). With no
// secret stored it reports that and mints nothing. A secret the endpoint
// refuses is an error finding, exit 2, in either flag position, as a table
// and as JSON, naming the provider's own code (invalid_client, AADSTS7000215)
// and never the secret; once the right one is stored the same daemon, not
// restarted, passes.
func TestTeamsChannelCheckRemoteMintsTheConnectorToken(t *testing.T) {
	withE2EBinary(t, func() {
		const purpose = composition.PurposeTeamsClientSecret
		const right, wrong = "the-right-client-secret~Ab1", "a-wrong-client-secret~Cd2"
		entra := newEntraStandIn(t, right)
		c := newCLISite(t, "adapters:\n  teams:\n    authority: "+entra.srv.URL+"\n")
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "teams", "--credential-ref", "teams",
			"--configuration", teamsTestConfig)
		store := func(secret string) {
			b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": secret})
			if err != nil {
				t.Fatal(err)
			}
			c.s.write("secrets/uma/default/teams@"+purpose, string(b))
		}
		type report struct {
			OK     bool
			Checks []struct {
				Findings []struct{ Severity, Scope, Code, Message string }
			}
		}
		finding := func(res report, scope string) (code, msg string, ok bool) {
			for _, f := range res.Checks[0].Findings {
				if f.Scope == scope && f.Severity == "error" {
					return f.Code, f.Message, true
				}
			}
			return "", "", false
		}
		check := func(exit int, args ...string) (report, result) {
			t.Helper()
			var res report
			r := c.heo(exit, append([]string{"channel", "check"}, append(args, "--format", "json")...)...)
			if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || len(res.Checks) != 1 {
				t.Fatalf("%v\n%s", err, r.stdout)
			}
			return res, r
		}
		scope := "credential:" + purpose

		// No secret stored: the diagnosis says so; nothing is minted.
		res, _ := check(2, "teams", "--remote")
		if code, _, ok := finding(res, scope); !ok || code != "missing" || res.OK {
			t.Errorf("no secret: %+v", res.Checks[0].Findings)
		}
		if n := entra.count(); n != 0 {
			t.Errorf("a check with no secret stored reached the token endpoint %d times", n)
		}

		// A secret the endpoint refuses, either flag position.
		store(wrong)
		for _, args := range [][]string{{"teams", "--remote"}, {"--remote", "teams"}} {
			res, r := check(2, args...)
			code, msg, ok := finding(res, scope)
			if !ok || res.OK || code != "invalid_client" || !strings.Contains(msg, "AADSTS7000215") || !strings.Contains(msg, purpose) {
				t.Errorf("%v: finding = %q %q (found %v), ok=%v", args, code, msg, ok, res.OK)
			}
			if strings.Contains(r.stdout+r.stderr, wrong) {
				t.Errorf("%v: the secret is in the output", args)
			}
		}
		table := c.heo(2, "channel", "check", "--remote", "teams").stdout
		if !strings.Contains(table, "invalid_client") || !strings.Contains(table, "AADSTS7000215") || strings.Contains(table, wrong) {
			t.Errorf("table:\n%s", table)
		}
		if got := entra.last(); got["client_secret"] != wrong || got["client_id"] != teamsTestApp ||
			got["path"] != "/"+teamsTestTenant+"/oauth2/v2.0/token" || got["grant_type"] != "client_credentials" {
			t.Errorf("last mint = %v", got)
		}

		// The operator stores the right one: no restart, no wait.
		store(right)
		res, _ = check(0, "teams", "--remote")
		if !res.OK {
			t.Errorf("after storing the right secret: %+v", res.Checks[0].Findings)
		}
		if got := entra.last(); got["client_secret"] != right {
			t.Errorf("the mint after the rotation = %v, want the new secret", got)
		}
		// The endpoint rotates its secret under the daemon: the next check
		// tells.
		entra.setSecret("rotated-at-the-endpoint~Ef3")
		res, _ = check(2, "teams", "--remote")
		if code, _, ok := finding(res, scope); !ok || code != "invalid_client" {
			t.Errorf("after the endpoint's rotation: %+v", res.Checks[0].Findings)
		}
		log := d.log()
		for _, secret := range []string{right, wrong, "heo-e2e-minted-connector-token"} {
			if strings.Contains(log, secret) {
				t.Errorf("the daemon log carries %q", secret)
			}
		}
		c.stop(d)
	})
}
