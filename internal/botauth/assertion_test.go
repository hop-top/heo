package botauth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/botauth"
	"hop.top/uma/model"
)

const assertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

var (
	keyPoolMu sync.Mutex
	keyPool   = map[int]*rsa.PrivateKey{}
)

// poolKey is the i-th RSA key of the test binary: generating one is slow,
// and tests only need a few that differ.
func poolKey(t *testing.T, i int) *rsa.PrivateKey {
	t.Helper()
	keyPoolMu.Lock()
	defer keyPoolMu.Unlock()
	if k, ok := keyPool[i]; ok {
		return k
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPool[i] = k
	return k
}

// issue is a self-signed certificate of key, valid between the two times.
func issue(t *testing.T, key *rsa.PrivateKey, notBefore, notAfter time.Time) *x509.Certificate {
	t.Helper()
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "heo test bot"}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature,
	}, &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "heo test bot"}}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func thumbprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// seenAssertion is an assertion the fake endpoint verified.
type seenAssertion struct {
	Raw    string
	Header map[string]any
	Claims map[string]any
}

// register makes cert one of the certificates the app has in its
// registration.
func (f *fakeEntra) register(tenant, app string, cert *x509.Certificate) {
	f.mu.Lock()
	f.certs[tenant+"/"+app] = append(f.certs[tenant+"/"+app], cert)
	f.mu.Unlock()
}

// unregister removes every certificate of the app.
func (f *fakeEntra) unregister(tenant, app string) {
	f.mu.Lock()
	delete(f.certs, tenant+"/"+app)
	f.mu.Unlock()
}

func (f *fakeEntra) verified() []seenAssertion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seenAssertion(nil), f.assertions...)
}

// checkAssertion judges the request as Microsoft Entra does a client
// assertion (RFC 7523): the type, a compact JWT signed RS256 by a
// certificate the app registered, found by its x5t#S256 header, naming
// this endpoint as audience and the app as issuer and subject, in its
// validity window and with an ID not seen before. It parses the token by
// hand, so it does not trust the library that signed it.
func (f *fakeEntra) checkAssertion(tenant, app, endpoint string, form url.Values) bool {
	fail := func(format string, a ...any) bool {
		f.t.Logf("fake Entra refuses the assertion: "+format, a...)
		return false
	}
	if form.Get("client_assertion_type") != assertionType {
		return fail("client_assertion_type = %q", form.Get("client_assertion_type"))
	}
	raw := form.Get("client_assertion")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fail("not a compact JWS")
	}
	var seg [3][]byte
	for i, p := range parts {
		b, err := base64.RawURLEncoding.DecodeString(p)
		if err != nil {
			return fail("segment %d is not unpadded base64url: %v", i, err)
		}
		seg[i] = b
	}
	var header, claims map[string]any
	if json.Unmarshal(seg[0], &header) != nil || json.Unmarshal(seg[1], &claims) != nil {
		return fail("header or claims are not JSON")
	}
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		return fail("alg %v typ %v, want RS256 JWT", header["alg"], header["typ"])
	}
	x5t, _ := header["x5t#S256"].(string)
	f.mu.Lock()
	registered := append([]*x509.Certificate(nil), f.certs[tenant+"/"+app]...)
	f.mu.Unlock()
	var cert *x509.Certificate
	for _, c := range registered {
		if thumbprint(c) == x5t {
			cert = c
		}
	}
	if cert == nil {
		return fail("no registered certificate has x5t#S256 %q", x5t)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return fail("registered certificate is not RSA")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], seg[2]); err != nil {
		return fail("signature: %v", err)
	}
	if claims["iss"] != app || claims["sub"] != app || claims["iss"] != form.Get("client_id") {
		return fail("iss %v sub %v client_id %v, want the app %s", claims["iss"], claims["sub"], form.Get("client_id"), app)
	}
	if claims["aud"] != endpoint {
		return fail("aud %v, want the token endpoint %s", claims["aud"], endpoint)
	}
	now := f.now()
	num := func(k string) time.Time {
		v, ok := claims[k].(float64)
		if !ok {
			return time.Time{}
		}
		return time.Unix(int64(v), 0)
	}
	nbf, exp := num("nbf"), num("exp")
	if nbf.IsZero() || exp.IsZero() {
		return fail("nbf and exp are required")
	}
	if nbf.After(now) || !exp.After(now) || exp.Sub(now) > 10*time.Minute {
		return fail("window nbf %v exp %v at %v, want nbf <= now < exp <= now + 10m", nbf, exp, now)
	}
	jti, _ := claims["jti"].(string)
	f.mu.Lock()
	defer f.mu.Unlock()
	if jti == "" || f.jtis[jti] {
		return fail("jti %q is empty or was used before", jti)
	}
	f.jtis[jti] = true
	f.assertions = append(f.assertions, seenAssertion{Raw: raw, Header: header, Claims: claims})
	return true
}

