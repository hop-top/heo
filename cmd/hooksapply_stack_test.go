package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/hooks"
	"hop.top/kit/go/console/cli"
)

// stackApp is the real app with a probe write added to its tree and
// the plugins granted a role, so a plugin call runs through the
// bridge `service start api` builds, over the app's own root.
func stackApp(t *testing.T, ran *[]string) *hookDispatcher {
	t.Helper()
	a := newApp()
	var mu sync.Mutex
	probe := &cobra.Command{Use: "probe"}
	add := &cobra.Command{
		Use: "add", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			mu.Lock()
			*ran = append(*ran, "ran")
			mu.Unlock()
			return nil
		},
	}
	add.Flags().String("idempotency-key", "", "")
	control.MarkDaemon(add)
	cli.SetSideEffect(add, cli.SideEffectWrite)
	cli.SetIdempotency(add, cli.IdempotencyConditional)
	probe.AddCommand(add)
	a.r.Cmd.AddCommand(probe)
	a.r.Viper.Set("authorization.grants", []any{
		map[string]any{"actor": "plugin:triage", "roles": []any{"desk"}},
		map[string]any{"actor": "plugin:other", "roles": []any{"desk"}},
	})
	return a.hookDispatcher()
}

var probeCall = hooks.Call{Verb: "probe add", Key: "k"}

// TestHookDispatcherAuditsThePluginActor: a plugin's operation reaches
// the audit sinks services.api.audit configures, attributed to the
// plugin.
func TestHookDispatcherAuditsThePluginActor(t *testing.T) {
	var ran []string
	chain := filepath.Join(t.TempDir(), "audit.chain")
	d := stackApp(t, &ran)
	d.a.r.Viper.Set("services.api.audit.sinks", []any{map[string]any{"type": "chain", "path": chain}})
	if err := d.Dispatch(context.Background(), "plugin:triage", probeCall); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Clean(chain))
	if err != nil {
		t.Fatalf("no audit chain written: %v", err)
	}
	if !strings.Contains(string(b), "plugin:triage") || !strings.Contains(string(b), "probe add") {
		t.Errorf("audit record does not name the plugin and the verb:\n%s", b)
	}
}

// TestHookDispatcherEnforcesThePolicy: a --policy that withholds writes
// refuses a plugin's write, permanently, and runs nothing.
func TestHookDispatcherEnforcesThePolicy(t *testing.T) {
	var ran []string
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	pol := filepath.Join(dir, "heo", "policies")
	if err := os.MkdirAll(pol, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pol, "readonly.yaml"),
		[]byte("name: readonly\nallow:\n  write: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := stackApp(t, &ran)
	if err := d.a.r.Cmd.PersistentFlags().Set("policy", "readonly"); err != nil {
		t.Fatal(err)
	}
	err := d.Dispatch(context.Background(), "plugin:triage", probeCall)
	if err == nil || !hooks.IsPermanent(err) {
		t.Fatalf("err = %v, want a permanent policy refusal", err)
	}
	if len(ran) != 0 {
		t.Errorf("a policy-refused write ran: %v", ran)
	}
}

// TestHookDispatcherRateLimitsPerPlugin: services.api.rate_limit
// throttles one plugin's writes without touching another's; a throttled
// call is retryable.
func TestHookDispatcherRateLimitsPerPlugin(t *testing.T) {
	var ran []string
	d := stackApp(t, &ran)
	v := d.a.r.Viper
	v.Set("services.api.rate_limit.enabled", true)
	v.Set("services.api.rate_limit.write.per_minute", 1)
	v.Set("services.api.rate_limit.write.burst", 1)
	ctx := context.Background()
	if err := d.Dispatch(ctx, "plugin:triage", probeCall); err != nil {
		t.Fatal(err)
	}
	err := d.Dispatch(ctx, "plugin:triage", probeCall)
	if err == nil || hooks.IsPermanent(err) {
		t.Fatalf("second call: err = %v, want a retryable rate limit", err)
	}
	if err := d.Dispatch(ctx, "plugin:other", probeCall); err != nil {
		t.Errorf("another plugin was throttled by the first: %v", err)
	}
	if len(ran) != 2 {
		t.Errorf("ran %d times, want 2", len(ran))
	}
}
