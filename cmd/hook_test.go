package cmd

import (
	"strings"
	"testing"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/hooks"
	"hop.top/kit/go/console/cli"
)

// TestHookTestVerdict: a test run succeeds only when delivering the
// event would: the plugin is admitted, answers, and its result would be
// applied.
func TestHookTestVerdict(t *testing.T) {
	ok := hooks.TestReport{Plugin: "p", Event: "e", Admitted: true, Ok: true, WouldApply: true}
	for name, tc := range map[string]struct {
		r    hooks.TestReport
		want string
	}{
		"ok":       {ok, ""},
		"skipped":  {hooks.TestReport{Plugin: "p", Event: "e", Skipped: "outside the filter"}, "would not be delivered"},
		"failed":   {hooks.TestReport{Plugin: "p", Event: "e", Admitted: true, Failure: &hooks.TestFailure{Class: hooks.ClassTimeout, Message: "slow"}}, "failed on event e (timeout): slow"},
		"rejected": {hooks.TestReport{Plugin: "p", Event: "e", Admitted: true, Ok: true, Rejected: "operations[0] x: no"}, "would be rejected: operations[0] x: no"},
	} {
		got := testVerdict(tc.r)
		if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
			t.Errorf("%s: verdict %q, want %q", name, got, tc.want)
		}
	}
}

func TestDeadStateOf(t *testing.T) {
	for in, want := range map[string]hooks.DeadState{"dead": hooks.StateDead, "replayed": hooks.StateReplayed, "discarded": hooks.StateDiscarded, "all": ""} {
		if got, err := deadStateOf(in); err != nil || got != want {
			t.Errorf("%q: %q, %v", in, got, err)
		}
	}
	if _, err := deadStateOf("nope"); err == nil {
		t.Error("an unknown state was accepted")
	}
}

// TestDropPlan: a drop previews the discard of a dead letter, and warns
// that one already resolved would be refused.
func TestDropPlan(t *testing.T) {
	p := dropPlan(hooks.DeadRecord{Plugin: "p", Event: "e", State: hooks.StateDead})
	if len(p.Effects) != 1 || p.Effects[0].Target != "dead-letter:p/e" || p.Effects[0].Reversible || len(p.Warnings) != 0 {
		t.Errorf("plan of a dead letter: %+v", p)
	}
	p = dropPlan(hooks.DeadRecord{Plugin: "p", Event: "e", State: hooks.StateDiscarded})
	if len(p.Effects) != 0 || len(p.Warnings) != 1 {
		t.Errorf("plan of a discarded letter: %+v", p)
	}
}

// TestHookVerbsDeclareTheirEffects: reads are reads, drop is destructive
// and local (not for a plugin, not served by default), and each is
// daemon-backed.
func TestHookVerbsDeclareTheirEffects(t *testing.T) {
	root := newApp().r.Cmd
	for path, want := range map[string]cli.SideEffect{
		"hook list":              cli.SideEffectRead,
		"hook test":              cli.SideEffectRead,
		"hook dead-letter list":  cli.SideEffectRead,
		"hook job get":           cli.SideEffectRead,
		"hook replay":            cli.SideEffectWriteLocal,
		"hook dead-letter retry": cli.SideEffectWriteLocal,
		"hook dead-letter drop":  cli.SideEffectDestructiveLocal,
	} {
		c := find(root, path)
		if c == nil {
			t.Errorf("%s: not in the tree", path)
			continue
		}
		if got, _ := cli.GetSideEffect(c); got != want {
			t.Errorf("%s: side effect %q, want %q", path, got, want)
		}
		if c.Annotations["heo/daemon"] != "true" {
			t.Errorf("%s: not daemon-backed", path)
		}
	}
}

// TestHookActionsAreOperatorOnly: the hook verbs' actions are held by the
// operator role and by no other.
func TestHookActionsAreOperatorOnly(t *testing.T) {
	for _, role := range authz.Roles {
		for _, act := range []string{authz.ActionHookRead, authz.ActionHookManage} {
			has := false
			for _, a := range authz.Actions(role) {
				has = has || a == act
			}
			if has != (role == authz.RoleOperator) {
				t.Errorf("role %s holds %s = %v", role, act, has)
			}
		}
	}
}
