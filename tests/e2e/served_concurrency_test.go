package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
)

// A surfaceCaller runs one command on one served surface and returns
// the command's data, or the reason the answer is not an intact one:
// a transport error, a non-zero exit, an undecodable body.
type surfaceCaller struct {
	name string
	// positional is whether the surface can pass positional
	// arguments. MCP cannot: a tool takes flags only.
	positional bool
	call       func(path, args []string, flags map[string]any, write bool) (json.RawMessage, error)
}

// envelopeData unwraps the {"data":...,"exit_code":N} body REST and
// gRPC answer with.
func envelopeData(status int, raw []byte) (json.RawMessage, error) {
	if status != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", status, raw)
	}
	var env struct {
		Data     json.RawMessage `json:"data"`
		DataJSON string          `json:"data_json"`
		ExitCode int             `json:"exit_code"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("undecodable answer: %v: %s", err, raw)
	}
	if env.ExitCode != 0 {
		return nil, fmt.Errorf("exit %d: %s", env.ExitCode, raw)
	}
	if len(env.Data) == 0 && env.DataJSON != "" {
		env.Data = json.RawMessage(env.DataJSON)
	}
	if len(env.Data) == 0 {
		return nil, fmt.Errorf("no data: %s", raw)
	}
	return env.Data, nil
}

func httpBody(method, url, token, body string) (int, []byte, error) {
	resp, err := authed(method, url, token, body)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

func argsBody(args []string, flags map[string]any) string {
	b, _ := json.Marshal(map[string]any{"args": args, "flags": flags})
	return string(b)
}

func (x *surfaces) restCaller(token string) surfaceCaller {
	return surfaceCaller{name: "rest", positional: true, call: func(path, args []string, flags map[string]any, write bool) (json.RawMessage, error) {
		url, method, body := "http://"+x.api+"/v1/commands/"+strings.Join(path, "/"), http.MethodPost, argsBody(args, flags)
		if !write {
			// A read is a GET: its flags and arguments ride the query.
			q := neturl.Values{}
			for k, v := range flags {
				q.Set(k, fmt.Sprint(v))
			}
			for _, a := range args {
				q.Add("arg", a)
			}
			url, method, body = url+"?"+q.Encode(), http.MethodGet, ""
		}
		status, raw, err := httpBody(method, url, token, body)
		if err != nil {
			return nil, err
		}
		return envelopeData(status, raw)
	}}
}

func (x *surfaces) rpcCaller(token string) surfaceCaller {
	return surfaceCaller{name: "rpc", positional: true, call: func(path, args []string, flags map[string]any, write bool) (json.RawMessage, error) {
		b, _ := json.Marshal(map[string]any{"path": path, "args": args, "flags": flags})
		status, raw, err := httpBody(http.MethodPost, "http://"+x.rpc+"/cmdsurface.v1.Commands/Invoke", token, string(b))
		if err != nil {
			return nil, err
		}
		return envelopeData(status, raw)
	}}
}

func socketCaller(sock string) surfaceCaller {
	return surfaceCaller{name: "socket", positional: true, call: func(path, args []string, flags map[string]any, write bool) (json.RawMessage, error) {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return nil, err
		}
		defer func() { _ = conn.Close() }()
		b, _ := json.Marshal(map[string]any{"path": path, "args": args, "flags": flags})
		if _, err := conn.Write(append(b, '\n')); err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
		raw, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var resp struct {
			OK     bool `json:"ok"`
			Result struct {
				Data     json.RawMessage `json:"data"`
				ExitCode int             `json:"exit_code"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("undecodable answer: %v: %s", err, raw)
		}
		if !resp.OK || resp.Result.ExitCode != 0 || len(resp.Result.Data) == 0 {
			return nil, fmt.Errorf("socket call failed: %s", raw)
		}
		return resp.Result.Data, nil
	}}
}

