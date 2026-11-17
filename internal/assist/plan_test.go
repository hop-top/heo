package assist_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/assist"
	"hop.top/kit/go/console/output"
)

// config builds the settings a profile resolves to: the built-in
// defaults with set on top, flat keys.
func config(t *testing.T, set map[string]any) *viper.Viper {
	t.Helper()
	v := viper.New()
	for k, val := range assist.Defaults() {
		v.SetDefault(k, val)
	}
	for k, val := range set {
		v.Set(k, val)
	}
	return v
}

func plan(t *testing.T, set map[string]any) (assist.Plan, error) {
	t.Helper()
	return assist.ConfigFrom(config(t, set)).Plan()
}

func wantPrerequisite(t *testing.T, err error, contains ...string) *output.Error {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != output.ExitPrerequisite {
		t.Fatalf("err = %v, want a PREREQUISITE error (exit 70)", err)
	}
	for _, c := range contains {
		if !strings.Contains(oe.Message+" "+oe.SuggestedFix, c) {
			t.Errorf("error %q (fix %q) lacks %q", oe.Message, oe.SuggestedFix, c)
		}
	}
	if oe.SuggestedFix == "" {
		t.Errorf("error %q has no suggested fix", oe.Message)
	}
	return oe
}

// TestDefaultIsALocalModel: out of the box the helpers talk to Ollama
// on this machine, a placeholder model the user sets, and no cloud.
func TestDefaultIsALocalModel(t *testing.T) {
	p, err := plan(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Provider != "ollama" || p.Class != assist.Local || p.Endpoint != "http://127.0.0.1:11434" ||
		p.Host != "127.0.0.1:11434" || p.Model != assist.DefaultModel || p.Timeout != 2*time.Minute || p.CredentialRef != "" {
		t.Fatalf("default plan = %+v", p)
	}
}

// TestAutoNeverResolvesToACloudProvider: whatever the environment holds,
// whatever is opted in, "auto" is the local default or an error, and
// no key is looked at.
func TestAutoNeverResolvesToACloudProvider(t *testing.T) {
	for k, v := range map[string]string{
		"LLM_PROVIDER": "openai", "LLM_BASE_URL": "https://api.openai.com", "LLM_API_KEY": "sk-secret",
		"LLM_FALLBACK": "openai", "OPENAI_API_KEY": "sk-secret", "ANTHROPIC_API_KEY": "sk-secret",
		"OLLAMA_API_KEY": "secret", "HEO_LLM_PROVIDER": "openai",
	} {
		t.Setenv(k, v)
	}
	p, err := plan(t, map[string]any{
		assist.KeyProvider:         "auto",
		"llm.cloud.ollama.enabled": true, "llm.cloud.openai.enabled": true,
		"llm.cloud.ollama.credential_ref": "ollama-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Provider != "ollama" || p.Class != assist.Local || p.Host != "127.0.0.1:11434" || p.CredentialRef != "" {
		t.Fatalf("auto = %+v, want the local default with no credential", p)
	}

	// auto with a non-local endpoint is refused, even when that provider
	// is opted in: auto does not follow an endpoint off the machine.
	_, err = plan(t, map[string]any{
		assist.KeyProvider: "auto", assist.KeyEndpoint: "http://ollama.example.com",
		"llm.cloud.ollama.enabled": true,
	})
	_ = wantPrerequisite(t, err, "auto", "ollama.example.com")
}

// TestCloudNeedsAnExplicitOptIn: a provider that is not on this machine
// runs only when it is named and opted in; the error says which key.
func TestCloudNeedsAnExplicitOptIn(t *testing.T) {
	cloud := map[string]any{assist.KeyProvider: "ollama", assist.KeyEndpoint: "https://ollama.example.com"}
	_, err := plan(t, cloud)
	_ = wantPrerequisite(t, err, "llm.cloud.ollama.enabled", "ollama.example.com")

	opted := map[string]any{"llm.cloud.ollama.enabled": true, "llm.cloud.ollama.credential_ref": "ollama-key"}
	for k, v := range cloud {
		opted[k] = v
	}
	p, err := plan(t, opted)
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != assist.Cloud || p.Host != "ollama.example.com" || p.CredentialRef != "ollama-key" {
		t.Fatalf("opted-in plan = %+v", p)
	}

	// The opt-in is per provider and does not make a local one cloud.
	p, err = plan(t, map[string]any{"llm.cloud.ollama.enabled": true})
	if err != nil || p.Class != assist.Local || p.CredentialRef != "" {
		t.Fatalf("a local provider with an opt-in set = %+v, %v", p, err)
	}
}

// TestNoProviderIsInventedOrSubstituted: an unknown provider, an empty
// model and a bad endpoint are errors that name the key, never a
// different provider.
func TestNoProviderIsInventedOrSubstituted(t *testing.T) {
	_, err := plan(t, map[string]any{assist.KeyProvider: "nosuch"})
	_ = wantPrerequisite(t, err, "llm.provider", "ollama")
	_, err = plan(t, map[string]any{assist.KeyModel: " "})
	_ = wantPrerequisite(t, err, "llm.model")
	_, err = plan(t, map[string]any{assist.KeyTimeout: "soon"})
	_ = wantPrerequisite(t, err, "llm.timeout")
	_, err = plan(t, map[string]any{assist.KeyMaxPromptChars: 10})
	_ = wantPrerequisite(t, err, "llm.max_prompt_chars")
	for _, bad := range []string{"not a url", "ftp://127.0.0.1", "http://user:pw@127.0.0.1:1", "http://127.0.0.1:1?x=y", "http://127.0.0.1:1/ a"} {
		_, err = plan(t, map[string]any{assist.KeyEndpoint: bad})
		oe := wantPrerequisite(t, err, "llm.endpoint")
		if strings.Contains(oe.Message, "pw") {
			t.Errorf("error for %q echoes the password: %s", bad, oe.Message)
		}
	}
}

func TestEndpointOverrideStaysLocalOnLoopbackAndLAN(t *testing.T) {
	for _, ep := range []string{"http://127.0.0.1:11500", "http://localhost:8080/", "http://[::1]:11434", "http://192.168.1.20:11434", "http://10.1.2.3:11434"} {
		p, err := plan(t, map[string]any{assist.KeyEndpoint: ep})
		if err != nil || p.Class != assist.Local {
			t.Errorf("%s = %+v, %v, want a local plan", ep, p, err)
		}
	}
	if p, _ := plan(t, map[string]any{assist.KeyEndpoint: "http://localhost:8080/"}); p.Endpoint != "http://localhost:8080" {
		t.Errorf("trailing slash kept: %q", p.Endpoint)
	}
}

func TestClassOfHost(t *testing.T) {
	local := []string{"127.0.0.1:11434", "127.1.2.3", "[::1]:80", "::1", "localhost", "localhost:11434", "a.localhost",
		"10.0.0.1", "172.16.0.1:1", "172.31.255.255", "192.168.0.1", "169.254.1.1", "fd00::1"}
	cloud := []string{"ollama.com", "api.openai.com:443", "8.8.8.8", "172.32.0.1", "100.64.0.1", "gpu-box.lan", "gpu-box", "0.0.0.0", "evil.localhost.example.com", ""}
	for _, h := range local {
		if got := assist.ClassOfHost(h); got != assist.Local {
			t.Errorf("ClassOfHost(%q) = %s, want local", h, got)
		}
	}
	for _, h := range cloud {
		if got := assist.ClassOfHost(h); got != assist.Cloud {
			t.Errorf("ClassOfHost(%q) = %s, want cloud", h, got)
		}
	}
}

// TestCheckFlagsWhatWillNotRun: config validate sees the same errors as
// a call, plus keys heo does not read and opt-ins that select nothing.
func TestCheckFlagsWhatWillNotRun(t *testing.T) {
	if got := assist.Check(config(t, nil)); len(got) != 0 {
		t.Fatalf("defaults: %+v", got)
	}
	got := assist.Check(config(t, map[string]any{
		assist.KeyEndpoint: "https://ollama.example.com", "llm.apikey": "x", "llm.cloud.openai.enabled": true,
		"llm.cloud.ollama.bogus": 1,
	}))
	want := map[string]bool{"llm.cloud.ollama.enabled": false, "llm.apikey": false, "llm.cloud.ollama.bogus": false, "llm.cloud.openai.enabled": true}
	for _, p := range got {
		w, ok := want[p.Key]
		if !ok || p.Warning != w {
			t.Errorf("problem %+v unexpected", p)
		}
		delete(want, p.Key)
	}
	if len(want) != 0 {
		t.Errorf("no problem reported for %v (got %+v)", want, got)
	}
}

// TestCloudKeyNeverGoesOverPlainHTTP: a cloud endpoint over http is
// refused once a key would ride on it, before any request is made.
func TestCloudKeyNeverGoesOverPlainHTTP(t *testing.T) {
	p, err := plan(t, map[string]any{
		assist.KeyProvider: "ollama", assist.KeyEndpoint: "http://ollama.example.com",
		"llm.cloud.ollama.enabled": true, "llm.cloud.ollama.credential_ref": "ollama-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Open("secret-key"); err != nil {
		oe := wantPrerequisite(t, err, "plain http", assist.KeyEndpoint)
		if strings.Contains(oe.Message+oe.SuggestedFix, "secret-key") {
			t.Errorf("error %q leaks the key", oe.Message)
		}
		return
	}
	t.Fatal("a key went to a cloud endpoint over plain http")
}

// TestNamedProviderIsCloudWhateverItsAddress: a provider declared with
// llm.cloud.<name>.type is a cloud provider even on a private address,
// needs its own opt-in, takes its endpoint from its section, and keys
// its credential by its name.
func TestNamedProviderIsCloudWhateverItsAddress(t *testing.T) {
	set := map[string]any{
		assist.KeyProvider:                "router",
		assist.KeyModel:                   "team/router:fast",
		"llm.cloud.router.type":           "openai",
		"llm.cloud.router.endpoint":       "http://10.0.0.5:4000/v1",
		"llm.cloud.router.credential_ref": "router-key",
	}
	_ = wantPrerequisite(t, mustFail(t, set), "llm.cloud.router.enabled")
	set["llm.cloud.router.enabled"] = true
	p, err := plan(t, set)
	if err != nil {
		t.Fatal(err)
	}
	if p.Provider != "openai" || p.Name != "router" || p.Class != assist.Cloud || p.Host != "10.0.0.5:4000" ||
		p.Endpoint != "http://10.0.0.5:4000/v1" || p.CredentialRef != "router-key" || p.Model != "team/router:fast" {
		t.Fatalf("plan = %+v", p)
	}
	set["llm.cloud.router.type"] = "nosuch"
	_ = wantPrerequisite(t, mustFail(t, set), "llm.cloud.router.type", "nosuch")
}

func mustFail(t *testing.T, set map[string]any) error {
	t.Helper()
	_, err := plan(t, set)
	if err == nil {
		t.Fatal("plan succeeded, want an error")
	}
	return err
}
