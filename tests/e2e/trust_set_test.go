package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestTrustLevelsAndManualSet: trust list carries threshold, level and
// label, derived by the daemon, on the CLI, REST and MCP; a person sets
// and resets a count by hand on every surface; a plugin token never does,
// whatever roles it holds; a never_auto class takes a count below its
// threshold only.
func TestTrustLevelsAndManualSet(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, neverAutoProfile)
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
		var res trustImportJSON
		e.json(&res, "operation", "trust", "import", "--file",
			`{"version":1,"entries":[`+entry("state", "archive", 2)+","+entry("edit", "message", 2)+`]}`)
		if res.Imported != 2 {
			t.Fatalf("import = %+v", res)
		}

		rows := func() map[string]map[string]any {
			t.Helper()
			var page struct{ Items []map[string]any }
			e.json(&page, "operation", "trust", "list", "--inbox", "fxin")
			by := map[string]map[string]any{}
			for _, r := range page.Items {
				by[fmt.Sprint(r["verb"])+":"+fmt.Sprint(r["class"])] = r
			}
			return by
		}
		got := rows()
		arch, edit := got["state:archive"], got["edit:message"]
		if arch["level"] != "ask_in_batches" || arch["level_label"] != "Ask in batches" || arch["threshold"] != float64(3) {
			t.Fatalf("archive row = %v", arch)
		}
		if edit["level"] != "never_auto" || edit["level_label"] != "Never auto" || edit["threshold"] != float64(15) {
			t.Fatalf("edit row = %v", edit)
		}
		archKey, editKey := fmt.Sprint(arch["key"]), fmt.Sprint(edit["key"])

		// REST list carries the same.
		code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/trust/list?inbox=fxin", person, "")
		if code != http.StatusOK || !strings.Contains(asString(out), `"level":"ask_in_batches"`) {
			t.Errorf("REST trust list = %d %v", code, out)
		}

		// A person promotes by REST; a plugin token is refused.
		body, _ := json.Marshal(map[string]any{"args": []string{archKey}, "flags": map[string]any{"inbox": "fxin", "count": 3, "note": "earned"}})
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/trust/set", plugin, string(body))
		if code != http.StatusForbidden || !strings.Contains(asString(out), "heo.authz.plugin_decide") {
			t.Errorf("REST plugin trust set = %d %v, want 403 heo.authz.plugin_decide", code, out)
		}
		if rows()["state:archive"]["clean"] != float64(2) {
			t.Fatal("a refused plugin change wrote")
		}
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/trust/set", person, string(body))
		if code != http.StatusOK || !strings.Contains(asString(out), `"outcome":"set"`) {
			t.Fatalf("REST trust set = %d %v", code, out)
		}
		if r := rows()["state:archive"]; r["clean"] != float64(3) || r["level"] != "auto" || r["approvals"] != float64(3) {
			t.Fatalf("after set = %v", r)
		}

		// MCP passes the key as --key; reset goes back to ask each.
		reset := m.callTool(t, "operation.trust.set", map[string]any{"key": archKey, "inbox": "fxin", "reset": true})
		if reset["outcome"] != "reset" {
			t.Fatalf("MCP reset = %v", reset)
		}
		if r := rows()["state:archive"]; r["clean"] != float64(0) || r["level"] != "ask_each" {
			t.Fatalf("after reset = %v", r)
		}
		if res := m.request(t, "tools/call", map[string]any{"name": "operation.trust.set", "arguments": map[string]any{"inbox": "fxin", "count": 1}}); res["isError"] != true {
			t.Errorf("MCP set without a key = %v, want a usage error", res)
		}
		if res := m.request(t, "tools/call", map[string]any{"name": "operation.trust.set", "arguments": map[string]any{"key": archKey, "inbox": "fxin"}}); res["isError"] != true {
			t.Errorf("MCP set without a change = %v, want a usage error", res)
		}

		// A never_auto class stops below its threshold of 15.
		r := e.s.run(nil, "operation", "trust", "set", editKey, "--inbox", "fxin", "--count", "15")
		e.s.want(r, 2, "never_auto above its threshold")
		e.s.want(e.s.run(nil, "operation", "trust", "set", editKey, "--inbox", "fxin", "--count", "9"), 0, "never_auto below its threshold")
		if r := rows()["edit:message"]; r["clean"] != float64(9) || r["level"] != "never_auto" {
			t.Fatalf("edit row = %v", r)
		}
		// --dry-run writes nothing.
		e.s.want(e.s.run(nil, "operation", "trust", "set", archKey, "--inbox", "fxin", "--count", "7", "--dry-run"), 0, "dry run")
		if rows()["state:archive"]["clean"] != float64(0) {
			t.Fatal("a dry run wrote")
		}
		// Neither held operations nor history moved.
		if code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/history", person, ""); code != http.StatusOK || strings.Contains(asString(out), "approved") {
			t.Errorf("held history = %d %v", code, out)
		}
	})
}
