package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// firstReplyProfile is the ladder with replies interactive and the person
// the plugins act for named: an import then earns reply:send in the keys a
// write is held under.
const firstReplyProfile = "held_operations:\n  enabled: true\n  on_behalf_of: uid:1001\n  trust:\n    enabled: true\n    unattended: false\n" +
	"authorization:\n  grants:\n    - actor: plugin:triage\n      roles: [desk, operator]\n" + noRateLimit

// TestFirstReplyToUnknownContactIsHeld: with 99 clean approvals on
// reply:send, a plugin's reply to a contact heo never wrote to is still
// held, with its reason; heo computed the fact, the request carried none.
func TestFirstReplyToUnknownContactIsHeld(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, firstReplyProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api := freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--addr", api)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api}
		person, plugin := auth.token(me()), auth.token("plugin:triage")

		var res trustImportJSON
		e.json(&res, "operation", "trust", "import", "--file",
			`{"version":1,"entries":[{"channel":"fxin","proposer":"plugin:triage","on_behalf_of":"uid:1001","verb":"reply","class":"send","clean":99}]}`)
		if res.Imported != 1 || !res.Entries[0].Automatic {
			t.Fatalf("import = %+v, want reply:send automatic at 99", res)
		}

		conv := string(e.fxMsg.Ref.Conversation.ID)
		body, _ := json.Marshal(map[string]any{"args": []string{conv}, "flags": map[string]any{"text": "hello back", "idempotency-key": "fr-1"}})
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/message/reply", plugin, string(body)); code != http.StatusOK {
			t.Fatalf("plugin reply = %d %v", code, out)
		}
		code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/list", person, "")
		data, _ := out["data"].(map[string]any)
		items, _ := data["items"].([]any)
		if code != http.StatusOK || len(items) != 1 {
			t.Fatalf("held list = %d %v, want the first reply held at count 99", code, out)
		}
		if r := fmt.Sprint(items[0].(map[string]any)["reason"]); !strings.Contains(r, "first reply") {
			t.Fatalf("held reason = %q", r)
		}
		if n := len(e.appliedLines()); n != 0 {
			t.Fatalf("a held first reply reached the provider (%d effects)", n)
		}
	})
}
