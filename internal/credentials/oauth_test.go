package credentials

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/profile"
	kitsecret "hop.top/kit/go/storage/secret"
	"hop.top/kit/go/storage/secret/agefile"
	"hop.top/kit/go/storage/secret/oauth"
	umasecret "hop.top/uma/contrib/kit/secret"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// idp is a token endpoint. Every access token it issues carries seed,
// so a leak of one is a failed noSeed.
type idp struct {
	srv *httptest.Server

	mu      sync.Mutex
	hits    int
	forms   []url.Values
	rotate  bool   // a refresh_token grant answers with a new refresh token
	rt      string // the refresh token it accepts now
	static  bool   // every call issues the same access token
	status  int    // non-zero: answer this status with errBody
	errBody string
	delay   time.Duration
	expires int
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	i := &idp{rt: seed + "-RT-0", expires: 3600}
	i.srv = httptest.NewServer(http.HandlerFunc(i.serve))
	t.Cleanup(i.srv.Close)
	return i
}

func (i *idp) url() string { return i.srv.URL + "/token" }

func (i *idp) calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.hits
}

func (i *idp) form(n int) url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.forms[n]
}

func (i *idp) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	i.mu.Lock()
	i.hits++
	n := i.hits
	i.forms = append(i.forms, r.PostForm)
	delay, status, body, rotate, static, exp := i.delay, i.status, i.errBody, i.rotate, i.static, i.expires
	wantRT := i.rt
	i.mu.Unlock()
	time.Sleep(delay)
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
		return
	}
	reply := map[string]any{"access_token": fmt.Sprintf("%s-AT-%d", seed, n), "token_type": "Bearer", "expires_in": exp}
	if static {
		reply["access_token"] = seed + "-AT-static"
	}
	if r.PostForm.Get("grant_type") == "refresh_token" {
		if r.PostForm.Get("refresh_token") != wantRT {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if rotate {
			i.mu.Lock()
			i.rt = fmt.Sprintf("%s-RT-%d", seed, n)
			reply["refresh_token"] = i.rt
			i.mu.Unlock()
		}
	}
	_ = json.NewEncoder(w).Encode(reply)
}

// clock is a test clock running ahead of the real one by off.
type clock struct{ off atomic.Int64 }

func (c *clock) now() time.Time { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) advance(d time.Duration) {
	c.off.Store(int64(d))
}

func secretEnv(v string) []byte {
	b, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
	return b
}

func grantEnv(t *testing.T, g oauth.Grant) []byte {
	t.Helper()
	b, err := oauth.EncodeGrant(g)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func rsaPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func clientCertEnv(t *testing.T) []byte {
	t.Helper()
	k, keyPEM := rsaPEM(t)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "heo-test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"v": 1, "kind": "client_cert",
		"certificate_pem": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"private_key_pem": keyPEM,
	})
	return b
}

func serviceAccountEnv(t *testing.T) []byte {
	t.Helper()
	_, keyPEM := rsaPEM(t)
	sa, _ := json.Marshal(map[string]string{"client_email": "sa@example.iam", "private_key": keyPEM, "private_key_id": "k1"})
	return secretEnv(string(sa))
}

