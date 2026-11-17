package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/hooks"
	"hop.top/kit/go/console/cli"
	"hop.top/uma/model"
)

// TestHookDispatcherChecksTheRealTree: what a plugin may name is what a
// REST caller may run, less what cannot be replayed or acts on the
// daemon's machine.
func TestHookDispatcherChecksTheRealTree(t *testing.T) {
	d := newApp().hookDispatcher()
	for _, tc := range []struct {
		name string
		c    hooks.Call
		want string // "" accepted, else a fragment of the refusal
	}{
		{"keyed write", hooks.Call{Verb: "note add", Args: []string{"c1"}, Flags: map[string]any{"text": "hi", "placement": "local"}}, ""},
		{"read", hooks.Call{Verb: "conversation get", Args: []string{"c1"}}, ""},
		{"naturally idempotent write", hooks.Call{Verb: "conversation mark-read", Args: []string{"c1"}}, ""},
		{"unknown verb", hooks.Call{Verb: "note shred"}, "unknown verb"},
		{"a group", hooks.Call{Verb: "note"}, "not a verb that runs"},
		{"self-hosting", hooks.Call{Verb: "plugin token mint", Args: []string{"x"}}, "not a verb that runs"},
		{"self-hosting revoke", hooks.Call{Verb: "plugin token revoke", Args: []string{"x"}}, "not a verb that runs"},
		{"terminal only", hooks.Call{Verb: "event watch"}, "terminal only"},
		{"acts on the daemon's machine", hooks.Call{Verb: "storage backup"}, "daemon's machine"},
		{"destructive", hooks.Call{Verb: "message delete", Args: []string{"m1"}}, "destructive"},
		{"unknown flag", hooks.Call{Verb: "note add", Flags: map[string]any{"colour": "red"}}, "no flag --colour"},
		{"global flag", hooks.Call{Verb: "note add", Flags: map[string]any{"profile": "other"}}, "no flag --profile"},
	} {
		err := d.Check(tc.c)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// echoTree is a command tree with one keyed write that records what it
// was run with, and one that fails.
func echoTree(ran *[]string, mu *sync.Mutex) *cobra.Command {
	root := &cobra.Command{Use: "heo", SilenceUsage: true, SilenceErrors: true}
	note := &cobra.Command{Use: "note"}
	add := &cobra.Command{
		Use: "add <conversation>", Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			text, _ := c.Flags().GetString("text")
			key, _ := c.Flags().GetString("idempotency-key")
			mu.Lock()
			*ran = append(*ran, strings.Join(append(args, text, key), "|"))
			mu.Unlock()
			if text == "boom" {
				return errors.New("store down")
			}
			return nil
		},
	}
	add.Flags().String("text", "", "")
	add.Flags().String("idempotency-key", "", "")
	control.MarkDaemon(add)
	cli.SetSideEffect(add, cli.SideEffectWrite)
	cli.SetIdempotency(add, cli.IdempotencyConditional)
	note.AddCommand(add)
	root.AddCommand(note)
	return root
}

// TestHookDispatcherRunsAsTheActorUnderItsKey: the command runs through
// the bridge as the plugin, with the operation's key as
// --idempotency-key; the daemon gate's refusal is permanent and runs
// nothing; a failing command is retryable.
func TestHookDispatcherRunsAsTheActorUnderItsKey(t *testing.T) {
	var ran []string
	var mu sync.Mutex
	admitted := map[model.ActorID]bool{"plugin:triage": true}
	var asked []model.ActorID
	d := &hookDispatcher{
		root: echoTree(&ran, &mu), serve: func() bool { return true },
		admit: func(a model.ActorID) (bool, string) {
			asked = append(asked, a)
			return admitted[a], "heo.authz.no_role"
		},
	}
	call := hooks.Call{Verb: "note add", Args: []string{"c1"}, Flags: map[string]any{"text": "hi"}, Key: "hook:triage:e1:abc"}
	if err := d.Dispatch(context.Background(), "plugin:triage", call); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "c1|hi|hook:triage:e1:abc" {
		t.Errorf("ran %v, want the command under the operation's key", ran)
	}
	if len(asked) != 1 || asked[0] != "plugin:triage" {
		t.Errorf("gate asked about %v, want plugin:triage", asked)
	}

	// Authorization decides: an actor the policy does not admit runs nothing.
	err := d.Dispatch(context.Background(), "plugin:rogue", call)
	if !hooks.IsPermanent(err) || !strings.Contains(err.Error(), "heo.authz.no_role") {
		t.Errorf("refused actor: err = %v, want a permanent refusal naming the reason", err)
	}
	if len(ran) != 1 {
		t.Errorf("a refused plugin ran the command: %v", ran)
	}

	// A command that fails is retried, not dead-lettered.
	call.Flags = map[string]any{"text": "boom"}
	err = d.Dispatch(context.Background(), "plugin:triage", call)
	if err == nil || hooks.IsPermanent(err) {
		t.Errorf("failing command: err = %v, want a retryable failure", err)
	}
}

// TestHookDispatcherReadsAPluginOnTheSocketAsRefused: the gate refuses
// a plugin actor on the control socket; the dispatcher is REST, so a
// plugin is admitted there only by the policy.
func TestHookDispatcherDoesNotUseTheSocket(t *testing.T) {
	var ran []string
	var mu sync.Mutex
	d := &hookDispatcher{root: echoTree(&ran, &mu), serve: func() bool { return true },
		admit: func(model.ActorID) (bool, string) { return true, "" }}
	if err := d.Dispatch(context.Background(), "plugin:triage",
		hooks.Call{Verb: "note add", Args: []string{"c1"}, Key: "k"}); err != nil {
		t.Fatalf("a plugin the policy admits was refused: %v", err)
	}
}
