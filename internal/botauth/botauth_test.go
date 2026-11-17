package botauth_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/botauth"
	"hop.top/uma/model"
)

const (
	tenantA = "1111aaaa-1111-4111-8111-11111111bbbb"
	appA    = "2222cccc-2222-4222-8222-22222222dddd"
	tenantB = "3333eeee-3333-4333-8333-33333333ffff"
	appB    = "4444aaaa-4444-4444-8444-44444444bbbb"

	secretA = "client-secret-of-bot-a~Zx9"
	secretB = "client-secret-of-bot-b~Qw3"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// mintRequest is one request the fake endpoint saw.
type mintRequest struct {
	Path          string
	Method        string
	ContentType   string
	Authorization string
	Form          url.Values
}

// fakeEntra is a token endpoint of our own: Microsoft Entra's documented
// client credentials answers, for any number of tenants and apps. Nothing
// reaches the network.
type fakeEntra struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// secrets is the client secret each "<tenant>/<app>" accepts.
	secrets map[string]string
	// script, when not empty, answers the next requests before anything else.
	script []func(w http.ResponseWriter)
	// ttl is the expires_in of an issued token.
	ttl      int
	requests []mintRequest
	issued   int
	// gate, when set, holds every request until it is closed.
	gate chan struct{}
	// echo makes an error description repeat the secret it was sent, as a
	// careless server might.
	echo bool
	// certs are the certificates each "<tenant>/<app>" has registered; now is
	// the clock a client assertion is judged by; jtis are the assertion IDs
	// seen, each good once; assertions are the ones that verified.
	certs      map[string][]*x509.Certificate
	now        func() time.Time
	jtis       map[string]bool
	assertions []seenAssertion
}

func newFakeEntra(t *testing.T) *fakeEntra {
	t.Helper()
	f := &fakeEntra{t: t, secrets: map[string]string{}, ttl: 3600, certs: map[string][]*x509.Certificate{}, jtis: map[string]bool{}, now: time.Now}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEntra) accept(tenant, app, secret string) {
	f.mu.Lock()
	f.secrets[tenant+"/"+app] = secret
	f.mu.Unlock()
}

func (f *fakeEntra) next(steps ...func(http.ResponseWriter)) {
	f.mu.Lock()
	f.script = append(f.script, steps...)
	f.mu.Unlock()
}

func (f *fakeEntra) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeEntra) seen() []mintRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mintRequest(nil), f.requests...)
}

func status(code int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { w.WriteHeader(code) }
}

func rateLimit(seconds int) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

func (f *fakeEntra) serve(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("fake Entra: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, mintRequest{
		Path: r.URL.Path, Method: r.Method, ContentType: r.Header.Get("Content-Type"),
		Authorization: r.Header.Get("Authorization"), Form: r.PostForm,
	})
	gate := f.gate
	var step func(http.ResponseWriter)
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if step != nil {
		step(w)
		return
	}
	tenant := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/oauth2/v2.0/token")
	app, secret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	assertion := r.PostForm.Get("client_assertion")
	f.mu.Lock()
	want, known := f.secrets[tenant+"/"+app]
	f.issued++
	n, ttl := f.issued, f.ttl
	echo := f.echo
	f.mu.Unlock()
	valid := known && want == secret
	desc := "AADSTS7000215: Invalid client secret provided. Ensure the secret being sent in the request is the client secret value. Trace ID: 0b0e0d0c Correlation ID: 1c2d3e4f Timestamp: 2026-10-14 09:00:00Z"
	if assertion != "" || r.PostForm.Get("client_assertion_type") != "" {
		valid = secret == "" && f.checkAssertion(tenant, app, "http://"+r.Host+r.URL.Path, r.PostForm)
		desc = "AADSTS700027: Client assertion contains an invalid signature. Trace ID: 0b0e0d0c Correlation ID: 1c2d3e4f Timestamp: 2026-10-14 09:00:00Z"
	}
	w.Header().Set("Content-Type", "application/json")
	if !valid || r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") != botauth.Scope {
		w.WriteHeader(http.StatusUnauthorized)
		if echo {
			desc += " (sent: " + secret + assertion + ")"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "invalid_client", "error_description": desc, "error_codes": []int{7000215},
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_type": "Bearer", "expires_in": ttl, "ext_expires_in": ttl,
		"access_token": fmt.Sprintf("access-token-%d-for-%s", n, tenant),
	})
}

