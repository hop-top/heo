package xrrx

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	xrr "hop.top/xrr"
)

// provider is a local stand-in that echoes enough of what it was sent to
// show what a cassette must not keep.
func provider(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=abc123")
		w.Header().Set("Location", "http://"+r.Host+"/v1/messages/m-1")
		w.WriteHeader(http.StatusCreated)
		// G705: a JSON body for a client the test owns, not markup.
		_, _ = fmt.Fprintf(w, `{"id":"m-1","received":%d,"self":"http://%s/v1/messages/m-1"}`, len(b), r.Host) //nolint:gosec // loopback JSON stand-in
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// send posts one message the way an adapter does: a bearer token, a
// credential field, a timestamp and a temp path in the body.
func send(t *testing.T, c *http.Client, base string, text string) (*http.Response, string, error) {
	t.Helper()
	body := `{"text":"` + text + `","sent_at":"2026-10-12T10:11:12Z","spool":"` + filepath.Join(os.TempDir(), "heo-spool", "x") + `","api_key":"k-not-for-disk"}`
	req, err := http.NewRequest(http.MethodPost, base+"/v1/messages?access_token=tok-not-for-disk&page=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-not-for-disk-0123456789abcdef")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session=not-for-disk")
	resp, err := c.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

func client(t *testing.T, mode xrr.Mode, dir, missLog string) *http.Client {
	t.Helper()
	e := &Env{Mode: mode, Dir: dir, MissLog: missLog}
	return &http.Client{Transport: e.Transport(nil)}
}

func readAll(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // G304: the test's own cassette directory
		if err != nil {
			t.Fatal(err)
		}
		sb.WriteString(e.Name() + ":\n" + string(b) + "\n")
	}
	return sb.String()
}

// TestTransportRecordsNoSecretNoPortNoTemp: what a recording keeps is
// what a replay needs and nothing the OS or the test chose.
func TestTransportRecordsNoSecretNoPortNoTemp(t *testing.T) {
	srv, hits := provider(t)
	dir := filepath.Join(t.TempDir(), "cas")
	resp, body, err := send(t, client(t, xrr.ModeRecord, dir, ""), srv.URL, "hello")
	if err != nil || resp.StatusCode != http.StatusCreated || hits.Load() != 1 {
		t.Fatalf("record: %v %v hits %d", resp, err, hits.Load())
	}
	// The caller gets the recorded form in every mode.
	if strings.Contains(body, srv.Listener.Addr().String()) {
		t.Errorf("response still names the listener: %s", body)
	}

	all := readAll(t, dir)
	host := srv.Listener.Addr().String()
	_, port, _ := strings.Cut(host, ":")
	for _, bad := range []string{
		"sk-not-for-disk", "tok-not-for-disk", "k-not-for-disk", "not-for-disk", "abc123", // credentials
		"Authorization", "authorization", "Cookie", "Set-Cookie", // credential headers
		host, ":" + port, "127.0.0.1", // the OS's port
		os.TempDir(), // temp path
	} {
		if strings.Contains(all, bad) {
			t.Errorf("cassette keeps %q:\n%s", bad, all)
		}
	}
	for _, want := range []string{LoopbackHost, TempDir, Time, "<redacted:"} {
		if !strings.Contains(all, want) {
			t.Errorf("cassette lacks the placeholder %q:\n%s", want, all)
		}
	}
	if fs, err := Audit(dir); err != nil || len(fs) != 0 {
		t.Errorf("audit of a fresh recording = %v, %v", fs, err)
	}
}

// TestTransportReplayNeedsNoProviderAndNewPort: a replay answers from
// the cassette on another port, another temp dir's worth of time later,
// and never reaches the provider.
func TestTransportReplayNeedsNoProviderAndNewPort(t *testing.T) {
	srv, hits := provider(t)
	dir := filepath.Join(t.TempDir(), "cas")
	if _, _, err := send(t, client(t, xrr.ModeRecord, dir, ""), srv.URL, "hello"); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	other, otherHits := provider(t)
	resp, body, err := send(t, client(t, xrr.ModeReplay, dir, ""), other.URL, "hello")
	if err != nil || resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"id":"m-1"`) {
		t.Fatalf("replay = %v %q %v", resp, body, err)
	}
	if hits.Load() != 1 || otherHits.Load() != 0 {
		t.Errorf("provider hits: recorded %d (want 1), replay target %d (want 0)", hits.Load(), otherHits.Load())
	}
}

// TestTransportReplayMissIsStrict: a request the cassette does not hold
// fails, is logged for the parent test to read, and is not sent on to
// the provider.
func TestTransportReplayMissIsStrict(t *testing.T) {
	srv, hits := provider(t)
	dir := filepath.Join(t.TempDir(), "cas")
	if _, _, err := send(t, client(t, xrr.ModeRecord, dir, ""), srv.URL, "hello"); err != nil {
		t.Fatal(err)
	}
	miss := filepath.Join(t.TempDir(), "miss.log")
	other, otherHits := provider(t)
	_, _, err := send(t, client(t, xrr.ModeReplay, dir, miss), other.URL, "a different message")
	if err == nil || !strings.Contains(err.Error(), "cassette miss") {
		t.Fatalf("a request with no cassette = %v, want a cassette miss", err)
	}
	log, _ := os.ReadFile(miss) //nolint:gosec // G304: the test's own miss log
	if !strings.Contains(string(log), "no cassette for POST") {
		t.Errorf("miss log = %q", log)
	}
	if hits.Load() != 1 || otherHits.Load() != 0 {
		t.Errorf("a miss reached a provider: %d, %d", hits.Load(), otherHits.Load())
	}
}
