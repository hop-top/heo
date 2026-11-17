package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// telegramAPI is the Telegram Bot API, in process: an http.RoundTripper
// under heo's HTTP client that answers the documented shapes of the
// methods the telegram.messages adapter calls, keeps the updates a test
// queues for getUpdates (a long poll that waits for one) and records each
// request. Nothing reaches the network. The tests that use it own both
// ends: they post the updates Telegram would send to the webhook or
// queue them for polling, and read here what the adapter sent back.
type telegramAPI struct {
	t     *testing.T
	token string
	botID int64

	mu    sync.Mutex
	calls []telegramCall
	// msgs holds the messages the bot posted, by "<chat>:<id>".
	msgs   map[string]*telegramPosted
	nextID map[int64]int64
	// queue is the updates Telegram still holds for the bot, by
	// update_id; getUpdates confirms (drops) those below its offset.
	queue []map[string]any
	// wake is closed and replaced whenever the queue grows, ending the
	// long poll waiting on it.
	wake chan struct{}
	// webhook, when set, makes getUpdates answer 409 as Telegram does.
	webhook bool
	// fail, when it names a method, answers it with that Bot API error.
	fail map[string]telegramFailure
}

// telegramFailure is a Bot API error object.
type telegramFailure struct {
	Code        int
	Description string
	RetryAfter  int
}

// telegramCall is one request the adapter made.
type telegramCall struct {
	Host   string
	Token  string // the bot token in the request path
	Method string
	Body   map[string]any
	// At is when the call arrived.
	At time.Time
}

type telegramPosted struct {
	Text      string
	Reactions []string
	Deleted   bool
	From      int64 // the user that wrote it: the bot, or 0 for a person's
}

func newTelegramAPI(t *testing.T, token string, botID int64) *telegramAPI {
	return &telegramAPI{
		t: t, token: token, botID: botID, msgs: map[string]*telegramPosted{}, nextID: map[int64]int64{},
		wake: make(chan struct{}), fail: map[string]telegramFailure{},
	}
}

// Do is ports.HTTPClient.
func (s *telegramAPI) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				s.t.Errorf("telegram request %s is not JSON: %v", req.URL.Path, err)
			}
		}
	}
	path := strings.TrimPrefix(req.URL.Path, "/bot")
	token, method, _ := strings.Cut(path, "/")
	s.mu.Lock()
	s.calls = append(s.calls, telegramCall{Host: req.URL.Host, Token: token, Method: method, Body: body, At: time.Now()})
	s.mu.Unlock()
	if req.URL.Host != "api.telegram.org" {
		return nil, fmt.Errorf("telegram fake: unexpected host %s", req.URL.Host)
	}
	if token != s.token {
		return telegramError(http.StatusNotFound, "Not Found", 0), nil
	}
	s.mu.Lock()
	f, failed := s.fail[method]
	s.mu.Unlock()
	if failed {
		return telegramError(f.Code, f.Description, f.RetryAfter), nil
	}
	if method == "getUpdates" {
		return s.getUpdates(req.Context(), body), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	num := func(k string) int64 { v, _ := body[k].(float64); return int64(v) }
	str := func(k string) string { v, _ := body[k].(string); return v }
	key := func() string { return fmt.Sprintf("%d:%d", num("chat_id"), num("message_id")) }
	switch method {
	case "sendMessage":
		chat := num("chat_id")
		s.nextID[chat]++
		id := s.nextID[chat]
		s.msgs[fmt.Sprintf("%d:%d", chat, id)] = &telegramPosted{Text: str("text"), From: s.botID}
		return telegramOK(map[string]any{
			"message_id": id, "date": time.Now().Unix(), "text": str("text"),
			"chat": map[string]any{"id": chat, "type": chatType(chat)},
			"from": map[string]any{"id": s.botID, "is_bot": true},
		}), nil
	case "editMessageText":
		m := s.msgs[key()]
		switch {
		case m == nil || m.Deleted:
			return telegramError(http.StatusBadRequest, "Bad Request: message to edit not found", 0), nil
		case m.From != s.botID:
			return telegramError(http.StatusBadRequest, "Bad Request: message can't be edited", 0), nil
		}
		m.Text = str("text")
		return telegramOK(map[string]any{"message_id": num("message_id"), "chat": map[string]any{"id": num("chat_id"), "type": chatType(num("chat_id"))}, "date": time.Now().Unix(), "text": m.Text}), nil
	case "deleteMessage":
		m := s.msgs[key()]
		if m == nil || m.Deleted {
			return telegramError(http.StatusBadRequest, "Bad Request: message to delete not found", 0), nil
		}
		m.Deleted = true
		return telegramOK(true), nil
	case "setMessageReaction":
		m := s.msgs[key()]
		if m == nil || m.Deleted {
			return telegramError(http.StatusBadRequest, "Bad Request: message to react not found", 0), nil
		}
		m.Reactions = nil
		if list, _ := body["reaction"].([]any); len(list) > 0 {
			for _, r := range list {
				if e, _ := r.(map[string]any); e != nil {
					m.Reactions = append(m.Reactions, fmt.Sprint(e["emoji"]))
				}
			}
		}
		return telegramOK(true), nil
	case "answerCallbackQuery":
		return telegramOK(true), nil
	}
	return telegramError(http.StatusNotFound, "Not Found", 0), nil
}