// secretBox is the app secret the credential store holds, rotatable.
type secretBox struct {
	t        *testing.T
	mu       sync.Mutex
	v        string
	err      error
	reads    int
	rejected int
}

func (s *secretBox) set(v string) {
	s.mu.Lock()
	s.v = v
	s.mu.Unlock()
}

func (s *secretBox) read(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.v, s.err
}

func (s *secretBox) credential(ctx context.Context) (botauth.Credential, error) {
	v, err := s.read(ctx)
	return botauth.Credential{Secret: v}, err
}

func (s *secretBox) reject(_ context.Context, k botauth.Kind) {
	if k != botauth.KindSecret {
		s.t.Errorf("the store was told a %s was rejected, want a secret", k)
	}
	s.mu.Lock()
	s.rejected++
	s.mu.Unlock()
}

// capture is a logger that keeps what it is told.
type capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capture) Info(msg any, kv ...any)  { c.add("info", msg, kv) }
func (c *capture) Error(msg any, kv ...any) { c.add("error", msg, kv) }

func (c *capture) add(level string, msg any, kv []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.buf, "%s %v %v\n", level, msg, kv)
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

type rig struct {
	f      *fakeEntra
	clock  *fakeClock
	secret *secretBox
	log    *capture
	m      *botauth.Minter
}

func newRig(t *testing.T, mutate ...func(*botauth.Options)) *rig {
	t.Helper()
	r := &rig{f: newFakeEntra(t), clock: newClock(), secret: &secretBox{t: t, v: secretA}, log: &capture{}}
	r.f.accept(tenantA, appA, secretA)
	r.f.now = r.clock.Now
	o := botauth.Options{
		Tenant: tenantA, AppID: appA, Authority: r.f.srv.URL, HTTP: r.f.srv.Client(),
		Credential: r.secret.credential, Rejected: r.secret.reject, Now: r.clock.Now, Log: r.log,
	}
	for _, m := range mutate {
		m(&o)
	}
	m, err := botauth.New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.m = m
	return r
}

// TestMintsWithClientCredentials: the request is Entra's documented
// client credentials grant: a POST of a form to the tenant's v2.0 token
// endpoint with the app ID, the secret in the body and the Bot Framework
// scope, no Authorization header; the token's expiry is the clock's plus
// the endpoint's expires_in.
func TestMintsWithClientCredentials(t *testing.T) {
	r := newRig(t)
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
		t.Fatalf("%d requests, want 1 (no probe of a second authentication style)", len(reqs))
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/"+tenantA+"/oauth2/v2.0/token" || !strings.HasPrefix(q.ContentType, "application/x-www-form-urlencoded") {
		t.Errorf("request = %s %s (%s)", q.Method, q.Path, q.ContentType)
	}
	if q.Authorization != "" {
		t.Error("the secret went in a header; Entra takes it in the body")
	}
	for k, want := range map[string]string{
		"grant_type": "client_credentials", "client_id": appA, "client_secret": secretA, "scope": "https://api.botframework.com/.default",
	} {
		if got := q.Form.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
}

