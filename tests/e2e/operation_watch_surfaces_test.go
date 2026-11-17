package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

// TestOperationWatchIsSocketOnly drives the e2e build on Postgres with
// the socket, REST, gRPC and MCP all serving: operation watch is
// withheld from the three network surfaces exactly as event watch is
// (REST describes it non-invocable and mounts no route, gRPC answers
// NotFound, MCP lists no tool and refuses a call) and runs over the
// control socket as short --step calls. A watch attached at the
// terminal to an operation that does not end holds nothing: the other
// surfaces, the socket and the terminal's other commands keep
// answering, and a served call that reaches the daemon without --step is
// refused at once. The read commands of the group are served everywhere.
func TestOperationWatchIsSocketOnly(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-wsurf"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "hi", "--idempotency-key", "surf-1")
		c.stop(d)

		auth := c.s.useHTTPAuth()
		api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
		d = c.s.start(nil, nil, []string{"socket", "receivers", "api", "rpc", "mcp"}, "-c", auth.config,
			"service", "start", "--disable=workers", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
			"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
		defer func() { c.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}
		token := auth.token(me())
		sock := d.addr("socket")
		op := string(sent.Operation.ID)
		path := []string{"operation", "watch"}

		// REST: described, not invocable, no route of any kind; the
		// group's reads are served.
		code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
		if code != http.StatusOK {
			t.Fatalf("discovery = %d", code)
		}
		seen := map[string]map[string]any{}
		cmds, _ := doc["commands"].([]any)
		for _, cm := range cmds {
			e, _ := cm.(map[string]any)
			name, _ := e["name"].(string)
			seen[name] = e
		}
		if e := seen["operation watch"]; e == nil || e["invocable"] != false || e["reason"] != "withheld-by-command" {
			t.Errorf("REST discovery of operation watch = %v", e)
		}
		for _, name := range []string{"operation get", "operation list"} {
			if e := seen[name]; e == nil || e["invocable"] == false {
				t.Errorf("REST discovery of %s = %v, want it served", name, e)
			}
		}
		for _, route := range []string{"/v1/commands/operation/watch", "/v1/commands/operation/watch/stream"} {
			within(t, 10*time.Second, "REST "+route, func() {
				if code, body := x.rest(t, http.MethodGet, route+"?arg="+op, token, ""); code != http.StatusNotFound {
					t.Errorf("REST %s = %d %v, want 404", route, code, body)
				}
			})
		}
		resp, err := authed(http.MethodGet, "http://"+api+"/openapi.json", token, "")
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || strings.Contains(string(spec), "operation/watch") ||
			!strings.Contains(string(spec), "operation/get") || !strings.Contains(string(spec), "operation/list") {
			t.Errorf("OpenAPI = %d; watch listed: %v, get listed: %v", resp.StatusCode,
				strings.Contains(string(spec), "operation/watch"), strings.Contains(string(spec), "operation/get"))
		}
		within(t, 10*time.Second, "REST operation get", func() {
			if code, body := x.rest(t, http.MethodGet, "/v1/commands/operation/get?arg="+op, token, ""); code != http.StatusOK ||
				!strings.Contains(asString(body), op) {
				t.Errorf("REST operation get = %d %v", code, body)
			}
		})

		// gRPC: NotFound; the reads are served.
		within(t, 10*time.Second, "gRPC operation watch", func() {
			code, body := x.invokeRPC(t, token, map[string]any{"path": path, "args": []string{op}})
			if code != http.StatusNotFound || !strings.Contains(strings.ToLower(asString(body)), "not_found") {
				t.Errorf("rpc operation watch = %d %v, want not_found", code, body)
			}
		})
		within(t, 10*time.Second, "gRPC operation list", func() {
			if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"operation", "list"}}); code != http.StatusOK {
				t.Errorf("rpc operation list = %d %v", code, body)
			}
		})

		// MCP: not a tool, and a call by name is refused.
		m := newMCPClient(t, "http://"+mcp+"/mcp", token)
		tools := m.tools(t)
		if tools["operation.watch"] || !tools["operation.get"] || !tools["operation.list"] {
			t.Errorf("MCP tools: operation.watch %v, operation.get %v, operation.list %v",
				tools["operation.watch"], tools["operation.get"], tools["operation.list"])
		}
		within(t, 10*time.Second, "MCP operation.watch", func() {
			b, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 99, "method": "tools/call",
				"params": map[string]any{"name": "operation.watch", "arguments": map[string]any{}},
			})
			status, raw := m.postStatus(t, string(b))
			if status < 400 && (strings.Contains(string(raw), `"isError":false`) || !strings.Contains(string(raw), "error")) {
				t.Errorf("MCP operation.watch = %d %s, want a refusal", status, raw)
			}
		})
		within(t, 10*time.Second, "MCP operation.get", func() {
			got := m.callTool(t, "operation.get", map[string]any{"key": "surf-1"})
			if !strings.Contains(asString(got), op) {
				t.Errorf("MCP operation.get = %v", got)
			}
		})

		// The socket: a served call without --step is refused at once,
		// where a stream would hold the daemon; with it, it runs one
		// step and answers the operation.
		within(t, 10*time.Second, "socket operation watch without --step", func() {
			resp := sockCall(t, sock, map[string]any{"path": path, "args": []string{op}})
			res, _ := resp["result"].(map[string]any)
			if resp["ok"] == true && res != nil && res["exit_code"] == float64(0) {
				t.Errorf("socket ran a watch inside the daemon: %v", resp)
			}
			if !strings.Contains(asString(resp), "control socket one step at a time") {
				t.Errorf("socket refusal does not say what to do: %v", resp)
			}
		})
		step := resultData(t, sockCall(t, sock, map[string]any{"path": path, "args": []string{op},
			"flags": map[string]any{"step": `{"op":"get"}`}}))
		if step["id"] != op || step["state"] != string(model.Queued) {
			t.Errorf("socket step = %v", step)
		}
		r := sockCall(t, sock, map[string]any{"path": path, "args": []string{op}, "flags": map[string]any{"step": `{"op":"sleep"}`}})
		if !strings.Contains(asString(r), "unknown step") {
			t.Errorf("unknown step = %v", r)
		}

		// A watch attached at the terminal to an operation that does not
		// end holds nothing.
		serving := func(label string) {
			t.Helper()
			within(t, 10*time.Second, label+": REST discovery", func() {
				if code, _ := x.rest(t, http.MethodGet, "/v1/commands", token, ""); code != http.StatusOK {
					t.Errorf("%s: REST discovery = %d", label, code)
				}
			})
			within(t, 10*time.Second, label+": MCP", func() { _ = m.tools(t) })
			within(t, 10*time.Second, label+": socket", func() {
				resultData(t, sockCall(t, sock, map[string]any{"path": []string{"daemon", "ping"}}))
			})
			within(t, 20*time.Second, label+": terminal", func() { c.heo(0, "operation", "get", op) })
		}
		wait := c.asyncRun("operation", "watch", op, "--timeout", "6s", "--format", "json")
		time.Sleep(1500 * time.Millisecond)
		serving("while a watch is attached")
		time.Sleep(1500 * time.Millisecond)
		serving("a moment later")
		res := wait()
		if res.exit != 6 || !strings.Contains(res.stderr, "still queued") {
			t.Errorf("attached watch: exit %d\nstdout: %s\nstderr: %s", res.exit, res.stdout, res.stderr)
		}
		if end := lastLine(t, res.stdout); end.State != model.Queued || end.ID != sent.Operation.ID {
			t.Errorf("attached watch wrote %+v", end)
		}
	})
}