// certBox is the certificate and key the credential store holds,
// rotatable.
type certBox struct {
	mu       sync.Mutex
	cert     *x509.Certificate
	key      *rsa.PrivateKey
	err      error
	reads    int
	rejected int
}

func (b *certBox) set(c *x509.Certificate, k *rsa.PrivateKey) {
	b.mu.Lock()
	b.cert, b.key = c, k
	b.mu.Unlock()
}

func (b *certBox) credential(context.Context) (botauth.Credential, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	return botauth.Credential{Certificate: b.cert, Key: b.key}, b.err
}

func (b *certBox) reject(_ context.Context, k botauth.Kind) {
	if k != botauth.KindCertificate {
		panic(fmt.Sprintf("the store was told a %s was rejected, want a certificate", k))
	}
	b.mu.Lock()
	b.rejected++
	b.mu.Unlock()
}

type certRig struct {
	*rig
	box  *certBox
	cert *x509.Certificate
	key  *rsa.PrivateKey
}

// newCertRig is a Minter of bot A that authenticates with a certificate
// registered for it, valid for three years from the fake clock's now.
func newCertRig(t *testing.T, mutate ...func(*botauth.Options)) *certRig {
	t.Helper()
	clock := newClock()
	key := poolKey(t, 0)
	cert := issue(t, key, clock.Now().Add(-time.Hour), clock.Now().Add(3*365*24*time.Hour))
	box := &certBox{cert: cert, key: key}
	f := newFakeEntra(t)
	f.now = clock.Now
	f.register(tenantA, appA, cert)
	r := &rig{f: f, clock: clock, log: &capture{}}
	o := botauth.Options{
		Tenant: tenantA, AppID: appA, Authority: f.srv.URL, HTTP: f.srv.Client(),
		Credential: box.credential, Rejected: box.reject, Now: clock.Now, Log: r.log,
	}
	for _, m := range mutate {
		m(&o)
	}
	m, err := botauth.New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	return &certRig{rig: r, box: box, cert: cert, key: key}
}

// TestMintsWithASignedClientAssertion: the request is Entra's documented
// certificate grant: the client credentials grant of the tenant's v2.0
// endpoint with a JWT bearer client assertion in place of the secret,
// RS256, x5t#S256 of the certificate, the endpoint as audience, the app as
// issuer and subject, a short window and an ID. The fake endpoint parses
// and verifies it by hand.
func TestMintsWithASignedClientAssertion(t *testing.T) {
	r := newCertRig(t)
	tok, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if want := "access-token-1-for-" + tenantA; tok.Value != want {
		t.Errorf("token = %q, want %q", tok.Value, want)
	}
	if want := r.clock.Now().Add(time.Hour); !tok.Expiry.Equal(want) {
		t.Errorf("expiry = %v, want the clock's now + expires_in = %v", tok.Expiry, want)
	}
	reqs := r.f.seen()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1", len(reqs))
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/"+tenantA+"/oauth2/v2.0/token" || !strings.HasPrefix(q.ContentType, "application/x-www-form-urlencoded") {
		t.Errorf("request = %s %s (%s)", q.Method, q.Path, q.ContentType)
	}
	if q.Authorization != "" {
		t.Error("the assertion went in a header; Entra takes it in the body")
	}
	for k, want := range map[string]string{
		"grant_type": "client_credentials", "client_id": appA, "scope": botauth.Scope, "client_assertion_type": assertionType,
	} {
		if got := q.Form.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
	if q.Form.Has("client_secret") {
		t.Error("a client secret was sent beside the assertion")
	}
	got := r.f.verified()
	if len(got) != 1 {
		t.Fatalf("%d assertions verified, want 1", len(got))
	}
	a := got[0]
	if a.Header["x5t#S256"] != thumbprint(r.cert) || a.Header["alg"] != "RS256" || a.Header["typ"] != "JWT" {
		t.Errorf("header = %v", a.Header)
	}
	now := float64(r.clock.Now().Unix())
	if a.Claims["iss"] != appA || a.Claims["sub"] != appA || a.Claims["aud"] != r.f.srv.URL+"/"+tenantA+"/oauth2/v2.0/token" {
		t.Errorf("claims = %v", a.Claims)
	}
	if exp := a.Claims["exp"].(float64); exp <= now || exp > now+600 {
		t.Errorf("exp = %v at %v, want within ten minutes", exp, now)
	}
	if nbf := a.Claims["nbf"].(float64); nbf > now {
		t.Errorf("nbf = %v after now %v", nbf, now)
	}
}