// postErr is postStatus for a goroutine that reports its own errors.
func (m *mcpClient) postErr(body string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, m.url, strings.NewReader(body)) //nolint:noctx // loopback test URL
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", m.session)
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// mcpCaller is one MCP session. Its calls are not safe to overlap: a
// session belongs to one goroutine.
func mcpCaller(m *mcpClient) surfaceCaller {
	return surfaceCaller{name: "mcp", call: func(path, args []string, flags map[string]any, write bool) (json.RawMessage, error) {
		if len(args) > 0 {
			return nil, fmt.Errorf("MCP takes no positional arguments")
		}
		id := m.next
		m.next++
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": strings.Join(path, "."), "arguments": flags},
		})
		status, raw, err := m.postErr(string(b))
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, fmt.Errorf("status %d: %s", status, raw)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var msg struct {
				ID     any `json:"id"`
				Result struct {
					IsError           bool            `json:"isError"`
					StructuredContent json.RawMessage `json:"structuredContent"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(data), &msg); err != nil {
				return nil, fmt.Errorf("undecodable answer: %v: %s", err, data)
			}
			if msg.ID != float64(id) {
				continue
			}
			if msg.Result.IsError || len(msg.Result.StructuredContent) == 0 {
				return nil, fmt.Errorf("tool error: %s", data)
			}
			return msg.Result.StructuredContent, nil
		}
		return nil, fmt.Errorf("no reply to call %d in %s", id, raw)
	}}
}

// callers returns n callers per surface: REST, gRPC and the socket
// are stateless, each MCP caller its own session.
func (x *surfaces) callers(t *testing.T, token, sock string, n int) []surfaceCaller {
	t.Helper()
	out := make([]surfaceCaller, 0, 4*n)
	for range n {
		out = append(out, x.restCaller(token), x.rpcCaller(token), socketCaller(sock),
			mcpCaller(newMCPClient(t, "http://"+x.mcp+"/mcp", token)))
	}
	return out
}

func messageText(m model.Message) string {
	var parts []string
	for _, p := range m.Content.Parts {
		parts = append(parts, p.Text)
	}
	return strings.Join(parts, "")
}

// checkReply decodes a reply and requires it to be the answer to the
// request that carried key in conv.
func checkReply(raw json.RawMessage, conv, key string) (model.ID, error) {
	var r replyJSON
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("undecodable reply: %v: %s", err, raw)
	}
	switch {
	case string(r.Conversation) != conv:
		return "", fmt.Errorf("reply for conversation %q, asked %q: %s", r.Conversation, conv, raw)
	case r.OperationKey != key:
		return "", fmt.Errorf("reply for key %q, asked %q: %s", r.OperationKey, key, raw)
	case r.Message == "" || r.Operation.ID == "":
		return "", fmt.Errorf("reply names no message or operation: %s", raw)
	}
	return r.Message, nil
}

// checkSeed requires a `message list --text` answer to hold exactly the
// seed message that carries its words.
func checkSeed(raw json.RawMessage, want string) error {
	var p pageJSON[model.Message]
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("undecodable page: %v: %s", err, raw)
	}
	if len(p.Items) != 1 || messageText(p.Items[0]) != want {
		return fmt.Errorf("message list for %q = %d items: %s", want, len(p.Items), raw)
	}
	return nil
}

// checkConversations requires a `conversation list` answer to hold conv.
func checkConversations(raw json.RawMessage, conv model.ConversationRef) error {
	var p pageJSON[model.Conversation]
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("undecodable page: %v: %s", err, raw)
	}
	for _, c := range p.Items {
		if c.Ref == conv {
			return nil
		}
	}
	return fmt.Errorf("conversation list lacks %v: %s", conv.ID, raw)
}

// retryable is whether err is an answer the surface documents as
// worth asking again: a unit lost to a concurrent writer (TRANSIENT)
// or a write tier asking the caller to slow down (RATE_LIMITED). The
// retry reuses the request's idempotency key, so it can never run twice.
func retryable(err error) bool {
	m := err.Error()
	return strings.Contains(m, "TRANSIENT") || strings.Contains(m, "RATE_LIMITED") || strings.Contains(m, "status 429")
}

// retries counts the documented-retry answers the tests below were given.
var retries atomic.Int64

// do runs the call, asking again, up to 60 times, while the answer is
// a retryable refusal. Any other failure is returned as it came.
func (c surfaceCaller) do(path, args []string, flags map[string]any, write bool) (json.RawMessage, error) {
	for attempt := 1; ; attempt++ {
		raw, err := c.call(path, args, flags, write)
		if err == nil || !retryable(err) || attempt == 60 {
			return raw, err
		}
		retries.Add(1)
		time.Sleep(time.Duration(10+attempt*5) * time.Millisecond)
	}
}

func seedText(i int) string { return fmt.Sprintf("seedpool%02d", i) }

// seedPool has the socket write n seed messages, each carrying
// one word no other message does.
func seedPool(t *testing.T, sock, conv string, n int) {
	t.Helper()
	c := socketCaller(sock)
	for i := range n {
		key := fmt.Sprintf("seed-%02d", i)
		raw, err := c.do([]string{"message", "reply"}, []string{conv}, map[string]any{"text": seedText(i), "idempotency-key": key}, true)
		if err == nil {
			_, err = checkReply(raw, conv, key)
		}
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

// hammerRun is a burst of commands running on every surface at once.
type hammerRun struct {
	done    chan struct{}
	mu      sync.Mutex
	replies map[model.ID]string // message -> the text asked
	errs    []string
}

func (h *hammerRun) fail(format string, args ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errs = append(h.errs, fmt.Sprintf(format, args...))
}

// startHammer has each caller run per iterations of: a reply with its
// own text and key (where the surface takes the conversation as an
// argument), a `message list --text` for one of the seeds, and a
// conversation list. Every answer must be intact and the one asked
// for; callers run concurrently, so every pair of surfaces overlaps.
// Callers belong to one goroutine each.
func startHammer(conv string, ref model.ConversationRef, callers []surfaceCaller, per, pool int, tag string) *hammerRun {
	h := &hammerRun{done: make(chan struct{}), replies: map[model.ID]string{}}
	var wg sync.WaitGroup
	for n, c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range per {
				who := fmt.Sprintf("%s-%s-%d-%02d", tag, c.name, n, i)
				if c.positional {
					text := "hammer " + who
					raw, err := c.do([]string{"message", "reply"}, []string{conv}, map[string]any{"text": text, "idempotency-key": who}, true)
					var msg model.ID
					if err == nil {
						msg, err = checkReply(raw, conv, who)
					}
					if err != nil {
						h.fail("%s reply %s: %v", c.name, who, err)
					} else {
						h.mu.Lock()
						if prev, dup := h.replies[msg]; dup {
							h.errs = append(h.errs, fmt.Sprintf("%s reply %s: message %s already answered %q", c.name, who, msg, prev))
						}
						h.replies[msg] = text
						h.mu.Unlock()
					}
				}
				seed := (n + i) % pool
				raw, err := c.do([]string{"message", "list"}, nil, map[string]any{
					"conversation": conv, "text": seedText(seed), "limit": 100,
				}, false)
				if err == nil {
					err = checkSeed(raw, seedText(seed))
				}
				if err != nil {
					h.fail("%s message list %s: %v", c.name, who, err)
				}
				raw, err = c.do([]string{"conversation", "list"}, nil, map[string]any{}, false)
				if err == nil {
					err = checkConversations(raw, ref)
				}
				if err != nil {
					h.fail("%s conversation list %s: %v", c.name, who, err)
				}
			}
		}()
	}
	go func() { wg.Wait(); close(h.done) }()
	return h
}

// wait returns the replies the burst made once every caller is done,
// and fails the test on any caller's bad answer.
func (h *hammerRun) wait(t *testing.T, limit time.Duration) map[model.ID]string {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(limit):
		t.Fatalf("served commands did not finish in %s", limit)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.errs {
		t.Error(e)
	}
	return h.replies
}

// untilAll reads events until each of ids has a message.created, and
// returns every event read.
func (p *watchProc) untilAll(ids map[model.ID]string) []model.Event {
	p.t.Helper()
	want := map[model.ID]bool{}
	for id := range ids {
		want[id] = true
	}
	if len(want) == 0 {
		return nil
	}
	return p.until(func(ev model.Event) bool {
		if ev.Kind == model.EventMessageCreated {
			delete(want, ev.Resource.ID)
		}
		return len(want) == 0
	})
}

// serveDesk restarts the desk's daemon with the socket, REST, gRPC and
// MCP all serving, and returns its surfaces, the bearer token and the
// socket path. The caller stops the daemon through x.d.
func serveDesk(t *testing.T, s *deskSite) (x *surfaces, token, sock string) {
	t.Helper()
	s.stop(s.d)
	auth := s.s.useHTTPAuth()
	api, rpc, mcp := freeAddr(t), freeAddr(t), freeAddr(t)
	d := s.s.start(nil, nil, []string{"socket", "workers", "receivers", "api", "rpc", "mcp"}, "-c", auth.config,
		"service", "start", "--enable", "api", "--enable", "rpc", "--enable", "mcp",
		"--addr", api, "--rpc-addr", rpc, "--mcp-addr", mcp)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("serving daemon log:\n%s", d.log())
		}
	})
	return &surfaces{d: d, auth: auth, api: api, rpc: rpc, mcp: mcp}, auth.token(me()), d.addr("socket")
}

// allMessageTexts pages every message of conv, by id.
func allMessageTexts(s *deskSite, conv string) map[model.ID]string {
	s.t.Helper()
	out := map[model.ID]string{}
	cursor := ""
	for range 100 {
		var p struct {
			Items    []model.Message
			Next     model.Cursor
			Complete bool
		}
		args := []string{"message", "list", "--conversation", conv, "--limit", "100"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		s.json(&p, args...)
		for _, m := range p.Items {
			out[m.Ref.ID] = messageText(m)
		}
		if p.Complete {
			return out
		}
		cursor = string(p.Next)
	}
	s.t.Fatal("message list never completed")
	return nil
}

// TestServedSurfacesConcurrent drives the release binary on Postgres
// with the socket, REST, gRPC and MCP all serving one command tree, and
// hammers them at once: every surface runs replies (MCP cannot pass the
// conversation, a positional, so it reads), message lists and
// conversation lists from several callers at the same time. Each caller
// must get its own answer, intact and decodable: its reply for its
// key and text, its list for its seed. Afterwards every reply is in the
// conversation exactly once with the text its caller sent.
func TestServedSurfacesConcurrent(t *testing.T) {
	const workers, per, pool = 4, 5, 5 // 20 iterations per surface
	s := newDeskSite(t)
	id := string(s.conv.ID)
	x, token, sock := serveDesk(t, s)
	defer func() { s.s.want(result{exit: x.d.term()}, 0) }()
	seedPool(t, sock, id, pool)
	before := allMessageTexts(s, id)

	run := startHammer(id, s.conv, x.callers(t, token, sock, workers), per, pool, "ch")
	replies := run.wait(t, 2*time.Minute)
	t.Logf("%d answers were documented retries (TRANSIENT, RATE_LIMITED)", retries.Load())

	if want := 3 * workers * per; len(replies) != want {
		t.Errorf("%d distinct replies, want %d", len(replies), want)
	}
	after := allMessageTexts(s, id)
	if len(after) != len(before)+len(replies) {
		t.Errorf("conversation holds %d messages, want %d", len(after), len(before)+len(replies))
	}
	for msg, text := range replies {
		if after[msg] != text {
			t.Errorf("message %s holds %q, its caller sent %q", msg, after[msg], text)
		}
	}
}
