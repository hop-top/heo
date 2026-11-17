package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/cli"
)

// runnableLeaves returns every runnable leaf of a fresh tree, by path.
func runnableLeaves(a *app) map[string]*cobra.Command {
	out := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			walk(sub)
		}
		if !c.HasSubCommands() && c.Runnable() {
			out[c.CommandPath()] = c
		}
	}
	walk(a.r.Cmd)
	return out
}

// shellWords splits a documented invocation the way a shell would
// before running it: whitespace separates words, single and double
// quotes group one, and a pipe ends the command (what follows consumes
// its output). Substitutions inside quotes stay literal text.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	var quote rune
	started := false
	flush := func() {
		if started {
			words = append(words, cur.String())
		}
		cur.Reset()
		started = false
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, started = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == '|' && !started:
			flush()
			return words
		default:
			cur.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		t.Fatalf("unterminated quote in %q", line)
	}
	flush()
	return words
}

// resolveInvocation checks a documented command line against a fresh
// command tree the way Execute would parse it: the words after `heo`
// name a runnable command, the flags exist on it (or are globals), the
// argument count passes its Args validator and every required flag and
// flag group is satisfied. It returns the command it resolved to.
func resolveInvocation(t *testing.T, line string) (*cobra.Command, []string, error) {
	t.Helper()
	words := shellWords(t, line)
	if len(words) < 2 || words[0] != "heo" {
		return nil, nil, errNotHeo
	}
	a := newApp()
	// Prepare installs what Execute does before it parses: the
	// idempotency-key flag among them.
	if err := a.r.Prepare(); err != nil {
		t.Fatal(err)
	}
	args := words[1:]
	c, rest, err := a.r.Cmd.Find(args)
	if err != nil {
		return nil, args, err
	}
	if !c.Runnable() {
		return c, args, errNotRunnable
	}
	if err := c.ParseFlags(rest); err != nil {
		return c, args, err
	}
	if err := c.ValidateArgs(c.Flags().Args()); err != nil {
		return c, args, err
	}
	if err := c.ValidateRequiredFlags(); err != nil {
		return c, args, err
	}
	return c, args, c.ValidateFlagGroups()
}

type guidanceError string

func (e guidanceError) Error() string { return string(e) }

const (
	errNotHeo      = guidanceError("does not start with heo and a command")
	errNotRunnable = guidanceError("is not a runnable command")
)

// TestGuidanceTablesNameRealLeaves: a row in leafExamples or
// leafNextSteps for a command that is gone is dead weight that reads
// like documentation.
func TestGuidanceTablesNameRealLeaves(t *testing.T) {
	leaves := runnableLeaves(newApp())
	for path := range leafExamples {
		if _, ok := leaves[path]; !ok {
			t.Errorf("leafExamples names %q, which is not a runnable leaf", path)
		}
	}
	for path := range leafNextSteps {
		if _, ok := leaves[path]; !ok {
			t.Errorf("leafNextSteps names %q, which is not a runnable leaf", path)
		}
	}
}

// TestGuidanceExamplesAreDerivedFromHelpText: every line of a cobra
// Example block is an invocation, so deriving kit/examples from it
// skips nothing, and the table fills only leaves that have no text.
func TestGuidanceExamplesAreDerivedFromHelpText(t *testing.T) {
	for path, c := range runnableLeaves(newApp()) {
		ex, ok := cli.GetExamples(c)
		if !ok {
			continue // reported by the conformance test
		}
		rendered := 0
		for _, line := range strings.Split(c.Example, "\n") {
			if strings.TrimSpace(line) != "" {
				rendered++
			}
		}
		if rendered != len(ex) {
			t.Errorf("%s: %d example(s) but %d line(s) of Example text", path, len(ex), rendered)
		}
		seen := map[string]bool{}
		for _, e := range ex {
			if e.Title == "" || e.Command == "" {
				t.Errorf("%s: example %+v lacks a title or a command", path, e)
			}
			if seen[e.Command] {
				t.Errorf("%s: %q is documented twice", path, e.Command)
			}
			seen[e.Command] = true
		}
	}
}

// TestGuidanceExamplesParseAgainstTheTree: every documented example is
// a command line that exists. It names the leaf it is documented on,
// passes only flags that leaf (or a global) declares, satisfies its
// argument and required-flag checks, and is accepted by Execute's own
// parser (--help appended answers without running anything). An example
// that invents a flag fails here, not in front of a caller.
func TestGuidanceExamplesParseAgainstTheTree(t *testing.T) {
	upgradeSandbox(t)
	for path, c := range runnableLeaves(newApp()) {
		ex, _ := cli.GetExamples(c)
		for _, e := range ex {
			t.Run(e.Command, func(t *testing.T) {
				got, args, err := resolveInvocation(t, e.Command)
				if err != nil {
					t.Fatalf("%s: example %q: %v", path, e.Command, err)
				}
				if got.CommandPath() != path {
					t.Errorf("example %q is documented on %s but runs %s", e.Command, path, got.CommandPath())
				}
				if r := runHeo(t, append(slices.Clone(args), "--help")...); r.exit != 0 {
					t.Errorf("%q --help: exit %d\n%s", e.Command, r.exit, r.stderr)
				}
			})
		}
	}
}

// TestGuidanceNextStepsParseAgainstTheTree: every next step is a real
// invocation, with real flags and the arguments its command takes.
// <placeholders> stand for values the caller fills in.
func TestGuidanceNextStepsParseAgainstTheTree(t *testing.T) {
	upgradeSandbox(t)
	for path, c := range runnableLeaves(newApp()) {
		steps, _ := cli.GetNextSteps(c)
		for _, s := range steps {
			t.Run(s.Suggest, func(t *testing.T) {
				if s.Suggest == "" || s.Reason == "" || s.When == "" {
					t.Errorf("%s: next step %+v lacks a condition, a command or a reason", path, s)
				}
				got, args, err := resolveInvocation(t, s.Suggest)
				if err != nil {
					t.Fatalf("%s: next step %q: %v", path, s.Suggest, err)
				}
				if got.CommandPath() == "heo" {
					t.Errorf("%s: next step %q names no command", path, s.Suggest)
				}
				if r := runHeo(t, append(slices.Clone(args), "--help")...); r.exit != 0 {
					t.Errorf("%q --help: exit %d\n%s", s.Suggest, r.exit, r.stderr)
				}
			})
		}
	}
}
