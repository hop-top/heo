package e2e

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/uma/model"
)

// entraCertStandIn is a Microsoft Entra token endpoint of the test's own,
// on loopback, for an app that proves itself with a certificate: it
// issues a Bot Connector token for an assertion signed (RS256, found by its
// x5t#S256 header) for one of the certificates registered, naming this
// tenant's endpoint as audience and the app as issuer and subject, and
// answers anything else as Entra does (401 invalid_client, AADSTS700027),
// repeating the assertion in its description as a careless server might.
type entraCertStandIn struct {
	srv *httptest.Server

	mu       sync.Mutex
	certs    []*x509.Certificate
	requests []map[string]string
}

func newEntraCertStandIn(t *testing.T) *entraCertStandIn {
	t.Helper()
	e := &entraCertStandIn{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		assertion := r.PostForm.Get("client_assertion")
		e.mu.Lock()
		e.requests = append(e.requests, map[string]string{
			"path": r.URL.Path, "client_id": r.PostForm.Get("client_id"), "client_secret": r.PostForm.Get("client_secret"),
			"assertion": assertion, "assertion_type": r.PostForm.Get("client_assertion_type"),
		})
		ok := e.verify(assertion, "http://"+r.Host+r.URL.Path)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || r.PostForm.Get("client_secret") != "" || r.PostForm.Get("client_id") != teamsTestApp ||
			r.PostForm.Get("scope") != "https://api.botframework.com/.default" || r.PostForm.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusUnauthorized)
			// #nosec G705 -- a JSON error for a client the test owns
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_codes": []int{700027},
				"error_description": "AADSTS700027: Client assertion contains an invalid signature. (sent " + assertion + ")",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "heo-e2e-minted-connector-token"}) //nolint:gosec // G101: a made-up token
	}))
	t.Cleanup(e.srv.Close)
	return e
}

// verify is the endpoint's judgment of an assertion; the caller holds e.mu.
func (e *entraCertStandIn) verify(assertion, endpoint string) bool {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return false
	}
	var seg [3][]byte
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			return false
		}
		seg[i] = b
	}
	var h, c map[string]any
	if json.Unmarshal(seg[0], &h) != nil || json.Unmarshal(seg[1], &c) != nil {
		return false
	}
	exp, _ := c["exp"].(float64)
	nbf, _ := c["nbf"].(float64)
	now := float64(time.Now().Unix())
	if h["alg"] != "RS256" || c["iss"] != teamsTestApp || c["sub"] != teamsTestApp || c["aud"] != endpoint || c["jti"] == "" ||
		nbf > now+60 || exp <= now || exp-nbf > 900 {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, cert := range e.certs {
		thumb := sha256.Sum256(cert.Raw)
		if h["x5t#S256"] == base64.RawURLEncoding.EncodeToString(thumb[:]) {
			return rsa.VerifyPKCS1v15(cert.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], seg[2]) == nil
		}
	}
	return false
}

func (e *entraCertStandIn) register(certs ...*x509.Certificate) {
	e.mu.Lock()
	e.certs = certs
	e.mu.Unlock()
}

func (e *entraCertStandIn) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.requests)
}

func (e *entraCertStandIn) last() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.requests) == 0 {
		return nil
	}
	return e.requests[len(e.requests)-1]
}

// teamsTestCert is a certificate and key made for the test, valid until
// notAfter.
type teamsTestCert struct {
	leaf              *x509.Certificate
	envelope, keyBody string
}

