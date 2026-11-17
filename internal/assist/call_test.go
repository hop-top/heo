package assist_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/assist"
	"hop.top/kit/go/ai/llm"
	llmerrors "hop.top/kit/go/ai/llm/errors"
	"hop.top/kit/go/console/output"
)

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestFailuresAreCorrectiveAndNeverRetriedElsewhere(t *testing.T) {
	cases := []struct {
		name string
		err  error
		exit int
		code string
		fix  string
	}{
		{"model missing", llmerrors.NewModel("m", "ollama"), output.ExitNotFound, "NOT_FOUND", "ollama pull"},
		{"credential refused", llmerrors.NewAuth("ollama", errors.New("401")), output.ExitUnauthorized, "UNAUTHORIZED", "credential_ref"},
		{"permission denied", llmerrors.NewPermissionDenied("ollama", errors.New("403")), output.ExitUnauthorized, "UNAUTHORIZED", "credential_ref"},
		{"rate limited", llmerrors.NewRateLimit("ollama", 0), output.ExitTransient, "TRANSIENT", "later"},
		{"server fault", llmerrors.NewHTTPStatusError(503, "503 Service Unavailable"), output.ExitTransient, "TRANSIENT", "again"},
		{"unreachable", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/api/chat", Err: errors.New("connection refused")}, output.ExitPrerequisite, "PREREQUISITE", "llm.endpoint"},
		{"timeout", context.DeadlineExceeded, output.ExitTransient, "TRANSIENT", "llm.timeout"},
		{"other", errors.New("ollama: decode response: EOF"), output.ExitGeneric, "GENERIC", "llm.provider"},
	}
	for _, c := range cases {
		m := &script{err: c.err}
		_, err := localCaller(m).Complete(t.Context(), []llm.Message{{Role: "user", Content: "x"}})
		var oe *output.Error
		if !errors.As(err, &oe) || oe.ExitCode != c.exit || oe.Code != c.code || !strings.Contains(oe.SuggestedFix, c.fix) {
			t.Errorf("%s: err = %#v, want exit %d %s with a fix naming %q", c.name, err, c.exit, c.code, c.fix)
		}
		if len(m.asked) != 1 {
			t.Errorf("%s: the provider was called %d times, want 1: nothing retries or falls back", c.name, len(m.asked))
		}
	}
}

// TestUnreachableLocalProviderIsAnErrorNotAFallback: the real adapter
// against a port nothing listens on fails at once with exit 70, naming
// the host and the key to fix, whatever the environment says about other
// providers.
func TestUnreachableLocalProviderIsAnErrorNotAFallback(t *testing.T) {
	for k, v := range map[string]string{"LLM_FALLBACK": "openai", "OPENAI_API_KEY": "sk-x", "LLM_PROVIDER": "openai"} {
		t.Setenv(k, v)
	}
	addr := closedPort(t)
	p, err := plan(t, map[string]any{assist.KeyEndpoint: "http://" + addr, assist.KeyModel: "m"})
	if err != nil {
		t.Fatal(err)
	}
	comp, closeFn, err := p.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	_, err = assist.Caller{Plan: p, LLM: comp}.Complete(t.Context(), []llm.Message{{Role: "user", Content: "x"}})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != output.ExitPrerequisite {
		t.Fatalf("err = %v, want exit 70", err)
	}
	if !strings.Contains(oe.Message, addr) || !strings.Contains(oe.Message, "does not fall back") || !strings.Contains(oe.SuggestedFix, "llm.endpoint") {
		t.Errorf("error = %q fix %q", oe.Message, oe.SuggestedFix)
	}
}

func TestACallIsBoundedByTheTimeout(t *testing.T) {
	m := blocking{}
	c := localCaller(m)
	c.Plan.Timeout = 30 * time.Millisecond
	start := time.Now()
	_, err := c.Complete(t.Context(), nil)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != output.ExitTransient || !strings.Contains(oe.Message, "did not answer") || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

type blocking struct{}

func (blocking) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, ctx.Err()
}

func TestMarkedCalls(t *testing.T) {
	if assist.IsCall(t.Context()) || !assist.IsCall(assist.WithCall(t.Context())) {
		t.Error("WithCall must mark exactly the context it returns")
	}
}

// TestALocalProviderIsNeverGivenACredential: a key handed to Open is
// dropped for a local plan and used for a cloud one.
func TestALocalProviderIsNeverGivenACredential(t *testing.T) {
	if got := assist.KeyFor(assist.Plan{Class: assist.Local}, "sk-secret"); got != "" {
		t.Errorf("local plan keeps the key %q", got)
	}
	if got := assist.KeyFor(assist.Plan{Class: assist.Cloud}, "sk-secret"); got != "sk-secret" {
		t.Errorf("cloud plan drops the key: %q", got)
	}
}

// egress records every request the default HTTP client is asked to make
// and fails it, as a machine with no network would.
type egress struct {
	mu    sync.Mutex
	hosts []string
}

func (e *egress) RoundTrip(r *http.Request) (*http.Response, error) {
	e.mu.Lock()
	e.hosts = append(e.hosts, r.URL.Host)
	e.mu.Unlock()
	return nil, errors.New("connection refused")
}

// TestNothingElseIsTriedWhenTheLocalProviderFails: the one request a call
// makes goes to the configured endpoint. A failure does not make a
// second request, to this host or another, whatever the environment
// says about fallbacks and keys.
func TestNothingElseIsTriedWhenTheLocalProviderFails(t *testing.T) {
	for k, v := range map[string]string{"LLM_FALLBACK": "openai,anthropic", "OPENAI_API_KEY": "sk-x", "LLM_API_KEY": "sk-x", "LLM_BASE_URL": "https://api.openai.com"} {
		t.Setenv(k, v)
	}
	rec := &egress{}
	prev := http.DefaultClient.Transport
	http.DefaultClient.Transport = rec
	t.Cleanup(func() { http.DefaultClient.Transport = prev })

	p, err := plan(t, map[string]any{assist.KeyEndpoint: "http://127.0.0.1:9", assist.KeyModel: "m"})
	if err != nil {
		t.Fatal(err)
	}
	comp, closeFn, err := p.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	_, err = assist.Caller{Plan: p, LLM: comp}.Complete(t.Context(), []llm.Message{{Role: "user", Content: "x"}})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != output.ExitPrerequisite {
		t.Fatalf("err = %v, want exit 70", err)
	}
	if len(rec.hosts) != 1 || rec.hosts[0] != "127.0.0.1:9" {
		t.Fatalf("requests = %v, want exactly one, to the configured endpoint", rec.hosts)
	}
}