// TestAssertionsAreSingleUseAndOnTheInjectedClock: every mint carries its
// own jti, even several at one instant, and its window is the Minter's
// clock's, not the wall clock's.
func TestAssertionsAreSingleUseAndOnTheInjectedClock(t *testing.T) {
	r := newCertRig(t)
	r.clock.Advance(-5 * 365 * 24 * time.Hour) // the wall clock is nowhere near
	for range 4 {
		if err := r.m.Probe(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	r.clock.Advance(time.Second)
	if err := r.m.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range r.f.verified() {
		jti, _ := a.Claims["jti"].(string)
		if len(jti) < 16 || seen[jti] {
			t.Errorf("jti %q is short or repeated", jti)
		}
		seen[jti] = true
		if exp := time.Unix(int64(a.Claims["exp"].(float64)), 0); time.Since(exp) < 365*24*time.Hour {
			t.Errorf("exp %v is near the wall clock: the window did not follow the injected clock", exp)
		}
	}
	if len(seen) != 5 {
		t.Errorf("%d distinct assertions, want 5", len(seen))
	}
	// A replayed assertion is refused by the endpoint.
	reqs := r.f.seen()
	replay := reqs[len(reqs)-1].Form
	resp, err := http.PostForm(r.f.srv.URL+"/"+tenantA+"/oauth2/v2.0/token", replay)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a replayed assertion = %d, want 401", resp.StatusCode)
	}
}

// TestCertificateRotationNeedsNoRestart: the certificate is read at every
// mint, so one rotated in the store signs the next, and a rotation made
// after a failure is tried at once instead of waiting out the backoff.
func TestCertificateRotationNeedsNoRestart(t *testing.T) {
	r := newCertRig(t)
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	key2 := poolKey(t, 1)
	cert2 := issue(t, key2, r.clock.Now().Add(-time.Hour), r.clock.Now().Add(2*365*24*time.Hour))
	r.f.unregister(tenantA, appA)
	r.f.register(tenantA, appA, cert2) // the old certificate is removed from the registration
	r.box.set(cert2, key2)
	r.clock.Advance(58 * time.Minute)
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatalf("after the rotation: %v", err)
	}
	got := r.f.verified()
	if last := got[len(got)-1]; last.Header["x5t#S256"] != thumbprint(cert2) {
		t.Errorf("the renewal was signed for %v, want the new certificate %s", last.Header["x5t#S256"], thumbprint(cert2))
	}

	// The operator stores a certificate Entra does not know, then the right one.
	key3 := poolKey(t, 2)
	cert3 := issue(t, key3, r.clock.Now().Add(-time.Hour), r.clock.Now().Add(365*24*time.Hour))
	r.box.set(cert3, key3)
	r.clock.Advance(2 * time.Hour)
	if _, err := r.m.Token(t.Context(), false); err == nil {
		t.Fatal("an unregistered certificate minted a token")
	}
	before := r.f.count()
	if _, err := r.m.Token(t.Context(), false); err == nil || r.f.count() != before {
		t.Fatalf("same certificate inside the backoff: %v, %d new requests, want none", err, r.f.count()-before)
	}
	r.box.set(cert2, key2)
	if _, err := r.m.Token(t.Context(), false); err != nil || r.f.count() != before+1 {
		t.Errorf("certificate corrected: %v, %d new requests, want one attempt at once", err, r.f.count()-before)
	}
}