// TestCachedUntilSkew: a token is served from memory until it is within
// the skew of its expiry; then the next call mints again, before the old
// one expires, on the injected clock.
func TestCachedUntilSkew(t *testing.T) {
	r := newRig(t)
	first, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(54 * time.Minute) // 6 minutes left: outside the 5 minute skew
	again, err := r.m.Token(t.Context(), false)
	if err != nil || again != first || r.f.count() != 1 {
		t.Fatalf("at 54 minutes: %+v %v after %d requests, want the cached token, 1 request", again, err, r.f.count())
	}
	r.clock.Advance(90 * time.Second) // 4m30s left: inside the skew
	renewed, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Value == first.Value || r.f.count() != 2 {
		t.Errorf("inside the skew: %+v after %d requests, want a new token, 2 requests", renewed, r.f.count())
	}
	if !renewed.Expiry.After(first.Expiry) || !first.Expiry.After(r.clock.Now()) {
		t.Errorf("the new token expires %v, the old %v, now %v: the renewal came after the old one lapsed", renewed.Expiry, first.Expiry, r.clock.Now())
	}
}

// TestShortLivedTokenRenewedHalfWay: a token living less than twice the
// skew is not renewed on every call: it is replaced half way.
func TestShortLivedTokenRenewedHalfWay(t *testing.T) {
	r := newRig(t)
	r.f.ttl = 240 // under 2 x 5 minutes
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(100 * time.Second)
	if _, err := r.m.Token(t.Context(), false); err != nil || r.f.count() != 1 {
		t.Fatalf("at 100s of 240s: %v, %d requests, want the cached token", err, r.f.count())
	}
	r.clock.Advance(30 * time.Second)
	if _, err := r.m.Token(t.Context(), false); err != nil || r.f.count() != 2 {
		t.Fatalf("at 130s of 240s: %v, %d requests, want a renewal", err, r.f.count())
	}
}

// TestConcurrentCallersShareOneMint: callers that need a token while one
// is being minted wait for that mint rather than making their own.
func TestConcurrentCallersShareOneMint(t *testing.T) {
	r := newRig(t)
	r.f.gate = make(chan struct{})
	const callers = 25
	var wg sync.WaitGroup
	tokens := make([]string, callers)
	errs := make([]error, callers)
	for i := range callers {
		wg.Go(func() {
			tok, err := r.m.Token(t.Context(), false)
			tokens[i], errs[i] = tok.Value, err
		})
	}
	// Every caller is waiting on the held request when it is released.
	time.Sleep(200 * time.Millisecond)
	close(r.f.gate)
	wg.Wait()
	for i := range callers {
		if errs[i] != nil || tokens[i] != tokens[0] || tokens[i] == "" {
			t.Fatalf("caller %d = %q, %v; caller 0 = %q", i, tokens[i], errs[i], tokens[0])
		}
	}
	if n := r.f.count(); n != 1 {
		t.Errorf("%d requests for %d concurrent callers, want 1", n, callers)
	}
}

// TestCallerCancelDoesNotFailTheSharedMint: a caller that gives up
// returns at once with its own error; the mint it started finishes for
// the others.
func TestCallerCancelDoesNotFailTheSharedMint(t *testing.T) {
	r := newRig(t)
	r.f.gate = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := r.m.Token(ctx, false); done <- err }()
	for r.f.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	other := make(chan error, 1)
	go func() { _, err := r.m.Token(t.Context(), false); other <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the cancelled caller = %v, want context.Canceled", err)
	}
	close(r.f.gate)
	if err := <-other; err != nil {
		t.Errorf("the caller that stayed = %v, want the token the first caller's mint got", err)
	}
	if n := r.f.count(); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

