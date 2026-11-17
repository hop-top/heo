package composition_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/credentials"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const mxName = "mx.example.com"

// testCA issues listener certificates for mxName.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "heo test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

// issue returns a PEM leaf for mxName valid over [notBefore, notAfter],
// and its PEM private key.
func (ca *testCA) issue(t *testing.T, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: mxName}, DNSNames: []string{mxName},
		NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk})
}

func (ca *testCA) valid(t *testing.T) (certPEM, keyPEM []byte) {
	return ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyStore is a credentials.Resolver holding one secret per reference.
type keyStore struct {
	mu       sync.Mutex
	keys     map[string][]byte
	requests []credentials.Request
}

func (s *keyStore) set(ref string, key []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = map[string][]byte{}
	}
	s.keys[ref] = key
}

func (s *keyStore) Resolve(_ context.Context, req credentials.Request) (ports.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	key, ok := s.keys[req.Reference]
	if !ok {
		return nil, model.NewError(model.NotFound, "no credential stored")
	}
	return ports.Secret{Bytes: append([]byte(nil), key...)}, nil
}

func (s *keyStore) opener() composition.CredentialsFunc {
	return func(context.Context) (credentials.Resolver, error) { return s, nil }
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func freeLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// serveMX opens the smtp.mail channel through the registered factory
// and runs its receiver until the test ends; it returns the listener
// address once it greets.
func serveMX(t *testing.T, o composition.Options) string {
	t.Helper()
	r, err := composition.Adapters(o)
	if err != nil {
		t.Fatal(err)
	}
	var f adapter.Factory
	for _, a := range r {
		if a.Factory.Describe().Type == model.TypeSmtpMail {
			f = a.Factory
		}
	}
	ch := channels(t)[model.TypeSmtpMail]
	inst, err := f.Open(t.Context(), ch, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- inst.(adapter.Receiver).Run(ctx, nopSink{}) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	addr := o.Viper().GetString(composition.KeySMTPAddr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = c.Close()
			return addr
		}
		select {
		case err := <-done:
			t.Fatalf("receiver stopped: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("receiver never listened on %s: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ehlo connects and greets; the caller closes the client.
func ehlo(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	c, err := smtp.Dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Hello("client.example.net"); err != nil {
		t.Fatal(err)
	}
	return c
}

// tlsOptions configures a loopback MX with the listener certificate in
// certFile and its key under reference "mx-key".
func tlsOptions(t *testing.T, store *keyStore, certFile string, clock func() time.Time) composition.Options {
	v := settings(
		composition.KeySMTPAddr, freeLoopback(t),
		composition.KeySMTPTLSCertFile, certFile,
		composition.KeySMTPTLSKeyRef, "mx-key",
	)
	return composition.Options{Viper: func() *viper.Viper { return v }, Credentials: store.opener(), Now: clock}
}

// TestSMTPStartTLSOffered: with a listener certificate and its key in
// the credential backend, the MX advertises STARTTLS and a sender that
// trusts the issuing CA completes the handshake, at TLS 1.2 or later;
// the key is requested for purpose smtp.tls.
func TestSMTPStartTLSOffered(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM := ca.valid(t)
	store := &keyStore{}
	store.set("mx-key", keyPEM)
	o := tlsOptions(t, store, writeFile(t, filepath.Join(t.TempDir(), "mx.pem"), certPEM), nil)
	addr := serveMX(t, o)

	c := ehlo(t, addr)
	if ok, _ := c.Extension("STARTTLS"); !ok {
		t.Fatal("STARTTLS not advertised with a listener certificate configured")
	}
	if err := c.StartTLS(&tls.Config{ServerName: mxName, RootCAs: ca.pool, MinVersion: tls.VersionTLS12}); err != nil {
		t.Fatalf("STARTTLS handshake: %v", err)
	}
	st, ok := c.TLSConnectionState()
	if !ok || st.Version < tls.VersionTLS12 {
		t.Fatalf("connection state %v, version %x", ok, st.Version)
	}
	if err := c.Quit(); err != nil {
		t.Fatal(err)
	}

	old := ehlo(t, addr)
	// A sender capped below TLS 1.2 is refused.
	err := old.StartTLS(&tls.Config{ServerName: mxName, RootCAs: ca.pool, MaxVersion: tls.VersionTLS11}) //nolint:gosec // G402: probes the listener's minimum
	if err == nil {
		t.Error("TLS 1.1 handshake accepted")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.requests) == 0 {
		t.Fatal("listener key never requested")
	}
	for _, req := range store.requests {
		if req.Reference != "mx-key" || req.Purpose != composition.PurposeSMTPTLS {
			t.Errorf("key requested as %+v", req)
		}
	}
}

// TestSMTPNoCertificateNoStartTLS: without a listener certificate the
// MX does not advertise STARTTLS (mail arrives in plain text).
func TestSMTPNoCertificateNoStartTLS(t *testing.T) {
	v := settings(composition.KeySMTPAddr, freeLoopback(t))
	addr := serveMX(t, composition.Options{Viper: func() *viper.Viper { return v }})
	c := ehlo(t, addr)
	if ok, _ := c.Extension("STARTTLS"); ok {
		t.Fatal("STARTTLS advertised without a listener certificate")
	}
}

// TestSMTPCertificateRotation: a rotated certificate file and key are
// served once the cached pair is older than the refresh interval.
func TestSMTPCertificateRotation(t *testing.T) {
	ca := newTestCA(t)
	certA, keyA := ca.valid(t)
	store := &keyStore{}
	store.set("mx-key", keyA)
	certFile := writeFile(t, filepath.Join(t.TempDir(), "mx.pem"), certA)
	clock := &fakeClock{now: time.Now()}
	addr := serveMX(t, tlsOptions(t, store, certFile, clock.Now))

	served := func() []byte {
		c := ehlo(t, addr)
		if err := c.StartTLS(&tls.Config{ServerName: mxName, RootCAs: ca.pool, MinVersion: tls.VersionTLS12}); err != nil { //nolint:gosec // G402: the test pins TLS 1.2 as the minimum the listener must serve
			t.Fatalf("handshake: %v", err)
		}
		st, _ := c.TLSConnectionState()
		_ = c.Quit()
		return st.PeerCertificates[0].Raw
	}
	leaf := func(p []byte) []byte {
		b, _ := pem.Decode(p)
		return b.Bytes
	}
	if got := served(); string(got) != string(leaf(certA)) {
		t.Fatal("first handshake did not serve the configured certificate")
	}
	certB, keyB := ca.valid(t)
	writeFile(t, certFile, certB)
	store.set("mx-key", keyB)
	if got := served(); string(got) != string(leaf(certA)) {
		t.Fatal("pair reloaded before the refresh interval")
	}
	clock.advance(2 * time.Minute)
	if got := served(); string(got) != string(leaf(certB)) {
		t.Fatal("rotated certificate not served after the refresh interval")
	}
}

// TestSMTPTLSChecked: config validate reads the certificate file and
// resolves the key: the pair must parse, match and be in date.
func TestSMTPTLSChecked(t *testing.T) {
	ca := newTestCA(t)
	dir := t.TempDir()
	now := time.Now()
	good, goodKey := ca.valid(t)
	_, otherKey := ca.valid(t)
	expired, expiredKey := ca.issue(t, now.Add(-48*time.Hour), now.Add(-time.Hour))
	soon, soonKey := ca.issue(t, now.Add(-time.Hour), now.Add(3*24*time.Hour))
	future, futureKey := ca.issue(t, now.Add(time.Hour), now.Add(48*time.Hour))

	for _, tc := range []struct {
		name      string
		cert      []byte // nil: no file
		key       []byte // nil: nothing stored
		certFile  string // overrides the written file
		keyRef    string // default "mx-key"
		wantKey   string
		want      string
		warning   bool
		noProblem bool
	}{
		{name: "valid", cert: good, key: goodKey, noProblem: true},
		{name: "mismatched pair", cert: good, key: otherKey, wantKey: composition.KeySMTPTLSKeyRef, want: "does not match"},
		{name: "expired", cert: expired, key: expiredKey, wantKey: composition.KeySMTPTLSCertFile, want: "expired"},
		{name: "not yet valid", cert: future, key: futureKey, wantKey: composition.KeySMTPTLSCertFile, want: "not valid before"},
		{name: "near expiry", cert: soon, key: soonKey, wantKey: composition.KeySMTPTLSCertFile, want: "expires", warning: true},
		{name: "missing file", certFile: filepath.Join(dir, "absent.pem"), key: goodKey, wantKey: composition.KeySMTPTLSCertFile, want: "cannot read"},
		{name: "not PEM", cert: []byte("not a certificate"), key: goodKey, wantKey: composition.KeySMTPTLSCertFile, want: "no PEM certificate"},
		{name: "key not stored", cert: good, wantKey: composition.KeySMTPTLSKeyRef, want: "no credential stored"},
		{name: "key not PEM", cert: good, key: []byte("hunter2"), wantKey: composition.KeySMTPTLSKeyRef, want: "not a PEM private key"},
		{name: "key without cert", certFile: "-", key: goodKey, wantKey: composition.KeySMTPTLSCertFile, want: "both"},
		{name: "cert without key", cert: good, keyRef: "-", wantKey: composition.KeySMTPTLSKeyRef, want: "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			certFile := tc.certFile
			switch certFile {
			case "-":
				certFile = ""
			case "":
				certFile = writeFile(t, filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".pem"), tc.cert)
			}
			ref := tc.keyRef
			switch ref {
			case "-":
				ref = ""
			case "":
				ref = "mx-key"
			}
			store := &keyStore{}
			if tc.key != nil {
				store.set("mx-key", tc.key)
			}
			v := settings(composition.KeySMTPAddr, "127.0.0.1:2525",
				composition.KeySMTPTLSCertFile, certFile, composition.KeySMTPTLSKeyRef, ref)
			probs := composition.Check(context.Background(), v, store.opener())
			if tc.noProblem {
				if len(probs) > 0 {
					t.Fatalf("problems: %+v", probs)
				}
				return
			}
			if len(probs) != 1 || probs[0].Key != tc.wantKey || !strings.Contains(probs[0].Message, tc.want) || probs[0].Warning != tc.warning {
				t.Fatalf("problems %+v, want one on %s containing %q (warning %v)", probs, tc.wantKey, tc.want, tc.warning)
			}
			if strings.Contains(probs[0].Message, "BEGIN") || strings.Contains(probs[0].Message, "hunter2") {
				t.Fatalf("problem carries key material: %s", probs[0].Message)
			}
		})
	}
}

// TestSMTPTLSListenerRefused: a listener whose key cannot be resolved
// never binds, rather than serving without the configured TLS.
func TestSMTPTLSListenerRefused(t *testing.T) {
	ca := newTestCA(t)
	certPEM, _ := ca.valid(t)
	o := tlsOptions(t, &keyStore{}, writeFile(t, filepath.Join(t.TempDir(), "mx.pem"), certPEM), nil)
	list, err := composition.Adapters(o)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := list[1].Factory.Open(t.Context(), channels(t)[model.TypeSmtpMail], adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := inst.(adapter.Receiver).Run(ctx, nopSink{}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("receiver ran without its key: %v", err)
	}
	if c, err := net.DialTimeout("tcp", o.Viper().GetString(composition.KeySMTPAddr), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("listener bound without its key")
	}
}
