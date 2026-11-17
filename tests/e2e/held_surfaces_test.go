package e2e

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// heldProfile turns the hold point on and grants the plugin actor the
// desk and operator roles: a role is never enough to decide.
const heldProfile = "held_operations:\n  enabled: true\nauthorization:\n  grants:\n" +
	"    - actor: plugin:triage\n      roles: [desk, operator]\n" + noRateLimit

// TestHeldOperationsOnServedSurfaces drives the e2e build on Postgres
// with the socket, REST, gRPC and MCP serving and the hold point on. A
// plugin actor's message edit is held; a person reads, approves and
// rejects held operations on every surface; a plugin token is refused on
// every deciding verb with the policy's reason and the operation stays
// held; the reads are served to a plugin and a person alike.
func TestHeldOperationsOnServedSurfaces(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, heldProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "rpc", "mcp", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
			"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}
		person, plugin := auth.token(me()), auth.token("plugin:triage")
		m := newMCPClient(t, "http://"+mcp+"/mcp", person)

		// Discovery: every verb is served; the reads are GET, the
		// decisions POST.
		code, doc := x.rest(t, http.MethodGet, "/v1/commands", person, "")
		if code != http.StatusOK {
			t.Fatalf("discovery = %d", code)
		}
		seen := map[string]map[string]any{}
		cmds, _ := doc["commands"].([]any)
		for _, cm := range cmds {
			ent, _ := cm.(map[string]any)
			name, _ := ent["name"].(string)
			seen[name] = ent
		}
		tools := m.tools(t)
		for name, method := range map[string]string{
			"operation held list": "GET", "operation held show": "GET", "operation held history": "GET",
			"operation trust list": "GET", "operation held approve": "POST", "operation held reject": "POST",
		} {
			ent := seen[name]
			if ent == nil || ent["invocable"] == false || !strings.EqualFold(fmt.Sprint(ent["method"]), method) {
				t.Errorf("REST discovery of %s = %v, want a %s route", name, ent, method)
			}
			if tool := strings.ReplaceAll(name, " ", "."); !tools[tool] {
				t.Errorf("MCP lists no tool %s", tool)
			}
		}

		// A plugin proposes two edits: both are held.
		propose := func(text string) string {
			t.Helper()
			body, _ := json.Marshal(map[string]any{"args": []string{string(e.fxMsg.Ref.ID)},
				"flags": map[string]any{"revision": string(e.message(e.fxMsg).Revision), "text": text, "idempotency-key": "p-" + text}})
			code, out := x.rest(t, http.MethodPost, "/v1/commands/message/edit", plugin, string(body))
			if code != http.StatusOK {
				t.Fatalf("plugin edit = %d %v", code, out)
			}
			return asString(out)
		}
		propose("first")
		listed := func(tok string) []map[string]any {
			t.Helper()
			code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/list", tok, "")
			if code != http.StatusOK {
				t.Fatalf("held list = %d %v", code, out)
			}
			data, _ := out["data"].(map[string]any)
			var items []map[string]any
			raw, _ := data["items"].([]any)
			for _, it := range raw {
				if o, ok := it.(map[string]any); ok {
					items = append(items, o)
				}
			}
			return items
		}
		items := listed(person)
		if len(items) != 1 {
			t.Fatalf("held list = %v, want one held edit", items)
		}
		op := fmt.Sprint(items[0]["id"])
		if op == "" || items[0]["status"] != "held" || items[0]["proposer"] != "plugin:triage" {
			t.Fatalf("held item = %v", items[0])
		}

		// Reads reach every surface, a plugin's included (they follow
		// the policy's operation.read).
		if got := listed(plugin); len(got) != 1 {
			t.Errorf("plugin's REST held list = %v", got)
		}
		if code, body := x.rest(t, http.MethodGet, "/v1/commands/operation/held/show?arg="+url.QueryEscape(op), person, ""); code != http.StatusOK ||
			!strings.Contains(asString(body), op) {
			t.Errorf("REST held show = %d %v", code, body)
		}
		if code, body := x.invokeRPC(t, person, map[string]any{"path": []string{"operation", "held", "show"}, "args": []string{op}}); code != http.StatusOK ||
			!strings.Contains(asString(body), op) {
			t.Errorf("rpc held show = %d %v", code, body)
		}
		if got := m.callTool(t, "operation.held.list", map[string]any{}); !strings.Contains(asString(got), op) {
			t.Errorf("MCP held list = %v", got)
		}
		if code, body := x.rest(t, http.MethodGet, "/v1/commands/operation/trust/list?inbox=fxin", person, ""); code != http.StatusOK {
			t.Errorf("REST trust list = %d %v", code, body)
		}
		if code, body := x.rest(t, http.MethodGet, "/v1/commands/operation/held/history?arg="+url.QueryEscape(op), person, ""); code != http.StatusOK ||
			!strings.Contains(asString(body), "proposed") {
			t.Errorf("REST held history = %d %v", code, body)
		}

		// MCP cannot pass a positional argument: --operation names the
		// operation instead, exactly one form. Neither is a usage error
		// (a refusal); so is both, through REST.
		for _, verb := range []string{"show", "approve", "reject"} {
			res := m.request(t, "tools/call", map[string]any{"name": "operation.held." + verb, "arguments": map[string]any{}})
			if res["isError"] != true {
				t.Errorf("MCP %s without an operation = %v, want a usage error", verb, res)
			}
		}
		if code, body := x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", person,
			`{"args":["`+op+`"],"flags":{"operation":["`+op+`"]}}`); code == http.StatusOK {
			t.Errorf("REST approve with both forms = %d %v, want a usage error", code, body)
		}

		// A plugin token never decides, whatever roles it holds: refused
		// with the policy's reason on every surface, nothing decided.
		const reason = "heo.authz.plugin_decide"
		for _, verb := range []string{"approve", "reject"} {
			route := "/v1/commands/operation/held/" + verb
			code, body := x.rest(t, http.MethodPost, route, plugin, `{"args":["`+op+`"]}`)
			if code != http.StatusForbidden || !strings.Contains(asString(body), reason) {
				t.Errorf("REST plugin %s = %d %v, want 403 %s", verb, code, body, reason)
			}
			code, body = x.invokeRPC(t, plugin, map[string]any{"path": []string{"operation", "held", verb}, "args": []string{op}})
			if body["exit_code"] != float64(5) || !strings.Contains(asString(body), reason) {
				t.Errorf("rpc plugin %s = %d %v", verb, code, body)
			}
		}
		if got := listed(person); len(got) != 1 || got[0]["status"] != "held" {
			t.Fatalf("after the plugin's refused decisions: %v, want the edit still held", got)
		}
		if n := len(e.appliedLines()); n != 0 {
			t.Fatalf("provider applied %d effects of a held edit", n)
		}

		// A person approves over REST: queued, applied by the workers.
		code, body := x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", person, `{"args":["`+op+`"],"flags":{"note":"checked"}}`)
		if code != http.StatusOK || !strings.Contains(asString(body), `"ok":true`) && !strings.Contains(asString(body), `"ok": true`) {
			t.Fatalf("REST approve = %d %v", code, body)
		}
		// The provider's effect lands inside the worker's call; the
		// message record, revision included, commits when the operation
		// settles. Wait for the operation, not the effect: the next
		// edit is revision-guarded.
		e.s.settleAccepted(d, model.ID(op))
		if n := len(e.appliedLines()); n != 1 {
			t.Fatalf("provider applied %d effects of the approved edit, want 1", n)
		}

		// A person shows then approves the next over MCP, naming it with
		// --operation: queued, applied by the workers.
		propose("second")
		items = listed(person)
		if len(items) != 1 {
			t.Fatalf("held list = %v, want the second edit alone", items)
		}
		opMCP := fmt.Sprint(items[0]["id"])
		if got := m.callTool(t, "operation.held.show", map[string]any{"operation": opMCP}); !strings.Contains(asString(got), opMCP) {
			t.Errorf("MCP held show --operation = %v", got)
		}
		got := m.callTool(t, "operation.held.approve", map[string]any{"operation": []any{opMCP}, "note": "checked"})
		if !strings.Contains(asString(got), `"ok":true`) && !strings.Contains(asString(got), `"ok": true`) {
			t.Fatalf("MCP approve = %v", got)
		}
		e.s.settleAccepted(d, model.ID(opMCP))
		if n := len(e.appliedLines()); n != 2 {
			t.Fatalf("provider applied %d effects after the MCP approval, want 2", n)
		}

		// A person rejects the next over gRPC: nothing is applied.
		propose("third")
		items = listed(person)
		if len(items) != 1 {
			t.Fatalf("held list = %v, want the third edit alone", items)
		}
		op2 := fmt.Sprint(items[0]["id"])
		code, out := x.invokeRPC(t, person, map[string]any{"path": []string{"operation", "held", "reject"}, "args": []string{op2}})
		if code != http.StatusOK || !strings.Contains(asString(out), `"ok\":true`) && !strings.Contains(asString(out), `"ok": true`) {
			t.Errorf("rpc reject = %d %v", code, out)
		}
		if got := listed(person); len(got) != 0 {
			t.Errorf("held after reject = %v", got)
		}
		if n := len(e.appliedLines()); n != 2 {
			t.Errorf("provider applied %d effects, want only the two approved", n)
		}

		// An array flag is a JSON array over MCP: --kind filters the
		// listing, and --operation names several operations at once.
		propose("fourth")
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/label/create", plugin,
			`{"args":["billing"],"flags":{"idempotency-key":"lc-array"}}`); code != http.StatusOK {
			t.Fatalf("plugin label create = %d %v", code, out)
		}
		items = listed(person)
		if len(items) != 2 {
			t.Fatalf("held list = %v, want the edit and the label create", items)
		}
		ids := []any{fmt.Sprint(items[0]["id"]), fmt.Sprint(items[1]["id"])}
		kinds := m.callTool(t, "operation.held.list", map[string]any{"kind": []any{"message.edit", "label.configure"}})
		if !strings.Contains(asString(kinds), fmt.Sprint(ids[0])) || !strings.Contains(asString(kinds), fmt.Sprint(ids[1])) {
			t.Errorf("MCP held list --kind [message.edit,label.configure] = %v, want both operations", kinds)
		}
		none := m.callTool(t, "operation.held.list", map[string]any{"kind": []any{"label.configure"}})
		if rows, _ := none["items"].([]any); len(rows) != 1 || fmt.Sprint(rows[0].(map[string]any)["kind"]) != "label.configure" {
			t.Errorf("MCP held list --kind [label.configure] = %v, want the label operation alone", none)
		}
		got = m.callTool(t, "operation.held.reject", map[string]any{"operation": ids})
		if !strings.Contains(asString(got), `"ok":true`) && !strings.Contains(asString(got), `"ok": true`) {
			t.Fatalf("MCP reject of two operations = %v", got)
		}
		if got := listed(person); len(got) != 0 {
			t.Errorf("held after the MCP reject of both = %v", got)
		}
	})
}