// oauthBackend opens a native profile on the file backend, whose
// clock the test drives, with token requests sent to the test's idp.
func oauthBackend(t *testing.T, clk *clock) *Backend {
	t.Helper()
	cfg := fileConfig(t)
	b, err := Open(context.Background(), Options{
		Profile: native, Tenant: model.TenantID(native.Dir()), Config: cfg, Now: clk.now,
		OAuth: OAuthOptions{LockDir: t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// put writes value under the kit key of ref.
func put(t *testing.T, b *Backend, ref string, value []byte) string {
	t.Helper()
	key, err := b.Key(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SurfaceTokens().Set(context.Background(), key, value); err != nil {
		t.Fatal(err)
	}
	return key
}

var acceptOAuth = []ports.CredentialKind{ports.KindOAuth2}

func oreq(ref string, fresh bool) Request {
	return Request{Reference: ref, Purpose: "gmail", Accept: acceptOAuth, Fresh: fresh}
}

func token(t *testing.T, c ports.Credential, err error) ports.OAuth2 {
	t.Helper()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	o, ok := c.(ports.OAuth2)
	if !ok {
		t.Fatalf("credential is %T, want ports.OAuth2", c)
	}
	return o
}

// seedGrant stores a grant of kind at ref, with the secrets it names,
// and returns its key.
func seedGrant(t *testing.T, b *Backend, i *idp, kind oauth.GrantKind, ref string) string {
	t.Helper()
	g := oauth.Grant{Kind: kind, TokenURL: i.url(), ClientID: "heo-client", Scopes: []string{"mail.read"}}
	put(t, b, "cs", secretEnv(seed+"-client-secret"))
	put(t, b, "rt", secretEnv(i.rt))
	switch kind {
	case oauth.ClientCredentials:
		g.SecretRef = "uma/default/cs"
	case oauth.RefreshToken:
		g.SecretRef = "uma/default/rt"
		g.ClientSecretRef = "uma/default/cs"
	case oauth.JWTBearer:
		g.ClientID, g.Subject = "", "boss@example.com"
		g.SecretRef = "uma/default/sa"
		put(t, b, "sa", serviceAccountEnv(t))
	case oauth.ClientAssertion:
		g.ClientCertRef = "uma/default/cert"
		put(t, b, "cert", clientCertEnv(t))
	}
	return put(t, b, ref, grantEnv(t, g))
}

// TestGrantKindsMintAccessTokens: an entry holding a grant of each kind
// resolves to an oauth2 credential carrying the access token the token
// endpoint issued and its expiry, the second call reusing it.
func TestGrantKindsMintAccessTokens(t *testing.T) {
	for _, tc := range []struct {
		kind      oauth.GrantKind
		grantType string
	}{
		{oauth.ClientCredentials, "client_credentials"},
		{oauth.RefreshToken, "refresh_token"},
		{oauth.JWTBearer, "urn:ietf:params:oauth:grant-type:jwt-bearer"},
		{oauth.ClientAssertion, "client_credentials"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			ctx := context.Background()
			i, clk := newIDP(t), &clock{}
			b := oauthBackend(t, clk)
			seedGrant(t, b, i, tc.kind, "gmail-main")
			c, err := b.Resolve(ctx, oreq("gmail-main", false))
			o := token(t, c, err)
			if o.AccessToken != seed+"-AT-1" {
				t.Errorf("access token = %q", o.AccessToken)
			}
			if d := time.Until(o.ExpiresAt); d < 3500*time.Second || d > 3601*time.Second {
				t.Errorf("expires in %v, want about an hour", d)
			}
			if got := i.form(0).Get("grant_type"); got != tc.grantType {
				t.Errorf("grant_type = %q, want %q", got, tc.grantType)
			}
			if tc.kind == oauth.ClientAssertion && i.form(0).Get("client_assertion") == "" {
				t.Error("no client_assertion sent")
			}
			c, err = b.Resolve(ctx, oreq("gmail-main", false))
			if o2 := token(t, c, err); o2.AccessToken != o.AccessToken || i.calls() != 1 {
				t.Errorf("second resolve: token %q, %d token requests; want the cached token, 1 request", o2.AccessToken, i.calls())
			}
		})
	}
}

// TestRefreshTokenRotationIsWrittenBack: a provider that rotates the
// refresh token has the new one stored before the old one is used
// again, and the cached access token is stored as a uma oauth2
// envelope another process reads without a request.
func TestRefreshTokenRotationIsWrittenBack(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	i.rotate = true
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.RefreshToken, "gmail-main")

	c, err := b.Resolve(ctx, oreq("gmail-main", false))
	first := token(t, c, err)
	sec, err := b.SurfaceTokens().Get(ctx, "uma/default/rt")
	if err != nil || !strings.Contains(string(sec.Value), seed+"-RT-1") {
		t.Fatalf("refresh token entry after rotation = %s, %v; want the rotated token", sec.Value, err)
	}

	// The cached token is a uma oauth2 envelope, readable through uma.
	all, _ := b.SurfaceTokens().List(ctx, "")
	var keys []string
	for _, k := range all {
		if strings.HasPrefix(k, "uma/default/gmail-main.token.") {
			keys = append(keys, k)
		}
	}
	if len(keys) != 1 {
		t.Fatalf("cached token keys = %v", keys)
	}
	via, err := umasecret.New(b.SurfaceTokens(), umasecret.WithKeyFunc(func(ports.CredentialRequest) (string, error) { return keys[0], nil }))
	if err != nil {
		t.Fatal(err)
	}
	uc, err := via.Resolve(ctx, ports.CredentialRequest{Channel: model.ChannelRef{Tenant: "default"}, Reference: "x", Accept: acceptOAuth})
	if got := token(t, uc, err); got.AccessToken != first.AccessToken || !got.ExpiresAt.Equal(first.ExpiresAt) {
		t.Errorf("uma reads the cached token as %+v, want %+v", got, first)
	}

	// Another process on the same store reuses the cache, no request.
	b2 := oauthBackend2(t, b, clk)
	c, err = b2.Resolve(ctx, oreq("gmail-main", false))
	if got := token(t, c, err); got.AccessToken != first.AccessToken || i.calls() != 1 {
		t.Errorf("second backend: %q after %d requests, want the cached token after 1", got.AccessToken, i.calls())
	}

	// Past expiry the stored (rotated) refresh token is exchanged.
	clk.advance(2 * time.Hour)
	c, err = b.Resolve(ctx, oreq("gmail-main", false))
	second := token(t, c, err)
	if second.AccessToken == first.AccessToken || i.calls() != 2 {
		t.Fatalf("after expiry: %q after %d requests", second.AccessToken, i.calls())
	}
	// The new token's expiry is stamped from the shifted clock.
	if d := second.ExpiresAt.Sub(clk.now()); d < 3500*time.Second || d > 3601*time.Second {
		t.Errorf("second token expires %v after the shifted clock, want about an hour", d)
	}
	if got := i.form(1).Get("refresh_token"); got != seed+"-RT-1" {
		t.Errorf("second exchange sent %q, want the rotated token", got)
	}
	sec, _ = b.SurfaceTokens().Get(ctx, "uma/default/rt")
	if !strings.Contains(string(sec.Value), seed+"-RT-2") {
		t.Errorf("refresh token entry = %s, want the second rotation", sec.Value)
	}
}

// oauthBackend2 opens a second backend on b's file store, as another
// process would.
func oauthBackend2(t *testing.T, b *Backend, clk *clock) *Backend {
	t.Helper()
	b2 := oauthBackend(t, clk)
	b2.kit, b2.store = b.kit, b.kit
	return b2
}

// TestSkewWindow: a cached token is reused until its expiry is inside
// the skew, then refreshed.
func TestSkewWindow(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")
	if _, err := b.Resolve(ctx, oreq("gmail-main", false)); err != nil {
		t.Fatal(err)
	}
	// 3600s lifetime, default 5 minute skew: stale from 3300s on.
	clk.advance(3290 * time.Second)
	if _, err := b.Resolve(ctx, oreq("gmail-main", false)); err != nil || i.calls() != 1 {
		t.Fatalf("10s before the skew window: %v, %d requests, want the cached token", err, i.calls())
	}
	clk.advance(3310 * time.Second)
	if _, err := b.Resolve(ctx, oreq("gmail-main", false)); err != nil || i.calls() != 2 {
		t.Fatalf("10s inside the skew window: %v, %d requests, want a refresh", err, i.calls())
	}
}

// TestConcurrentResolvesRefreshOnce: callers resolving the same grant
// at once share one token request.
func TestConcurrentResolvesRefreshOnce(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	i.delay = 100 * time.Millisecond
	i.rotate = true
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.RefreshToken, "gmail-main")
	var wg sync.WaitGroup
	toks := make([]string, 16)
	for n := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := b.Resolve(ctx, oreq("gmail-main", false))
			if err == nil {
				toks[n] = c.(ports.OAuth2).AccessToken
			}
		}()
	}
	wg.Wait()
	if i.calls() != 1 {
		t.Errorf("%d token requests for 16 concurrent resolves, want 1", i.calls())
	}
	for n, tok := range toks {
		if tok != toks[0] || tok == "" {
			t.Fatalf("resolve %d got %q, want %q", n, tok, toks[0])
		}
	}
}

