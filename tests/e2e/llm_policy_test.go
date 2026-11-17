package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	xrr "hop.top/xrr"
)

// The LLM helpers' rules, tested without a model: what they refuse,
// what a preview shows, and that nothing leaves for a provider the
// configuration did not choose. The daemon here replays an empty
// cassette strictly, so any HTTP request it makes is a miss the test
// reads: a request that reached a provider is a failed test.

// strictEnv is the environment of a daemon that may make no model call:
// replay of an empty cassette, with a miss log. extra is added.
func strictEnv(t *testing.T, s *sandbox, extra ...string) (env []string, missLog string) {
	t.Helper()
	missLog = s.path("xrr-miss-llm.log")
	env = []string{xrr.EnvMode + "=" + string(xrr.ModeReplay), xrr.EnvCassetteDir + "=" + t.TempDir(), "HEO_E2E_XRR_MISS_LOG=" + missLog}
	return append(env, extra...), missLog
}

// misses is what the daemon's replay found no cassette for.
func misses(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own sandbox
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// newStrictLLMSite is the synthetic desk with a daemon that makes no
// model call, configured with extra (the llm section and the rest).
func newStrictLLMSite(t *testing.T, extra string, envExtra ...string) (*llmSite, string) {
	t.Helper()
	// The site is built before the helper that needs its sandbox: the
	// sandbox is the one thing both share.
	c := newCLISite(t, extra)
	env, miss := strictEnv(t, c.s, envExtra...)
	l := &llmSite{cliSite: c, env: env}
	l.d = l.startDaemon()
	l.seed()
	return l, miss
}

// dryRun runs a helper under --dry-run and decodes its plan.
func (l *llmSite) dryRun(args ...string) planJSON {
	l.t.Helper()
	var p planJSON
	l.json(&p, append(args, "--dry-run")...)
	return p
}

func argOf(t *testing.T, p planJSON, key string) string {
	t.Helper()
	raw, ok := p.Args[key]
	if !ok {
		t.Fatalf("plan has no %q: %+v", key, p.Args)
	}
	return strings.Trim(string(raw), `"`)
}

// TestLLMPreviewSendsNothingAndRolesGate: --dry-run reads the
// conversation as the caller and says where a call would go and what it
// would carry, and sends nothing (the daemon replays an empty cassette
// and nothing was missed); the roles decide who may run a helper and
// which audience it reads: a viewer's preview leaves the note out, an
// operator alone and an external actor are refused, and a viewer may not
// draft.
func TestLLMPreviewSendsNothingAndRolesGate(t *testing.T) {
	withE2EBinary(t, func() {
		l, miss := newStrictLLMSite(t, llmConfig("http://127.0.0.1:1"))
		before := l.counts()

		// The desk.
		for _, args := range [][]string{
			{"conversation", "summarize", l.conv}, {"conversation", "triage", l.conv}, {"message", "draft", l.conv},
		} {
			p := l.dryRun(args...)
			if len(p.Effects) != 1 || p.Effects[0].Kind != "send" || !strings.HasPrefix(p.Effects[0].Target, "llm:ollama@127.0.0.1:1") ||
				!strings.Contains(p.Effects[0].Detail, "a preview sends nothing") {
				t.Errorf("%v preview effects = %+v", args, p.Effects)
			}
			wantCount, wantAudience := "4", "desk"
			if args[0] == "message" { // a draft reads the public messages
				wantCount, wantAudience = "3", "public"
			}
			if argOf(t, p, "provider") != "ollama" || argOf(t, p, "model") != llmModel || argOf(t, p, "class") != "local" ||
				argOf(t, p, "endpoint") != "127.0.0.1:1" || argOf(t, p, "message_count") != wantCount || argOf(t, p, "audience") != wantAudience ||
				argOf(t, p, "truncated") != "false" || argOf(t, p, "prompt_chars") == "0" {
				t.Errorf("%v preview = %+v", args, p.Args)
			}
		}
		r := l.heo(0, "conversation", "summarize", l.conv, "--dry-run")
		for _, want := range []string{"provider", "ollama", "class", "local", "message count", "prompt chars", "a preview sends nothing"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text preview lacks %q:\n%s", want, r.stdout)
			}
		}
		if after := l.counts(); after != before {
			t.Errorf("a preview changed the conversation: %+v -> %+v", before, after)
		}

		// A viewer reads the public messages only, and may not draft.
		l.restartAs("authorization:\n  owner: [viewer]\n")
		for _, args := range [][]string{{"conversation", "summarize", l.conv}, {"conversation", "triage", l.conv}} {
			p := l.dryRun(args...)
			if argOf(t, p, "audience") != "public" || argOf(t, p, "message_count") != "3" {
				t.Errorf("viewer %v preview = %+v", args, p.Args)
			}
		}
		wantStderr(t, l.heo(5, "message", "draft", l.conv, "--dry-run"), "UNAUTHORIZED", "message.reply")

		// An operator alone reads no content; nor does an external actor.
		for _, roles := range []string{"operator", "external"} {
			l.restartAs("authorization:\n  owner: [" + roles + "]\n")
			for _, args := range [][]string{
				{"conversation", "summarize", l.conv, "--dry-run"}, {"conversation", "triage", l.conv, "--dry-run"},
				{"message", "draft", l.conv, "--dry-run"}, {"message", "draft", l.conv},
			} {
				wantStderr(t, l.heo(5, args...), "UNAUTHORIZED", "heo.authz")
			}
		}

		// No role, no helper.
		l.restartAs("authorization:\n  owner: []\n")
		wantStderr(t, l.heo(5, "conversation", "summarize", l.conv), "UNAUTHORIZED")

		if m := misses(t, miss); m != "" {
			t.Errorf("a helper reached for a provider:\n%s", m)
		}
	})
}