// TestBackoffAndRecovery: a 5xx and a 429 fail the call as a retryable
// error; calls inside the wait fail at once without a request, the wait
// grows and honors Retry-After; once the endpoint answers, the token is
// served and nothing is held against it.
func TestBackoffAndRecovery(t *testing.T) {
	r := newRig(t)
	r.f.next(status(http.StatusServiceUnavailable), rateLimit(30))
	ctx := t.Context()

	_, err := r.m.Token(ctx, false)
	if me := (*model.Error)(nil); !errors.As(err, &me) || me.Code != model.Transient || me.Retry != model.RetrySameOperation {
		t.Fatalf("503 = %v, want a transient retryable error", err)
	}
	if _, err := r.m.Token(ctx, false); err == nil || r.f.count() != 1 {
		t.Fatalf("inside the wait: %v after %d requests, want the same failure, 1 request", err, r.f.count())
	}
	r.clock.Advance(botauth.DefaultBackoffMin + time.Second)
	_, err = r.m.Token(ctx, false)
	if me := (*model.Error)(nil); !errors.As(err, &me) || me.Code != model.RateLimited || me.RetryAfter != 30*time.Second {
		t.Fatalf("429 = %v, want rate_limited asking 30s", err)
	}
	if r.f.count() != 2 {
		t.Fatalf("%d requests, want 2", r.f.count())
	}
	r.clock.Advance(20 * time.Second) // past the doubled backoff, inside Retry-After
	if _, err := r.m.Token(ctx, false); err == nil || r.f.count() != 2 {
		t.Fatalf("inside Retry-After: %v after %d requests, want a failure with no request", err, r.f.count())
	}
	r.clock.Advance(11 * time.Second)
	tok, err := r.m.Token(ctx, false)
	if err != nil || tok.Value == "" || r.f.count() != 3 {
		t.Fatalf("after the wait: %+v, %v, %d requests, want a token on request 3", tok, err, r.f.count())
	}
	if again, err := r.m.Token(ctx, false); err != nil || again != tok || r.f.count() != 3 {
		t.Errorf("after recovery: %v %v, %d requests, want the cached token", again, err, r.f.count())
	}
	if !strings.Contains(r.log.String(), "minted again") {
		t.Errorf("the recovery was not logged:\n%s", r.log)
	}
}

// TestBackoffGrowsAndIsCapped: each failure doubles the wait, up to the
// cap.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	r := newRig(t, func(o *botauth.Options) { o.BackoffMin, o.BackoffMax = time.Second, 8*time.Second })
	for range 10 {
		r.f.next(status(http.StatusBadGateway))
	}
	wants := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, wait := range wants {
		before := r.f.count()
		if _, err := r.m.Token(t.Context(), false); err == nil || r.f.count() != before+1 {
			t.Fatalf("attempt %d: %v, %d new requests, want a failed request", i+1, err, r.f.count()-before)
		}
		r.clock.Advance(wait - time.Millisecond)
		if _, err := r.m.Token(t.Context(), false); err == nil || r.f.count() != before+1 {
			t.Fatalf("attempt %d, %v in: reached the endpoint before the wait of %v was over", i+1, wait-time.Millisecond, wait)
		}
		r.clock.Advance(time.Millisecond)
	}
}

// TestValidTokenServedWhileEndpointFails: a refresh that fails does not
// take away a token that is still valid.
func TestValidTokenServedWhileEndpointFails(t *testing.T) {
	r := newRig(t)
	first, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	r.clock.Advance(57 * time.Minute) // inside the skew, 3 minutes left
	r.f.next(status(http.StatusInternalServerError))
	got, err := r.m.Token(t.Context(), false)
	if err != nil || got != first {
		t.Fatalf("refresh failing: %+v, %v, want the token still valid", got, err)
	}
	r.clock.Advance(4 * time.Minute) // lapsed
	r.f.next(status(http.StatusInternalServerError))
	if _, err := r.m.Token(t.Context(), false); err == nil {
		t.Error("a lapsed token was served")
	}
}

// TestInvalidClientIsAnErrorWithTheProvidersCode: a refused secret fails
// as unauthorized naming the provider's own error code and AADSTS code,
// tells the credential store it was rejected, and Probe reports the same
// with the status.
func TestInvalidClientIsAnErrorWithTheProvidersCode(t *testing.T) {
	r := newRig(t)
	r.secret.set("a-wrong-secret-value")
	_, err := r.m.Token(t.Context(), false)
	me := (*model.Error)(nil)
	if !errors.As(err, &me) || me.Code != model.Unauthorized || me.Retry != model.NoRetry {
		t.Fatalf("a refused secret = %v, want unauthorized, no retry", err)
	}
	for _, want := range []string{"invalid_client", "AADSTS7000215"} {
		if !strings.Contains(me.SafeMessage, want) {
			t.Errorf("message %q lacks the provider's %s", me.SafeMessage, want)
		}
	}
	if r.secret.rejected != 1 {
		t.Errorf("the store was told of the rejection %d times, want 1", r.secret.rejected)
	}
	perr := r.m.Probe(t.Context())
	var mint *botauth.MintError
	if !errors.As(perr, &mint) || mint.Status != http.StatusUnauthorized || mint.Code != "invalid_client" || mint.STS != "AADSTS7000215" || mint.Temporary {
		t.Errorf("Probe = %#v, want 401 invalid_client AADSTS7000215, not temporary", perr)
	}
	if r.f.count() != 2 {
		t.Errorf("%d requests: Probe must reach the endpoint even inside the backoff", r.f.count())
	}
}

