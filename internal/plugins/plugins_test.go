package plugins_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/plugins"
)

// cfg builds a viper from a plugins list and grants for the named actors.
func cfg(list []any, granted ...string) *viper.Viper {
	v := viper.New()
	v.Set(plugins.KeyPlugins, list)
	var grants []any
	for _, a := range granted {
		grants = append(grants, map[string]any{"actor": a, "roles": []any{"desk"}})
	}
	v.Set("authorization.grants", grants)
	return v
}

func entry(name string, extra map[string]any) map[string]any {
	m := map[string]any{"name": name, "command": "sh"}
	for k, x := range extra {
		m[k] = x
	}
	return m
}

func keys(probs []plugins.Problem) []string {
	var out []string
	for _, p := range probs {
		if !p.Warning {
			out = append(out, p.Key)
		}
	}
	return out
}

func TestDefaults(t *testing.T) {
	list, probs := plugins.Read(cfg([]any{entry("triage", nil)}))
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	p := list[0]
	if p.Mode != "exec" || p.Timeout != 30*time.Second || p.Concurrency != 1 ||
		p.Retry.MaxAttempts != 5 || p.Retry.Backoff.Initial != time.Second || p.Retry.Backoff.Max != 5*time.Minute {
		t.Errorf("defaults: %+v", p)
	}
	if p.Actor() != "plugin:triage" {
		t.Errorf("actor %s", p.Actor())
	}
}

func TestFullEntry(t *testing.T) {
	list, probs := plugins.Read(cfg([]any{entry("fwd", map[string]any{
		"args": []any{"-v"}, "mode": "stream", "timeout": "2m", "concurrency": 1,
		"filter": map[string]any{"kinds": []any{"message.received"}, "channels": []any{"mail"}, "inboxes": []any{"support"}},
		"retry":  map[string]any{"max_attempts": 3, "backoff": map[string]any{"initial": "2s", "max": "1m"}},
	})}))
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	p := list[0]
	if p.Mode != "stream" || p.Timeout != 2*time.Minute || p.Retry.MaxAttempts != 3 ||
		p.Retry.Backoff.Initial != 2*time.Second || p.Retry.Backoff.Max != time.Minute ||
		!slices.Equal(p.Filter.Kinds, []string{"message.received"}) ||
		!slices.Equal(p.Filter.Inboxes, []string{"support"}) || !slices.Equal(p.Args, []string{"-v"}) {
		t.Errorf("parsed: %+v", p)
	}
}

func TestDryRun(t *testing.T) {
	list, probs := plugins.Read(cfg([]any{entry("a", nil), entry("b", map[string]any{"dry_run": true}), entry("c", map[string]any{"dry_run": false})}))
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	if list[0].DryRun || !list[1].DryRun || list[2].DryRun {
		t.Errorf("dry_run = %v %v %v, want false true false (off by default)", list[0].DryRun, list[1].DryRun, list[2].DryRun)
	}
	if list[0].Consumer() != plugins.ConsumerName("a", list[0].Filter) || list[1].Consumer() != plugins.ConsumerName("b", list[1].Filter) {
		t.Error("dry_run changed a consumer name: it must not open a new subscription")
	}
	_, probs = plugins.Read(cfg([]any{entry("a", map[string]any{"dry_run": "yes"})}))
	if !slices.Contains(keys(probs), "plugins[0].dry_run") {
		t.Errorf("a non-boolean dry_run was accepted: %v", probs)
	}
}