// TestLLMNoFallbackWhenTheLocalModelIsDown: with the local server not
// answering, a helper fails with exit 70 and says how to fix it; the
// daemon tries nothing else, whatever the environment says about other
// providers. The daemon replays an empty cassette, so every request it
// makes is a miss: only the local one is there.
func TestLLMNoFallbackWhenTheLocalModelIsDown(t *testing.T) {
	withE2EBinary(t, func() {
		noise := []string{
			"LLM_PROVIDER=openai", "LLM_FALLBACK=openai,anthropic", "LLM_BASE_URL=https://api.openai.com/v1",
			"LLM_API_KEY=sk-should-never-be-read", "OPENAI_API_KEY=sk-should-never-be-read",
			"ANTHROPIC_API_KEY=sk-should-never-be-read", "OLLAMA_API_KEY=should-never-be-read",
		}
		l, miss := newStrictLLMSite(t, llmConfig("http://127.0.0.1:1"), noise...)

		// Configuration ignores all of it: the provider is the local default.
		if got := strings.TrimSpace(l.s.run(noise, "config", "get", "llm.provider").stdout); got != "ollama" {
			t.Errorf("llm.provider = %q with kit's LLM_* variables set, want the local default", got)
		}
		for _, args := range [][]string{{"conversation", "summarize", l.conv}, {"conversation", "triage", l.conv}, {"message", "draft", l.conv}} {
			r := l.heo(70, args...)
			wantStderr(t, r, "PREREQUISITE", "ollama at 127.0.0.1:1 is not reachable", "does not fall back", "llm.endpoint")
		}
		m := misses(t, miss)
		if !strings.Contains(m, "loopback.invalid") && !strings.Contains(m, "127.0.0.1") {
			t.Errorf("the local request is missing from the miss log:\n%s", m)
		}
		for _, line := range strings.Split(strings.TrimSpace(m), "\n") {
			if !strings.Contains(line, "POST http://127.0.0.1:1/api/chat:") {
				t.Errorf("a request went somewhere but the configured local endpoint:\n%s", line)
			}
		}
		if n := strings.Count(m, "\n"); n != 3 {
			t.Errorf("%d requests for 3 calls, want one each (no retry, no second provider):\n%s", n, m)
		}
		if log := l.d.log(); strings.Contains(log, "sk-should-never-be-read") {
			t.Error("the daemon log carries a key from the environment")
		}
	})
}