// TestFreshInvalidatesOnce: a Fresh request after the provider
// rejected the token served drops it once and serves a new one; when
// the endpoint can only issue the rejected token again, it is refused
// as unauthorized after that one drop.
func TestFreshInvalidatesOnce(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")

	c, err := b.Resolve(ctx, oreq("gmail-main", false))
	first := token(t, c, err)
	c, err = b.Resolve(ctx, oreq("gmail-main", true))
	second := token(t, c, err)
	if second.AccessToken == first.AccessToken || i.calls() != 2 {
		t.Fatalf("fresh: %q then %q after %d requests, want a new token after exactly 2", first.AccessToken, second.AccessToken, i.calls())
	}
	// An endpoint that reissues the same token.
	i2 := newIDP(t)
	i2.static = true
	b2 := oauthBackend(t, clk)
	seedGrant(t, b2, i2, oauth.ClientCredentials, "gmail-main")
	if _, err := b2.Resolve(ctx, oreq("gmail-main", false)); err != nil {
		t.Fatal(err)
	}
	_, err = b2.Resolve(ctx, oreq("gmail-main", true))
	if !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("fresh with an unchanged token: %v, want unauthorized", err)
	}
	if i2.calls() != 2 {
		t.Errorf("%d token requests, want 1 to serve + exactly 1 after the drop", i2.calls())
	}
	d := b2.Check(ctx, oreq("gmail-main", false))
	if d.Status != StatusRejected || d.RejectedAt == nil {
		t.Errorf("check after the rejection: %+v", d)
	}
}

