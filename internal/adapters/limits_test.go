package adapters_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/adapter/chat/discord"
	"hop.top/uma/adapter/chat/telegram"
	"hop.top/uma/adapter/ticket/github"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

func limitsRegistry(t *testing.T) *adapters.Registry {
	t.Helper()
	account := func(model.Channel) (string, error) { return "acct", nil }
	r := adapters.NewRegistry()
	gh := github.NewFactory(github.Options{})
	zd := zendesk.NewFactory()
	for _, a := range []adapters.Adapter{
		{Factory: gh, Account: account, Extensions: gh.Extensions()},
		{Factory: zd, Account: account, Extensions: zd.Extensions()},
	} {
		if err := r.Register(a); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func verbOf(t *testing.T, vs []adapters.VerbState, name string) adapters.VerbState {
	t.Helper()
	for _, v := range vs {
		if v.Verb == name {
			return v
		}
	}
	t.Fatalf("no verb %q", name)
	return adapters.VerbState{}
}

// TestVerbLimitsFromDescriptor: limits are what the adapter declares:
// the work item transitions, the shipped effects the verb runs; nothing
// for a verb the adapter does not declare.
func TestVerbLimitsFromDescriptor(t *testing.T) {
	r := limitsRegistry(t)
	gh := r.VerbStates(model.Channel{Type: github.Type})
	it := verbOf(t, gh, "conversation status")
	if it.State != adapters.VerbDeclared || len(it.Limits.Transitions) == 0 {
		t.Fatalf("issue transition: %+v", it)
	}
	for _, tr := range it.Limits.Transitions {
		if tr.To == "" || len(tr.From) == 0 {
			t.Errorf("transition %+v", tr)
		}
	}
	iu := verbOf(t, gh, "conversation update")
	if !slices.Equal(iu.Limits.Effects, []string{github.ExtensionUpdate}) {
		t.Errorf("conversation update effects: %+v", iu.Limits)
	}
	if lim := verbOf(t, gh, "conversation tag").Limits; !reflect.DeepEqual(lim, adapters.VerbLimits{}) {
		t.Errorf("unsupported verb carries limits: %+v", lim)
	}
	zd := r.VerbStates(model.Channel{Type: zendesk.Type})
	if got := verbOf(t, zd, "conversation update").Limits.Effects; !slices.Equal(got, []string{zendesk.ExtensionUpdate}) {
		t.Errorf("ticket transition effects: %v", got)
	}
}

// TestVerbLimitsReactions: a reaction verb reports the adapter's
// reaction cardinality, multiple when it declares none.
func TestVerbLimitsReactions(t *testing.T) {
	account := func(model.Channel) (string, error) { return "acct", nil }
	r := adapters.NewRegistry()
	for _, a := range []adapters.Adapter{
		{Factory: telegram.NewFactory(telegram.Options{}), Account: account},
		{Factory: discord.NewFactory(), Account: account},
	} {
		if err := r.Register(a); err != nil {
			t.Fatal(err)
		}
	}
	for typ, want := range map[model.TypeKey]string{telegram.Type: "single", discord.Type: "multiple"} {
		for _, name := range []string{"reaction add", "reaction remove"} {
			v := verbOf(t, r.VerbStates(model.Channel{Type: typ}), name)
			if v.State != adapters.VerbDeclared || v.Limits.Reactions != want {
				t.Errorf("%s %s: %+v, want reactions %q", typ, name, v, want)
			}
		}
		if got := verbOf(t, r.VerbStates(model.Channel{Type: typ}), "message edit").Limits.Reactions; got != "" {
			t.Errorf("%s message edit reports reactions %q", typ, got)
		}
	}
}

// TestVerbStateSchema pins the JSON shape of a verb row: the keys a
// caller may rely on.
func TestVerbStateSchema(t *testing.T) {
	r := limitsRegistry(t)
	keys := func(v any) []string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	gh := r.VerbStates(model.Channel{Type: github.Type})
	declared := verbOf(t, gh, "conversation status")
	if got, want := keys(declared), []string{"capability", "limits", "state", "verb"}; !slices.Equal(got, want) {
		t.Errorf("declared row keys %v, want %v", got, want)
	}
	if got, want := keys(declared.Limits), []string{"transitions"}; !slices.Equal(got, want) {
		t.Errorf("limits keys %v, want %v", got, want)
	}
	if got, want := keys(declared.Limits.Transitions[0]), []string{"from", "reasons", "to"}; !slices.Equal(got, want) {
		t.Errorf("transition keys %v, want %v", got, want)
	}
	if got, want := keys(verbOf(t, gh, "conversation update").Limits), []string{"effects"}; !slices.Equal(got, want) {
		t.Errorf("effects limits keys %v, want %v", got, want)
	}
	off := verbOf(t, gh, "conversation tag")
	if got, want := keys(off), []string{"capability", "detail", "limits", "reason", "state", "verb"}; !slices.Equal(got, want) {
		t.Errorf("unsupported row keys %v, want %v", got, want)
	}
	if got := keys(off.Limits); len(got) != 0 {
		t.Errorf("unsupported limits keys %v, want none", got)
	}
	raw, _ := json.Marshal(off.Limits)
	if string(raw) != "{}" {
		t.Errorf("empty limits encode as %s, want {}", raw)
	}
}

// TestRequiredPermissions: a channel reports what its adapter's
// descriptor lists as required permissions, always a slice: empty for an
// adapter that lists none or one that is not in the build.
func TestRequiredPermissions(t *testing.T) {
	r := limitsRegistry(t)
	got := r.RequiredPermissions(model.Channel{Type: github.Type})
	if want := github.NewFactory(github.Options{}).Describe().RequiredPermissions; len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("github permissions %v, want %v", got, want)
	}
	// The result is a copy: editing it leaves the descriptor alone.
	got[0] = "mutated"
	if again := r.RequiredPermissions(model.Channel{Type: github.Type}); again[0] == "mutated" {
		t.Error("RequiredPermissions aliases the descriptor")
	}
	missing := r.RequiredPermissions(model.Channel{Type: "no.such"})
	if missing == nil || len(missing) != 0 {
		t.Errorf("missing adapter permissions = %#v, want empty non-nil", missing)
	}
}