// TestSecretAndCertificateAreSwappedAtTheNextMint: what the store holds
// decides, at every mint, which of the two proves the app.
func TestSecretAndCertificateAreSwappedAtTheNextMint(t *testing.T) {
	r := newCertRig(t)
	var mu sync.Mutex
	useSecret := false
	r.f.accept(tenantA, appA, secretA)
	m, err := botauth.New(botauth.Options{
		Tenant: tenantA, AppID: appA, Authority: r.f.srv.URL, HTTP: r.f.srv.Client(), Now: r.clock.Now,
		Credential: func(ctx context.Context) (botauth.Credential, error) {
			mu.Lock()
			defer mu.Unlock()
			if useSecret {
				return botauth.Credential{Secret: secretA}, nil
			}
			return r.box.credential(ctx)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	useSecret = true
	mu.Unlock()
	if err := m.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	reqs := r.f.seen()
	if len(reqs) != 2 || reqs[0].Form.Has("client_secret") || !reqs[1].Form.Has("client_secret") || reqs[1].Form.Has("client_assertion") {
		t.Errorf("requests = %+v, want an assertion, then a secret and no assertion", reqs)
	}
}

// TestCertificateThatIsNotTheKeysIsRefusedBeforeAnyRequest: a key that is
// not the certificate's cannot make an assertion Entra would accept; the
// refusal is local, names no material and sends nothing.
func TestCertificateThatIsNotTheKeysIsRefusedBeforeAnyRequest(t *testing.T) {
	r := newCertRig(t)
	r.box.set(r.cert, poolKey(t, 1))
	_, err := r.m.Token(t.Context(), false)
	me := (*model.Error)(nil)
	if !errors.As(err, &me) || me.Code != model.Unauthorized || me.Retry != model.NoRetry {
		t.Fatalf("a mismatched key = %v, want unauthorized, no retry", err)
	}
	if !strings.Contains(me.SafeMessage, "private key") || !strings.Contains(me.SafeMessage, "certificate") {
		t.Errorf("message %q does not say the key and the certificate differ", me.SafeMessage)
	}
	if r.f.count() != 0 {
		t.Errorf("%d requests, want none", r.f.count())
	}
	// Nothing is held against the endpoint: the right key works at once.
	r.box.set(r.cert, r.key)
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Errorf("after the fix = %v, want a token at once", err)
	}
}

// TestExpiredCertificateIsRefusedBeforeAnyRequest: after its not_after a
// certificate signs nothing, and a token minted earlier is served while it
// lasts.
func TestExpiredCertificateIsRefusedBeforeAnyRequest(t *testing.T) {
	r := newCertRig(t)
	notAfter := r.cert.NotAfter
	first, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := r.m.CertificateNotAfter(); !ok || !got.Equal(notAfter) {
		t.Errorf("CertificateNotAfter = %v, %v, want %v", got, ok, notAfter)
	}
	r.clock.Advance(notAfter.Sub(r.clock.Now()) - time.Minute) // a minute before not_after
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatalf("a minute before not_after = %v, want a token", err)
	}
	n := r.f.count()
	r.clock.Advance(2 * time.Hour)
	_, err = r.m.Token(t.Context(), false)
	me := (*model.Error)(nil)
	if !errors.As(err, &me) || me.Code != model.Unauthorized || !strings.Contains(me.SafeMessage, "expired") {
		t.Fatalf("an expired certificate = %v, want unauthorized, expired", err)
	}
	if r.f.count() != n {
		t.Errorf("%d requests after the expiry, want none", r.f.count()-n)
	}
	if perr := r.m.Probe(t.Context()); model.CodeOf(perr) != model.Unauthorized {
		t.Errorf("Probe of an expired certificate = %v, want unauthorized", perr)
	}
	_ = first
}

// TestEndpointRefusingTheCertificate: a certificate Entra does not know
// fails as unauthorized naming the provider's codes and the certificate,
// not a secret, and the store is told it was a certificate.
func TestEndpointRefusingTheCertificate(t *testing.T) {
	r := newCertRig(t)
	r.f.unregister(tenantA, appA)
	_, err := r.m.Token(t.Context(), false)
	me := (*model.Error)(nil)
	if !errors.As(err, &me) || me.Code != model.Unauthorized || me.Retry != model.NoRetry {
		t.Fatalf("a refused certificate = %v, want unauthorized, no retry", err)
	}
	for _, want := range []string{"invalid_client", "AADSTS700027", "certificate"} {
		if !strings.Contains(me.SafeMessage, want) {
			t.Errorf("message %q lacks %s", me.SafeMessage, want)
		}
	}
	if strings.Contains(me.SafeMessage, "secret") {
		t.Errorf("message %q talks of a secret, and none was sent", me.SafeMessage)
	}
	if r.box.rejected != 1 {
		t.Errorf("the store was told of the rejection %d times, want 1", r.box.rejected)
	}
	var mint *botauth.MintError
	if perr := r.m.Probe(t.Context()); !errors.As(perr, &mint) || mint.Kind != botauth.KindCertificate || !mint.Rejects() {
		t.Errorf("Probe = %#v, want a rejected certificate", perr)
	}
}

// TestCertificateTenantsAreIsolated: two bots in two tenants each sign for
// their own app, tenant endpoint and certificate; an assertion made for
// one is no good at the other's endpoint, and a failure of one is not the
// other's.
func TestCertificateTenantsAreIsolated(t *testing.T) {
	f := newFakeEntra(t)
	clock := newClock()
	f.now = clock.Now
	keyA, keyB := poolKey(t, 0), poolKey(t, 1)
	certA := issue(t, keyA, clock.Now().Add(-time.Hour), clock.Now().Add(24*time.Hour))
	certB := issue(t, keyB, clock.Now().Add(-time.Hour), clock.Now().Add(24*time.Hour))
	f.register(tenantA, appA, certA)
	f.register(tenantB, appB, certB)
	mk := func(tenant, app string, box *certBox) *botauth.Minter {
		m, err := botauth.New(botauth.Options{
			Tenant: tenant, AppID: app, Authority: f.srv.URL, HTTP: f.srv.Client(), Credential: box.credential, Now: clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	boxA, boxB := &certBox{cert: certA, key: keyA}, &certBox{cert: certB, key: keyB}
	a, b := mk(tenantA, appA, boxA), mk(tenantB, appB, boxB)
	ta, err := a.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := b.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ta.Value, tenantA) || !strings.HasSuffix(tb.Value, tenantB) {
		t.Errorf("tokens %q and %q: each must come from its own tenant's endpoint", ta.Value, tb.Value)
	}
	byTenant := map[string]seenAssertion{}
	for _, q := range f.seen() {
		byTenant[q.Path] = seenAssertion{Raw: q.Form.Get("client_assertion")}
	}
	// A's assertion, offered to B's endpoint, is refused: wrong audience,
	// wrong app, wrong certificate.
	bad := url.Values{
		"grant_type": {"client_credentials"}, "scope": {botauth.Scope}, "client_id": {appB},
		"client_assertion_type": {assertionType}, "client_assertion": {byTenant["/"+tenantA+"/oauth2/v2.0/token"].Raw},
	}
	resp, err := http.PostForm(f.srv.URL+"/"+tenantB+"/oauth2/v2.0/token", bad)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("A's assertion at B's endpoint = %d, want 401", resp.StatusCode)
	}
	// B's certificate offered for A's app is refused, and A's failure is not B's.
	boxA.set(certB, keyB)
	clock.Advance(2 * time.Hour)
	if _, err := a.Token(t.Context(), false); err == nil {
		t.Error("A minted with B's certificate")
	}
	if got, err := b.Token(t.Context(), false); err != nil || got.Value == tb.Value {
		t.Errorf("B = %+v, %v, want a fresh token of its own", got, err)
	}
}

// TestNothingOfTheKeyIsLoggedOrReturned: whatever the endpoint says, even
// a description repeating the whole assertion, neither the private key,
// the assertion nor a token reaches a log line or an error's text.
func TestNothingOfTheKeyIsLoggedOrReturned(t *testing.T) {
	r := newCertRig(t)
	r.f.echo = true
	tok, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	r.f.unregister(tenantA, appA)
	r.clock.Advance(2 * time.Hour)
	var shown []string
	_, err = r.m.Token(t.Context(), false)
	shown = append(shown, fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, err))
	perr := r.m.Probe(t.Context())
	shown = append(shown, fmt.Sprintf("%v|%+v|%#v", perr, perr, perr))
	r.f.next(status(http.StatusInternalServerError))
	_, err = r.m.Token(t.Context(), false)
	shown = append(shown, fmt.Sprintf("%v|%+v", err, err))
	r.box.set(r.cert, poolKey(t, 1))
	_, err = r.m.Token(t.Context(), false)
	shown = append(shown, fmt.Sprintf("%v|%+v", err, err))
	all := strings.Join(shown, "\n") + "\n" + r.log.String()

	der, _ := x509.MarshalPKCS8PrivateKey(r.key)
	leaks := []string{tok.Value, "access-token-", base64.StdEncoding.EncodeToString(der)[:60],
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), r.key.D.String()}
	for _, q := range r.f.seen() {
		leaks = append(leaks, q.Form.Get("client_assertion"))
	}
	for _, leak := range leaks {
		if leak != "" && strings.Contains(all, leak) {
			t.Errorf("%.40q leaked into errors or the log:\n%s", leak, all)
		}
	}
	if !strings.Contains(r.log.String(), "invalid_client") {
		t.Errorf("the failure was not logged with the provider's code:\n%s", r.log)
	}
}

