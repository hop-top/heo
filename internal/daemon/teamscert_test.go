package daemon_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/profile"
	"hop.top/uma/model"
)

// teamsCertificate is a self-signed RSA certificate valid until notAfter,
// and the PEM of it and of its private key, as an operator stores them.
type teamsCertificate struct {
	leaf     *x509.Certificate
	certPEM  string
	keyPEM   string
	notAfter time.Time
}

func newTeamsCertificate(t *testing.T, notAfter time.Time) teamsCertificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "heo test bot"},
		NotBefore: notAfter.Add(-2 * 365 * 24 * time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
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
	return teamsCertificate{
		leaf: leaf, notAfter: notAfter,
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
}

// teamsStore holds credentials under reference teamsRef in heo's file
// backend, as `heo credential` would: a secret under the secret's purpose,
// a certificate and key under the certificate's.
type teamsStore struct {
	t *testing.T
	b *credentials.Backend
}

func newTeamsStore(t *testing.T) (*recordingCreds, *teamsStore) {
	t.Helper()
	b, err := credentials.Open(t.Context(), credentials.Options{
		Profile: profile.Default(), Tenant: "acme",
		Config: credentials.Config{Backend: credentials.BackendFile, KeyringService: "heo", FileDir: t.TempDir(), OpenbaoMount: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &recordingCreds{next: b.Channels()}, &teamsStore{t: t, b: b}
}

func (s *teamsStore) put(purpose string, envelope map[string]any) {
	s.t.Helper()
	keys, err := s.b.Keys(teamsRef, purpose)
	if err != nil {
		s.t.Fatal(err)
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := s.b.SurfaceTokens().Set(s.t.Context(), keys[0], data); err != nil {
		s.t.Fatal(err)
	}
}

func (s *teamsStore) secret(v string) {
	s.t.Helper()
	s.put(composition.PurposeTeamsClientSecret, map[string]any{"v": 1, "kind": "secret", "value": v})
}

func (s *teamsStore) certificate(c teamsCertificate) {
	s.t.Helper()
	s.put(composition.PurposeTeamsClientCertificate, map[string]any{"v": 1, "kind": "client_cert", "certificate_pem": c.certPEM, "private_key_pem": c.keyPEM})
}

// TestTeamsConnectorCertificate: a bot whose app proves itself with a
// certificate mints its Connector token with a signed assertion for its
// tenant and app, no secret in the request; a certificate stored over it
// is used by the next mint with no restart; a certificate Entra refuses
// fails the send as unauthorized in its own words, and the store is told it
// was the certificate that was rejected; two credentials stored, or an
// expired certificate, fail the send before anything reaches Microsoft.
func TestTeamsConnectorCertificate(t *testing.T) {
	var n int
	send := func(s *teamsSite) model.Operation {
		t.Helper()
		n++
		acc, err := s.r.client().Conversations().Start(t.Context(), model.StartConversation{
			Command: s.r.cmd("start-cert-" + strconv.Itoa(n)), Inbox: s.inbox, Recipient: teamsRoom(s, teamsGroup), Content: desk.TextContent("hi"),
		})
		if err != nil {
			t.Fatal(err)
		}
		return s.settled(acc)
	}
	far := time.Now().Add(2 * 365 * 24 * time.Hour)
	first, second := newTeamsCertificate(t, far), newTeamsCertificate(t, far.Add(24*time.Hour))

	creds, store := newTeamsStore(t)
	store.certificate(first)
	s := newTeamsSite(t, creds, daemon.NameWebhook, daemon.NameWorkers)
	s.bot.registerCerts(first.leaf)
	if op := send(s); op.State != model.ProviderAccepted {
		t.Fatalf("send = %s %+v", op.State, op.Failure)
	}
	mints := s.bot.minted()
	if len(mints) != 1 {
		t.Fatalf("%d mints, want 1", len(mints))
	}
	if m := mints[0]; m.Path != "/"+teamsTenantID+"/oauth2/v2.0/token" || m.Grant != "client_credentials" || m.App != teamsAppID ||
		m.Secret != "" || m.Authorization != "" || m.Assertion == "" || !strings.HasPrefix(m.AssertionType, "urn:ietf:params:oauth:client-assertion-type:") {
		t.Errorf("mint = %+v, want the client credentials request with an assertion and no secret", m)
	}

	// The operator rotates the certificate: the app registration moves to the
	// new one and the store holds it; the service refuses the token it was
	// given, and the mint that follows is signed with the new certificate.
	s.bot.registerCerts(second.leaf)
	store.certificate(second)
	s.bot.setConnectorToken("a-newer-connector-token")
	s.clock.advance(time.Minute)
	if op := send(s); op.State != model.ProviderAccepted {
		t.Fatalf("send after the rotation = %s %+v, want the new certificate used", op.State, op.Failure)
	}
	if got := s.bot.mintCount(); got != 2 {
		t.Errorf("%d mints, want 2", got)
	}

	// A certificate Entra does not know.
	creds, store = newTeamsStore(t)
	store.certificate(first)
	bad := newTeamsSite(t, creds, daemon.NameWebhook, daemon.NameWorkers)
	bad.bot.registerCerts(second.leaf)
	op := send(bad)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unauthorized {
		t.Fatalf("a refused certificate = %s %+v, want failed as unauthorized", op.State, op.Failure)
	}
	for _, want := range []string{"invalid_client", "AADSTS700027", "certificate"} {
		if !strings.Contains(op.Failure.SafeMessage, want) {
			t.Errorf("the failure says %q, want %s", op.Failure.SafeMessage, want)
		}
	}
	if strings.Contains(op.Failure.SafeMessage, "secret") || strings.Contains(op.Failure.SafeMessage, first.keyPEM[:40]) {
		t.Errorf("the failure %q names a secret or holds key material", op.Failure.SafeMessage)
	}
	if !creds.askedFresh(teamsRef, composition.PurposeTeamsClientCertificate) || creds.askedFresh(teamsRef, composition.PurposeTeamsClientSecret) {
		t.Error("the store was not told the certificate, and only it, was rejected")
	}
	store.certificate(second)
	if op := send(bad); op.State != model.ProviderAccepted {
		t.Errorf("after the corrected certificate = %s %+v, want it used at once", op.State, op.Failure)
	}

	// Two credentials: refused before any request.
	creds, store = newTeamsStore(t)
	store.certificate(first)
	store.secret(teamsSecret)
	both := newTeamsSite(t, creds, daemon.NameWebhook, daemon.NameWorkers)
	both.bot.registerCerts(first.leaf)
	op = send(both)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Conflict ||
		!strings.Contains(op.Failure.SafeMessage, composition.PurposeTeamsClientSecret) || !strings.Contains(op.Failure.SafeMessage, composition.PurposeTeamsClientCertificate) {
		t.Fatalf("both stored = %s %+v, want failed as a conflict naming both purposes", op.State, op.Failure)
	}
	if got := both.bot.total() + both.bot.mintCount(); got != 0 {
		t.Errorf("both stored: Microsoft was reached %d times", got)
	}

	// A certificate past its not_after.
	creds, store = newTeamsStore(t)
	store.certificate(newTeamsCertificate(t, time.Now().Add(-time.Hour)))
	old := newTeamsSite(t, creds, daemon.NameWebhook, daemon.NameWorkers)
	op = send(old)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Unauthorized || !strings.Contains(op.Failure.SafeMessage, "expired") {
		t.Fatalf("an expired certificate = %s %+v, want failed as unauthorized, expired", op.State, op.Failure)
	}
	if got := old.bot.total() + old.bot.mintCount(); got != 0 {
		t.Errorf("an expired certificate reached Microsoft %d times", got)
	}
	if code, body := old.activity(tmMessage("1760000400002", teamsPerson, teamsChannel, "channel", "received")); code != http.StatusOK {
		t.Errorf("receiving with an expired certificate = %d %q, want 200: inbound needs no credential", code, body)
	}
}