// TestFreshAfterOtherCallerRefreshedDropsNothing: when the token
// served last is no longer the current one, a Fresh request serves the
// current one without another request.
func TestFreshAfterOtherCallerRefreshedDropsNothing(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")
	c, err := b.Resolve(ctx, oreq("gmail-main", false))
	first := token(t, c, err)
	// The provider rejected it; two callers ask fresh.
	var wg sync.WaitGroup
	out := make([]string, 2)
	for n := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := b.Resolve(ctx, oreq("gmail-main", true))
			if err == nil {
				out[n] = c.(ports.OAuth2).AccessToken
			} else {
				t.Errorf("fresh caller %d: %v", n, err)
			}
		}()
	}
	wg.Wait()
	for _, tok := range out {
		if tok == "" || tok == first.AccessToken {
			t.Errorf("fresh callers got %q, want a new token", tok)
		}
	}
	if i.calls() > 3 {
		t.Errorf("%d token requests for two fresh callers", i.calls())
	}
}

// TestStoredCredentialStillResolves: an entry holding a credential
// envelope, a static oauth2 token included, resolves as before and
// never reaches a token endpoint.
func TestStoredCredentialStillResolves(t *testing.T) {
	ctx := context.Background()
	b := oauthBackend(t, &clock{})
	put(t, b, "static", []byte(`{"v":1,"kind":"oauth2","access_token":"`+seed+`","expires_at":"2099-01-01T00:00:00Z"}`))
	c, err := b.Resolve(ctx, oreq("static", false))
	if o := token(t, c, err); o.AccessToken != seed {
		t.Errorf("static token = %q", o.AccessToken)
	}
	if d := b.Check(ctx, oreq("static", false)); d.Status != StatusOK || d.GrantKind != "" {
		t.Errorf("static check: %+v", d)
	}
}

// TestGrantReplacedIsUsed: a grant stored anew (other scopes) is used
// on the next call, not the one the backend first read.
func TestGrantReplacedIsUsed(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	key := seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")
	if _, err := b.Resolve(ctx, oreq("gmail-main", false)); err != nil {
		t.Fatal(err)
	}
	g := oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "heo-client", Scopes: []string{"mail.send"}, SecretRef: "uma/default/cs"} //nolint:gosec // fixture grant, not a credential
	if err := b.SurfaceTokens().Set(ctx, key, grantEnv(t, g)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resolve(ctx, oreq("gmail-main", false)); err != nil {
		t.Fatal(err)
	}
	if i.calls() != 2 || i.form(1).Get("scope") != "mail.send" {
		t.Errorf("%d requests, last scope %q; want a new token for the new grant", i.calls(), i.form(1).Get("scope"))
	}
}

