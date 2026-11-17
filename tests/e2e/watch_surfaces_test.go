package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// within runs f and fails the test when it does not return in d: a
// served call that holds the daemon shows up as a hang.
func within(t *testing.T, d time.Duration, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return in %s", what, d)
	}
}

// TestEventWatchIsSocketOnly drives the release binary on Postgres with
// the socket, REST, gRPC and MCP all serving: event watch is withheld
// from the three network surfaces (REST describes it non-invocable and
// mounts no route, gRPC answers NotFound, MCP lists no tool and refuses
// a call) and runs over the control socket, which carries the
// terminal's watch as short --step calls. event list, a plain request
// and reply, is served on all of them. A watch attached at the
// terminal never holds the daemon: REST, gRPC, MCP, the socket and the
// terminal's other commands keep answering, and a served call that
// reaches the daemon without --step is refused at once, not run as a
// stream.
func TestEventWatchIsSocketOnly(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	x, token, sock := serveDesk(t, s)
	d := x.d
	defer func() { s.s.want(result{exit: d.term()}, 0) }()
	path := []string{"event", "watch"}

	// REST: described, not invocable, no route of any kind.
	code, doc := x.rest(t, http.MethodGet, "/v1/commands", token, "")
	if code != http.StatusOK {
		t.Fatalf("discovery = %d", code)
	}
	var seen, listed bool
	cmds, _ := doc["commands"].([]any)
	for _, c := range cmds {
		e, _ := c.(map[string]any)
		switch e["name"] {
		case "event watch":
			seen = true
			if e["invocable"] != false || e["reason"] != "withheld-by-command" {
				t.Errorf("REST discovery of event watch = %v", e)
			}
		case "event list":
			// A plain request and reply: served, unlike the stream.
			listed = true
			if e["invocable"] != true {
				t.Errorf("REST discovery of event list = %v", e)
			}
		}
	}
	if !seen || !listed {
		t.Errorf("REST discovery describes event watch %v, event list %v", seen, listed)
	}
	for _, route := range []string{"/v1/commands/event/watch", "/v1/commands/event/watch/stream"} {
		within(t, 10*time.Second, "REST "+route, func() {
			if code, body := x.rest(t, http.MethodGet, route, token, ""); code != http.StatusNotFound {
				t.Errorf("REST %s = %d %v, want 404", route, code, body)
			}
		})
	}
	resp, err := authed(http.MethodGet, "http://"+x.api+"/openapi.json", token, "")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(spec), "event/watch") ||
		!strings.Contains(string(spec), "conversation/list") || !strings.Contains(string(spec), "event/list") {
		t.Errorf("OpenAPI = %d, event watch listed: %v, conversation list listed: %v", resp.StatusCode,
			strings.Contains(string(spec), "event/watch"), strings.Contains(string(spec), "conversation/list"))
	}

	// gRPC: NotFound.
	within(t, 10*time.Second, "gRPC event watch", func() {
		code, body := x.invokeRPC(t, token, map[string]any{"path": path})
		if code != http.StatusNotFound || !strings.Contains(strings.ToLower(asString(body)), "not_found") {
			t.Errorf("rpc event watch = %d %v, want not_found", code, body)
		}
	})

	// MCP: not a tool, and a call by name is refused.
	m := newMCPClient(t, "http://"+x.mcp+"/mcp", token)
	tools := m.tools(t)
	if tools["event.watch"] || !tools["conversation.list"] || !tools["event.list"] {
		t.Errorf("MCP tools: event.watch %v, event.list %v, conversation.list %v",
			tools["event.watch"], tools["event.list"], tools["conversation.list"])
	}
	within(t, 10*time.Second, "MCP event.watch", func() {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 99, "method": "tools/call",
			"params": map[string]any{"name": "event.watch", "arguments": map[string]any{}},
		})
		status, raw := m.postStatus(t, string(b))
		if status < 400 && (strings.Contains(string(raw), `"isError":false`) || !strings.Contains(string(raw), "error")) {
			t.Errorf("MCP event.watch = %d %s, want a refusal", status, raw)
		}
	})

	// The socket: a served call without --step is refused at once,
	// where a stream would hold the daemon; with it, it runs one step.
	within(t, 10*time.Second, "socket event watch without --step", func() {
		resp := sockCall(t, sock, map[string]any{"path": path, "flags": map[string]any{"consumer": "raw"}})
		res, _ := resp["result"].(map[string]any)
		if resp["ok"] == true && res != nil && res["exit_code"] == float64(0) {
			t.Errorf("socket ran a stream inside the daemon: %v", resp)
		}
		if !strings.Contains(asString(resp), "control socket one step at a time") {
			t.Errorf("socket refusal does not say what to do: %v", resp)
		}
	})
	step := resultData(t, sockCall(t, sock, map[string]any{"path": path, "flags": map[string]any{
		"consumer": "raw", "from": "beginning", "step": `{"op":"open"}`,
	}}))
	if step["subscription"] == nil || step["subscription"] == "" {
		t.Errorf("socket open step = %v", step)
	}

	// Every surface runs commands before a watch attaches.
	ran := func(label string) {
		t.Helper()
		within(t, 10*time.Second, label+": REST", func() {
			if code, body := x.rest(t, http.MethodGet, "/v1/commands/conversation/list", token, ""); code != http.StatusOK {
				t.Errorf("%s: REST conversation list = %d %v", label, code, body)
			}
		})
		within(t, 10*time.Second, label+": gRPC", func() {
			if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"conversation", "list"}}); code != http.StatusOK {
				t.Errorf("%s: rpc conversation list = %d %v", label, code, body)
			}
		})
		within(t, 10*time.Second, label+": MCP", func() { m.callTool(t, "conversation.list", map[string]any{}) })
		within(t, 10*time.Second, label+": REST event list", func() {
			if code, body := x.rest(t, http.MethodGet, "/v1/commands/event/list", token, ""); code != http.StatusOK {
				t.Errorf("%s: REST event list = %d %v", label, code, body)
			}
		})
		within(t, 10*time.Second, label+": gRPC event list", func() {
			if code, body := x.invokeRPC(t, token, map[string]any{"path": []string{"event", "list"}}); code != http.StatusOK {
				t.Errorf("%s: rpc event list = %d %v", label, code, body)
			}
		})
		within(t, 10*time.Second, label+": MCP event list", func() { m.callTool(t, "event.list", map[string]any{}) })
	}
	ran("before the watch")

	// A watch attached at the terminal, over that socket, holds nothing:
	// REST, gRPC, MCP, the socket and the terminal's other commands run
	// real commands beside it, all at once, and each caller gets its own
	// intact answer while the watch still sees every event.
	reply := func(text, key string) replyJSON {
		t.Helper()
		// The burst before it may have spent the write tier: exit 6 is
		// the documented "retry", with the same key.
		args := []string{"message", "reply", id, "--text", text, "--idempotency-key", key, "--format", "json"}
		var res result
		for range 50 {
			if res = s.s.run(nil, args...); res.exit != 6 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		s.s.want(res, 0, args...)
		var r replyJSON
		if err := json.Unmarshal([]byte(res.stdout), &r); err != nil {
			t.Fatalf("heo %v: %v\n%s", args, err, res.stdout)
		}
		return r
	}
	created := func(msg model.ID) func(model.Event) bool {
		return func(ev model.Event) bool { return ev.Kind == model.EventMessageCreated && ev.Resource.ID == msg }
	}
	const pool = 2
	var w *watchProc
	seedPool(t, sock, id, pool)
	beside := func(label, tag string) {
		t.Helper()
		run := startHammer(id, s.conv, x.callers(t, token, sock, 2), 3, pool, tag)
		within(t, 20*time.Second, label+": REST discovery", func() {
			if code, _ := x.rest(t, http.MethodGet, "/v1/commands", token, ""); code != http.StatusOK {
				t.Errorf("%s: REST discovery = %d", label, code)
			}
		})
		within(t, 10*time.Second, label+": MCP", func() { _ = m.tools(t) })
		within(t, 10*time.Second, label+": socket", func() {
			resultData(t, sockCall(t, sock, map[string]any{"path": []string{"daemon", "ping"}}))
		})
		within(t, 30*time.Second, label+": terminal", func() { s.heo(0, "conversation", "list") })
		w.untilAll(run.wait(t, time.Minute))
	}
	w = s.s.watch("--consumer", "attached", "--from", "beginning", "--format", "json")
	r1 := reply("first while watching", "w1")
	w.until(created(r1.Message))
	beside("while a watch is attached", "wa")
	r2 := reply("second while watching", "w2")
	w.until(created(r2.Message))
	beside("after it streamed more", "wb")
	if code, stderr := w.interrupt(); code != 0 {
		t.Fatalf("attached watch exit %d\n%s", code, stderr)
	}
	ran("after it stopped")
}
