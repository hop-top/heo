// Package consenttest is a fake authorization server for tests of the
// interactive consent: an authorize endpoint that sends the browser back
// to the redirect it was given, and a token endpoint that checks the PKCE
// verifier against the challenge it saw. No test reaches a real provider.
package consenttest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/consent"
)

// The tokens the fake issues.
const (
	RefreshToken = "REFRESH-TOKEN-SEED-9f3" //nolint:gosec // fake issuer seed value, not a credential
	AccessToken  = "ACCESS-TOKEN-SEED-4c1"
)

// IDP is the fake authorization server.
type IDP struct {
	srv *httptest.Server

	mu         sync.Mutex
	authorizes []url.Values
	forms      []url.Values
	challenges map[string]string // code -> code_challenge
	deny       bool
	noRefresh  bool
	badClient  bool
}

// New starts a fake provider that stops with the test.
func New(t testing.TB) *IDP {
	t.Helper()
	i := &IDP{challenges: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", i.authorize)
	mux.HandleFunc("/token", i.token)
	i.srv = httptest.NewServer(mux)
	t.Cleanup(i.srv.Close)
	return i
}

// URL is the server's base URL.
func (i *IDP) URL() string { return i.srv.URL }

// Deny makes the user decline consent: the redirect carries access_denied.
func (i *IDP) Deny() { i.set(func() { i.deny = true }) }

// NoRefreshToken makes the token endpoint issue no refresh token.
func (i *IDP) NoRefreshToken() { i.set(func() { i.noRefresh = true }) }

// RejectClient makes the token endpoint reject the client.
func (i *IDP) RejectClient() { i.set(func() { i.badClient = true }) }

func (i *IDP) set(f func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f()
}

// Authorizations are the authorization requests the server saw.
func (i *IDP) Authorizations() []url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]url.Values(nil), i.authorizes...)
}

// Exchanges are the token requests the server saw.
func (i *IDP) Exchanges() []url.Values {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]url.Values(nil), i.forms...)
}

// Provider points p's endpoints at the fake.
func (i *IDP) Provider(p consent.Provider) consent.Provider {
	p.AuthURL, p.TokenURL = i.srv.URL+"/authorize", i.srv.URL+"/token"
	return p
}

// Browse is the user's browser: it follows the consent URL in the
// background, and the redirect it ends in lands on the loopback
// listener.
func Browse(authorizeURL string) {
	go func() {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(authorizeURL)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
}

func (i *IDP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	i.mu.Lock()
	i.authorizes = append(i.authorizes, q)
	n, deny := len(i.authorizes), i.deny
	i.mu.Unlock()
	redirect, state := q.Get("redirect_uri"), q.Get("state")
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || redirect == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	if deny {
		http.Redirect(w, r, redirect+"?error=access_denied&state="+url.QueryEscape(state), http.StatusFound) //nolint:gosec // fake issuer test server: redirect_uri is the loopback listener under test
		return
	}
	code := fmt.Sprintf("code-%d", n)
	i.mu.Lock()
	i.challenges[code] = q.Get("code_challenge")
	i.mu.Unlock()
	http.Redirect(w, r, redirect+"?code="+code+"&state="+url.QueryEscape(state), http.StatusFound) //nolint:gosec // fake issuer test server: redirect_uri is the loopback listener under test
}

func (i *IDP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	i.mu.Lock()
	i.forms = append(i.forms, r.PostForm)
	challenge, known := i.challenges[r.PostForm.Get("code")]
	noRefresh, badClient := i.noRefresh, i.badClient
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if r.PostForm.Get("grant_type") == "refresh_token" && r.PostForm.Get("refresh_token") == RefreshToken && !badClient {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": AccessToken + "-refreshed", "token_type": "Bearer", "expires_in": 3600})
		return
	}
	switch {
	case badClient:
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
		return
	case r.PostForm.Get("grant_type") != "authorization_code" || !known || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	reply := map[string]any{"access_token": AccessToken, "token_type": "Bearer", "expires_in": 3600}
	if !noRefresh {
		reply["refresh_token"] = RefreshToken
	}
	_ = json.NewEncoder(w).Encode(reply)
}