func TestReadRefuses(t *testing.T) {
	cases := []struct {
		name string
		list []any
		key  string
	}{
		{"unknown kind", []any{entry("a", map[string]any{"filter": map[string]any{"kinds": []any{"message.recieved"}}})}, "plugins[0].filter.kinds[0]"},
		{"duplicate name", []any{entry("a", nil), entry("a", nil)}, "plugins[1].name"},
		{"bad name", []any{entry("Bad_Name", nil)}, "plugins[0].name"},
		{"no name", []any{map[string]any{"command": "sh"}}, "plugins[0].name"},
		{"neither command nor socket", []any{map[string]any{"name": "a"}}, "plugins[0].command"},
		{"both command and socket", []any{entry("a", map[string]any{"socket": "/tmp/a.sock"})}, "plugins[0].socket"},
		{"relative socket", []any{map[string]any{"name": "a", "socket": "a.sock"}}, "plugins[0].socket"},
		{"socket with concurrency", []any{map[string]any{"name": "a", "socket": "/tmp/a.sock", "concurrency": 2}}, "plugins[0].concurrency"},
		{"bad mode", []any{entry("a", map[string]any{"mode": "daemon"})}, "plugins[0].mode"},
		{"zero timeout", []any{entry("a", map[string]any{"timeout": "0s"})}, "plugins[0].timeout"},
		{"bad timeout", []any{entry("a", map[string]any{"timeout": "soon"})}, "plugins[0].timeout"},
		{"zero concurrency", []any{entry("a", map[string]any{"concurrency": 0})}, "plugins[0].concurrency"},
		{"stream concurrency", []any{entry("a", map[string]any{"mode": "stream", "concurrency": 2})}, "plugins[0].concurrency"},
		{"zero attempts", []any{entry("a", map[string]any{"retry": map[string]any{"max_attempts": 0}})}, "plugins[0].retry.max_attempts"},
		{"backoff max below initial", []any{entry("a", map[string]any{"retry": map[string]any{"backoff": map[string]any{"initial": "1m", "max": "1s"}}})}, "plugins[0].retry.backoff.max"},
		{"unknown field", []any{entry("a", map[string]any{"roles": []any{"desk"}})}, "plugins[0].roles"},
		{"unknown filter field", []any{entry("a", map[string]any{"filter": map[string]any{"actors": []any{"x"}}})}, "plugins[0].filter.actors"},
		{"not a list", nil, "plugins"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := cfg(c.list)
			if c.name == "not a list" {
				v.Set(plugins.KeyPlugins, "x")
			}
			_, probs := plugins.Read(v)
			if !slices.Contains(keys(probs), c.key) {
				t.Errorf("want error at %s, got %v", c.key, probs)
			}
		})
	}
}

func TestCheckMissingGrant(t *testing.T) {
	probs := plugins.Check(cfg([]any{entry("a", nil), entry("b", nil)}, "plugin:a"))
	if got := keys(probs); !slices.Equal(got, []string{"plugins[1].name"}) {
		t.Errorf("want only plugin b refused, got %v", probs)
	}
	if !strings.Contains(probs[0].Message, "plugin:b") {
		t.Errorf("message %q", probs[0].Message)
	}
}

func TestCheckCommandNotFound(t *testing.T) {
	probs := plugins.Check(cfg([]any{entry("a", map[string]any{"command": "/nonexistent/heo-plugin"})}, "plugin:a"))
	if got := keys(probs); !slices.Equal(got, []string{"plugins[0].command"}) {
		t.Errorf("got %v", probs)
	}
	if probs := plugins.Check(cfg([]any{entry("a", nil)}, "plugin:a")); len(probs) != 0 {
		t.Errorf("sh is on PATH: %v", probs)
	}
}

func TestCheckSocketNotProbed(t *testing.T) {
	v := cfg([]any{map[string]any{"name": "a", "socket": "/nonexistent/a.sock"}}, "plugin:a")
	if probs := plugins.Check(v); len(probs) != 0 {
		t.Errorf("a socket may appear later: %v", probs)
	}
}

func TestNoSection(t *testing.T) {
	if probs := plugins.Check(viper.New()); len(probs) != 0 {
		t.Errorf("%v", probs)
	}
}

// TestEventKindsMatchUma: the kinds a plugin may name are exactly the
// builtin Event* constants of uma's model package. uma exports no list, so
// the constants are read from source, as the catalog test does.
func TestEventKindsMatchUma(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "hop.top/uma/model").Output()
	if err != nil {
		t.Fatalf("locating uma model: %v", err)
	}
	fset := token.NewFileSet()
	files := parseNonTestFiles(t, fset, strings.TrimSpace(string(out)))
	var want []string
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if !strings.HasPrefix(n.Name, "Event") || i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						k, _ := strconv.Unquote(lit.Value)
						want = append(want, k)
					}
				}
			}
		}
	}
	got := slices.Clone(plugins.EventKinds)
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("plugins.EventKinds %v\numa kinds %v", got, want)
	}
}

// parseNonTestFiles parses the non-test Go files of dir.
func parseNonTestFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}
