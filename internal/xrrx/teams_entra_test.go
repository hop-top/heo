package xrrx

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	xrr "hop.top/xrr"
)

// tmMint is the daemon's token request for the placeholder bot, as heo
// makes it.
func tmMint(mod ...func(url.Values)) (string, url.Values) {
	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {TeamsAppID}, "client_secret": {TeamsStandInSecret},
		"scope": {"https://api.botframework.com/.default"},
	}
	for _, m := range mod {
		m(form)
	}
	return "https://" + TeamsEntraHost + "/" + TeamsTenantID + "/oauth2/v2.0/token", form
}

func tmPostForm(rt http.RoundTripper, rawURL string, form url.Values, header ...string) (*http.Response, string, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

// TestTeamsTokenRequestIsAnsweredLocallyAndOnlyForThePlaceholderBot: the
// daemon mints its Bot Connector token for the placeholder bot and tenant
// with the stand-in secret; the sandbox answers with the stand-in token
// and reaches nothing. Any other request to Microsoft Entra is refused:
// another tenant, app, secret, scope or grant, a query, a header carrying
// the secret, extra fields.
func TestTeamsTokenRequestIsAnsweredLocallyAndOnlyForThePlaceholderBot(t *testing.T) {
	spy := &fakeTeams{}
	live := &Env{Mode: xrr.ModeRecord, Teams: newTeams(t, 3)}
	rt := live.forwarder(spy)

	u, form := tmMint()
	resp, body, err := tmPostForm(rt, u, form)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the placeholder bot's mint = %v %v", resp, err)
	}
	var got struct {
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.TokenType != "Bearer" || got.ExpiresIn <= 0 || got.AccessToken != TeamsStandInToken {
		t.Fatalf("answer %q = %+v, %v, want a Bearer stand-in token with a lifetime", body, got, err)
	}
	if strings.Contains(body, live.Teams.Token) {
		t.Error("the answer carries the real token")
	}

	for name, tc := range map[string]struct {
		url  string
		form url.Values
		hdr  []string
	}{
		"another tenant":      {url: "https://" + TeamsEntraHost + "/" + tmTenant + "/oauth2/v2.0/token", form: form},
		"the common endpoint": {url: "https://" + TeamsEntraHost + "/common/oauth2/v2.0/token", form: form},
		"v1 endpoint":         {url: "https://" + TeamsEntraHost + "/" + TeamsTenantID + "/oauth2/token", form: form},
		"a query":             {url: u + "?x=1", form: form},
		"plain http":          {url: "http://" + TeamsEntraHost + "/" + TeamsTenantID + "/oauth2/v2.0/token", form: form},
		"another app":         {url: u, form: func() url.Values { _, f := tmMint(func(v url.Values) { v.Set("client_id", tmApp) }); return f }()},
		"the real secret": {url: u, form: func() url.Values {
			_, f := tmMint(func(v url.Values) { v.Set("client_secret", "a-real-secret~9") })
			return f
		}()},
		"another scope": {url: u, form: func() url.Values {
			_, f := tmMint(func(v url.Values) { v.Set("scope", "https://graph.microsoft.com/.default") })
			return f
		}()},
		"another grant":          {url: u, form: func() url.Values { _, f := tmMint(func(v url.Values) { v.Set("grant_type", "password") }); return f }()},
		"an extra field":         {url: u, form: func() url.Values { _, f := tmMint(func(v url.Values) { v.Set("username", "x") }); return f }()},
		"a field twice":          {url: u, form: func() url.Values { _, f := tmMint(func(v url.Values) { v.Add("scope", "x") }); return f }()},
		"a missing field":        {url: u, form: func() url.Values { _, f := tmMint(func(v url.Values) { v.Del("scope") }); return f }()},
		"the secret in a header": {url: u, form: form, hdr: []string{"Authorization", "Basic abc"}},
	} {
		if _, _, err := tmPostForm(rt, tc.url, tc.form, tc.hdr...); err == nil {
			t.Errorf("%s: answered", name)
		}
	}
	// The sandbox's own transport refuses any other host, too.
	if _, _, err := tmPostForm(live.Teams.TokenTransport(), "https://login.microsoftonline.us/"+TeamsTenantID+"/oauth2/v2.0/token", form); err == nil {
		t.Error("another cloud's endpoint was answered")
	}
	if req, _ := http.NewRequest(http.MethodGet, u, nil); true {
		if _, err := rt.RoundTrip(req); err == nil {
			t.Error("a GET was answered")
		}
	}
	if _, err := rt.RoundTrip(mustJSON(t, u)); err == nil {
		t.Error("a JSON body was answered")
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("a token request reached the network: %v", got)
	}
}

func mustJSON(t *testing.T, u string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(`{"grant_type":"client_credentials"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestForwarderNeverReachesEntra: Microsoft Entra's token endpoint is
// reached by no recording, replay or passthrough; a live recording's
// sandbox answers the placeholder bot's request itself.
func TestForwarderNeverReachesEntra(t *testing.T) {
	spy := &fakeTeams{}
	u, form := tmMint()
	for name, e := range map[string]*Env{
		"replay":                     {Mode: xrr.ModeReplay},
		"record, no live":            {Mode: xrr.ModeRecord},
		"passthrough":                {Mode: xrr.ModePassthrough},
		"record, live but not Teams": {Mode: xrr.ModeRecord},
	} {
		if _, _, err := tmPostForm(e.forwarder(spy), u, form); err == nil {
			t.Errorf("%s: Microsoft Entra was reached", name)
		}
	}
	// A real tenant's mint, with a live sandbox: refused, not sent.
	live := &Env{Mode: xrr.ModeRecord, Teams: newTeams(t, 3)}
	real := "https://" + TeamsEntraHost + "/" + tmTenant + "/oauth2/v2.0/token"
	if _, _, err := tmPostForm(live.forwarder(spy), real, form); err == nil {
		t.Error("a live recording reached a real tenant's token endpoint")
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("refused traffic reached the network: %v", got)
	}
}

// TestTheConnectorTransportStillRefusesEntra: the Bot Connector's own
// transport answers nothing at the token endpoint, as before.
func TestTheConnectorTransportStillRefusesEntra(t *testing.T) {
	s := newTeams(t, 3)
	u, form := tmMint()
	if _, _, err := tmPostForm(s.Transport(&fakeTeams{}), u, form); err == nil {
		t.Error("the Bot Connector transport answered a token request")
	}
}

// TestConnectorAcceptsTheTokenAsXRRHandsItOver: the token endpoint's answer
// is recorded through xrr, which redacts the access_token field, so the
// daemon holds the redacted copy when it calls the Bot Connector; the
// sandbox takes it for the stand-in token, and still refuses a real token.
func TestConnectorAcceptsTheTokenAsXRRHandsItOver(t *testing.T) {
	s := newTeams(t, 3)
	f := &fakeTeams{}
	rt := s.Transport(f)
	for _, token := range []string{TeamsStandInToken, "<redacted:ACCESS_TOKEN>"} {
		if _, _, err := teamsDoAs(t, rt, http.MethodPost, "https://"+TeamsServiceHost+"/teams/v3/conversations", token, tmCreate()); err != nil {
			t.Errorf("token %q: %v", token, err)
		}
	}
	for _, token := range []string{tmToken, s.Token, "<redacted:CLIENT_SECRET>", ""} {
		if _, _, err := teamsDoAs(t, rt, http.MethodPost, "https://"+TeamsServiceHost+"/teams/v3/conversations", token, tmCreate()); err == nil {
			t.Errorf("token %q was accepted", token)
		}
	}
}

// A bot whose app proves itself with a certificate mints with a signed
// client assertion. The recorded daemon holds a stand-in certificate and
// key for the placeholder bot; the sandbox answers that one request, and
// no other, as it does the stand-in secret's.

const tmAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

var tmEntraURL = "https://" + TeamsEntraHost + "/" + TeamsTenantID + "/oauth2/v2.0/token"

// tmStandIn is a sandbox that knows the stand-in certificate, and the key
// the recorded daemon signs with.
func tmStandIn(t *testing.T) (*TeamsSandbox, *rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	certPEM, keyPEM, err := NewTeamsStandInCertificate()
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(keyPEM)
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key := k.(*rsa.PrivateKey)
	s := newTeams(t, 3)
	if err := s.UseStandInCertificate(certPEM); err != nil {
		t.Fatal(err)
	}
	cb, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return s, key, cert
}

func tmThumb(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// tmAssertion signs an assertion as heo does; mod changes the claims or the
// header (nil values delete), and method the algorithm.
func tmAssertion(t *testing.T, key crypto.Signer, cert *x509.Certificate, mod ...func(claims jwt.MapClaims, header map[string]any)) string {
	t.Helper()
	now := time.Now()
	claims := jwt.MapClaims{
		"aud": tmEntraURL, "iss": TeamsAppID, "sub": TeamsAppID, "jti": fmt.Sprintf("jti-%d", now.UnixNano()),
		"nbf": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["x5t#S256"] = tmThumb(cert)
	for _, m := range mod {
		m(claims, tok.Header)
	}
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func tmCertMint(assertion string, mod ...func(url.Values)) url.Values {
	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {TeamsAppID}, "scope": {teamsScope},
		"client_assertion_type": {tmAssertionType}, "client_assertion": {assertion},
	}
	for _, m := range mod {
		m(form)
	}
	return form
}

// TestTeamsTokenRequestWithTheStandInCertificateIsAnsweredLocally: the
// placeholder bot's certificate grant, signed with the stand-in key for the
// stand-in certificate, is answered with the stand-in token and reaches
// nothing.
func TestTeamsTokenRequestWithTheStandInCertificateIsAnsweredLocally(t *testing.T) {
	spy := &fakeTeams{}
	s, key, cert := tmStandIn(t)
	live := &Env{Mode: xrr.ModeRecord, Teams: s}
	rt := live.forwarder(spy)
	resp, body, err := tmPostForm(rt, tmEntraURL, tmCertMint(tmAssertion(t, key, cert)))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("the placeholder bot's certificate mint = %v %v", resp, err)
	}
	var got struct {
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.TokenType != "Bearer" || got.ExpiresIn <= 0 || got.AccessToken != TeamsStandInToken {
		t.Fatalf("answer %q = %+v, %v, want a Bearer stand-in token with a lifetime", body, got, err)
	}
	if got := spy.sent(); len(got) != 0 {
		t.Errorf("a token request reached the network: %v", got)
	}
}

// TestTeamsTokenRequestWithACertificateAnswersOnlyTheStandIn: anything but
// the stand-in's own assertion for the placeholder bot is refused: another
// key, another certificate, another app or tenant, another algorithm, a
// long-lived or lapsed or unsigned assertion, a secret beside it, extra
// fields, and any certificate request at all in a sandbox that holds no
// stand-in.
func TestTeamsTokenRequestWithACertificateAnswersOnlyTheStandIn(t *testing.T) {
	s, key, cert := tmStandIn(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rt := s.TokenTransport()
	good := tmAssertion(t, key, cert)
	if _, _, err := tmPostForm(rt, tmEntraURL, tmCertMint(good)); err != nil {
		t.Fatalf("the stand-in's own assertion: %v", err)
	}
	type mod = func(jwt.MapClaims, map[string]any)
	for name, tc := range map[string]struct {
		form url.Values
		url  string
	}{
		"another key":        {form: tmCertMint(tmAssertion(t, other, cert))},
		"another thumbprint": {form: tmCertMint(tmAssertion(t, key, cert, func(_ jwt.MapClaims, h map[string]any) { h["x5t#S256"] = "AAAA" }))},
		"no thumbprint":      {form: tmCertMint(tmAssertion(t, key, cert, func(_ jwt.MapClaims, h map[string]any) { delete(h, "x5t#S256") }))},
		"another issuer":     {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) { c["iss"] = tmApp })))},
		"another subject":    {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) { c["sub"] = tmApp })))},
		"another audience": {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) {
			c["aud"] = "https://" + TeamsEntraHost + "/" + tmTenant + "/oauth2/v2.0/token"
		})))},
		"an assertion lapsed": {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			c["nbf"] = time.Now().Add(-2 * time.Hour).Unix()
		})))},
		"a day long": {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) {
			c["exp"] = time.Now().Add(24 * time.Hour).Unix()
		})))},
		"no expiry":              {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) { delete(c, "exp") })))},
		"no ID":                  {form: tmCertMint(tmAssertion(t, key, cert, mod(func(c jwt.MapClaims, _ map[string]any) { delete(c, "jti") })))},
		"not a JWT":              {form: tmCertMint("not.a.jwt")},
		"no assertion":           {form: tmCertMint("", func(v url.Values) { v.Del("client_assertion") })},
		"another assertion type": {form: tmCertMint(good, func(v url.Values) { v.Set("client_assertion_type", "urn:other") })},
		"a secret beside it":     {form: tmCertMint(good, func(v url.Values) { v.Set("client_secret", TeamsStandInSecret) })},
		"another app":            {form: tmCertMint(good, func(v url.Values) { v.Set("client_id", tmApp) })},
		"another scope":          {form: tmCertMint(good, func(v url.Values) { v.Set("scope", "https://graph.microsoft.com/.default") })},
		"another grant":          {form: tmCertMint(good, func(v url.Values) { v.Set("grant_type", "password") })},
		"an extra field":         {form: tmCertMint(good, func(v url.Values) { v.Set("username", "x") })},
		"another tenant":         {form: tmCertMint(good), url: "https://" + TeamsEntraHost + "/" + tmTenant + "/oauth2/v2.0/token"},
	} {
		u := tc.url
		if u == "" {
			u = tmEntraURL
		}
		if _, _, err := tmPostForm(rt, u, tc.form); err == nil {
			t.Errorf("%s: answered", name)
		}
	}
	// RSA-PSS is not RS256: refused, though the key is the right one.
	ps := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
		"aud": tmEntraURL, "iss": TeamsAppID, "sub": TeamsAppID, "jti": "x", "nbf": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
	})
	ps.Header["x5t#S256"] = tmThumb(cert)
	if signed, err := ps.SignedString(key); err != nil {
		t.Fatal(err)
	} else if _, _, err := tmPostForm(rt, tmEntraURL, tmCertMint(signed)); err == nil {
		t.Error("a PS256 assertion was answered")
	}
	// An HS256 assertion made with the certificate's public key as the secret
	// is the classic confusion: refused.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"aud": tmEntraURL, "iss": TeamsAppID, "sub": TeamsAppID, "jti": "x", "exp": time.Now().Add(time.Minute).Unix(),
	})
	hs.Header["x5t#S256"] = tmThumb(cert)
	signed, err := hs.SignedString(cert.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tmPostForm(rt, tmEntraURL, tmCertMint(signed)); err == nil {
		t.Error("an HMAC assertion was answered")
	}
	// A sandbox that was told no stand-in certificate answers no certificate.
	none := newTeams(t, 3).TokenTransport()
	if _, _, err := tmPostForm(none, tmEntraURL, tmCertMint(good)); err == nil {
		t.Error("a sandbox with no stand-in certificate answered an assertion")
	}
	// The stand-in secret is still answered, and a secret request still needs
	// its own fields.
	u, form := tmMint()
	if _, _, err := tmPostForm(rt, u, form); err != nil {
		t.Errorf("the stand-in secret's request: %v", err)
	}
}

// TestTeamsStandInCertificate: a stand-in certificate is an RSA key and a
// self-signed certificate for the placeholder bot, naming nobody, valid far
// ahead; the sandbox takes only a certificate (PEM) and hands the child its
// public part, never the key.
func TestTeamsStandInCertificate(t *testing.T) {
	certPEM, keyPEM, err := NewTeamsStandInCertificate()
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cert.PublicKey.(*rsa.PublicKey); !ok || cert.NotAfter.Before(time.Now().Add(365*24*time.Hour)) || !strings.Contains(cert.Subject.CommonName, "heo-adapter-test") {
		t.Errorf("certificate = %v until %v, want an RSA one for the placeholder valid for a year or more", cert.Subject, cert.NotAfter)
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("the pair does not load: %v", err)
	}
	other, _, _ := NewTeamsStandInCertificate()
	if string(other) == string(certPEM) {
		t.Error("two stand-ins are the same certificate")
	}

	s := newTeams(t, 3)
	for name, in := range map[string][]byte{"a key": keyPEM, "garbage": []byte("nope"), "nothing": nil} {
		if err := s.UseStandInCertificate(in); err == nil {
			t.Errorf("%s was taken for a certificate", name)
		}
	}
	if err := s.UseStandInCertificate(certPEM); err != nil {
		t.Fatal(err)
	}
	var carried string
	for _, kv := range s.ChildEnv() {
		if v, ok := strings.CutPrefix(kv, EnvTeamsStandInCertificate+"="); ok {
			carried = v
		}
		if strings.Contains(kv, "PRIVATE") || strings.Contains(kv, string(keyPEM)) {
			t.Errorf("the child is told something of the key: %.30s", kv)
		}
	}
	raw, err := base64.StdEncoding.DecodeString(carried)
	if err != nil || string(raw) != string(cb.Bytes) {
		t.Errorf("the child is told %q, want the certificate's DER in base64", carried)
	}
	// A recorded process built from that environment answers the same stand-in.
	env := map[string]string{}
	for _, kv := range s.ChildEnv() {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	got, err := TeamsSandboxFromEnv(func(k string) string { return env[k] })
	if err == nil && got.standIn == nil {
		t.Error("the child's sandbox holds no stand-in certificate")
	}
	env[EnvTeamsStandInCertificate] = "bm90IGEgY2VydA=="
	if _, err := TeamsSandboxFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("an environment with a certificate that is none was taken")
	}
}

// TestNormalizeTeamsTokenRequest: the recorded form of a certificate
// mint is the same on every run: the assertion, which holds a time and an
// ID, is a placeholder, so a cassette recorded once replays on any run and
// holds no signed token.
func TestNormalizeTeamsTokenRequest(t *testing.T) {
	s, key, cert := tmStandIn(t)
	_ = s
	a, b := tmCertMint(tmAssertion(t, key, cert)), tmCertMint(tmAssertion(t, key, cert, func(c jwt.MapClaims, _ map[string]any) { c["jti"] = "another" }))
	na, nb := normalizeTeamsTokenBody([]byte(a.Encode())), normalizeTeamsTokenBody([]byte(b.Encode()))
	if string(na) != string(nb) {
		t.Errorf("two mints normalize differently:\n%s\n%s", na, nb)
	}
	if strings.Contains(string(na), "eyJ") || !strings.Contains(string(na), "client_assertion=") {
		t.Errorf("normalized = %s, want the assertion replaced", na)
	}
	// A secret mint is left as the sandbox's own form.
	_, sec := tmMint()
	if got := string(normalizeTeamsTokenBody([]byte(sec.Encode()))); got != sec.Encode() {
		t.Errorf("a secret mint normalized to %s, want it unchanged", got)
	}
	if got := normalizeTeamsTokenBody([]byte("not a form %zz")); string(got) != "not a form %zz" {
		t.Errorf("an unreadable body = %q, want it unchanged", got)
	}
}