// TestLLMLocalServerNotRunning: against a port nothing listens on, with
// no cassette involved, the error is the same corrective one.
func TestLLMLocalServerNotRunning(t *testing.T) {
	withE2EBinary(t, func() {
		l := newLLMSite(t, llmConfig("http://"+freeAddr(t)), "")
		wantStderr(t, l.heo(70, "conversation", "summarize", l.conv), "PREREQUISITE", "is not reachable", "does not fall back", "llm.endpoint")
		var e struct {
			Code         string
			ExitCode     int    `json:"exit_code"`
			SuggestedFix string `json:"suggested_fix"`
		}
		r := l.s.run(nil, "conversation", "summarize", l.conv, "--format", "json")
		l.s.want(r, 70, "conversation", "summarize")
		if err := json.Unmarshal([]byte(r.stderr), &e); err != nil || e.Code != "PREREQUISITE" || e.ExitCode != 70 || e.SuggestedFix == "" {
			t.Errorf("structured error = %+v (%v)\n%s", e, err, r.stderr)
		}
	})
}

// TestLLMCloudNeedsOptInAndConsent: a provider off this machine is
// refused until the configuration opts in to it by name; once it has, a
// call is refused until it says --send-to-cloud; the credential is
// resolved only then; and `auto` never goes there. The endpoint is a
// name no request reaches: the daemon replays an empty cassette.
func TestLLMCloudNeedsOptInAndConsent(t *testing.T) {
	withE2EBinary(t, func() {
		cloud := "llm:\n  model: " + llmModel + "\n  endpoint: https://ollama.example.com\n"
		l, miss := newStrictLLMSite(t, cloud)
		sum := []string{"conversation", "summarize", l.conv}

		// Not opted in: every form is refused, naming the key.
		for _, args := range [][]string{sum, append(append([]string{}, sum...), "--dry-run"), append(append([]string{}, sum...), "--send-to-cloud")} {
			wantStderr(t, l.heo(70, args...), "PREREQUISITE", "llm.cloud.ollama.enabled", "ollama.example.com")
		}
		if r := l.s.run(nil, "config", "validate"); r.exit != 2 || !strings.Contains(r.stdout, "llm.cloud.ollama.enabled") || !strings.Contains(r.stdout, "ollama.example.com") {
			t.Errorf("config validate must report the missing opt-in (exit %d):\n%s", r.exit, r.stdout)
		}

		// auto does not follow an endpoint off the machine, opted in or not.
		l.restartAs("llm:\n  provider: auto\n  cloud:\n    ollama:\n      enabled: true\n")
		wantStderr(t, l.heo(70, sum...), "PREREQUISITE", "auto")

		// Opted in, with a credential reference.
		l.restartAs("llm:\n  cloud:\n    ollama:\n      enabled: true\n      credential_ref: ollama-key\n")
		p := l.dryRun(sum...)
		if argOf(t, p, "class") != "cloud" || argOf(t, p, "endpoint") != "ollama.example.com" || len(p.Effects) != 1 {
			t.Errorf("cloud preview = %+v", p)
		}
		r := l.heo(0, append(append([]string{}, sum...), "--dry-run")...)
		if !strings.Contains(r.stdout, "cloud") || !strings.Contains(r.stdout, "leave this machine") {
			t.Errorf("a cloud preview must say the text would leave:\n%s", r.stdout)
		}
		// --offline: a cloud call needs the network.
		wantStderr(t, l.heo(2, append(append([]string{}, sum...), "--send-to-cloud", "--offline")...), "USAGE", "--offline", "ollama.example.com")
		// No consent for this call: refused, exit 7, before any credential
		// is read or request made.
		wantStderr(t, l.heo(7, sum...), "CONSENT_REFUSED", "cloud provider", "--send-to-cloud")
		// Consent, but the credential is not there.
		wantStderr(t, l.heo(70, append(append([]string{}, sum...), "--send-to-cloud")...), "PREREQUISITE", "ollama-key", "llm.ollama", "does not resolve")
		if m := misses(t, miss); m != "" {
			t.Errorf("a request left before consent and a credential:\n%s", m)
		}
		// Consent and a credential: the call is made, and the strict cassette
		// misses it, which is how the test sees it left, for the host the
		// configuration named and no other.
		key, _ := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": strings.Repeat("k", 32)})
		l.s.write("secrets/uma/default/ollama-key", string(key))
		wantStderr(t, l.heo(70, append(append([]string{}, sum...), "--send-to-cloud")...), "PREREQUISITE", "ollama.example.com", "does not fall back")
		if m := misses(t, miss); !strings.Contains(m, "ollama.example.com") || strings.Count(m, "\n") != 1 {
			t.Errorf("the one request should be to the opted-in host:\n%s", m)
		}
	})
}

