package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/adapters"
	"hop.top/kit/go/console/cli"
)

// find returns the command at path ("message edit") below root, nil
// when the tree has none.
func find(root *cobra.Command, path string) *cobra.Command {
	c := root
	for _, name := range strings.Fields(path) {
		if c = child(c, name); c == nil {
			return nil
		}
	}
	return c
}

// walkAll visits every command below root.
func walkAll(c *cobra.Command, fn func(*cobra.Command)) {
	for _, sub := range c.Commands() {
		fn(sub)
		walkAll(sub, fn)
	}
}

// TestProviderVerbsCarryTheirCapability: every provider-dependent verb
// heo's static map names is in the tree, says so on the first line of
// its help, and carries the heo/capability annotation naming the
// capability the map gives it; and no command carries the annotation
// without being in the map. The discovery surfaces (help, spec, REST,
// MCP) read the same tree, so one verb cannot be marked on one of them
// and not another.
func TestProviderVerbsCarryTheirCapability(t *testing.T) {
	a := newApp()
	want := map[string]string{}
	for _, v := range adapters.ProviderVerbs() {
		want[v.Name] = string(v.Capability)
		c := find(a.r.Cmd, v.Name)
		if c == nil {
			t.Errorf("%s: provider-dependent verb is not in the tree", v.Name)
			continue
		}
		if got := c.Annotations[annotationCapability]; got != string(v.Capability) {
			t.Errorf("%s: %s = %q, want %q", v.Name, annotationCapability, got, v.Capability)
		}
		if first, _, _ := strings.Cut(c.Long, "\n"); first != providerLine(v.Capability) {
			t.Errorf("%s: first help line %q, want %q", v.Name, first, providerLine(v.Capability))
		}
		if !strings.Contains(c.Long, "UNSUPPORTED") && !strings.Contains(c.Long, "exit 2") {
			t.Errorf("%s: help does not say a channel lacking the capability is refused (exit 2)", v.Name)
		}
	}
	walkAll(a.r.Cmd, func(c *cobra.Command) {
		cap, ok := c.Annotations[annotationCapability]
		if !ok {
			return
		}
		path := strings.TrimPrefix(c.CommandPath(), a.r.Cmd.Name()+" ")
		if want[path] != cap {
			t.Errorf("%s carries %s=%q but the static verb map says %q", path, annotationCapability, cap, want[path])
		}
	})
}

// TestMessageDeleteIsDestructiveShared: a provider deletion is the
// destructive-shared tier, which kit withholds from REST, rpc and MCP
// until the operator names the surface; edit and the reactions are
// write-shared, and the reads are reads.
func TestMessageDeleteIsDestructiveShared(t *testing.T) {
	a := newApp()
	for path, want := range map[string]cli.SideEffect{
		"message delete":      cli.SideEffectDestructiveShared,
		"message edit":        cli.SideEffectWriteShared,
		"reaction add":        cli.SideEffectWriteShared,
		"reaction remove":     cli.SideEffectWriteShared,
		"reaction list":       cli.SideEffectRead,
		"participant list":    cli.SideEffectRead,
		"participant add":     cli.SideEffectWriteShared,
		"participant remove":  cli.SideEffectDestructiveShared,
		"receipt send":        cli.SideEffectWriteShared,
		"history fetch":       cli.SideEffectWrite,
		"conversation status": cli.SideEffectWriteShared,
		"conversation update": cli.SideEffectWriteShared,
		"adapter get":         cli.SideEffectRead,
		"message get":         cli.SideEffectRead,
		"message reply":       cli.SideEffectWriteShared,
	} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Errorf("%s: not in the tree", path)
			continue
		}
		if got, _ := cli.GetSideEffect(c); got != want {
			t.Errorf("%s: side effect %q, want %q", path, got, want)
		}
	}
}

// TestConversationVerbsRegistered: the verbs uma now has a call for are
// registered as provider-dependent verbs (participant add and remove,
// receipt send, history fetch), each carrying its capability; the data
// reads stay ungated. The conversation workflow verbs (assign, snooze,
// link, mark-read and their inverses) are uma's local state, not
// provider verbs: none carries a capability.
func TestConversationVerbsRegistered(t *testing.T) {
	a := newApp()
	for path, capability := range map[string]string{
		"participant add": "membership", "participant remove": "membership",
		"receipt send": "receipt", "history fetch": "history",
	} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Errorf("%s: not in the tree", path)
			continue
		}
		if got := c.Annotations[annotationCapability]; got != capability {
			t.Errorf("%s: capability %q, want %q", path, got, capability)
		}
	}
	for _, path := range []string{"participant list", "reaction list"} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Errorf("%s: not in the tree", path)
			continue
		}
		if _, gated := c.Annotations[annotationCapability]; gated {
			t.Errorf("%s: a data read carries a capability gate", path)
		}
	}
	for _, path := range []string{
		"conversation mark-read", "conversation mark-unread", "conversation assign", "conversation unassign",
		"conversation snooze", "conversation unsnooze", "conversation link", "conversation unlink",
	} {
		c := find(a.r.Cmd, path)
		if c == nil {
			t.Errorf("%s: not in the tree", path)
			continue
		}
		if got, gated := c.Annotations[annotationCapability]; gated {
			t.Errorf("%s: a local workflow verb carries capability %q", path, got)
		}
	}
}