// TestGrantFailuresMapAndNeverLeak: each way a grant fails is one uma
// error and diagnosis status, with fixed text.
func TestGrantFailuresMapAndNeverLeak(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		setup  func(*idp)
		code   model.ErrorCode
		status Status
	}{
		{"consent withdrawn", func(i *idp) { i.status, i.errBody = 400, `{"error":"invalid_grant","error_description":"`+seed+`"}` }, model.Unauthorized, StatusRejected},
		{"client refused", func(i *idp) { i.status, i.errBody = 401, `{"error":"invalid_client"}` }, model.Unauthorized, StatusRejected},
		{"endpoint down", func(i *idp) { i.status, i.errBody = 503, seed }, model.Transient, StatusUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, clk := newIDP(t), &clock{}
			tc.setup(i)
			b := oauthBackend(t, clk)
			seedGrant(t, b, i, oauth.RefreshToken, "gmail-main")
			_, err := b.Resolve(ctx, oreq("gmail-main", false))
			if !model.IsCode(err, tc.code) {
				t.Fatalf("resolve: %v, want %s", err, tc.code)
			}
			noSeed(t, "error", err)
			d := b.Check(ctx, oreq("gmail-main", false))
			if d.Status != tc.status || d.GrantKind != "refresh_token" {
				t.Errorf("diagnosis: %+v, want %s", d, tc.status)
			}
			noSeed(t, "diagnosis", d)
		})
	}

	t.Run("malformed grant", func(t *testing.T) {
		b := oauthBackend(t, &clock{})
		put(t, b, "bad", []byte(`{"v":1,"kind":"refresh_token","token_url":"https://example.com/t","secret_ref":"`+seed+`"}`))
		_, err := b.Resolve(ctx, oreq("bad", false))
		if !model.IsCode(err, model.Permanent) {
			t.Fatalf("resolve: %v, want permanent", err)
		}
		d := b.Check(ctx, oreq("bad", false))
		if d.Status != StatusMalformed {
			t.Errorf("diagnosis: %+v", d)
		}
		noSeed(t, "error", err)
		noSeed(t, "diagnosis", d)
	})

	t.Run("secret the grant names is missing", func(t *testing.T) {
		i := newIDP(t)
		b := oauthBackend(t, &clock{})
		g := oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "c", SecretRef: "uma/default/nope"}
		put(t, b, "g", grantEnv(t, g))
		d := b.Check(ctx, oreq("g", false))
		if d.Status != StatusMalformed || i.calls() != 0 {
			t.Errorf("diagnosis: %+v after %d requests", d, i.calls())
		}
	})

	t.Run("kind not accepted", func(t *testing.T) {
		i := newIDP(t)
		b := oauthBackend(t, &clock{})
		seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")
		_, err := b.Resolve(ctx, Request{Reference: "gmail-main", Purpose: "imap", Accept: acceptPassword})
		if !model.IsCode(err, model.Unsupported) || i.calls() != 0 {
			t.Errorf("resolve: %v after %d requests, want unsupported with no request", err, i.calls())
		}
	})
}

// TestDiagnosisOfAGrant: grant kind and the expiry of the access
// token, and no token material.
func TestDiagnosisOfAGrant(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	i.rotate = true
	b := oauthBackend(t, clk)
	seedGrant(t, b, i, oauth.RefreshToken, "gmail-main")
	d := b.Check(ctx, oreq("gmail-main", false))
	if d.Status != StatusOK || d.Kind != ports.KindOAuth2 || d.GrantKind != "refresh_token" || d.ExpiresAt == nil {
		t.Fatalf("diagnosis: %+v", d)
	}
	if dur := d.ExpiresAt.Sub(clk.now()); dur < 3500*time.Second {
		t.Errorf("expires at %v, want about an hour from now", d.ExpiresAt)
	}
	noSeed(t, "diagnosis", d)
	out, _ := json.Marshal(d)
	for _, s := range []string{seed, "AT-", "RT-"} {
		if strings.Contains(string(out), s) {
			t.Errorf("diagnosis JSON carries %q: %s", s, out)
		}
	}
}

// readOnly hides everything but reads of a store.
type readOnly struct{ s kitsecret.Store }

func (r readOnly) Get(ctx context.Context, k string) (*kitsecret.Secret, error) {
	return r.s.Get(ctx, k)
}
func (r readOnly) List(ctx context.Context, p string) ([]string, error) { return r.s.List(ctx, p) }
func (r readOnly) Exists(ctx context.Context, k string) (bool, error)   { return r.s.Exists(ctx, k) }

