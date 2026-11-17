package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRouter is an in-process server speaking the OpenAI chat API, the
// way a routing server in front of several models does. It records
// what each request carried.
type fakeRouter struct {
	srv *httptest.Server

	mu    sync.Mutex
	auth  []string
	paths []string
	model []string
	// status, when not 200, is what every request is answered with,
	// with a Retry-After.
	status int
}

func newFakeRouter(t *testing.T) *fakeRouter {
	t.Helper()
	f := &fakeRouter{status: http.StatusOK}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		f.model = append(f.model, req.Model)
		status := f.status
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"no capacity","type":"server_error"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","created":1,"model":%q,`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"a duplicate charge, refund it"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, req.Model)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRouter) setStatus(s int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
}

func (f *fakeRouter) seen() (auth, paths, models []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...), append([]string(nil), f.paths...), append([]string(nil), f.model...)
}

// TestLLMNamedOpenAIProvider: a routing server that speaks the OpenAI
// chat API is a named cloud provider (llm.cloud.<name>.type). The model
// name reaches it unchanged, the bearer key is the one in the credential
// store under the provider's reference, and a 503 with Retry-After is a
// retryable error answered by one request, not retried by heo.
func TestLLMNamedOpenAIProvider(t *testing.T) {
	withE2EBinary(t, func() {
		f := newFakeRouter(t)
		const model = "team/router:fast-v2"
		cfg := fmt.Sprintf("  all:\n    timeouts:\n      command: 5m\nllm:\n  provider: router\n  model: %q\n  timeout: 60s\n"+
			"  cloud:\n    router:\n      type: openai\n      endpoint: %q\n      enabled: true\n      credential_ref: router-key\n", model, f.srv.URL+"/v1")
		l := newLLMSiteEnv(t, cfg, "", nil)
		sum := []string{"conversation", "summarize", l.conv}
		withFlag := func(a ...string) []string { return append(append([]string{}, a...), "--send-to-cloud") }

		// The provider is a cloud one by declaration, whatever its address.
		p := l.dryRun(sum...)
		if argOf(t, p, "provider") != "openai" || argOf(t, p, "class") != "cloud" || argOf(t, p, "model") != model {
			t.Errorf("preview = %+v", p.Args)
		}
		if r := l.s.run(nil, "config", "validate"); r.exit != 0 {
			t.Errorf("config validate (exit %d):\n%s%s", r.exit, r.stdout, r.stderr)
		}

		// The key is in the credential store.
		key, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": "sk-router-0123456789"})
		l.s.write("secrets/uma/default/router-key", string(key))

		r := l.heo(0, withFlag(sum...)...)
		if !strings.Contains(r.stdout, "duplicate charge") {
			t.Errorf("summary lacks the model's answer:\n%s", r.stdout)
		}
		auth, paths, models := f.seen()
		if len(auth) != 1 || auth[0] != "Bearer sk-router-0123456789" {
			t.Errorf("Authorization = %q, want one request with the stored key as a bearer token", auth)
		}
		if len(paths) != 1 || paths[0] != "/v1/chat/completions" || len(models) != 1 || models[0] != model {
			t.Errorf("paths = %q models = %q, want /v1/chat/completions with model %q unchanged", paths, models, model)
		}

		// 503 with Retry-After: retryable (exit 6), one request.
		f.setStatus(http.StatusServiceUnavailable)
		r = l.heo(6, withFlag(sum...)...)
		wantStderr(t, r, "TRANSIENT", "503")
		if auth, _, _ = f.seen(); len(auth) != 2 {
			t.Errorf("a 503 made %d requests, want 1: heo does not retry", len(auth)-1)
		}
	})
}