// getUpdates is the long poll: updates below offset are confirmed and
// forgotten, the rest up to limit are answered, and with none the call
// waits for one, until timeout seconds pass or the caller gives up.
func (s *telegramAPI) getUpdates(ctx context.Context, body map[string]any) *http.Response {
	offset, _ := body["offset"].(float64)
	limit, _ := body["limit"].(float64)
	timeout, _ := body["timeout"].(float64)
	deadline := time.After(time.Duration(timeout) * time.Second)
	for {
		s.mu.Lock()
		if s.webhook {
			s.mu.Unlock()
			return telegramError(http.StatusConflict, "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first", 0)
		}
		kept := s.queue[:0]
		for _, u := range s.queue {
			if id, _ := u["update_id"].(int64); float64(id) >= offset {
				kept = append(kept, u)
			}
		}
		s.queue = kept
		if len(s.queue) > 0 {
			n := min(len(s.queue), max(int(limit), 1))
			out := append([]map[string]any(nil), s.queue[:n]...)
			s.mu.Unlock()
			return telegramOK(out)
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-deadline:
			return telegramOK([]map[string]any{})
		case <-ctx.Done():
			return telegramOK([]map[string]any{})
		}
	}
}

// enqueue holds update for the bot's next getUpdates.
func (s *telegramAPI) enqueue(update map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, update)
	close(s.wake)
	s.wake = make(chan struct{})
}

// pending is how many updates Telegram still holds for the bot.
func (s *telegramAPI) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// setWebhook makes the bot's getUpdates refuse, as while a webhook is set.
func (s *telegramAPI) setWebhook(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhook = on
	close(s.wake)
	s.wake = make(chan struct{})
}

// failWith makes method answer with f until cleared by a zero Code.
func (s *telegramAPI) failWith(method string, f telegramFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Code == 0 {
		delete(s.fail, method)
		return
	}
	s.fail[method] = f
}

// called returns the requests made to method, in order.
func (s *telegramAPI) called(method string) []telegramCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []telegramCall
	for _, c := range s.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// total is how many requests were made.
func (s *telegramAPI) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// person is a message a person wrote in chat, as Telegram numbers it:
// later sends of the bot in that chat continue after it.
func (s *telegramAPI) person(chat, id int64, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs[fmt.Sprintf("%d:%d", chat, id)] = &telegramPosted{Text: text}
	s.nextID[chat] = max(s.nextID[chat], id)
}

// posted is the message at "<chat>:<id>", a copy; nil when none.
func (s *telegramAPI) posted(id string) *telegramPosted {
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

func chatType(id int64) string {
	switch {
	case id > 0:
		return "private"
	case id < -1000000000000:
		return "supergroup"
	}
	return "group"
}

func telegramOK(result any) *http.Response {
	b, _ := json.Marshal(map[string]any{"ok": true, "result": result})
	return telegramReply(http.StatusOK, string(b))
}

func telegramError(code int, description string, retryAfter int) *http.Response {
	e := map[string]any{"ok": false, "error_code": code, "description": description}
	if retryAfter > 0 {
		e["parameters"] = map[string]any{"retry_after": retryAfter}
	}
	b, _ := json.Marshal(e)
	return telegramReply(code, string(b))
}

func telegramReply(status int, body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader([]byte(body)))}
}
