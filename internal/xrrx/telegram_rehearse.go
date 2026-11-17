package xrrx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// telegramRehearsal is the in-memory Bot API a rehearsal answers from: the
// messages the recording sent, their text and reaction.
type telegramRehearsal struct {
	sends int
	msgs  map[int64]*telegramRehearsed
}

type telegramRehearsed struct {
	text, reaction string
	deleted        bool
}

// canned is the answer a rehearsal gives instead of Telegram's: the
// documented shapes of the few methods a recording may call, over a small
// chat that holds only what the recording sent. It names the real bot and
// chat, and a name and a title for each, so the scrub that follows has
// something to do. It is a stand-in for debugging a recording's path
// before it sends anything; nothing it says is Telegram's.
func (s *TelegramSandbox) canned(method string, req *http.Request) (*http.Response, error) {
	var args map[string]any
	if req.GetBody != nil {
		rc, _ := req.GetBody()
		raw, _ := io.ReadAll(rc)
		args, _ = decodeObject(raw)
	}
	text, _ := args["text"].(string)
	var id int64
	if n, ok := args["message_id"].(json.Number); ok {
		id, _ = n.Int64()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rehearse.msgs == nil {
		s.rehearse.msgs = map[int64]*telegramRehearsed{}
	}
	if s.known == nil {
		s.known = map[int64]bool{}
	}
	msgs := s.rehearse.msgs
	failed := func(code int, description string) (*http.Response, error) {
		return cannedTelegram(code, map[string]any{"ok": false, "error_code": code, "description": description}), nil
	}
	ok := func(result any) (*http.Response, error) {
		return cannedTelegram(http.StatusOK, map[string]any{"ok": true, "result": result}), nil
	}
	chatKind := "supergroup"
	if s.ChatID > 0 {
		chatKind = "private"
	}
	message := func(id int64, m *telegramRehearsed) map[string]any {
		return map[string]any{
			"message_id": id, "date": time.Now().Unix(), "text": m.text,
			"from": map[string]any{"id": s.BotID, "is_bot": true, "first_name": "Rehearsal Bot", "username": s.BotUsername},
			"chat": map[string]any{"id": s.ChatID, "type": chatKind, "title": "Rehearsal Chat"},
		}
	}
	switch method {
	case "sendMessage":
		if s.rehearse.sends >= s.Max {
			return nil, fmt.Errorf("xrrx: Telegram sandbox: the cap of %d sends is spent, send refused", s.Max)
		}
		s.rehearse.sends++
		id := int64(100 + s.rehearse.sends)
		m := &telegramRehearsed{text: text}
		msgs[id] = m
		s.known[id] = true
		return ok(message(id, m))
	case "editMessageText":
		m := msgs[id]
		switch {
		case m == nil || m.deleted:
			return failed(http.StatusBadRequest, "Bad Request: message to edit not found")
		case m.text == text:
			return failed(http.StatusBadRequest, "Bad Request: message is not modified")
		}
		m.text = text
		return ok(message(id, m))
	case "deleteMessage":
		m := msgs[id]
		if m == nil || m.deleted {
			return failed(http.StatusBadRequest, "Bad Request: message to delete not found")
		}
		m.deleted = true
		return ok(true)
	case "setMessageReaction":
		m := msgs[id]
		if m == nil || m.deleted {
			return failed(http.StatusBadRequest, "Bad Request: message to react not found")
		}
		m.reaction = ""
		if list, _ := args["reaction"].([]any); len(list) > 0 {
			if e, _ := list[0].(map[string]any); e != nil {
				m.reaction, _ = e["emoji"].(string)
			}
		}
		return ok(true)
	}
	return failed(http.StatusNotFound, "Not Found")
}

func cannedTelegram(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(b))),
	}
}
