package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// mcpClient is one streamable-HTTP MCP session, initialized.
type mcpClient struct {
	url, token, session string
	next                int
}

func newMCPClient(t *testing.T, url, token string) *mcpClient {
	t.Helper()
	m := &mcpClient{url: url, token: token, next: 1}
	m.request(t, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	})
	m.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	return m
}

func (m *mcpClient) post(t *testing.T, body string) []byte {
	t.Helper()
	status, b := m.postStatus(t, body)
	if status >= 400 {
		t.Fatalf("MCP %d: %s", status, b)
	}
	return b
}

// postStatus posts body and returns the status and the reply, whatever
// the status.
func (m *mcpClient) postStatus(t *testing.T, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, m.url, strings.NewReader(body)) //nolint:noctx // loopback test URL
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if m.session != "" {
		req.Header.Set("Mcp-Session-Id", m.session)
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		m.session = id
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// request sends one JSON-RPC request and returns its result.
func (m *mcpClient) request(t *testing.T, method string, params any) map[string]any {
	t.Helper()
	id := m.next
	m.next++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	raw := m.post(t, string(b))
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var msg map[string]any
		if err := json.Unmarshal([]byte(data), &msg); err != nil {
			t.Fatal(err, data)
		}
		if msg["id"] == float64(id) {
			res, _ := msg["result"].(map[string]any)
			if res == nil {
				t.Fatalf("MCP %s: %v", method, msg)
			}
			return res
		}
	}
	t.Fatalf("MCP %s: no reply in %s", method, raw)
	return nil
}

func (m *mcpClient) listTools(t *testing.T) []map[string]any {
	t.Helper()
	res := m.request(t, "tools/list", map[string]any{})
	list, _ := res["tools"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		if tool, ok := x.(map[string]any); ok {
			out = append(out, tool)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no tools listed: %v", res)
	}
	return out
}

// tools is the set of tool names listed.
func (m *mcpClient) tools(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, tool := range m.listTools(t) {
		names[fmt.Sprint(tool["name"])] = true
	}
	return names
}

// callTool calls name and returns its structuredContent, failing on a
// tool error.
func (m *mcpClient) callTool(t *testing.T, name string, args map[string]any) map[string]any {
	t.Helper()
	res := m.request(t, "tools/call", map[string]any{"name": name, "arguments": args})
	if res["isError"] == true {
		t.Fatalf("tool %s failed: %v", name, res)
	}
	data, _ := res["structuredContent"].(map[string]any)
	return data
}

// mcpToolNames lists the tools of the MCP endpoint at url.
func mcpToolNames(t *testing.T, url, token string) map[string]bool {
	t.Helper()
	return newMCPClient(t, url, token).tools(t)
}

// regexpReady matches svc's ready_reported line.
func regexpReady(svc string) *regexp.Regexp {
	return regexp.MustCompile(`ready_reported object=service .*service=` + regexp.QuoteMeta(svc) + `\b`)
}