// TestSecretRotationNeedsNoRestart: the secret is read at every mint, so
// one rotated in the store is used by the next, and a rotation made after
// a failure is tried at once instead of waiting out the backoff.
func TestSecretRotationNeedsNoRestart(t *testing.T) {
	r := newRig(t)
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	// The old secret is revoked; the operator stores the new one.
	const rotated = "the-rotated-client-secret~Rt7"
	r.f.accept(tenantA, appA, rotated)
	r.secret.set(rotated)
	r.clock.Advance(58 * time.Minute)
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Fatalf("after the rotation: %v", err)
	}
	reqs := r.f.seen()
	if got := reqs[len(reqs)-1].Form.Get("client_secret"); got != rotated {
		t.Errorf("the renewal sent the old secret %q", got)
	}

	// A bad secret, then its correction inside the backoff.
	r.secret.set("typo-in-the-secret")
	r.clock.Advance(2 * time.Hour)
	if _, err := r.m.Token(t.Context(), false); err == nil {
		t.Fatal("a wrong secret minted a token")
	}
	before := r.f.count()
	if _, err := r.m.Token(t.Context(), false); err == nil || r.f.count() != before {
		t.Fatalf("same wrong secret inside the backoff: %v, %d new requests, want none", err, r.f.count()-before)
	}
	r.secret.set(rotated)
	if _, err := r.m.Token(t.Context(), false); err != nil || r.f.count() != before+1 {
		t.Errorf("secret corrected: %v, %d new requests, want one attempt at once", err, r.f.count()-before)
	}
}

// TestSecretStoreErrorsPassThrough: a secret that cannot be read is the
// store's error, not a failed mint: nothing is sent and nothing is held
// against the endpoint.
func TestSecretStoreErrorsPassThrough(t *testing.T) {
	r := newRig(t)
	r.secret.err = model.NewError(model.NotFound, "no credential stored")
	if _, err := r.m.Token(t.Context(), false); model.CodeOf(err) != model.NotFound || r.f.count() != 0 {
		t.Fatalf("no secret stored = %v after %d requests, want the store's not_found", err, r.f.count())
	}
	r.secret.err = nil
	if _, err := r.m.Token(t.Context(), false); err != nil {
		t.Errorf("once stored = %v, want a token at once", err)
	}
}

// TestNothingSecretIsLoggedOrReturned: whatever the endpoint says, even
// an error description repeating the secret, neither the secret nor a
// token reaches a log line or an error's text.
func TestNothingSecretIsLoggedOrReturned(t *testing.T) {
	r := newRig(t)
	r.f.echo = true
	tok, err := r.m.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	r.secret.set("wrong-secret-that-gets-echoed-back")
	r.clock.Advance(2 * time.Hour)
	var shown []string
	_, err = r.m.Token(t.Context(), false)
	shown = append(shown, fmt.Sprintf("%v|%+v|%#v|%s", err, err, err, err))
	perr := r.m.Probe(t.Context())
	shown = append(shown, fmt.Sprintf("%v|%+v|%#v", perr, perr, perr))
	r.f.next(status(http.StatusInternalServerError))
	r.secret.set(secretA)
	_, err = r.m.Token(t.Context(), false)
	shown = append(shown, fmt.Sprintf("%v|%+v", err, err))
	all := strings.Join(shown, "\n") + "\n" + r.log.String()
	for _, secret := range []string{secretA, "wrong-secret-that-gets-echoed-back", tok.Value, "access-token-"} {
		if strings.Contains(all, secret) {
			t.Errorf("%q leaked into errors or the log:\n%s", secret, all)
		}
	}
	if !strings.Contains(r.log.String(), "invalid_client") {
		t.Errorf("the failure was not logged with the provider's code:\n%s", r.log)
	}
}

