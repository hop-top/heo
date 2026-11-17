package xrrx

import (
	"bytes"
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
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// What a Teams recording's daemon mints its Bot Connector token with. heo
// mints it from the bot's client secret, or from a certificate and its key,
// at the tenant's Microsoft Entra token endpoint; the recorded daemon holds
// a stand-in secret or a stand-in certificate for the placeholder bot and
// tenant, and the real credential is never the recording's to hold (the
// operator mints the real token and passes it in EnvTeamsConnectorToken).
const (
	// TeamsEntraHost is the Microsoft Entra endpoint the daemon mints at.
	TeamsEntraHost = "login.microsoftonline.com"
	// TeamsStandInSecret is the bot's client secret heo's credential store
	// holds in every recording and replay.
	TeamsStandInSecret = "heo-adapter-test-stand-in-client-secret" // #nosec G101 -- a stand-in, not a credential

	teamsTokenPath = "/" + TeamsTenantID + "/oauth2/v2.0/token"
	teamsScope     = "https://api.botframework.com/.default"
	// teamsAssertionType is the client_assertion_type of a certificate grant.
	teamsAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	// teamsMaxAssertionLife bounds exp - nbf of a stand-in assertion; heo's
	// own is ten minutes.
	teamsMaxAssertionLife = 15 * time.Minute
	// teamsAssertionPlaceholder is what a recorded request holds in place of
	// the assertion, which carries a time and an ID of its own.
	teamsAssertionPlaceholder = "<assertion>"
)

// NewTeamsStandInCertificate makes the stand-in certificate and RSA key
// (PEM; the key PKCS#8) a recorded daemon proves the placeholder bot with:
// self-signed, naming nobody, valid for years. Each call makes a new pair:
// the test that stores it in heo's credential store hands the sandbox the
// certificate alone (UseStandInCertificate), so nothing of the key is in a
// file of the repository or an environment.
func NewTeamsStandInCertificate() (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "heo-adapter-test-stand-in"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

// UseStandInCertificate tells the sandbox the stand-in certificate (PEM) of
// the placeholder bot: the one a certificate grant's assertion must be
// signed for. Without it the sandbox answers the stand-in secret only.
func (s *TeamsSandbox) UseStandInCertificate(certPEM []byte) error {
	blk, _ := pem.Decode(certPEM)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return errors.New("xrrx: the stand-in certificate is not a PEM certificate")
	}
	return s.useStandIn(blk.Bytes)
}

func (s *TeamsSandbox) useStandIn(der []byte) error {
	c, err := x509.ParseCertificate(der)
	if err != nil {
		return errors.New("xrrx: the stand-in certificate does not parse")
	}
	if _, ok := c.PublicKey.(*rsa.PublicKey); !ok {
		return errors.New("xrrx: the stand-in certificate is not an RSA one")
	}
	s.standIn = c
	return nil
}

// TokenTransport is what answers the daemon's request for a Bot Connector
// token in a live recording or its rehearsal: Microsoft Entra is never
// reached. The one request it answers is the documented client credentials
// grant of the placeholder tenant and bot with the Bot Framework scope, and
// either the stand-in secret or an assertion signed RS256 for the stand-in
// certificate (RFC 7523: for this endpoint, by and about the placeholder
// bot, short-lived); it gets the stand-in token, which Transport swaps for
// the real one on its way to the Bot Connector. Any other request to
// Microsoft Entra (another tenant, another app, another secret or key,
// another scope, a query) is refused, so a mint of the real bot, with a
// real secret or key, cannot leave the process.
func (s *TeamsSandbox) TokenTransport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		refuse := func(why string) (*http.Response, error) {
			return nil, fmt.Errorf("xrrx: Teams sandbox: %s %s refused: %s", req.Method, ScrubText(req.URL.Redacted()), why)
		}
		u := req.URL
		if req.Method != http.MethodPost || u.Scheme != "https" || u.Host != TeamsEntraHost || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || u.Path != teamsTokenPath {
			return refuse("only the placeholder tenant's token request (POST https://" + TeamsEntraHost + teamsTokenPath + "); Microsoft Entra is not reached")
		}
		if req.Header.Get("Authorization") != "" {
			return refuse("the client credentials go in the body")
		}
		if mt := req.Header.Get("Content-Type"); !strings.HasPrefix(mt, "application/x-www-form-urlencoded") {
			return refuse("the body must be a form")
		}
		if req.Body == nil {
			return refuse("no body")
		}
		raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<13))
		_ = req.Body.Close()
		if err != nil {
			return refuse("unreadable body")
		}
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			return refuse("the body is not a form")
		}
		want := url.Values{
			"grant_type": {"client_credentials"}, "client_id": {TeamsAppID}, "scope": {teamsScope},
		}
		if _, certificate := form["client_assertion"]; certificate {
			want.Set("client_assertion_type", teamsAssertionType)
			want.Set("client_assertion", form.Get("client_assertion"))
			if err := s.checkStandInAssertion(form.Get("client_assertion")); err != nil {
				return refuse("the assertion is not the stand-in certificate's for the placeholder bot: " + err.Error())
			}
		} else {
			want.Set("client_secret", TeamsStandInSecret)
		}
		if len(form) != len(want) {
			return refuse("only the grant, the placeholder app, the stand-in secret or assertion and the Bot Framework scope")
		}
		for k, v := range want {
			if got := form[k]; len(got) != 1 || got[0] != v[0] {
				return refuse("only the grant, the placeholder app, the stand-in secret or assertion and the Bot Framework scope")
			}
		}
		body, _ := json.Marshal(map[string]any{
			"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": TeamsStandInToken,
		})
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
			Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)),
		}, nil
	})
}