func newTeamsTestCert(t *testing.T, notAfter time.Time) teamsTestCert {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "heo e2e bot"}, NotBefore: notAfter.Add(-2 * 365 * 24 * time.Hour),
		NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	b, err := json.Marshal(map[string]any{
		"v": 1, "kind": "client_cert", "certificate_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "private_key_pem": keyPEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	return teamsTestCert{leaf: leaf, envelope: string(b), keyBody: strings.Split(keyPEM, "\n")[1]}
}

// TestTeamsChannelCheckRemoteCertificate: on the built binary, `channel
// check --remote` of a microsoft.teams channel whose app proves itself with
// a certificate mints the bot's Bot Connector token with a signed client
// assertion at the host's token endpoint, no secret in the request. One
// credential is stored, not none and not both. A certificate the endpoint
// does not know, or past its not_after, is an error finding, exit 2, in
// either flag position, as a table and as JSON, naming the provider's
// code and never the key; one that works but ends within thirty days is a
// warning, exit 0; a certificate stored over the old one is used by the
// same daemon, not restarted.
func TestTeamsChannelCheckRemoteCertificate(t *testing.T) {
	withE2EBinary(t, func() {
		const secretPurpose, certPurpose = composition.PurposeTeamsClientSecret, composition.PurposeTeamsClientCertificate
		entra := newEntraCertStandIn(t)
		c := newCLISite(t, "adapters:\n  teams:\n    authority: "+entra.srv.URL+"\n")
		d := c.start()
		c.heo(0, "channel", "create", "--type", string(model.TypeMicrosoftTeams), "--key", "teams", "--credential-ref", "teams",
			"--configuration", teamsTestConfig)
		type finding struct{ Severity, Scope, Code, Message string }
		type report struct {
			OK     bool
			Checks []struct {
				Findings []finding
				Remote   struct {
					Credentials []struct{ Purpose, Status string }
				}
			}
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
		find := func(res report, scope string) (finding, bool) {
			for _, f := range res.Checks[0].Findings {
				if f.Scope == scope {
					return f, true
				}
			}
			return finding{}, false
		}
		storeCert := func(tc teamsTestCert) { c.s.write("secrets/uma/default/teams@"+certPurpose, tc.envelope) }
		far := time.Now().Add(2 * 365 * 24 * time.Hour)

		// Nothing stored: one finding naming both purposes; nothing is minted.
		res, _ := check(2, "teams", "--remote")
		if f, ok := find(res, "credential:"+secretPurpose); !ok || f.Code != "missing" || f.Severity != "error" ||
			!strings.Contains(f.Message, secretPurpose) || !strings.Contains(f.Message, certPurpose) {
			t.Errorf("nothing stored: %+v", res.Checks[0].Findings)
		}
		if entra.count() != 0 {
			t.Errorf("a check with nothing stored reached the token endpoint %d times", entra.count())
		}

		// A certificate the endpoint knows, either flag position.
		first := newTeamsTestCert(t, far)
		storeCert(first)
		entra.register(first.leaf)
		for _, args := range [][]string{{"teams", "--remote"}, {"--remote", "teams"}} {
			res, r := check(0, args...)
			if f, found := find(res, "credential:"+certPurpose); !res.OK || found {
				t.Errorf("%v: %+v, ok=%v, want no finding about the credential", args, f, res.OK)
			}
			if got := res.Checks[0].Remote.Credentials; len(got) != 1 || got[0].Purpose != certPurpose || got[0].Status != "ok" {
				t.Errorf("%v: credentials = %+v, want the stored certificate alone", args, got)
			}
			if strings.Contains(r.stdout+r.stderr, first.keyBody) {
				t.Errorf("%v: the key is in the output", args)
			}
		}
		if got := entra.last(); got["client_id"] != teamsTestApp || got["client_secret"] != "" || got["path"] != "/"+teamsTestTenant+"/oauth2/v2.0/token" ||
			got["assertion"] == "" || got["assertion_type"] != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
			t.Errorf("last mint = %v, want the app's assertion at its tenant's endpoint and no secret", got)
		}

		// A certificate the endpoint does not know (the app's registration moved on).
		entra.register()
		for _, args := range [][]string{{"teams", "--remote"}, {"--remote", "teams"}} {
			res, r := check(2, args...)
			f, ok := find(res, "credential:"+certPurpose)
			if !ok || res.OK || f.Severity != "error" || f.Code != "invalid_client" || !strings.Contains(f.Message, "AADSTS700027") ||
				!strings.Contains(f.Message, "certificate") || !strings.Contains(f.Message, certPurpose) {
				t.Errorf("%v: finding = %+v (found %v), ok=%v", args, f, ok, res.OK)
			}
			if out := r.stdout + r.stderr; strings.Contains(out, first.keyBody) || strings.Contains(out, entra.last()["assertion"]) {
				t.Errorf("%v: the key or the assertion is in the output", args)
			}
		}
		table := c.heo(2, "channel", "check", "--remote", "teams").stdout
		if !strings.Contains(table, "invalid_client") || !strings.Contains(table, "AADSTS700027") || strings.Contains(table, first.keyBody) {
			t.Errorf("table:\n%s", table)
		}

		// The operator rotates: a new certificate, registered and stored; the
		// same daemon uses it.
		second := newTeamsTestCert(t, far)
		storeCert(second)
		entra.register(second.leaf)
		if res, _ := check(0, "teams", "--remote"); !res.OK {
			t.Errorf("after the rotation: %+v", res.Checks[0].Findings)
		}

		// One that ends soon: still works, and says so.
		soon := newTeamsTestCert(t, time.Now().Add(10*24*time.Hour))
		storeCert(soon)
		entra.register(soon.leaf)
		res, _ = check(0, "teams", "--remote")
		f, ok := find(res, "credential:"+certPurpose)
		if !ok || f.Severity != "warning" || f.Code != "expires_soon" || !res.OK || !strings.Contains(f.Message, "expires on") ||
			!strings.Contains(f.Message, soon.leaf.NotAfter.UTC().Format(time.DateOnly)) {
			t.Errorf("a certificate ending in 10 days: %+v (found %v), ok=%v", f, ok, res.OK)
		}
		if tbl := c.heo(0, "channel", "check", "--remote", "teams").stdout; !strings.Contains(tbl, "expires_soon") {
			t.Errorf("table lacks the warning:\n%s", tbl)
		}

		// One past its not_after: an error from the credential's own diagnosis.
		old := newTeamsTestCert(t, time.Now().Add(-time.Hour))
		storeCert(old)
		entra.register(old.leaf)
		before := entra.count()
		res, _ = check(2, "teams", "--remote")
		if f, ok := find(res, "credential:"+certPurpose); !ok || f.Severity != "error" || f.Code != "expired" || res.OK {
			t.Errorf("an expired certificate: %+v", res.Checks[0].Findings)
		}
		if entra.count() != before {
			t.Error("an expired certificate was sent to the token endpoint")
		}

		// A secret beside the certificate: exactly one is the operator's to store.
		storeCert(second)
		entra.register(second.leaf)
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": "a-client-secret-beside-the-cert~Gh4"})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/teams@"+secretPurpose, string(b))
		before = entra.count()
		res, r := check(2, "teams", "--remote")
		if f, ok := find(res, "credential:"+secretPurpose); !ok || f.Code != "conflict" || f.Severity != "error" ||
			!strings.Contains(f.Message, secretPurpose) || !strings.Contains(f.Message, certPurpose) || res.OK {
			t.Errorf("both stored: %+v", res.Checks[0].Findings)
		}
		if entra.count() != before || strings.Contains(r.stdout+r.stderr, "a-client-secret-beside-the-cert") {
			t.Error("both stored: a request was made or the secret was shown")
		}

		log := d.log()
		for _, leak := range []string{first.keyBody, second.keyBody, soon.keyBody, old.keyBody, "heo-e2e-minted-connector-token", entra.last()["assertion"]} {
			if leak != "" && strings.Contains(log, leak) {
				t.Errorf("the daemon log carries %.30q", leak)
			}
		}
		c.stop(d)
	})
}
