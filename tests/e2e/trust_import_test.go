package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// trustProfile turns the hold point and the trust ladder on and gives the
// plugin actor the desk and operator roles: no role lets it import trust.
const trustProfile = "held_operations:\n  enabled: true\n  trust:\n    enabled: true\n    unattended: false\n" +
	"authorization:\n  grants:\n    - actor: plugin:triage\n      roles: [desk, operator]\n" + noRateLimit

type trustImportJSON struct {
	DryRun    bool `json:"dry_run"`
	Imported  int
	Unchanged int
	Refused   int
	Failed    int
	Automatic int `json:"becomes_automatic"`
	Entries   []struct {
		Index            int
		Key              string
		Status           string
		Reason           string
		Clean            int64
		Automatic        bool
		BecomesAutomatic bool `json:"becomes_automatic"`
	}
}

// TestTrustImport drives `operation trust import` on the e2e build with
// the socket, REST and MCP serving: a file, standard input and inline
// JSON; a dry run that writes nothing; one refused entry refusing the
// file; an idempotent re-import; the trust list showing what became
// automatic; and the surfaces: MCP passes the file as the JSON itself,
// and a plugin token is refused whatever roles it holds.
func TestTrustImport(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, trustProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api, mcp := freeAddr(t), freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "mcp", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--enable", "mcp", "--addr", api, "--mcp-addr", mcp)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api, mcp: mcp}
		person, plugin := auth.token(me()), auth.token("plugin:triage")
		m := newMCPClient(t, "http://"+mcp+"/mcp", person)

		entry := func(verb, class string, clean int) string {
			return fmt.Sprintf(`{"channel":"fxin","proposer":"plugin:triage","on_behalf_of":"uid:1001","verb":%q,"class":%q,"clean":%d}`, verb, class, clean)
		}
		file := func(entries ...string) string {
			return `{"version":1,"entries":[` + strings.Join(entries, ",") + `]}`
		}
		good := file(entry("state", "read", 3), entry("state", "trash", 1))
		path := e.s.path("trust.json")
		e.s.write("trust.json", good)
		listed := func() []map[string]any {
			t.Helper()
			var page struct{ Items []map[string]any }
			e.json(&page, "operation", "trust", "list", "--inbox", "fxin")
			return page.Items
		}

		// A dry run reports what an import would leave and writes nothing.
		var dry trustImportJSON
		e.json(&dry, "operation", "trust", "import", "--file", path, "--dry-run")
		if !dry.DryRun || dry.Imported != 2 || dry.Automatic != 1 || len(dry.Entries) != 2 ||
			!dry.Entries[0].BecomesAutomatic || dry.Entries[1].Automatic {
			t.Fatalf("dry run = %+v", dry)
		}
		if got := listed(); len(got) != 0 {
			t.Fatalf("a dry run wrote %v", got)
		}

		// One bad entry refuses the file: exit 2, the report printed,
		// nothing written. So does a field outside the schema.
		bad := file(entry("state", "read", 3), entry("send-everything", "", 1))
		r := e.s.runIn(bad, nil, "operation", "trust", "import", "--file", "-", "--format", "json")
		e.s.want(r, 2, "import of a bad file")
		var refused trustImportJSON
		if err := json.Unmarshal([]byte(r.stdout), &refused); err != nil || refused.Refused != 1 || refused.Imported != 0 ||
			refused.Entries[0].Status != "not_written" || !strings.Contains(refused.Entries[1].Reason, "send-everything") {
			t.Fatalf("refused report = %+v (%v)\n%s", refused, err, r.stdout)
		}
		if got := listed(); len(got) != 0 {
			t.Fatalf("a refused file wrote %v", got)
		}
		for name, in := range map[string]string{
			"unknown field":   `{"version":1,"entries":[],"extra":1}`,
			"unknown entry":   strings.Replace(good, `"clean":3`, `"clean":3,"subject":"hello"`, 1),
			"version":         strings.Replace(good, `"version":1`, `"version":2`, 1),
			"unknown channel": strings.Replace(good, `"fxin"`, `"nowhere"`, 1),
		} {
			e.s.want(e.s.runIn(in, nil, "operation", "trust", "import", "--file", "-"), 2, name)
		}
		e.s.want(e.s.run(nil, "operation", "trust", "import", "--file", e.s.path("absent.json")), 2, "missing file")
		if got := listed(); len(got) != 0 {
			t.Fatalf("a refused file wrote %v", got)
		}

		// The import from a file, then from standard input and inline
		// JSON: the same file again leaves everything as it is.
		var first, again, inline trustImportJSON
		e.json(&first, "operation", "trust", "import", "--file", path)
		if first.Imported != 2 || first.Unchanged != 0 || !first.Entries[0].Automatic {
			t.Fatalf("import = %+v", first)
		}
		rows := listed()
		if len(rows) != 2 {
			t.Fatalf("trust list after import = %v", rows)
		}
		auto := 0
		for _, row := range rows {
			if row["automatic"] == true {
				auto++
			}
		}
		if auto != 1 {
			t.Fatalf("trust list shows %d automatic keys, want the state:read one: %v", auto, rows)
		}
		r = e.s.runIn(good, nil, "operation", "trust", "import", "--file", "-", "--format", "json")
		e.s.want(r, 0, "re-import from stdin")
		if err := json.Unmarshal([]byte(r.stdout), &again); err != nil || again.Imported != 0 || again.Unchanged != 2 {
			t.Fatalf("re-import = %+v (%v)", again, err)
		}
		e.json(&inline, "operation", "trust", "import", "--file", good)
		if inline.Imported != 0 || inline.Unchanged != 2 {
			t.Fatalf("inline re-import = %+v", inline)
		}

		// MCP cannot read the caller's files: the object is the value.
		got := m.callTool(t, "operation.trust.import", map[string]any{"file": file(entry("state", "archive", 3)), "dry-run": true})
		if got["dry_run"] != true || got["imported"] != float64(1) {
			t.Errorf("MCP dry run = %v", got)
		}
		if res := m.request(t, "tools/call", map[string]any{"name": "operation.trust.import", "arguments": map[string]any{"file": path}}); res["isError"] != true {
			t.Errorf("MCP with a file path = %v, want a usage error", res)
		}
		got = m.callTool(t, "operation.trust.import", map[string]any{"file": file(entry("state", "archive", 3))})
		if got["imported"] != float64(1) {
			t.Errorf("MCP import = %v", got)
		}
		if n := len(listed()); n != 3 {
			t.Errorf("trust list after the MCP import has %d keys, want 3", n)
		}

		// REST: a person imports; a plugin token never does, whatever
		// roles it holds, and nothing is written.
		body, _ := json.Marshal(map[string]any{"flags": map[string]any{"file": file(entry("state", "star", 3))}})
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/operation/trust/import", person, string(body)); code != http.StatusOK ||
			!strings.Contains(asString(out), `"imported":1`) {
			t.Errorf("REST import = %d %v", code, out)
		}
		body, _ = json.Marshal(map[string]any{"flags": map[string]any{"file": file(entry("state", "spam", 99))}})
		code, out := x.rest(t, http.MethodPost, "/v1/commands/operation/trust/import", plugin, string(body))
		if code != http.StatusForbidden || !strings.Contains(asString(out), "heo.authz.plugin_decide") {
			t.Errorf("REST plugin import = %d %v, want 403 heo.authz.plugin_decide", code, out)
		}
		if n := len(listed()); n != 4 {
			t.Errorf("trust list has %d keys after a refused plugin import, want 4", n)
		}
	})
}
