package composition_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// tlsCert is a self-signed certificate of key, as tls.X509KeyPair gives it
// back from the PEM an operator stores.
func tlsCert(t *testing.T, key crypto.Signer, notAfter time.Time) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "heo test bot"},
		NotBefore: notAfter.Add(-2 * 365 * 24 * time.Hour), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf
}

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// entraCerts is a token endpoint of our own that takes a client assertion
// signed by one of the certificates the app registered, as Entra does.
type entraCerts struct {
	srv *httptest.Server

	mu    sync.Mutex
	certs []*x509.Certificate
	forms []map[string]string
}

func newEntraCerts(t *testing.T, certs ...*x509.Certificate) *entraCerts {
	t.Helper()
	e := &entraCerts{certs: certs}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		e.mu.Lock()
		e.forms = append(e.forms, form)
		ok := e.verify(form["client_assertion"], form["client_id"])
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || form["client_secret"] != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "invalid_client", "error_description": "AADSTS700027: Client assertion contains an invalid signature. (" + form["client_assertion"] + ")",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3600, "access_token": "minted-for" + r.URL.Path})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *entraCerts) verify(assertion, app string) bool {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return false
	}
	hdr, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(hdr, &h) != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	for _, c := range e.certs {
		thumb := sha256.Sum256(c.Raw)
		if h["x5t#S256"] == base64.RawURLEncoding.EncodeToString(thumb[:]) {
			return rsa.VerifyPKCS1v15(c.PublicKey.(*rsa.PublicKey), crypto.SHA256, sum[:], sig) == nil
		}
	}
	return false
}

func (e *entraCerts) set(certs ...*x509.Certificate) {
	e.mu.Lock()
	e.certs = certs
	e.mu.Unlock()
}

func (e *entraCerts) seen() []map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]map[string]string(nil), e.forms...)
}

// store is a credentials port holding one credential per reference and
// purpose, which records what it was asked.
type store struct {
	mu    sync.Mutex
	byKey map[string]ports.Credential
	asked []ports.CredentialRequest
}

func newStore() *store { return &store{byKey: map[string]ports.Credential{}} }

func (s *store) put(ref, purpose string, c ports.Credential) {
	s.mu.Lock()
	s.byKey[ref+"|"+purpose] = c
	s.mu.Unlock()
}

func (s *store) Resolve(_ context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, req)
	c, ok := s.byKey[req.Reference+"|"+req.Purpose]
	if !ok {
		return nil, model.NewError(model.NotFound, "no credential stored")
	}
	if !req.Accepts(c.Kind()) {
		return nil, model.NewError(model.Unsupported, "stored credential kind "+string(c.Kind())+" not accepted")
	}
	return c, nil
}

func (s *store) askedFor(purpose string, fresh bool) (n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.asked {
		if r.Purpose == purpose && r.Fresh == fresh {
			n++
		}
	}
	return n
}

const (
	certTenant = "1111aaaa-1111-4111-8111-11111111bbbb"
	certApp    = "2222cccc-2222-4222-8222-22222222dddd"
	certChanID = "00000000-0000-4000-8000-00000000000a"
)

type certSite struct {
	a     adapters.Adapter
	e     *entraCerts
	st    *store
	clock *fixedClock
	deps  adapter.Dependencies
	ch    model.Channel
}

