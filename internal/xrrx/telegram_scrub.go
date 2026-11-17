package xrrx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// telegramIdentityKeys are the keys under which a Bot API answer names a
// user or a chat. Such an object keeps its ID, its kind and the bot's own
// username and nothing else: the names, titles, photos and languages of
// the account and the group are not the recording's to keep.
var telegramIdentityKeys = map[string]bool{
	"from": true, "chat": true, "sender_chat": true, "via_bot": true, "user": true, "actor_chat": true,
	"forward_from": true, "forward_from_chat": true, "sender_user": true,
}

// telegramKeptKeys are what such an object keeps.
var telegramKeptKeys = map[string]bool{"id": true, "is_bot": true, "type": true, "username": true}

// telegramMigrateKeys are the keys of an error's parameters that carry a
// chat ID.
var telegramMigrateKeys = map[string]bool{"migrate_to_chat_id": true, "migrate_from_chat_id": true}

// telegramNameKeyRe is a key that names a person or a group: none may be
// left in an answer.
var telegramNameKeyRe = regexp.MustCompile(`"(?:first_name|last_name|language_code|title)"\s*:`)

// scrubResponse is resp as a recording keeps it: the bot, the chat and
// every other ID replaced, the names dropped, churning headers removed. An
// answer that still names what the sandbox exists to keep out is not
// handed on.
func (s *TelegramSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	body := raw
	if len(bytes.TrimSpace(raw)) > 0 {
		if body, err = s.ScrubJSON(raw); err != nil {
			body = []byte(s.scrubString(string(raw)))
		}
	}
	for k := range resp.Header {
		if k != "Content-Type" && k != "Retry-After" {
			delete(resp.Header, k)
		}
	}
	all := string(body)
	for _, vs := range resp.Header {
		all += "\n" + strings.Join(vs, "\n")
	}
	if err := s.residue(all); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	return resp, nil
}

// numberIn is the pattern of the digits of id standing alone.
func numberIn(id int64) *regexp.Regexp {
	digits := strconv.FormatInt(id, 10)
	digits = strings.TrimPrefix(digits, "-")
	return regexp.MustCompile(`(?:^|[^0-9])` + regexp.QuoteMeta(digits) + `(?:$|[^0-9])`)
}

// residue fails when text still names what the sandbox exists to keep out.
func (s *TelegramSandbox) residue(text string) error {
	fail := func(what string) error {
		return fmt.Errorf("xrrx: Telegram sandbox: the answer still holds %s after the scrub, not handed on", what)
	}
	_, secret, _ := strings.Cut(s.Token, ":")
	low := strings.ToLower(text)
	for _, lit := range []string{s.Token, secret, s.BotUsername} {
		if lit != "" && strings.Contains(low, strings.ToLower(lit)) {
			return fail("the bot")
		}
	}
	for _, id := range []int64{s.BotID, s.ChatID} {
		if id != 0 && numberIn(id).MatchString(text) {
			return fail("the bot or the chat")
		}
	}
	if telegramTokenAnyRe.MatchString(text) {
		return fail("a token")
	}
	if telegramNameKeyRe.MatchString(text) {
		return fail("a name")
	}
	return nil
}

// scrubString replaces the token, the bot's username and the IDs of the
// bot, the chat and every other account in in.
func (s *TelegramSandbox) scrubString(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubLocked(in)
}

func (s *TelegramSandbox) scrubLocked(in string) string {
	_, secret, _ := strings.Cut(s.Token, ":")
	if secret != "" {
		in = strings.ReplaceAll(in, secret, "<token>")
	}
	if s.BotUsername != "" {
		in = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(s.BotUsername)).ReplaceAllString(in, TelegramBotUsername)
	}
	for _, r := range []struct {
		real, placeholder int64
	}{{s.ChatID, TelegramChatID}, {s.BotID, TelegramBotID}} {
		if r.real != 0 {
			digits := strings.TrimPrefix(strconv.FormatInt(r.real, 10), "-")
			plain := strings.TrimPrefix(strconv.FormatInt(r.placeholder, 10), "-")
			in = regexp.MustCompile(`(^|[^0-9])`+regexp.QuoteMeta(digits)+`($|[^0-9])`).ReplaceAllString(in, "${1}"+plain+"${2}")
		}
	}
	return in
}

// mapID is the placeholder of a real ID: the bot's and the chat's own, and
// a stable one of the same kind for anyone else.
func (s *TelegramSandbox) mapID(n int64) int64 {
	switch n {
	case s.ChatID:
		return TelegramChatID
	case s.BotID:
		return TelegramBotID
	case TelegramChatID, TelegramBotID, TelegramPerson:
		return n
	}
	if s.others == nil {
		s.others = map[int64]int64{}
	}
	if p, ok := s.others[n]; ok {
		return p
	}
	var p int64
	if n < 0 {
		p = -1000000000100 - int64(len(s.others))
	} else {
		p = telegramOtherBase + int64(len(s.others))
	}
	s.others[n] = p
	return p
}

// ScrubJSON rewrites a Bot API JSON document so it names no one: the
// token, the bot's username and the bot, chat and every other ID are
// replaced wherever they appear, an object naming a user or a chat keeps
// only its ID, kind and the bot's username. Keys come out sorted.
func (s *TelegramSandbox) ScrubJSON(b []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("xrrx: more than one JSON document")
	}
	s.mu.Lock()
	v = s.scrubValue(v, "")
	s.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (s *TelegramSandbox) scrubValue(v any, key string) any {
	switch t := v.(type) {
	case string:
		return s.scrubLocked(t)
	case json.Number:
		if telegramMigrateKeys[key] {
			if n, err := t.Int64(); err == nil {
				return json.Number(strconv.FormatInt(s.mapID(n), 10))
			}
		}
		return t
	case []any:
		for i := range t {
			t[i] = s.scrubValue(t[i], key)
		}
		return t
	case map[string]any:
		if telegramIdentityKeys[key] {
			return s.scrubIdentity(t)
		}
		for k, c := range t {
			t[k] = s.scrubValue(c, k)
		}
		return t
	}
	return v
}

// scrubIdentity is a user or chat object as a recording keeps it.
func (s *TelegramSandbox) scrubIdentity(o map[string]any) map[string]any {
	out := map[string]any{}
	for k, c := range o {
		if !telegramKeptKeys[k] {
			continue
		}
		switch k {
		case "id":
			if n, ok := c.(json.Number); ok {
				if id, err := n.Int64(); err == nil {
					out[k] = json.Number(strconv.FormatInt(s.mapID(id), 10))
				}
			}
		case "username":
			// Only the bot's own name is kept, as its placeholder.
			if name, _ := c.(string); strings.EqualFold(name, s.BotUsername) {
				out[k] = TelegramBotUsername
			}
		default:
			out[k] = c
		}
	}
	return out
}
