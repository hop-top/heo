package composition

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"hop.top/heo/internal/credentials"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The MX listener's STARTTLS certificate: a PEM chain read from
// adapters.smtp.tls.cert_file, which is not secret, and the private key
// resolved from the profile's credential backend under
// adapters.smtp.tls.key_ref for purpose smtp.tls. The key never sits in
// configuration or in a plain file heo reads.

const (
	// tlsRefresh is how long a loaded pair is served before the file is
	// read and the key resolved again, so a rotation applies without a
	// restart.
	tlsRefresh = time.Minute
	// certExpiryWarning is how close to expiry `config validate` warns.
	certExpiryWarning = 14 * 24 * time.Hour
)

// CredentialsFunc opens the profile's credential backend.
type CredentialsFunc func(context.Context) (credentials.Resolver, error)

// tlsProblem is a finding about one setting of the pair.
type tlsProblem struct {
	key string
	msg string
}

func (p *tlsProblem) Error() string { return p.key + ": " + p.msg }

// readChain reads the certificate chain file: its PEM blocks and the
// parsed leaf (the first certificate).
func readChain(path string) ([]byte, *x509.Certificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-configured certificate file
	if err != nil {
		return nil, nil, &tlsProblem{KeySMTPTLSCertFile, fmt.Sprintf("cannot read %s", path)}
	}
	rest := data
	for {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			return nil, nil, &tlsProblem{KeySMTPTLSCertFile, fmt.Sprintf("%s holds no PEM certificate", path)}
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, nil, &tlsProblem{KeySMTPTLSCertFile, fmt.Sprintf("%s: first certificate does not parse", path)}
		}
		return data, leaf, nil
	}
}

