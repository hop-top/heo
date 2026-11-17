package assist_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/assist"
	"hop.top/kit/go/ai/llm"
	"hop.top/kit/go/console/output"
)

// routerCaller is a caller on the real OpenAI-compatible adapter,
// pointed at srv.
func routerCaller(t *testing.T, srv *httptest.Server) assist.Caller {
	t.Helper()
	p, err := plan(t, map[string]any{
		assist.KeyProvider:                "router",
		assist.KeyModel:                   "m",
		"llm.cloud.router.type":           "openai",
		"llm.cloud.router.endpoint":       srv.URL + "/v1",
		"llm.cloud.router.credential_ref": "router-key",
		"llm.cloud.router.enabled":        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	comp, closeFn, err := p.Open("sk-x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFn)
	return assist.Caller{Plan: p, LLM: comp}
}

func TestProviderRetryAfterIsSurfacedAndNotRetried(t *testing.T) {
	future := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	cases := []struct {
		name, header string
		status       int
		want         string // "" = no delay surfaced
	}{
		{"seconds on 429", "30", 429, "30s"},
		{"http-date on 429", future, 429, "1m"},
		{"seconds on 503", "120", 503, "2m0s"},
		{"http-date on 503", future, 503, "1m"},
		{"negative", "-5", 429, ""},
		{"zero", "0", 429, ""},
		{"garbage", "soon", 429, ""},
		{"fractional", "1.5", 429, ""},
		{"date in the past", "Mon, 02 Jan 2006 15:04:05 GMT", 429, ""},
		{"absent", "", 429, ""},
		{"absurd", "99999999999", 429, "1h0m0s"},
		{"overflowing", "99999999999999999999999", 503, "1h0m0s"},
	}
	for _, c := range cases {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if c.header != "" {
				w.Header().Set("Retry-After", c.header)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
		}))
		_, err := routerCaller(t, srv).Complete(t.Context(), []llm.Message{{Role: "user", Content: "x"}})
		srv.Close()
		var oe *output.Error
		if !errors.As(err, &oe) || oe.ExitCode != output.ExitTransient {
			t.Errorf("%s: err = %#v, want TRANSIENT exit 6", c.name, err)
			continue
		}
		if hits.Load() != 1 {
			t.Errorf("%s: %d requests, want 1", c.name, hits.Load())
		}
		got := strings.TrimPrefix(oe.Cause, "retry_after=")
		if c.want == "" {
			if oe.Cause != "" || strings.Contains(oe.SuggestedFix, "after") {
				t.Errorf("%s: cause %q fix %q, want no delay", c.name, oe.Cause, oe.SuggestedFix)
			}
			continue
		}
		if c.want == "1m" { // an HTTP-date is relative to now: accept 1m0s +- a few seconds
			d, perr := time.ParseDuration(got)
			if perr != nil || d < 80*time.Second || d > 90*time.Second {
				t.Errorf("%s: cause %q, want about 90s", c.name, oe.Cause)
			}
		} else if got != c.want {
			t.Errorf("%s: cause %q, want retry_after=%s", c.name, oe.Cause, c.want)
		}
		if !strings.Contains(oe.SuggestedFix, "retry after "+got) {
			t.Errorf("%s: fix %q lacks the delay %s", c.name, oe.SuggestedFix, got)
		}
	}
}