// newCertSite is a microsoft.teams channel on a token endpoint that knows
// certs, with a credentials port holding creds.
func newCertSite(t *testing.T, certs ...*x509.Certificate) *certSite {
	t.Helper()
	e := newEntraCerts(t, certs...)
	st := newStore()
	clock := &fixedClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	s := &certSite{
		a: teamsAdapter(t, e.srv.URL), e: e, st: st, clock: clock,
		deps: adapter.Dependencies{Credentials: st, HTTP: e.srv.Client(), Clock: clock},
		ch:   teamsChannel("bot", certChanID, certTenant, certApp),
	}
	inst, err := s.a.Factory.Open(t.Context(), s.ch, s.deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return s
}

// TestTeamsCertificatePurpose: the certificate is its own purpose, a
// client_cert credential, beside the client secret, and exactly one of the
// two is the operator's to store.
func TestTeamsCertificatePurpose(t *testing.T) {
	a := teamsAdapter(t, "")
	if composition.PurposeTeamsClientCertificate != "microsoft.teams.client_certificate" {
		t.Errorf("purpose = %q", composition.PurposeTeamsClientCertificate)
	}
	want := []string{composition.PurposeTeamsClientSecret, composition.PurposeTeamsClientCertificate}
	if strings.Join(a.CredentialPurposes, ",") != strings.Join(want, ",") {
		t.Errorf("purposes = %v, want %v", a.CredentialPurposes, want)
	}
	if len(a.CredentialOneOf) != 1 || strings.Join(a.CredentialOneOf[0], ",") != strings.Join(want, ",") {
		t.Errorf("alternatives = %v, want the secret or the certificate", a.CredentialOneOf)
	}
	if k := a.CredentialKinds[composition.PurposeTeamsClientCertificate]; len(k) != 1 || k[0] != ports.KindClientCert {
		t.Errorf("certificate kinds = %v, want client_cert", k)
	}
	if k := a.CredentialKinds[composition.PurposeTeamsClientSecret]; len(k) != 1 || k[0] != ports.KindSecret {
		t.Errorf("secret kinds = %v, want secret", k)
	}
}

// TestTeamsMintsWithTheStoredCertificate: a channel whose reference holds a
// certificate and no secret mints with a signed client assertion at its own
// tenant and app, and the backend is asked for each purpose as the kind it
// is.
func TestTeamsMintsWithTheStoredCertificate(t *testing.T) {
	key := rsaKey(t)
	tc, leaf := tlsCert(t, key, time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newCertSite(t, leaf)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: tc})
	if p := s.a.ProbeCredential(t.Context(), s.ch); p != nil {
		t.Fatalf("probe = %+v, want the mint to work", p)
	}
	forms := s.e.seen()
	if len(forms) != 1 || forms[0]["client_id"] != certApp || forms[0]["client_secret"] != "" ||
		forms[0]["client_assertion_type"] != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" {
		t.Fatalf("requests = %v, want one with the app's assertion and no secret", forms)
	}
	kinds := map[string][]ports.CredentialKind{}
	for _, r := range s.st.asked {
		kinds[r.Purpose] = r.Accept
	}
	if len(kinds) != 2 || len(kinds[composition.PurposeTeamsClientSecret]) != 1 || kinds[composition.PurposeTeamsClientSecret][0] != ports.KindSecret ||
		len(kinds[composition.PurposeTeamsClientCertificate]) != 1 || kinds[composition.PurposeTeamsClientCertificate][0] != ports.KindClientCert {
		t.Errorf("asked %v, want the secret as a secret and the certificate as a client_cert, nothing else", kinds)
	}
}

// TestTeamsCertificateRotatedInTheStoreIsUsedWithoutARestart: a certificate
// stored over the old one signs the next mint of the minter already open.
func TestTeamsCertificateRotatedInTheStoreIsUsedWithoutARestart(t *testing.T) {
	tc1, leaf1 := tlsCert(t, rsaKey(t), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	tc2, leaf2 := tlsCert(t, rsaKey(t), time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newCertSite(t, leaf1)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: tc1})
	if p := s.a.ProbeCredential(t.Context(), s.ch); p != nil {
		t.Fatal(p)
	}
	s.e.set(leaf2) // the old one is removed from the app registration
	if p := s.a.ProbeCredential(t.Context(), s.ch); p == nil || p.Code != "invalid_client" {
		t.Fatalf("probe with the old certificate after the registration moved on = %+v, want invalid_client", p)
	}
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: tc2})
	if p := s.a.ProbeCredential(t.Context(), s.ch); p != nil {
		t.Errorf("probe after the store holds the new certificate = %+v, want it used at once", p)
	}
}

// TestTeamsEntraRefusingTheCertificate: the probe says what Entra said, in
// the certificate's words, names the purpose to fix, leaves out every byte
// of what was sent, and the store is told the certificate (not a secret)
// was rejected.
func TestTeamsEntraRefusingTheCertificate(t *testing.T) {
	tc, _ := tlsCert(t, rsaKey(t), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	_, other := tlsCert(t, rsaKey(t), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newCertSite(t, other)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: tc})
	p := s.a.ProbeCredential(t.Context(), s.ch)
	if p == nil || p.Purpose != composition.PurposeTeamsClientCertificate || p.Code != "invalid_client" || p.Warning ||
		!strings.Contains(p.Message, "AADSTS700027") || !strings.Contains(p.Message, "certificate") || !strings.Contains(p.Message, composition.PurposeTeamsClientCertificate) {
		t.Fatalf("probe = %+v, want invalid_client, AADSTS700027 and the certificate purpose to fix", p)
	}
	if strings.Contains(p.Message, "client secret") {
		t.Errorf("message %q talks of a client secret, and none was stored", p.Message)
	}
	if sent := s.e.seen()[0]["client_assertion"]; strings.Contains(p.Message, sent) {
		t.Error("the message holds the assertion")
	}
	if s.st.askedFor(composition.PurposeTeamsClientCertificate, true) != 1 || s.st.askedFor(composition.PurposeTeamsClientSecret, true) != 0 {
		t.Errorf("fresh asked: certificate %d, secret %d, want the certificate once and the secret never",
			s.st.askedFor(composition.PurposeTeamsClientCertificate, true), s.st.askedFor(composition.PurposeTeamsClientSecret, true))
	}
}

