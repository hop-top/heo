package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// slackAPI is the Slack Web API and a response_url, in process: an
// http.RoundTripper under heo's HTTP client that answers the documented
// shapes of the methods the slack.chat adapter calls and records each
// request. Nothing reaches the network. The tests that use it own both
// ends: they sign the requests Slack would send to the webhook, and read
// here what the adapter sent back.
type slackAPI struct {
	t *testing.T

	mu    sync.Mutex
	calls []slackCall
	seq   int
	// msgs holds the messages posted, by "<channel>:<ts>", with their
	// text and reactions.
	msgs map[string]*slackPosted
	// fail, when it names a method, answers it with the Slack error code.
	fail map[string]string
	// views holds the modal views opened, by the ID Slack gave them.
	views map[string]any
	// lose, when it names a method, has the request recorded and then
	// fails the connection before any answer: the answer is lost.
	lose string
}

// slackCall is one request the adapter made.
type slackCall struct {
	Host   string
	Method string // the Web API method, or the response_url's path
	Auth   string
	Body   map[string]any
	// At is when the request reached the fake.
	At time.Time
}

type slackPosted struct {
	Text      string
	Reactions []string
	Deleted   bool
}

func newSlackAPI(t *testing.T) *slackAPI {
	return &slackAPI{t: t, msgs: map[string]*slackPosted{}, fail: map[string]string{}, views: map[string]any{}}
}

// Do is ports.HTTPClient.
func (s *slackAPI) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				s.t.Errorf("slack request %s is not JSON: %v", req.URL.Path, err)
			}
		}
	}
	method := strings.TrimPrefix(req.URL.Path, "/api/")
	if req.URL.Host == "hooks.slack.com" {
		method = strings.TrimPrefix(req.URL.Path, "/")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slackCall{Host: req.URL.Host, Method: method, Auth: req.Header.Get("Authorization"), Body: body, At: time.Now()})
	if req.URL.Host == "hooks.slack.com" {
		return slackReply(http.StatusOK, "ok", false), nil
	}
	if req.URL.Host != "slack.com" {
		return nil, fmt.Errorf("slack fake: unexpected host %s", req.URL.Host)
	}
	if method == s.lose && method != "" {
		return nil, io.ErrUnexpectedEOF
	}
	if code := s.fail[method]; code != "" {
		return slackReply(http.StatusOK, fmt.Sprintf(`{"ok":false,"error":%q}`, code), true), nil
	}
	str := func(k string) string { v, _ := body[k].(string); return v }
	switch method {
	case "chat.postMessage":
		s.seq++
		ts := fmt.Sprintf("1760000000.%06d", s.seq)
		s.msgs[str("channel")+":"+ts] = &slackPosted{Text: str("text")}
		return slackJSON(map[string]any{"ok": true, "channel": str("channel"), "ts": ts}), nil
	case "chat.update":
		if m := s.msgs[str("channel")+":"+str("ts")]; m != nil {
			m.Text = str("text")
			return slackJSON(map[string]any{"ok": true, "channel": str("channel"), "ts": str("ts")}), nil
		}
		return slackJSON(map[string]any{"ok": false, "error": "message_not_found"}), nil
	case "chat.delete":
		if m := s.msgs[str("channel")+":"+str("ts")]; m != nil {
			m.Deleted = true
			return slackJSON(map[string]any{"ok": true, "channel": str("channel"), "ts": str("ts")}), nil
		}
		return slackJSON(map[string]any{"ok": false, "error": "message_not_found"}), nil
	case "reactions.add", "reactions.remove":
		m := s.msgs[str("channel")+":"+str("timestamp")]
		if m == nil {
			return slackJSON(map[string]any{"ok": false, "error": "message_not_found"}), nil
		}
		if method == "reactions.add" {
			m.Reactions = append(m.Reactions, str("name"))
		} else {
			kept := m.Reactions[:0]
			for _, r := range m.Reactions {
				if r != str("name") {
					kept = append(kept, r)
				}
			}
			m.Reactions = kept
		}
		return slackJSON(map[string]any{"ok": true}), nil
	case "views.open", "views.push":
		// A view that was opened keeps the ID Slack gave it, for an update.
		s.seq++
		id := fmt.Sprintf("V%010d", s.seq)
		s.views[id] = body["view"]
		return slackJSON(map[string]any{"ok": true, "view": map[string]any{"id": id}}), nil
	case "views.update":
		id := str("view_id")
		if _, ok := s.views[id]; !ok && id != "" {
			return slackJSON(map[string]any{"ok": false, "error": "not_found"}), nil
		}
		s.views[id] = body["view"]
		return slackJSON(map[string]any{"ok": true, "view": map[string]any{"id": id}}), nil
	}
	return slackJSON(map[string]any{"ok": false, "error": "unknown_method"}), nil
}

// failWith makes method answer with the Slack error code until cleared.
func (s *slackAPI) failWith(method, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		delete(s.fail, method)
		return
	}
	s.fail[method] = code
}

// loseAnswer makes method record each request and then fail without an
// answer, until cleared with "".
func (s *slackAPI) loseAnswer(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lose = method
}

// called returns the requests made to method, in order.
func (s *slackAPI) called(method string) []slackCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []slackCall
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// total is how many requests were made.
func (s *slackAPI) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// posted is the message at "<channel>:<ts>", a copy; nil when none.
func (s *slackAPI) posted(id string) *slackPosted {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msgs[id]
	if m == nil {
		return nil
	}
	c := *m
	c.Reactions = append([]string(nil), m.Reactions...)
	return &c
}

func slackJSON(v any) *http.Response {
	b, _ := json.Marshal(v)
	return slackReply(http.StatusOK, string(b), true)
}

func slackReply(status int, body string, asJSON bool) *http.Response {
	h := http.Header{}
	if asJSON {
		h.Set("Content-Type", "application/json; charset=utf-8")
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader([]byte(body)))}
}