// TestCachedTokenNeedsNoCertificate: a Minter serves a token that is still
// good without reading the certificate again, so a store that cannot be read
// for a moment costs nothing.
func TestCachedTokenNeedsNoCertificate(t *testing.T) {
	r := newCertRig(t)
	first, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	reads := r.box.reads
	r.box.err = model.NewError(model.NotFound, "no credential stored")
	r.clock.Advance(10 * time.Minute)
	if got, err := r.m.Token(t.Context(), false); err != nil || got != first || r.box.reads != reads {
		t.Errorf("cached = %+v, %v after %d reads, want the cached token, no read", got, err, r.box.reads-reads)
	}
}

// TestACredentialIsASecretOrACertificate: both, or neither, is refused
// before any request.
func TestACredentialIsASecretOrACertificate(t *testing.T) {
	r := newCertRig(t)
	for name, c := range map[string]botauth.Credential{
		"both":            {Secret: secretA, Certificate: r.cert, Key: r.key},
		"neither":         {},
		"a cert, no key":  {Certificate: r.cert},
		"a key, no cert":  {Key: r.key},
		"a key, a secret": {Secret: secretA, Key: r.key},
	} {
		m, err := botauth.New(botauth.Options{
			Tenant: tenantA, AppID: appA, Authority: r.f.srv.URL, HTTP: r.f.srv.Client(), Now: r.clock.Now,
			Credential: func(context.Context) (botauth.Credential, error) { return c, nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = m.Token(t.Context(), false)
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s = %v, want a validation error", name, err)
		}
	}
	if r.f.count() != 0 {
		t.Errorf("%d requests, want none", r.f.count())
	}
}

// TestCredentialRendersRedacted: a credential that reaches a log call or a
// format verb by accident shows nothing of the secret or the key.
func TestCredentialRendersRedacted(t *testing.T) {
	r := newCertRig(t)
	for name, c := range map[string]botauth.Credential{
		"secret": {Secret: secretA}, "certificate": {Certificate: r.cert, Key: r.key},
	} {
		js, _ := json.Marshal(map[string]any{"c": c})
		var sb strings.Builder
		slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "c", c)
		all := fmt.Sprintf("%v|%+v|%#v|%s|%d|", c, c, c, c, c) + string(js) + sb.String()
		for _, leak := range []string{secretA, r.key.D.String(), "MIIE"} {
			if strings.Contains(all, leak) {
				t.Errorf("%s: %q leaked: %s", name, leak, all)
			}
		}
		if !strings.Contains(all, "[redacted]") {
			t.Errorf("%s: %s, want [redacted]", name, all)
		}
	}
}