// TestFreshAfterTheConnectorRefusedAToken: asking for a fresh token
// replaces the cached one, but a token minted moments ago is not minted
// over again, so callers refused the same old token share one new one.
func TestFreshAfterTheConnectorRefusedAToken(t *testing.T) {
	r := newRig(t)
	first, _ := r.m.Token(t.Context(), false)
	// The token just minted is what a fresh request gets.
	if got, err := r.m.Token(t.Context(), true); err != nil || got != first || r.f.count() != 1 {
		t.Fatalf("fresh right after a mint: %+v, %v, %d requests, want the same token", got, err, r.f.count())
	}
	r.clock.Advance(time.Minute)
	got, err := r.m.Token(t.Context(), true)
	if err != nil || got.Value == first.Value || r.f.count() != 2 {
		t.Fatalf("fresh a minute on: %+v, %v, %d requests, want a new token", got, err, r.f.count())
	}
	if again, _ := r.m.Token(t.Context(), true); again != got || r.f.count() != 2 {
		t.Errorf("a second fresh request minted again: %d requests", r.f.count())
	}
	if cached, _ := r.m.Token(t.Context(), false); cached != got {
		t.Errorf("an ordinary call after a fresh one = %+v, want the new token", cached)
	}
}

// TestTenantsAreIsolated: two bots in two tenants each mint with their
// own app, secret and tenant endpoint, hold their own token, and a
// failure of one is not the other's.
func TestTenantsAreIsolated(t *testing.T) {
	f := newFakeEntra(t)
	f.accept(tenantA, appA, secretA)
	f.accept(tenantB, appB, secretB)
	clock := newClock()
	mk := func(tenant, app string, secret *secretBox) *botauth.Minter {
		m, err := botauth.New(botauth.Options{
			Tenant: tenant, AppID: app, Authority: f.srv.URL, HTTP: f.srv.Client(), Credential: secret.credential, Now: clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	boxA, boxB := &secretBox{t: t, v: secretA}, &secretBox{t: t, v: secretB}
	a, b := mk(tenantA, appA, boxA), mk(tenantB, appB, boxB)
	ta, err := a.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := b.Token(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ta.Value, tenantA) || !strings.HasSuffix(tb.Value, tenantB) || ta.Value == tb.Value {
		t.Errorf("tokens %q and %q: each must come from its own tenant's endpoint", ta.Value, tb.Value)
	}
	for _, q := range f.seen() {
		switch q.Path {
		case "/" + tenantA + "/oauth2/v2.0/token":
			if q.Form.Get("client_id") != appA || q.Form.Get("client_secret") != secretA {
				t.Errorf("tenant A's endpoint was sent %v", q.Form.Get("client_id"))
			}
		case "/" + tenantB + "/oauth2/v2.0/token":
			if q.Form.Get("client_id") != appB || q.Form.Get("client_secret") != secretB {
				t.Errorf("tenant B's endpoint was sent %v", q.Form.Get("client_id"))
			}
		default:
			t.Errorf("request to %s", q.Path)
		}
	}
	// A's secret goes bad; B is unaffected, and A's wrong secret is never
	// offered to B's tenant.
	boxA.set("not-the-secret")
	clock.Advance(2 * time.Hour)
	if _, err := a.Token(t.Context(), false); err == nil {
		t.Error("A minted with a wrong secret")
	}
	if got, err := b.Token(t.Context(), false); err != nil || got.Value == tb.Value {
		t.Errorf("B = %+v, %v, want a fresh token of its own", got, err)
	}
}

// TestEndpointUnreachable: no answer is a transient failure that never
// spells out the request.
func TestEndpointUnreachable(t *testing.T) {
	r := newRig(t)
	r.f.srv.Close()
	_, err := r.m.Token(t.Context(), false)
	me := (*model.Error)(nil)
	if !errors.As(err, &me) || me.Code != model.Transient || !strings.Contains(me.SafeMessage, "unreachable") {
		t.Fatalf("unreachable = %v, want transient, unreachable", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), secretA) {
		t.Errorf("error spells out the request: %v", err)
	}
}

// TestAuthority: where the secret may be sent.
func TestAuthority(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"", botauth.DefaultAuthority, true},
		{"https://login.microsoftonline.us/", "https://login.microsoftonline.us", true},
		{"http://127.0.0.1:8123", "http://127.0.0.1:8123", true},
		{"http://localhost:8123", "http://localhost:8123", true},
		{"http://[::1]:8123", "http://[::1]:8123", true},
		{"http://login.microsoftonline.com", "", false},
		{"http://10.0.0.5", "", false},
		{"https://user:pw@login.microsoftonline.com", "", false},
		{"https://login.microsoftonline.com/tenant", "", false},
		{"https://login.microsoftonline.com?x=1", "", false},
		{"ftp://login.microsoftonline.com", "", false},
		{"login.microsoftonline.com", "", false},
	} {
		got, err := botauth.ParseAuthority(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseAuthority(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// TestNewRefusesIncompleteOptions.
func TestNewRefusesIncompleteOptions(t *testing.T) {
	ok := botauth.Options{Tenant: tenantA, AppID: appA, HTTP: http.DefaultClient, Credential: func(context.Context) (botauth.Credential, error) { return botauth.Credential{Secret: "s"}, nil }}
	if _, err := botauth.New(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*botauth.Options){
		"no tenant":     func(o *botauth.Options) { o.Tenant = "" },
		"no app":        func(o *botauth.Options) { o.AppID = "" },
		"no client":     func(o *botauth.Options) { o.HTTP = nil },
		"no credential": func(o *botauth.Options) { o.Credential = nil },
		"bad endpoint":  func(o *botauth.Options) { o.Authority = "http://example.com" },
	} {
		o := ok
		mutate(&o)
		if _, err := botauth.New(o); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// TestAnswerWithoutAUsableLifetimeOrToken: an answer that is no token (no
// access_token, no or a negative expires_in) fails as an invalid response,
// not as a token that never expires; the token type is not checked, as a
// recorded answer has it redacted.
func TestAnswerWithoutAUsableLifetimeOrToken(t *testing.T) {
	r := newRig(t)
	reply := func(v map[string]any) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	for name, v := range map[string]map[string]any{
		"no token":     {"token_type": "Bearer", "expires_in": 3600},
		"no lifetime":  {"token_type": "Bearer", "access_token": "x"},
		"zero":         {"token_type": "Bearer", "access_token": "x", "expires_in": 0},
		"negative":     {"token_type": "Bearer", "access_token": "x", "expires_in": -5},
		"a year":       {"token_type": "Bearer", "access_token": "x", "expires_in": 31536000},
		"not a number": {"token_type": "Bearer", "access_token": "x", "expires_in": "soon"},
	} {
		r.f.next(reply(v))
		err := r.m.Probe(t.Context())
		var mint *botauth.MintError
		if !errors.As(err, &mint) || mint.Code != "invalid_response" {
			t.Errorf("%s: %v, want invalid_response", name, err)
		}
	}
	r.f.next(reply(map[string]any{"token_type": "<redacted:TOKEN_TYPE>", "access_token": "<redacted:ACCESS_TOKEN>", "expires_in": 3599})) //nolint:gosec // G101: a recorded answer's placeholders
	if err := r.m.Probe(t.Context()); err != nil {
		t.Errorf("a recorded answer = %v, want a token", err)
	}
}
