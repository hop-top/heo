package cmd

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/refdoc"
)

// updateDocs rewrites the generated docs instead of comparing them:
// `make docs-gen` runs this test with it.
var updateDocs = flag.Bool("update-docs", false, "rewrite the generated docs")

// generatedDocs maps each generated file, relative to the repository
// root, to the function that renders it.
var generatedDocs = map[string]func([]refdoc.Leaf) string{
	"docs/manual/command-reference.md": refdoc.Reference,
	"docs/cheatsheet-agent.md":         refdoc.CheatsheetAgent,
	"docs/cheatsheet-human.md":         refdoc.CheatsheetHuman,
}

// manifest is the part of `heo spec --format json` the docs use.
type manifest struct {
	Commands []struct {
		Path       []string `json:"path"`
		Short      string   `json:"short"`
		SideEffect string   `json:"side_effect"`
		Idempotent string   `json:"idempotent"`
		DryRun     bool     `json:"dry_run_supported"`
		Examples   []struct {
			Title   string `json:"title"`
			Command string `json:"command"`
		} `json:"examples"`
	} `json:"commands"`
}

// docLeaves reads the capability manifest through the same entry point
// an agent uses (`heo spec`), and adds the three facts the manifest
// leaves out from the tree's own annotations: where the verb runs,
// whether it is self-hosting, and the surfaces it opts out of.
func docLeaves(t *testing.T) []refdoc.Leaf {
	t.Helper()
	upgradeSandbox(t)
	r := runHeo(t, "spec", "--format", "json")
	if r.exit != 0 {
		t.Fatalf("heo spec: exit %d\n%s", r.exit, r.stderr)
	}
	var m manifest
	if err := json.Unmarshal([]byte(r.stdout), &m); err != nil {
		t.Fatalf("heo spec output: %v", err)
	}
	a := newApp()
	if err := a.r.Prepare(); err != nil {
		t.Fatal(err)
	}
	tree := runnableLeaves(a)
	var out []refdoc.Leaf
	for _, c := range m.Commands {
		l := refdoc.Leaf{
			Path:       strings.Join(c.Path[1:], " "),
			Short:      c.Short,
			SideEffect: c.SideEffect,
			Idempotent: c.Idempotent,
			DryRun:     c.DryRun,
		}
		for _, e := range c.Examples {
			l.Examples = append(l.Examples, refdoc.Example{Title: e.Title, Command: e.Command})
		}
		if cmd, ok := tree[strings.Join(c.Path, " ")]; ok {
			l.Daemon = cmd.Annotations["heo/daemon"] == "true"
			l.SelfHosting = cmd.Annotations["kit/self-hosting"] == "true"
			l.Capability = cmd.Annotations[annotationCapability]
			if w := cmd.Annotations["kit/withhold-from"]; w != "" {
				l.Withheld = strings.Split(w, ",")
			}
		}
		out = append(out, l)
	}
	return out
}

// TestGeneratedDocsAreCurrent: the command reference and both
// cheatsheets are what the generator prints now. A new verb, a changed
// side effect or a reworded example fails here until `make docs-gen`
// regenerates the files.
func TestGeneratedDocsAreCurrent(t *testing.T) {
	leaves := docLeaves(t)
	for path, render := range generatedDocs {
		want := render(leaves)
		file := filepath.Join("..", path)
		if *updateDocs {
			if err := os.WriteFile(file, []byte(want), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(file) //nolint:gosec // G304: a path from generatedDocs
		if err != nil {
			t.Fatalf("%s: %v; run `make docs-gen`", path, err)
		}
		if string(got) != want {
			t.Errorf("%s is stale; run `make docs-gen` and commit the result", path)
		}
	}
}

// TestGeneratedDocsFitTheLineBudget: prose lines stay within 100
// columns and table rows within 256, the repository's markdown limits.
func TestGeneratedDocsFitTheLineBudget(t *testing.T) {
	leaves := docLeaves(t)
	for path, render := range generatedDocs {
		inFence := false
		for i, line := range strings.Split(render(leaves), "\n") {
			if strings.HasPrefix(line, "```") {
				inFence = !inFence
			}
			limit := 100
			if strings.HasPrefix(line, "|") {
				limit = 256
			}
			if inFence && !strings.HasPrefix(line, "```") && strings.HasPrefix(line, "heo ") {
				limit = 256 // an example is copied whole
			}
			if n := len([]rune(line)); n > limit {
				t.Errorf("%s:%d: %d columns, limit %d", path, i+1, n, limit)
			}
		}
	}
}

// TestRefdocMetaMatchesTheTree: every verb in the manifest has a
// metadata row (roles, uma backing) and every row names a verb the tree
// still has.
func TestRefdocMetaMatchesTheTree(t *testing.T) {
	var have []string
	for _, l := range docLeaves(t) {
		have = append(have, l.Path)
		if _, ok := refdoc.Lookup(l.Path); !ok {
			t.Errorf("%q has no row in refdoc verbs (roles and uma backing)", l.Path)
		}
	}
	for _, p := range refdoc.Paths() {
		if !slices.Contains(have, p) {
			t.Errorf("refdoc verbs names %q, which is not a verb of the tree", p)
		}
	}
}

// TestGrammarCatalogIsAccountedFor: every verb the grammar names is in
// the tree or recorded as a gap with its reason, and no gap names a
// verb the tree now has.
func TestGrammarCatalogIsAccountedFor(t *testing.T) {
	var have []string
	for _, l := range docLeaves(t) {
		have = append(have, l.Path)
	}
	gaps := map[string]bool{}
	for _, g := range refdoc.Gaps {
		if g.Why == "" || g.Status == "" {
			t.Errorf("gap %q lacks a status or a reason", g.Verb)
		}
		gaps[g.Verb] = true
		if slices.Contains(have, g.Verb) {
			t.Errorf("gap %q is in the command tree: delete the row", g.Verb)
		}
	}
	for _, v := range refdoc.Catalog {
		inTree, recorded := slices.Contains(have, v), gaps[v]
		switch {
		case !inTree && !recorded:
			t.Errorf("grammar verb %q is neither implemented nor recorded as a gap", v)
		case inTree && recorded:
			t.Errorf("grammar verb %q is both implemented and a gap", v)
		}
	}
	for v := range gaps {
		if !strings.HasPrefix(v, "(") && !slices.Contains(refdoc.Catalog, v) {
			t.Errorf("gap %q is not a verb of the grammar catalog", v)
		}
	}
}