// resolveKey resolves the listener key: the PEM private key held by a
// secret envelope. The caller clears the returned bytes.
func resolveKey(ctx context.Context, creds CredentialsFunc, ref string) ([]byte, error) {
	if creds == nil {
		return nil, &tlsProblem{KeySMTPTLSKeyRef, "no credential backend to resolve it in"}
	}
	r, err := creds(ctx)
	if err != nil {
		return nil, &tlsProblem{KeySMTPTLSKeyRef, "credential backend unavailable: " + err.Error()}
	}
	cred, err := r.Resolve(ctx, credentials.Request{
		Reference: ref, Purpose: PurposeSMTPTLS, Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return nil, &tlsProblem{KeySMTPTLSKeyRef, fmt.Sprintf("%q (purpose %s): %s", ref, PurposeSMTPTLS, diagnose(ctx, r, ref, err))}
	}
	s, ok := cred.(ports.Secret)
	if !ok {
		return nil, &tlsProblem{KeySMTPTLSKeyRef, fmt.Sprintf("%q: want a secret envelope holding the PEM private key", ref)}
	}
	return s.Bytes, nil
}

// diagnoser is a Resolver that also diagnoses a reference, as the
// profile's credential backend does: status, message and fix, never
// material.
type diagnoser interface {
	Check(context.Context, credentials.Request) credentials.Diagnosis
}

// diagnose explains a failed key resolution: the backend's diagnosis
// when it gives one, the error's fixed text otherwise.
func diagnose(ctx context.Context, r credentials.Resolver, ref string, err error) string {
	d, ok := r.(diagnoser)
	if !ok || ctx.Err() != nil {
		return safeMessage(err)
	}
	diag := d.Check(ctx, credentials.Request{Reference: ref, Purpose: PurposeSMTPTLS, Accept: []ports.CredentialKind{ports.KindSecret}})
	if diag.Status == credentials.StatusOK {
		return safeMessage(err)
	}
	msg := fmt.Sprintf("%s: %s", diag.Status, diag.Message)
	if diag.SuggestedFix != "" {
		msg += "; " + diag.SuggestedFix
	}
	return msg
}

// safeMessage is the fixed text of a credential error, never material.
func safeMessage(err error) string {
	var me *model.Error
	if errors.As(err, &me) && me.SafeMessage != "" {
		return me.SafeMessage
	}
	return "credential store unavailable"
}

// loadPair reads the chain, resolves the key and pairs them.
func loadPair(ctx context.Context, cfg SMTPConfig, creds CredentialsFunc) (*tls.Certificate, *x509.Certificate, error) {
	chain, leaf, err := readChain(cfg.CertFile)
	if err != nil {
		return nil, nil, err
	}
	key, err := resolveKey(ctx, creds, cfg.KeyRef)
	if err != nil {
		return nil, nil, err
	}
	defer clear(key)
	if b, _ := pem.Decode(key); b == nil || !strings.HasSuffix(b.Type, "PRIVATE KEY") {
		return nil, nil, &tlsProblem{KeySMTPTLSKeyRef, fmt.Sprintf("%q is not a PEM private key", cfg.KeyRef)}
	}
	pair, err := tls.X509KeyPair(chain, key)
	if err != nil {
		return nil, nil, &tlsProblem{KeySMTPTLSKeyRef, fmt.Sprintf(
			"%q does not match the certificate in %s, or is not an RSA, ECDSA or Ed25519 key", cfg.KeyRef, cfg.CertFile)}
	}
	return &pair, leaf, nil
}

// CheckSMTPTLS checks the listener certificate for `config validate`:
// the chain file parses, the key resolves and matches it, and the leaf
// is valid at now; it warns when the leaf expires within two weeks.
func CheckSMTPTLS(ctx context.Context, cfg SMTPConfig, creds CredentialsFunc, now time.Time) []Problem {
	_, leaf, err := loadPair(ctx, cfg, creds)
	var tp *tlsProblem
	switch {
	case errors.As(err, &tp):
		return []Problem{{Key: tp.key, Message: tp.msg}}
	case err != nil:
		return []Problem{{Key: KeySMTPTLSCertFile, Message: "certificate unusable"}}
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	switch {
	case now.After(leaf.NotAfter):
		return []Problem{{Key: KeySMTPTLSCertFile, Message: fmt.Sprintf("certificate expired at %s", stamp(leaf.NotAfter))}}
	case now.Before(leaf.NotBefore):
		return []Problem{{Key: KeySMTPTLSCertFile, Message: fmt.Sprintf("certificate not valid before %s", stamp(leaf.NotBefore))}}
	case leaf.NotAfter.Sub(now) < certExpiryWarning:
		return []Problem{{Key: KeySMTPTLSCertFile, Warning: true, Message: fmt.Sprintf(
			"certificate expires at %s; renew it and replace the file and the stored key", stamp(leaf.NotAfter))}}
	}
	return nil
}

// tlsSource serves the listener pair, reloading it once it is older than
// tlsRefresh. A failed reload keeps serving the last pair and is
// retried after another tlsRefresh.
type tlsSource struct {
	cfg   SMTPConfig
	creds CredentialsFunc
	now   func() time.Time

	mu     sync.Mutex
	pair   *tls.Certificate
	loaded time.Time
}

func newTLSSource(cfg SMTPConfig, creds CredentialsFunc, now func() time.Time) *tlsSource {
	if now == nil {
		now = time.Now
	}
	return &tlsSource{cfg: cfg, creds: creds, now: now}
}

// config is the receiver's STARTTLS configuration.
func (s *tlsSource) config() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			return s.get(hello.Context())
		},
	}
}

func (s *tlsSource) get(ctx context.Context) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.pair != nil && now.Sub(s.loaded) < tlsRefresh {
		return s.pair, nil
	}
	pair, _, err := loadPair(ctx, s.cfg, s.creds)
	s.loaded = now
	if err != nil {
		if s.pair != nil {
			return s.pair, nil
		}
		return nil, err
	}
	s.pair = pair
	return pair, nil
}