// TestHeldLocalWritesOnServedSurfaces: a plugin actor's label create and
// conversation resolve, which uma holds only because the daemon opts them
// in, are held through REST: nothing is written until a person approves,
// and a rejected one leaves nothing.
func TestHeldLocalWritesOnServedSurfaces(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, heldProfile)
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api := freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--addr", api)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api}
		person, plugin := auth.token(me()), auth.token("plugin:triage")
		conv := string(e.fxMsg.Ref.Conversation.ID)

		var cv model.Conversation
		e.json(&cv, "conversation", "get", conv)
		post := func(route, body string) {
			t.Helper()
			if code, out := x.rest(t, http.MethodPost, route, plugin, body); code != http.StatusOK ||
				!strings.Contains(asString(out), "held") {
				t.Fatalf("plugin %s = %d %v, want a held operation", route, code, out)
			}
		}
		only := func() string {
			t.Helper()
			code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/list", person, "")
			data, _ := out["data"].(map[string]any)
			raw, _ := data["items"].([]any)
			if code != http.StatusOK || len(raw) != 1 {
				t.Fatalf("held list = %d %v, want one", code, out)
			}
			return fmt.Sprint(raw[0].(map[string]any)["id"])
		}
		decide := func(verb, id string) {
			t.Helper()
			if code, body := x.rest(t, http.MethodPost, "/v1/commands/operation/held/"+verb, person, `{"args":["`+id+`"]}`); code != http.StatusOK {
				t.Fatalf("REST %s = %d %v", verb, code, body)
			}
		}
		var labels pageJSON[model.Label]

		// A held label create writes nothing; approved, it writes once.
		post("/v1/commands/label/create", `{"args":["billing"],"flags":{"idempotency-key":"lc-1"}}`)
		e.json(&labels, "label", "list")
		if len(labels.Items) != 0 {
			t.Fatalf("labels while held = %+v", labels.Items)
		}
		decide("approve", only())
		e.json(&labels, "label", "list")
		if len(labels.Items) != 1 || labels.Items[0].Name != "billing" {
			t.Fatalf("labels after approve = %+v", labels.Items)
		}

		// A held resolve writes nothing; rejected, it still writes nothing.
		post("/v1/commands/conversation/resolve", `{"args":["`+conv+`"],"flags":{"revision":"`+string(cv.Revision)+`","idempotency-key":"rs-1"}}`)
		e.json(&cv, "conversation", "get", conv)
		if cv.Status != model.ConversationOpen {
			t.Fatalf("conversation while held = %s", cv.Status)
		}
		decide("reject", only())
		e.json(&cv, "conversation", "get", conv)
		if cv.Status != model.ConversationOpen {
			t.Fatalf("conversation after reject = %s", cv.Status)
		}
	})
}