// TestLLMServedSurfaces: the helpers are served like other reads. Over
// rpc a preview reads as the caller and sends nothing; over REST (a GET:
// kit carries no --dry-run on a read) a call with the local model down
// fails with the same corrective error; and a cloud provider is refused
// on both until the call itself carries send-to-cloud.
func TestLLMServedSurfaces(t *testing.T) {
	withE2EBinary(t, func() {
		l, miss := newStrictLLMSite(t, llmConfig("http://127.0.0.1:1"))
		sum := "/v1/commands/conversation/summarize?arg=" + url.QueryEscape(l.conv)
		auth := l.s.useHTTPAuth()
		serve := func() (*surfaces, func()) {
			if l.d != nil {
				l.stop(l.d)
				l.d = nil
			}
			api, rpcAddr := freeAddr(t), freeAddr(t)
			env, _ := strictEnv(t, l.s)
			env = append(env, "HEO_E2E_XRR_MISS_LOG="+miss)
			d := l.s.start(env, nil, []string{"socket", "workers", "receivers", "api", "rpc"}, "-c", auth.config,
				"service", "start", "--enable", "api", "--enable", "rpc", "--addr", api, "--rpc-addr", rpcAddr)
			l.d = d
			return &surfaces{auth: auth, api: api, rpc: rpcAddr}, func() {
				l.s.want(result{exit: d.term()}, 0)
				l.d = nil
			}
		}
		rpc := func(x *surfaces, flags map[string]any) (int, map[string]any) {
			return x.invokeRPC(t, auth.token(me()), map[string]any{"path": []string{"conversation", "summarize"}, "args": []string{l.conv}, "flags": flags})
		}

		// The local provider.
		x, stop := serve()
		code, body := rpc(x, map[string]any{"dry-run": true})
		data, _ := body["data"].(map[string]any)
		args, _ := data["args"].(map[string]any)
		if code != http.StatusOK || args["class"] != "local" || args["audience"] != "desk" {
			t.Fatalf("rpc preview = %d %v", code, body)
		}
		if code, body = x.rest(t, http.MethodGet, sum, "", ""); code != http.StatusUnauthorized {
			t.Errorf("unauthenticated = %d %v", code, body)
		}
		if code, body = x.rest(t, http.MethodGet, sum, auth.token(me()), ""); code < 400 || !strings.Contains(toJSON(body), "does not fall back") {
			t.Errorf("REST call with the local model down = %d %v", code, body)
		}
		stop()

		// A cloud provider the configuration opted in to.
		l.s.write("cfg/heo/profiles/default.yaml",
			"llm:\n  endpoint: https://ollama.example.com\n  cloud:\n    ollama:\n      enabled: true\n      credential_ref: ollama-key\n")
		x, stop = serve()
		defer stop()
		code, body = rpc(x, map[string]any{"dry-run": true})
		data, _ = body["data"].(map[string]any)
		args, _ = data["args"].(map[string]any)
		if code != http.StatusOK || args["class"] != "cloud" {
			t.Fatalf("rpc cloud preview = %d %v", code, body)
		}
		if code, body = rpc(x, nil); body["exit_code"] != float64(7) || !strings.Contains(toJSON(body), "--send-to-cloud") {
			t.Errorf("rpc cloud call without consent = %d %v", code, body)
		}
		if code, body = x.rest(t, http.MethodGet, sum, auth.token(me()), ""); code < 400 || !strings.Contains(toJSON(body), "--send-to-cloud") {
			t.Errorf("REST cloud call without consent = %d %v", code, body)
		}
		// With consent the call gets as far as the credential, which is not stored.
		if code, body = x.rest(t, http.MethodGet, sum+"&send-to-cloud=true", auth.token(me()), ""); code < 400 || !strings.Contains(toJSON(body), "does not resolve") {
			t.Errorf("REST cloud call with consent = %d %v", code, body)
		}
		if m := misses(t, miss); strings.Contains(m, "example.com") || strings.Count(m, "\n") != 1 {
			t.Errorf("only the local request should have been attempted:\n%s", m)
		}
	})
}
