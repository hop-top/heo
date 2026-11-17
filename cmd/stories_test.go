package cmd

import (
	"path/filepath"
	"testing"

	"hop.top/kit/go/conformance/story"
	"hop.top/kit/go/conformance/story/parser"
)

// storiesDir holds the story documents `kit conformance verify-stories`
// validates: the user intents heo advertises, as command sequences.
const storiesDir = "../e2e/stories"

// TestStoryStepsResolveInTheTree is the semantic tier of story
// validation, run against heo's real command tree: every step invokes
// heo, names a runnable command, and passes only flags that command
// (or a global) declares, with an argument count its Args validator
// accepts. A story that names a verb or flag heo does not have is a
// story no caller can follow.
func TestStoryStepsResolveInTheTree(t *testing.T) {
	paths, err := story.Discover(storiesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no story documents under %s", storiesDir)
	}
	for _, p := range paths {
		ps, err := parser.ParseFile(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		s := ps.Story
		t.Run(filepath.Base(p), func(t *testing.T) {
			if s.Binary != "heo" {
				t.Fatalf("binary = %q, want heo", s.Binary)
			}
			for _, step := range s.Steps {
				if len(step.Invoke) < 2 || step.Invoke[0] != "heo" {
					t.Errorf("step %s: invoke %v does not start with heo and a command", step.ID, step.Invoke)
					continue
				}
				a := newApp()
				// Prepare installs what Execute does before it parses: the
				// idempotency-key flag among them.
				if err := a.r.Prepare(); err != nil {
					t.Fatal(err)
				}
				args := step.Invoke[1:]
				c, rest, err := a.r.Cmd.Find(args)
				if err != nil {
					t.Errorf("step %s: %v", step.ID, err)
					continue
				}
				if !c.Runnable() {
					t.Errorf("step %s: %s is not a runnable command", step.ID, c.CommandPath())
					continue
				}
				if err := c.ParseFlags(rest); err != nil {
					t.Errorf("step %s: %s: %v", step.ID, c.CommandPath(), err)
					continue
				}
				if err := c.ValidateArgs(c.Flags().Args()); err != nil {
					t.Errorf("step %s: %s: %v", step.ID, c.CommandPath(), err)
				}
			}
		})
	}
}