// TestHeldEditAndBatchApproveOnServedSurfaces drives edit and the
// selector approval on every surface the same build serves: a person
// seeds an edit from `held show`, edits over MCP, and approves a class
// in a mailbox in one call; a plugin token is refused on both; an empty
// or oversized selector approves nothing; the batch counts once per
// trust key; a stale target is told from a revision mismatch.
func TestHeldEditAndBatchApproveOnServedSurfaces(t *testing.T) {
	withE2EBinary(t, func() {
		e := newEffectsSite(t, heldProfile)
		// The channel's webhook goes with this daemon: receive first.
		msgs := []model.Message{e.fxMsg,
			e.receive(e.fx, "fxin", "fx-1", "ev-fx-2", "pm-fx-2", "second"),
			e.receive(e.fx, "fxin", "fx-1", "ev-fx-3", "pm-fx-3", "third")}
		e.stop(e.d)
		auth := e.s.useHTTPAuth()
		api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
		d := e.s.start(nil, nil, []string{"socket", "receivers", "api", "rpc", "mcp", "workers"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
			"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
		defer func() { e.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}
		person, plugin := auth.token(me()), auth.token("plugin:triage")
		m := newMCPClient(t, "http://"+mcp+"/mcp", person)

		tools := m.tools(t)
		if !tools["operation.held.edit"] {
			t.Fatal("MCP lists no tool operation.held.edit")
		}
		_, doc := x.rest(t, http.MethodGet, "/v1/commands", person, "")
		var edit map[string]any
		cmds, _ := doc["commands"].([]any)
		for _, cm := range cmds {
			if ent, _ := cm.(map[string]any); ent["name"] == "operation held edit" {
				edit = ent
			}
		}
		if edit == nil || edit["invocable"] == false || !strings.EqualFold(fmt.Sprint(edit["method"]), "POST") {
			t.Errorf("REST discovery of operation held edit = %v, want a POST route", edit)
		}

		// A plugin proposes an edit of each of three messages: three held
		// operations of one class in one mailbox, plus a reply (another
		// class, on a conversation).
		for i, msg := range msgs {
			body, _ := json.Marshal(map[string]any{"args": []string{string(msg.Ref.ID)},
				"flags": map[string]any{"revision": string(e.message(msg).Revision), "text": fmt.Sprintf("proposed %d", i), "idempotency-key": fmt.Sprintf("be-%d", i)}})
			if code, out := x.rest(t, http.MethodPost, "/v1/commands/message/edit", plugin, string(body)); code != http.StatusOK {
				t.Fatalf("plugin edit %d = %d %v", i, code, out)
			}
		}
		held := func(tok string) map[string]map[string]any {
			t.Helper()
			code, out := x.rest(t, http.MethodGet, "/v1/commands/operation/held/list", tok, "")
			data, _ := out["data"].(map[string]any)
			raw, _ := data["items"].([]any)
			if code != http.StatusOK {
				t.Fatalf("held list = %d %v", code, out)
			}
			byTarget := map[string]map[string]any{}
			for _, it := range raw {
				o, _ := it.(map[string]any)
				byTarget[fmt.Sprint(o["target"])] = o
			}
			return byTarget
		}
		opOf := func(msg model.Message) string {
			t.Helper()
			o := held(person)["message/"+string(msg.Ref.ID)]
			if o == nil {
				t.Fatalf("no held operation for message %s", msg.Ref.ID)
			}
			return fmt.Sprint(o["id"])
		}
		op0, op1, op2 := opOf(msgs[0]), opOf(msgs[1]), opOf(msgs[2])

		// Edit, seeded from `held show`: the document carries the stored
		// intent, the operation and its revision. A person edits it over MCP.
		show := m.callTool(t, "operation.held.show", map[string]any{"operation": op0})
		intent, _ := show["intent"].(map[string]any)
		if intent == nil || !strings.Contains(asString(intent), "proposed 0") {
			t.Fatalf("held show carries no intent: %v", show)
		}
		editDoc := func(show map[string]any, text string) string {
			doc := map[string]any{}
			for k, v := range show {
				doc[k] = v
			}
			in := map[string]any{}
			for k, v := range show["intent"].(map[string]any) {
				in[k] = v
			}
			in["content"] = map[string]any{"parts": []any{map[string]any{"kind": "text", "text": text}}}
			doc["intent"] = in
			return asString(doc)
		}
		fixed := editDoc(show, "fixed by a person")

		// A plugin never edits, whatever roles it holds: refused with the
		// policy's reason on REST and gRPC, nothing decided.
		const reason = "heo.authz.plugin_decide"
		body, _ := json.Marshal(map[string]any{"args": []string{op0}, "flags": map[string]any{"args": fixed}})
		code, out := x.rest(t, http.MethodPost, "/v1/commands/operation/held/edit", plugin, string(body))
		if code != http.StatusForbidden || !strings.Contains(asString(out), reason) {
			t.Errorf("REST plugin edit = %d %v, want 403 %s", code, out, reason)
		}
		code, out = x.invokeRPC(t, plugin, map[string]any{"path": []string{"operation", "held", "edit"}, "args": []string{op0}, "flags": map[string]any{"args": fixed}})
		if out["exit_code"] != float64(5) || !strings.Contains(asString(out), reason) {
			t.Errorf("rpc plugin edit = %d %v", code, out)
		}
		if got := held(person); len(got) != 3 {
			t.Fatalf("held after the plugin's refused edits = %d, want 3", len(got))
		}

		// Usage errors decide nothing: no args, both ways of naming the
		// operation, a document read from another operation, a bad shape.
		for name, args := range map[string]map[string]any{
			"no args":         {"operation": op0},
			"no operation":    {"args": fixed},
			"not JSON":        {"operation": op0, "args": "{nope"},
			"another's seed":  {"operation": op1, "args": fixed},
			"not the intent":  {"operation": op0, "args": `{"message":{"id":"other"}}`},
			"intent not json": {"operation": op0, "args": `{"intent":"x"}`},
		} {
			if res := m.request(t, "tools/call", map[string]any{"name": "operation.held.edit", "arguments": args}); res["isError"] != true {
				t.Errorf("MCP edit %s = %v, want an error", name, res)
			}
		}
		body, _ = json.Marshal(map[string]any{"args": []string{op0}, "flags": map[string]any{"operation": op0, "args": fixed}})
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/operation/held/edit", person, string(body)); code == http.StatusOK {
			t.Errorf("REST edit naming the operation twice = %d %v", code, out)
		}
		if got := held(person); len(got) != 3 || len(e.appliedLines()) != 0 {
			t.Fatalf("held after refused edits = %d, applied %d", len(got), len(e.appliedLines()))
		}

		// --dry-run previews the edit and writes nothing.
		code, out = x.invokeRPC(t, person, map[string]any{"path": []string{"operation", "held", "edit"}, "args": []string{op0}, "flags": map[string]any{"args": fixed, "dry-run": true}})
		if code != http.StatusOK || !strings.Contains(asString(out), "effects") || !strings.Contains(asString(out), "operation:"+op0) {
			t.Fatalf("rpc edit preview = %d %v", code, out)
		}
		if got := held(person); len(got) != 3 {
			t.Fatalf("held after the preview = %d", len(got))
		}

		got := m.callTool(t, "operation.held.edit", map[string]any{"operation": op0, "args": fixed, "note": "typo", "corrected-keys": []any{}})
		if !strings.Contains(asString(got), `"ok":true`) || !strings.Contains(asString(got), `"decision":"edited"`) || !strings.Contains(asString(got), `"correction":true`) {
			t.Fatalf("MCP edit = %v", got)
		}
		e.s.settleAccepted(d, model.ID(op0))
		if lines := strings.Join(e.appliedLines(), "\n"); len(e.appliedLines()) != 1 || !strings.Contains(lines, "fixed by a person") {
			t.Fatalf("provider applied %q, want the one edit with the person's text", lines)
		}
		if got := held(person); len(got) != 2 {
			t.Fatalf("held after the edit = %d, want 2", len(got))
		}

		// Batch approve. The plugin never does, and an empty selector
		// approves nothing.
		const class, inbox = "edit:message", "fxin"
		body, _ = json.Marshal(map[string]any{"flags": map[string]any{"class": class, "inbox": inbox}})
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", plugin, string(body))
		if code != http.StatusForbidden || !strings.Contains(asString(out), reason) {
			t.Errorf("REST plugin selector approve = %d %v, want 403 %s", code, out, reason)
		}
		code, out = x.invokeRPC(t, plugin, map[string]any{"path": []string{"operation", "held", "approve"}, "flags": map[string]any{"class": class, "inbox": inbox}})
		if out["exit_code"] != float64(5) || !strings.Contains(asString(out), reason) {
			t.Errorf("rpc plugin selector approve = %d %v", code, out)
		}
		for name, args := range map[string]map[string]any{
			"class only": {"class": class}, "inbox only": {"inbox": inbox}, "proposer only": {"proposer": "plugin:triage"},
			"max only": {"max": 5}, "with an operation": {"class": class, "inbox": inbox, "operation": []any{op1}},
		} {
			if res := m.request(t, "tools/call", map[string]any{"name": "operation.held.approve", "arguments": args}); res["isError"] != true {
				t.Errorf("MCP approve with %s = %v, want a usage error", name, res)
			}
		}
		if got := held(person); len(got) != 2 || len(e.appliedLines()) != 1 {
			t.Fatalf("held after refused selectors = %d, applied %d", len(got), len(e.appliedLines()))
		}

		// --dry-run lists the matches; the cap refuses, never truncates.
		pre := m.callTool(t, "operation.held.approve", map[string]any{"class": class, "inbox": inbox, "dry-run": true})
		if effects, _ := pre["effects"].([]any); len(effects) != 2 || !strings.Contains(asString(pre), op1) || !strings.Contains(asString(pre), op2) {
			t.Fatalf("MCP selector preview = %v, want the two held edits", pre)
		}
		if got := held(person); len(got) != 2 {
			t.Fatalf("held after the preview = %d", len(got))
		}
		if res := m.request(t, "tools/call", map[string]any{"name": "operation.held.approve", "arguments": map[string]any{"class": class, "inbox": inbox, "max": 1}}); res["isError"] != true ||
			!strings.Contains(asString(res), "nothing approved") {
			t.Errorf("MCP selector past --max = %v, want it refused with nothing approved", res)
		}
		if got := held(person); len(got) != 2 || len(e.appliedLines()) != 1 {
			t.Fatalf("held after the oversized selector = %d, applied %d", len(got), len(e.appliedLines()))
		}
		// Another class in the mailbox matches nothing.
		none := m.callTool(t, "operation.held.approve", map[string]any{"class": "trash", "inbox": inbox})
		if rs, _ := none["results"].([]any); len(rs) != 0 || none["failed"] != float64(0) {
			t.Errorf("selector of another class = %v, want no results", none)
		}

		// The real run approves both under one batch id.
		got = m.callTool(t, "operation.held.approve", map[string]any{"class": class, "inbox": inbox, "note": "bulk"})
		rs, _ := got["results"].([]any)
		batch, _ := got["batch"].(string)
		if u, err := uuid.Parse(batch); len(rs) != 2 || got["failed"] != float64(0) || err != nil || u.Version() != 7 {
			t.Fatalf("MCP selector approve = %v, want 2 results under a UUIDv7 batch", got)
		}
		e.s.settleAccepted(d, model.ID(op1))
		e.s.settleAccepted(d, model.ID(op2))
		if n := len(e.appliedLines()); n != 3 {
			t.Fatalf("provider applied %d effects, want 3", n)
		}
		if got := held(person); len(got) != 0 {
			t.Fatalf("held after the batch = %d", len(got))
		}
		// Counted once per trust key: the edit was a correction, the two
		// batched approvals one clean approval.
		code, out = x.rest(t, http.MethodGet, "/v1/commands/operation/trust/list?inbox="+inbox, person, "")
		data, _ := out["data"].(map[string]any)
		rows, _ := data["items"].([]any)
		var row map[string]any
		for _, r := range rows {
			if o, _ := r.(map[string]any); o["class"] == "message" && o["verb"] == "edit" {
				row = o
			}
		}
		if code != http.StatusOK || row == nil || row["approvals"] != float64(1) || row["clean"] != float64(1) || row["corrections"] != float64(1) {
			t.Fatalf("trust of the class = %v (%d), want one clean approval for the batch and one correction for the edit", row, code)
		}

		// A stale target is told from a revision mismatch: a reply held on
		// a conversation that a person then resolved is refused as stale, for an
		// approval and for an edit (which has no --force).
		conv := string(e.fxMsg.Ref.Conversation.ID)
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/message/reply", plugin,
			`{"args":["`+conv+`"],"flags":{"text":"hello back","idempotency-key":"st-reply"}}`); code != http.StatusOK {
			t.Fatalf("plugin reply = %d %v", code, out)
		}
		var reply map[string]any
		for _, o := range held(person) {
			reply = o
		}
		if reply == nil {
			t.Fatal("the reply was not held")
		}
		rop := fmt.Sprint(reply["id"])
		var cv model.Conversation
		e.json(&cv, "conversation", "get", conv)
		if code, out := x.rest(t, http.MethodPost, "/v1/commands/conversation/resolve", person,
			`{"args":["`+conv+`"],"flags":{"revision":"`+string(cv.Revision)+`","idempotency-key":"st-resolve"}}`); code != http.StatusOK {
			t.Fatalf("person resolve = %d %v", code, out)
		}
		stale := func(what string, code int, out map[string]any) {
			t.Helper()
			if code != http.StatusConflict || !strings.Contains(asString(out), "stale") {
				t.Errorf("%s = %d %v, want 409 stale", what, code, out)
			}
		}
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", person, `{"args":["`+rop+`"]}`)
		stale("REST approve after the target moved", code, out)
		reshow := m.callTool(t, "operation.held.show", map[string]any{"operation": rop})
		body, _ = json.Marshal(map[string]any{"args": []string{rop}, "flags": map[string]any{"args": editDoc(reshow, "hello again")}})
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/held/edit", person, string(body))
		stale("REST edit after the target moved", code, out)
		if strings.Contains(asString(out), "--force") {
			t.Errorf("a stale edit suggests --force: %v", out)
		}
		code, out = x.rest(t, http.MethodPost, "/v1/commands/operation/held/approve", person,
			`{"args":["`+rop+`"],"flags":{"revision":"not-it"}}`)
		if code != http.StatusConflict || strings.Contains(asString(out), "stale") {
			t.Errorf("REST approve at a wrong revision = %d %v, want 409 and not stale", code, out)
		}
		if got := held(person); len(got) != 1 {
			t.Fatalf("held after the refusals = %d, want the reply still held", len(got))
		}
	})
}