func TestAgefileIsNotWritable(t *testing.T) {
	if oauth.IsWritable(agefile.New("/x", "/y")) {
		t.Error("agefile declares itself writable")
	}
	if !oauth.IsWritable(newFileStore(t.TempDir())) {
		t.Error("the file backend does not declare itself writable")
	}
}

// readOnlyBackend is a native profile whose backend cannot be written,
// as agefile: the store is the file store, hidden behind reads.
func readOnlyBackend(t *testing.T, clk *clock) *Backend {
	t.Helper()
	b := oauthBackend(t, clk)
	b.name, b.store = BackendAgefile, readOnly{b.kit}
	return b
}

// TestRefreshTokenGrantRefusedOnReadOnlyBackends: agefile and aps
// profiles cannot hold a rotated refresh token; the refusal names the
// writable backends, before any request is made.
func TestRefreshTokenGrantRefusedOnReadOnlyBackends(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}

	age := readOnlyBackend(t, clk)
	seedGrantVia(t, age, i, oauth.RefreshToken, "gmail-main")

	apsRef := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	g := grantEnv(t, oauth.Grant{Kind: oauth.RefreshToken, TokenURL: i.url(), ClientID: "c", SecretRef: "RT"})
	apsProfile(t, "noor", "GMAIL_MAIN='"+string(g)+"'\nRT='"+string(secretEnv(i.rt))+"'\n")
	aps, err := Open(ctx, Options{Profile: apsRef, Tenant: model.TenantID(apsRef.Dir()), Config: fileConfig(t), OAuth: OAuthOptions{LockDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		b   *Backend
		ref string
	}{"agefile": {age, "gmail-main"}, "aps": {aps, "GMAIL_MAIN"}} {
		t.Run(name, func(t *testing.T) {
			r := tc.b.RefuseGrant(ctx, oreq(tc.ref, false))
			if r == nil || r.Grant != "refresh_token" {
				t.Fatalf("RefuseGrant = %+v", r)
			}
			for _, want := range []string{"writable", "keyring", "file", "openbao"} {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message %q lacks %q", r.Message, want)
				}
			}
			if strings.Contains(r.Message, "agefile, ") {
				t.Errorf("message %q offers agefile as writable", r.Message)
			}
			_, err := tc.b.Resolve(ctx, oreq(tc.ref, false))
			if !model.IsCode(err, model.Unsupported) {
				t.Errorf("resolve: %v, want unsupported", err)
			}
			if d := tc.b.Check(ctx, oreq(tc.ref, false)); d.Status != StatusUnsupported || !strings.Contains(d.Message, "writable") {
				t.Errorf("diagnosis: %+v", d)
			}
			noSeed(t, "refusal", r)
		})
	}
	if i.calls() != 0 {
		t.Errorf("%d token requests; a refused grant must send none", i.calls())
	}

	// A writable backend is not refused.
	nat := oauthBackend(t, clk)
	seedGrant(t, nat, i, oauth.RefreshToken, "gmail-main")
	if r := nat.RefuseGrant(ctx, oreq("gmail-main", false)); r != nil {
		t.Errorf("file backend refused: %+v", r)
	}
	// Nor an entry that is no grant, or a grant of a kind that does
	// not rotate.
	put(t, nat, "pw", envelope(t, "x"))
	if r := nat.RefuseGrant(ctx, oreq("pw", false)); r != nil {
		t.Errorf("password refused: %+v", r)
	}
	if r := nat.RefuseGrant(ctx, oreq("missing", false)); r != nil {
		t.Errorf("missing refused: %+v", r)
	}
}

// seedGrantVia is seedGrant for a backend whose store is read-only:
// it writes through the backend's own mutable store.
func seedGrantVia(t *testing.T, b *Backend, i *idp, kind oauth.GrantKind, ref string) {
	t.Helper()
	seedGrant(t, b, i, kind, ref)
}

