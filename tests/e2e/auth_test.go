package e2e

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// httpAuth is a JWT authenticator for the sandbox's HTTP surfaces:
// kit's jwt mode, trusting one Ed25519 key, set for every HTTP
// listener through services.all. Pass config as `-c config` to
// `service start`.
type httpAuth struct {
	config string
	key    ed25519.PrivateKey
}

func (s *sandbox) useHTTPAuth() *httpAuth {
	s.t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		s.t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		s.t.Fatal(err)
	}
	keyFile := s.write("auth/signer.pem", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})))
	config := s.write("auth/auth.yaml", fmt.Sprintf(
		"services:\n  all:\n    auth:\n      mode: jwt\n      jwt:\n        public_key_files: [%q]\n", keyFile))
	return &httpAuth{config: config, key: priv}
}

// token mints a bearer token for subject, valid for an hour.
func (h *httpAuth) token(subject string) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: h.key}, nil)
	if err != nil {
		panic(err)
	}
	now := time.Now()
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Subject: subject, IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
	}).Serialize()
	if err != nil {
		panic(err)
	}
	return raw
}

// me is the actor the daemon names the test's own user: its socket
// peer uid, and the subject its tokens carry.
func me() string { return fmt.Sprintf("uid:%d", os.Getuid()) }

// authed sends method url with body as a bearer of token.
func authed(method, url, token, body string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, strings.NewReader(body)) //nolint:noctx // loopback test URL
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}
