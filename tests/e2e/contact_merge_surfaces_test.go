package e2e

import (
	"net/http"
	"testing"
)

// TestContactMergeOnlyOnSocket: merging contacts is destructive and
// shared, so REST, gRPC and MCP withhold it until the operator names
// the surface, while the other contact writes are served like any
// write. The control socket serves all of them.
func TestContactMergeOnlyOnSocket(t *testing.T) {
	s := newSandbox(t)
	x := startSurfaces(t, s)
	defer func() { s.want(result{exit: x.d.term()}, 0) }()
	token := x.auth.token(me())

	code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
	if code != http.StatusOK {
		t.Fatalf("discovery = %d", code)
	}
	cmds, _ := doc["commands"].([]any)
	seen := map[string]map[string]any{}
	for _, c := range cmds {
		e, _ := c.(map[string]any)
		name, _ := e["name"].(string)
		seen[name] = e
	}
	merge := seen["contact merge"]
	if merge == nil {
		t.Fatal("REST discovery does not list contact merge")
	}
	if merge["invocable"] != false || merge["reason"] != "unauthorized-destructive" {
		t.Errorf("REST discovery of contact merge: %v", merge)
	}
	for _, name := range []string{"contact create", "contact update"} {
		if e := seen[name]; e == nil || e["invocable"] != true {
			t.Errorf("REST discovery of %s: %v", name, e)
		}
	}
	if code, body := x.rest(t, http.MethodPost, "/v1/commands/contact/merge", token,
		`{"args":["ct_1","ct_2"],"flags":{"reason":"same_person","confirm":"yes"}}`); code < 400 {
		t.Errorf("REST ran a contact merge: %d %v", code, body)
	}
	if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"contact", "merge"}, "args": []string{"ct_1", "ct_2"},
		"flags": map[string]any{"reason": "same_person", "confirm": "yes"}}); code < 400 {
		t.Errorf("rpc ran a contact merge: %d %v", code, body)
	}
	m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
	tools := m.tools(t)
	if tools["contact.merge"] {
		t.Error("MCP lists contact.merge")
	}
	if !tools["contact.create"] || !tools["contact.update"] {
		t.Errorf("MCP lacks the other contact writes: %v", tools)
	}
}
