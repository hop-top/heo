package xrrx

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// discordRehearsal is the in-memory Discord a rehearsal answers from: the
// messages the recording sent, their text and reactions.
type discordRehearsal struct {
	sends int
	msgs  map[string]*discordRehearsed
}

type discordRehearsed struct {
	text      string
	reactions map[string]bool
	deleted   bool
	reply     string
}

// canned is the answer a rehearsal gives instead of Discord's: the
// documented shapes of the few routes a recording may call, over a small
// channel that holds only what the recording sent. It names the real bot,
// application, server and channel, and a name, an avatar and a member for
// the bot, so the scrub that follows has something to do. It is a
// stand-in for debugging a recording's path before it sends anything;
// nothing it says is Discord's.
func (s *DiscordSandbox) canned(route string, req *http.Request) (*http.Response, error) {
	var args map[string]any
	if req.GetBody != nil {
		rc, _ := req.GetBody()
		raw, _ := io.ReadAll(rc)
		args, _ = decodeObject(raw)
	}
	text, _ := args["content"].(string)
	parts := strings.Split(strings.TrimPrefix(req.URL.EscapedPath(), discordAPIPrefix+"channels/"), "/")
	id := ""
	if len(parts) > 2 {
		id = parts[2]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rehearse.msgs == nil {
		s.rehearse.msgs = map[string]*discordRehearsed{}
	}
	if s.known == nil {
		s.known = map[string]bool{}
	}
	msgs := s.rehearse.msgs
	failed := func(status, code int, message string) (*http.Response, error) {
		return cannedDiscord(status, map[string]any{"code": code, "message": message}), nil
	}
	message := func(id string, m *discordRehearsed) map[string]any {
		out := map[string]any{
			"id": id, "channel_id": s.ChannelID, "content": m.text, "type": 0, "flags": 0, "tts": false, "pinned": false,
			"mention_everyone": false, "mentions": []any{}, "mention_roles": []any{}, "attachments": []any{}, "embeds": []any{},
			"components": []any{}, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "edited_timestamp": nil,
			"author": map[string]any{
				"id": s.BotUserID, "bot": true, "username": "Rehearsal Bot", "global_name": "Rehearsal Bot",
				"avatar": "0123456789abcdef", "discriminator": "0", "public_flags": 0,
			},
			"member": map[string]any{"nick": "Rehearsal", "roles": []any{}},
		}
		if m.reply != "" {
			out["type"] = 19
			out["message_reference"] = map[string]any{"message_id": m.reply, "channel_id": s.ChannelID, "guild_id": s.GuildID}
		}
		return out
	}
	switch route {
	case "send":
		if s.rehearse.sends >= s.Max {
			return nil, fmt.Errorf("xrrx: Discord sandbox: the cap of %d sends is spent, send refused", s.Max)
		}
		s.rehearse.sends++
		mid := strconv.Itoa(1_700_000_000_000_000_000 + s.rehearse.sends)
		m := &discordRehearsed{text: text, reactions: map[string]bool{}}
		if ref, ok := args["message_reference"].(map[string]any); ok {
			m.reply, _ = ref["message_id"].(string)
		}
		msgs[mid] = m
		s.known[mid] = true
		return cannedDiscord(http.StatusOK, message(mid, m)), nil
	case "edit":
		m := msgs[id]
		if m == nil || m.deleted {
			return failed(http.StatusNotFound, 10008, "Unknown Message")
		}
		m.text = text
		return cannedDiscord(http.StatusOK, message(id, m)), nil
	case "delete":
		m := msgs[id]
		if m == nil || m.deleted {
			return failed(http.StatusNotFound, 10008, "Unknown Message")
		}
		m.deleted = true
		return cannedDiscordEmpty(), nil
	case "react", "unreact":
		m := msgs[id]
		if m == nil || m.deleted {
			return failed(http.StatusNotFound, 10008, "Unknown Message")
		}
		emoji := ""
		if len(parts) > 4 {
			emoji, _ = url.PathUnescape(parts[4])
		}
		if route == "react" {
			m.reactions[emoji] = true
		} else {
			delete(m.reactions, emoji)
		}
		return cannedDiscordEmpty(), nil
	}
	return failed(http.StatusNotFound, 0, "404: Not Found")
}

func cannedDiscord(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(b))),
	}
}

func cannedDiscordEmpty() *http.Response {
	return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{}, Body: http.NoBody}
}
