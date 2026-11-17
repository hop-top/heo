package daemon_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// relayGateway is an operator's iMessage gateway, in process: an
// http.RoundTripper under heo's HTTP client that serves the callback and
// the lookup the way docs/adapters/relay.messages.md describes them:
// it records each Idempotency-Key it acted on, answers a repeated key
// from that record, and answers the lookup from it. It checks the bearer
// secret of every call and records each request, so a test can say what
// reached which host. Nothing reaches the network; the tests that use it
// own both ends, posting the gateway's inbound messages to the channel's
// route and reading here what heo sent back.
type relayGateway struct {
	t      *testing.T
	host   string
	secret string

	mu    sync.Mutex
	calls []relayCall
	// sent holds the message ID the gateway answered for each key.
	sent map[string]string
	n    int
	// lose makes the next that many callbacks act (the key is recorded and
	// the message sent) and lose their answer: the connection drops.
	lose int
	// status, when set for a path, answers every call to it with that
	// status and no action.
	status map[string]int
	// stray lists the hosts that were called and are not the gateway's.
	stray []string
}

// relayCall is one request heo made.
type relayCall struct {
	Method, Host, Path string
	Query              url.Values
	Authorization      string
	Header             http.Header
	Key                string
	Body               map[string]any
}

func newRelayGateway(t *testing.T, host, secret string) *relayGateway {
	return &relayGateway{t: t, host: host, secret: secret, sent: map[string]string{}, status: map[string]int{}}
}

// Do is ports.HTTPClient.
func (g *relayGateway) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				g.t.Errorf("gateway request %s is not JSON: %v", req.URL.Path, err)
			}
		}
	}
	call := relayCall{
		Method: req.Method, Host: req.URL.Host, Path: req.URL.Path, Query: req.URL.Query(),
		Authorization: req.Header.Get("Authorization"), Header: req.Header.Clone(), Key: req.Header.Get("Idempotency-Key"), Body: body,
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call)
	if req.URL.Host != g.host {
		g.stray = append(g.stray, req.URL.Host)
		return nil, fmt.Errorf("gateway fake: unexpected host %s", req.URL.Host)
	}
	if code, ok := g.status[req.URL.Path]; ok {
		return answer(code, `{"error":"scripted"}`), nil
	}
	if call.Authorization != "Bearer "+g.secret {
		return answer(http.StatusUnauthorized, `{"error":"unauthorized"}`), nil
	}
	switch {
	case req.Method == http.MethodPost && req.URL.Path == "/uma/reply":
		return g.reply(call)
	case req.Method == http.MethodGet && req.URL.Path == "/uma/lookup":
		key := call.Query.Get("operation_id")
		if id, ok := g.sent[key]; ok {
			return answer(http.StatusOK, fmt.Sprintf(`{"status":"sent","message_id":%q}`, id)), nil
		}
		return answer(http.StatusNotFound, `{"status":"not_found"}`), nil
	}
	return answer(http.StatusNotFound, `{"error":"not_found"}`), nil
}

// reply acts on a callback: the key is recorded before anything else, a
// key already held is answered from its record.
func (g *relayGateway) reply(call relayCall) (*http.Response, error) {
	if id, ok := g.sent[call.Key]; ok {
		return answer(http.StatusOK, fmt.Sprintf(`{"message_id":%q}`, id)), nil
	}
	g.n++
	id := fmt.Sprintf("gw-%d", g.n)
	g.sent[call.Key] = id
	if g.lose > 0 {
		g.lose--
		return nil, errors.New("connection reset by peer")
	}
	return answer(http.StatusOK, fmt.Sprintf(`{"message_id":%q}`, id)), nil
}

func answer(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code, Status: http.StatusText(code), Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader([]byte(body))), ContentLength: int64(len(body)),
	}
}

// called is the requests made to path, in order.
func (g *relayGateway) called(path string) []relayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []relayCall
	for _, c := range g.calls {
		if strings.HasSuffix(c.Path, path) {
			out = append(out, c)
		}
	}
	return out
}

func (g *relayGateway) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

func (g *relayGateway) strays() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.stray...)
}

func (g *relayGateway) setLose(n int) {
	g.mu.Lock()
	g.lose = n
	g.mu.Unlock()
}

func (g *relayGateway) setStatus(path string, code int) {
	g.mu.Lock()
	g.status[path] = code
	g.mu.Unlock()
}
