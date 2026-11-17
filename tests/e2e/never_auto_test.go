package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// neverAutoProfile is trustProfile with the edit class capped at asking in
// batches and the person the plugins act for named, so the keys an import
// writes are the keys a write is held under.
const neverAutoProfile = "held_operations:\n  enabled: true\n  on_behalf_of: uid:1001\n  trust:\n    enabled: true\n    unattended: false\n" +
	"    never_auto: [\"edit:message\"]\n" +
	"authorization:\n  grants:\n    - actor: plugin:triage\n      roles: [desk, operator]\n" + noRateLimit

// TestNeverAutoClassStaysHeld: a class under never_auto is held at a clean
// count of 99, shown with its reason in the trust list, and a batch
// approval still decides it.
func TestNeverAutoClassStaysHeld(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, neverAutoProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api := freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--addr", api)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api}
		person, plugin := auth.token(me()), auth.token("plugin:triage")

		// 99 clean approvals earned elsewhere: kept, and the class stays held.
		imp := `{"version":1,"entries":[{"channel":"fxin","proposer":"plugin:triage","on_behalf_of":"uid:1001","verb":"edit","class":"message","clean":99}]}`
		var res trustImportJSON
		e.json(&res, "operation", "trust", "import", "--file", imp)
		if res.Imported != 1 || res.Automatic != 0 || res.Entries[0].Automatic || res.Entries[0].Clean != 99 {
			t.Fatalf("import = %+v, want the count kept and the class not automatic", res)
		}

		body, _ := json.Marshal(map[string]any{"args": []string{string(e.fxMsg.Ref.ID)},
			"flags": map[string]any{"revision": string(e.message(e.fxMsg).Revision), "text": "capped", "idempotency-key": "na-1"}})
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/message/edit", plugin, string(body)); code != http.StatusOK {
			t.Fatalf("plugin edit = %d %v", code, out)
		}
		if n := len(e.appliedLines()); n != 0 {
			t.Fatalf("a never-automatic edit at count 99 was applied (%d effects)", n)
		}
		code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/list", person, "")
		data, _ := out["data"].(map[string]any)
		items, _ := data["items"].([]any)
		if code != http.StatusOK || len(items) != 1 {
			t.Fatalf("held list = %d %v, want the edit held", code, out)
		}
		op := fmt.Sprint(items[0].(map[string]any)["id"])
		if r := fmt.Sprint(items[0].(map[string]any)["reason"]); !strings.Contains(r, "never automatic") {
			t.Fatalf("held reason = %q", r)
		}

		// The trust list keeps the count and says why it is held.
		var page struct{ Items []map[string]any }
		e.json(&page, "operation", "trust", "list", "--inbox", "fxin")
		if len(page.Items) != 1 || page.Items[0]["clean"] != float64(99) || page.Items[0]["automatic"] != false ||
			!strings.Contains(fmt.Sprint(page.Items[0]["reason"]), "never automatic") {
			t.Fatalf("trust list = %v", page.Items)
		}

		// A batch approval still decides it.
		body, _ = json.Marshal(map[string]any{"flags": map[string]any{"class": "edit:message", "inbox": "fxin"}})
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", person, string(body))
		if code != http.StatusOK || !strings.Contains(asString(out), op) {
			t.Fatalf("batch approve = %d %v", code, out)
		}
		e.s.settleAccepted(d, model.ID(op))
		if n := len(e.appliedLines()); n != 1 {
			t.Fatalf("provider applied %d effects after the batch approval, want 1", n)
		}
	})
}
