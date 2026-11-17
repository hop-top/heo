package xrrx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
)

// rehearsal is the in-memory Slack a rehearsal answers from: the messages
// the recording posted, their text and reactions.
type rehearsal struct {
	posts int
	msgs  map[string]*rehearsed
}

type rehearsed struct {
	text, thread string
	reactions    []string
	deleted      bool
}

// canned is the answer a rehearsal gives instead of Slack's: the
// documented shapes of the few methods a recording may call, over a small
// workspace that holds only what the recording posted. It is a stand-in
// for debugging a recording's path before it posts anything; nothing it
// says is Slack's.
func (s *SlackSandbox) canned(method string, req *http.Request) (*http.Response, error) {
	var args map[string]any
	if req.GetBody != nil {
		rc, _ := req.GetBody()
		raw, _ := io.ReadAll(rc)
		args, _ = decodeObject(raw)
	}
	str := func(k string) string { v, _ := args[k].(string); return v }
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rehearse.msgs == nil {
		s.rehearse.msgs = map[string]*rehearsed{}
	}
	if s.known == nil {
		s.known = map[string]bool{}
	}
	msgs := s.rehearse.msgs
	failed := func(code string) (*http.Response, error) {
		return cannedJSON(map[string]any{"ok": false, "error": code}), nil
	}
	message := func(ts string, m *rehearsed) map[string]any {
		out := map[string]any{"type": "message", "user": s.BotUserID, "text": m.text, "ts": ts}
		if m.thread != "" {
			out["thread_ts"] = m.thread
		}
		if len(m.reactions) > 0 {
			var rs []any
			for _, r := range m.reactions {
				rs = append(rs, map[string]any{"name": r, "count": 1, "users": []any{s.BotUserID}})
			}
			out["reactions"] = rs
		}
		return out
	}
	switch method {
	case "chat.postMessage":
		if s.rehearse.posts >= s.Max {
			return nil, fmt.Errorf("xrrx: Slack sandbox: the cap of %d posts is spent, post refused", s.Max)
		}
		s.rehearse.posts++
		ts := fmt.Sprintf("1760000000.%06d", s.rehearse.posts)
		m := &rehearsed{text: str("text"), thread: str("thread_ts")}
		msgs[ts] = m
		s.known[ts] = true
		return cannedJSON(map[string]any{"ok": true, "channel": s.ChannelID, "ts": ts, "message": message(ts, m)}), nil
	case "chat.update", "chat.delete":
		m := msgs[str("ts")]
		if m == nil || m.deleted {
			return failed("message_not_found")
		}
		if method == "chat.delete" {
			m.deleted = true
		} else {
			m.text = str("text")
		}
		return cannedJSON(map[string]any{"ok": true, "channel": s.ChannelID, "ts": str("ts")}), nil
	case "reactions.add", "reactions.remove":
		m := msgs[str("timestamp")]
		if m == nil || m.deleted {
			return failed("message_not_found")
		}
		has := slices.Contains(m.reactions, str("name"))
		switch {
		case method == "reactions.add" && has:
			return failed("already_reacted")
		case method == "reactions.remove" && !has:
			return failed("no_reaction")
		case method == "reactions.add":
			m.reactions = append(m.reactions, str("name"))
		default:
			m.reactions = slices.DeleteFunc(m.reactions, func(r string) bool { return r == str("name") })
		}
		return cannedJSON(map[string]any{"ok": true}), nil
	case "reactions.get":
		m := msgs[str("timestamp")]
		if m == nil || m.deleted {
			return failed("message_not_found")
		}
		return cannedJSON(map[string]any{"ok": true, "type": "message", "channel": s.ChannelID, "message": message(str("timestamp"), m)}), nil
	}
	// conversations.history lists newest first; replies, the parent first.
	var tss []string
	for ts, m := range msgs {
		if m.deleted {
			continue
		}
		if method == "conversations.replies" && ts != str("ts") && m.thread != str("ts") {
			continue
		}
		if method == "conversations.history" && m.thread != "" && m.thread != ts {
			continue
		}
		tss = append(tss, ts)
	}
	sort.Strings(tss)
	if method == "conversations.history" {
		slices.Reverse(tss)
	}
	list := []any{}
	for _, ts := range tss {
		list = append(list, message(ts, msgs[ts]))
	}
	return cannedJSON(map[string]any{"ok": true, "messages": list, "has_more": false}), nil
}

func cannedJSON(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body: io.NopCloser(strings.NewReader(string(b))),
	}
}