// TestReadOnlyBackendsCacheInMemory: a grant that does not rotate is
// served on a backend that cannot be written, the access token held
// in memory only: one request for repeated calls, and nothing written.
func TestReadOnlyBackendsCacheInMemory(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}

	age := readOnlyBackend(t, clk)
	seedGrantVia(t, age, i, oauth.ClientCredentials, "gmail-main")
	before, _ := age.kit.List(ctx, "")

	apsRef := profile.Ref{Scheme: profile.ApsScheme, ID: "noor"}
	g := grantEnv(t, oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "c", SecretRef: "CS"})
	path := apsProfile(t, "noor", "GMAIL_MAIN='"+string(g)+"'\nCS='"+string(secretEnv(seed))+"'\n")
	aps, err := Open(ctx, Options{Profile: apsRef, Tenant: model.TenantID(apsRef.Dir()), Config: fileConfig(t), Now: clk.now, OAuth: OAuthOptions{LockDir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	apsBefore, _ := os.ReadFile(path) //nolint:gosec // the test's own temp file

	for name, tc := range map[string]struct {
		b   *Backend
		ref string
	}{"agefile": {age, "gmail-main"}, "aps": {aps, "GMAIL_MAIN"}} {
		t.Run(name, func(t *testing.T) {
			start := i.calls()
			c, err := tc.b.Resolve(ctx, oreq(tc.ref, false))
			first := token(t, c, err)
			c, err = tc.b.Resolve(ctx, oreq(tc.ref, false))
			if second := token(t, c, err); second.AccessToken != first.AccessToken || i.calls() != start+1 {
				t.Errorf("%q then %q after %d requests, want the in-memory token after 1", first.AccessToken, second.AccessToken, i.calls()-start)
			}
			c, err = tc.b.Resolve(ctx, oreq(tc.ref, true))
			if third := token(t, c, err); third.AccessToken == first.AccessToken || i.calls() != start+2 {
				t.Errorf("fresh: %q after %d requests, want a new token after 2", third.AccessToken, i.calls()-start)
			}
		})
	}
	after, _ := age.kit.List(ctx, "")
	if len(after) != len(before) {
		t.Errorf("the read-only backend's store changed: %v -> %v", before, after)
	}
	if got, _ := os.ReadFile(path); string(got) != string(apsBefore) { //nolint:gosec // the test's own temp file
		t.Errorf("aps secrets changed:\n%s", got)
	}
}

// TestGrantCarriesSASLIdentity: the User and AuthzID of the oauth2
// envelope cached for a grant reach the credential, so IMAP XOAUTH2
// can name the mailbox; a token heo mints itself carries none.
func TestGrantCarriesSASLIdentity(t *testing.T) {
	ctx := context.Background()
	i, clk := newIDP(t), &clock{}
	b := oauthBackend(t, clk)
	key := seedGrant(t, b, i, oauth.ClientCredentials, "gmail-main")
	g, err := oauth.DecodeGrant(grantEnv(t, oauth.Grant{Kind: oauth.ClientCredentials, TokenURL: i.url(), ClientID: "heo-client", //nolint:gosec // fixture grant, not a credential
		Scopes: []string{"mail.read"}, SecretRef: "uma/default/cs"}))
	if err != nil {
		t.Fatal(err)
	}
	env, err := oauth.EncodeCachedToken(oauth.CachedToken{
		Token: oauth.Token{AccessToken: seed + "-AT-cached", ExpiresAt: clk.now().Add(time.Hour)},
		User:  "support@example.com", AuthzID: "shared@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SurfaceTokens().Set(ctx, oauth.TokenKey(key, g), env); err != nil {
		t.Fatal(err)
	}
	c, err := b.Resolve(ctx, oreq("gmail-main", false))
	o := token(t, c, err)
	if o.AccessToken != seed+"-AT-cached" || o.User != "support@example.com" || o.AuthzID != "shared@example.com" {
		t.Errorf("credential = user %q authzid %q token %q; want the cached identity and token", o.User, o.AuthzID, o.AccessToken)
	}
	if i.calls() != 0 {
		t.Errorf("%d token requests, want none for a fresh cached token", i.calls())
	}

	// A token heo mints itself has no identity.
	i2 := newIDP(t)
	b2 := oauthBackend(t, &clock{})
	seedGrant(t, b2, i2, oauth.ClientCredentials, "gmail-main")
	c, err = b2.Resolve(ctx, oreq("gmail-main", false))
	if o := token(t, c, err); o.User != "" || o.AuthzID != "" {
		t.Errorf("minted token carries identity %q/%q", o.User, o.AuthzID)
	}
}
