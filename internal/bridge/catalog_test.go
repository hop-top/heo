package bridge_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"hop.top/heo/internal/bridge"
	"hop.top/uma/model"
)

const (
	hooksDoc       = "../../docs/manual/hooks.md"
	envelopeSchema = "../../docs/hook-envelope.schema.json"
)

// umaEventKinds reads the builtin event kinds from uma's model package:
// every string constant named Event*. uma exports no list, so a kind
// added there appears here without anyone remembering to register it.
func umaEventKinds(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.Dir}}", "hop.top/uma/model").Output()
	if err != nil {
		t.Fatalf("locating uma model: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	fset := token.NewFileSet()
	files := parseNonTestFiles(t, fset, dir)
	var kinds []string
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
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					kinds = append(kinds, v)
				}
			}
		}
	}
	sort.Strings(kinds)
	return kinds
}

// catalogRow is one row of the event catalog table in hooks.md.
type catalogRow struct {
	scope string
	refs  []string
}

func readCatalog(t *testing.T) map[string]catalogRow {
	t.Helper()
	raw, err := os.ReadFile(hooksDoc)
	if err != nil {
		t.Fatalf("event catalog: %v", err)
	}
	rows := map[string]catalogRow{}
	in := false
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "<!-- catalog:start"):
			in = true
		case strings.HasPrefix(line, "<!-- catalog:end"):
			in = false
		case in && strings.HasPrefix(line, "| `"):
			cells := strings.Split(strings.Trim(line, "| "), "|")
			if len(cells) < 4 {
				t.Fatalf("catalog row needs kind, scope, references, when: %q", line)
			}
			kind := strings.Trim(strings.TrimSpace(cells[0]), "`")
			if _, dup := rows[kind]; dup {
				t.Errorf("catalog lists %s twice", kind)
			}
			row := catalogRow{scope: strings.TrimSpace(cells[1])}
			for _, r := range strings.Split(cells[2], ",") {
				if r = strings.Trim(strings.TrimSpace(r), "`"); r != "" && r != "-" {
					row.refs = append(row.refs, r)
				}
			}
			sort.Strings(row.refs)
			rows[kind] = row
		}
	}
	return rows
}

// TestCatalogListsEveryEventKind: every builtin uma event kind has a row
// in the hooks.md catalog and every row names a kind uma has. Adding a
// kind to uma without deciding whether a plugin may subscribe to it, and
// with which references, fails here.
func TestCatalogListsEveryEventKind(t *testing.T) {
	rows := readCatalog(t)
	if len(rows) == 0 {
		t.Fatal("no catalog rows between the catalog markers")
	}
	known := map[string]bool{}
	for _, k := range umaEventKinds(t) {
		known[k] = true
		row, ok := rows[k]
		if !ok {
			t.Errorf("event kind %s has no row in %s", k, hooksDoc)
			continue
		}
		if row.scope != "desk" && row.scope != "record" {
			t.Errorf("%s: scope %q is neither desk nor record", k, row.scope)
		}
	}
	for k := range rows {
		if !known[k] {
			t.Errorf("catalog row %s is not an event kind of uma", k)
		}
	}
}

// TestCatalogReferencesMatchTheBridge: the references a catalog row
// promises are exactly the ones the bridge's payload fills for that kind,
// given a payload that names every reference it could.
func TestCatalogReferencesMatchTheBridge(t *testing.T) {
	rows := readCatalog(t)
	conv := map[string]any{"id": "conv-1"}
	full := map[string]any{
		"message":      map[string]any{"conversation": conv, "id": "msg-1"},
		"conversation": conv,
		"label":        map[string]any{"id": "lbl-1"},
		"operation":    map[string]any{"id": "op-1"},
		"interaction":  map[string]any{"conversation": conv, "id": "ix-1"},
		"task":         map[string]any{"conversation": conv, "id": "tk-1"},
		"work_item":    map[string]any{"conversation": conv, "id": "wi-1"},
	}
	for _, kind := range umaEventKinds(t) {
		row, ok := rows[kind]
		if !ok {
			continue // reported by TestCatalogListsEveryEventKind
		}
		_, p := bridge.Map(umaEvent(kind, model.Public, msgRes, ext(t, "uma."+kind, full)))
		var got []string
		for name, v := range map[string]string{"conversation": p.Conversation, "message": p.Message, "label": p.Label, "operation": p.Operation} {
			if v != "" {
				got = append(got, name)
			}
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, append([]string(nil), row.refs...)) && (len(got) > 0 || len(row.refs) > 0) {
			t.Errorf("%s: catalog references %v, bridge fills %v", kind, row.refs, got)
		}
	}
}

// TestEnvelopeEventIsReferencesOnly: the envelope schema's event object
// closes its properties to exactly the bridge's reference payload, so a
// field carrying content cannot slip into the contract unnoticed.
func TestEnvelopeEventIsReferencesOnly(t *testing.T) {
	raw, err := os.ReadFile(envelopeSchema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			Event struct {
				Additional *bool                      `json:"additionalProperties"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"event"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	ev := s.Properties.Event
	if ev.Additional == nil || *ev.Additional {
		t.Error("envelope event must set additionalProperties: false")
	}
	var want []string
	pt := reflect.TypeOf(bridge.Payload{})
	for i := 0; i < pt.NumField(); i++ {
		want = append(want, strings.Split(pt.Field(i).Tag.Get("json"), ",")[0])
	}
	var got []string
	for k := range ev.Properties {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("envelope event properties %v, bridge payload fields %v", got, want)
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