// checkStandInAssertion verifies a client assertion as Entra would for the
// stand-in certificate: RS256, the certificate named by its x5t#S256
// header and the signature checked with its public key, this endpoint as
// audience, the placeholder bot as issuer and subject, an ID, and a short
// window that has begun and not ended.
func (s *TeamsSandbox) checkStandInAssertion(assertion string) error {
	if s.standIn == nil {
		return errors.New("no stand-in certificate is known to the sandbox")
	}
	sum := sha256.Sum256(s.standIn.Raw)
	thumb := base64.RawURLEncoding.EncodeToString(sum[:])
	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(TeamsAppID), jwt.WithSubject(TeamsAppID),
		jwt.WithAudience("https://"+TeamsEntraHost+teamsTokenPath), jwt.WithExpirationRequired(), jwt.WithIssuedAt(),
	).ParseWithClaims(assertion, claims, func(t *jwt.Token) (any, error) {
		if got, _ := t.Header["x5t#S256"].(string); got != thumb {
			return nil, errors.New("not signed for the stand-in certificate")
		}
		return s.standIn.PublicKey, nil
	})
	if err != nil {
		return errors.New("it does not verify")
	}
	if id, _ := claims["jti"].(string); id == "" {
		return errors.New("no ID")
	}
	nbf, errN := claims.GetNotBefore()
	exp, errE := claims.GetExpirationTime()
	if errN != nil || errE != nil || nbf == nil || exp == nil || exp.Sub(nbf.Time) > teamsMaxAssertionLife {
		return errors.New("no short window")
	}
	return nil
}

// normalizeTeamsTokenBody is a token request's recorded body: the
// assertion of a certificate grant, which carries a time and an ID, is a
// placeholder, so the same mint is the same request on every run and the
// cassette holds no signed token. A secret grant's form is left as it is.
func normalizeTeamsTokenBody(body []byte) []byte {
	form, err := url.ParseQuery(string(body))
	if err != nil || !form.Has("client_assertion") {
		return body
	}
	form.Set("client_assertion", teamsAssertionPlaceholder)
	return []byte(form.Encode())
}