// TestTeamsExactlyOneOfSecretAndCertificate: both stored is refused before
// anything is sent, saying which two; neither stored is the diagnosis's.
func TestTeamsExactlyOneOfSecretAndCertificate(t *testing.T) {
	tc, leaf := tlsCert(t, rsaKey(t), time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newCertSite(t, leaf)
	if p := s.a.ProbeCredential(t.Context(), s.ch); p != nil {
		t.Errorf("nothing stored: probe = %+v, want the credential diagnosis to say so", p)
	}
	s.st.put("bot", composition.PurposeTeamsClientSecret, ports.Secret{Bytes: []byte("a-client-secret~1")})
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: tc})
	p := s.a.ProbeCredential(t.Context(), s.ch)
	if p == nil || p.Code != string(model.Conflict) || p.Warning ||
		!strings.Contains(p.Message, composition.PurposeTeamsClientSecret) || !strings.Contains(p.Message, composition.PurposeTeamsClientCertificate) {
		t.Fatalf("both stored: probe = %+v, want a conflict naming both purposes", p)
	}
	if n := len(s.e.seen()); n != 0 {
		t.Errorf("%d requests left with two credentials stored, want none", n)
	}
	if strings.Contains(p.Message, "a-client-secret") {
		t.Errorf("message %q holds the secret", p.Message)
	}
}

// TestTeamsCertificateTheMintCannotUse: what the credential diagnosis
// cannot see (a key that is not RSA, a certificate past its not_after on the
// host's clock) is the probe's finding, an error, with the purpose to fix.
func TestTeamsCertificateTheMintCannotUse(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecCert, ecLeaf := tlsCert(t, ec, time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC))
	s := newCertSite(t, ecLeaf)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: ecCert})
	p := s.a.ProbeCredential(t.Context(), s.ch)
	if p == nil || p.Purpose != composition.PurposeTeamsClientCertificate || p.Warning || !strings.Contains(p.Message, "RSA") || p.Code != string(model.Validation) {
		t.Errorf("an EC key: probe = %+v, want an RSA-only refusal", p)
	}

	rsaCert, rsaLeaf := tlsCert(t, rsaKey(t), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	s = newCertSite(t, rsaLeaf)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: rsaCert})
	p = s.a.ProbeCredential(t.Context(), s.ch)
	if p == nil || p.Warning || !strings.Contains(p.Message, "expired") || p.Code != string(model.Unauthorized) {
		t.Errorf("an expired certificate: probe = %+v, want an error saying it expired", p)
	}
	if len(s.e.seen()) != 0 {
		t.Error("an expired certificate was sent")
	}
}

// TestTeamsCertificateExpiryWarning: a certificate that works but ends
// within thirty days is a warning with the date, not an error; further out
// it is silent. The lead is on the host's clock.
func TestTeamsCertificateExpiryWarning(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		notAfter time.Time
		warn     bool
	}{
		"in 20 days": {now.Add(20 * 24 * time.Hour), true},
		"in 29 days": {now.Add(29 * 24 * time.Hour), true},
		"in 31 days": {now.Add(31 * 24 * time.Hour), false},
		"in a year":  {now.Add(365 * 24 * time.Hour), false},
	} {
		cert, leaf := tlsCert(t, rsaKey(t), tc.notAfter)
		s := newCertSite(t, leaf)
		s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: cert})
		p := s.a.ProbeCredential(t.Context(), s.ch)
		switch {
		case !tc.warn && p != nil:
			t.Errorf("%s: probe = %+v, want nothing", name, p)
		case tc.warn && (p == nil || !p.Warning || p.Code != "expires_soon" || p.Purpose != composition.PurposeTeamsClientCertificate ||
			!strings.Contains(p.Message, tc.notAfter.Format(time.DateOnly)) || !strings.Contains(p.Message, "replace")):
			t.Errorf("%s: probe = %+v, want an expires_soon warning with the date", name, p)
		}
		if len(s.e.seen()) != 1 {
			t.Errorf("%s: %d requests, want the mint made as well", name, len(s.e.seen()))
		}
	}
	// A part of a day counts as a day: ten days and a few seconds is ten.
	cert, leaf := tlsCert(t, rsaKey(t), now.Add(10*24*time.Hour-time.Hour))
	s := newCertSite(t, leaf)
	s.st.put("bot", composition.PurposeTeamsClientCertificate, ports.ClientCert{Certificate: cert})
	if p := s.a.ProbeCredential(t.Context(), s.ch); p == nil || !strings.Contains(p.Message, "(in 10 days)") {
		t.Errorf("a certificate with 9 days 23 hours left: probe = %+v, want it in 10 days", p)
	}
	// A secret has no such lead here: the warning is the certificate's.
	st := newCertSite(t)
	st.st.put("bot", composition.PurposeTeamsClientSecret, ports.Secret{Bytes: []byte("a-client-secret~1"), ExpiresAt: now.Add(24 * time.Hour)})
	if p := st.a.ProbeCredential(t.Context(), st.ch); p != nil && p.Warning {
		t.Errorf("a secret: probe = %+v, want no certificate warning", p)
	}
}
