package xrrx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// slackDropKeys are the keys of a Slack answer a recording never keeps:
// the app's profile, the workspace's names, generated block IDs and links
// that name the workspace.
var slackDropKeys = map[string]bool{
	"bot_profile": true, "team": true, "user_team": true, "source_team": true,
	"blocks": true, "permalink": true, "client_msg_id": true, "response_metadata": true,
}

// slackSubdomainRe is a workspace's own host.
var slackSubdomainRe = regexp.MustCompile(`(?i)\b([a-z0-9-]+)\.slack\.com\b`)

// scrubResponse is resp as a recording keeps it: the workspace, bot,
// channel and every other ID replaced, other people's messages dropped,
// churning headers removed. An answer that still names what the sandbox
// exists to keep out is not handed on.
func (s *SlackSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
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

// residue fails when text still names what the sandbox exists to keep out.
func (s *SlackSandbox) residue(text string) error {
	fail := func(what string) error {
		return fmt.Errorf("xrrx: Slack sandbox: the answer still holds %s after the scrub, not handed on", what)
	}
	low := strings.ToLower(text)
	for _, lit := range []string{s.Token, s.TeamID, s.BotUserID, s.BotID, s.ChannelID} {
		if lit != "" && strings.Contains(low, strings.ToLower(lit)) {
			return fail("the workspace")
		}
	}
	if slackTokenAnyRe.MatchString(text) {
		return fail("a token")
	}
	for _, id := range slackIDInTextRe.FindAllString(text, -1) {
		if hasDigit(id) && !slackPlaceholderRe.MatchString(id) {
			return fail("an ID")
		}
	}
	for _, m := range slackSubdomainRe.FindAllStringSubmatch(text, -1) {
		if sub := strings.ToLower(m[1]); sub != "hooks" && sub != "api" && sub != "www" {
			return fail("a workspace's host")
		}
	}
	return nil
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

// scrubString replaces the workspace, the bot, the channel and every other
// ID in in.
func (s *SlackSandbox) scrubString(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubLocked(in)
}

func (s *SlackSandbox) scrubLocked(in string) string {
	if s.others == nil {
		s.others, s.counters = map[string]string{}, map[string]int{}
	}
	for _, r := range [][2]string{{s.ChannelID, SlackChannelID}, {s.TeamID, SlackTeamID}, {s.BotUserID, SlackBotUserID}, {s.BotID, SlackBotID}} {
		if r[0] != "" {
			in = strings.ReplaceAll(in, r[0], r[1])
		}
	}
	return slackIDInTextRe.ReplaceAllStringFunc(in, func(id string) string {
		if !hasDigit(id) || slackPlaceholderRe.MatchString(id) {
			return id
		}
		if p, ok := s.others[id]; ok {
			return p
		}
		prefix := id[:1]
		if prefix == "W" {
			prefix = "U"
		}
		s.counters[prefix]++
		p := fmt.Sprintf("%s%010d", prefix, 9+s.counters[prefix])
		s.others[id] = p
		return p
	})
}

// ScrubJSON rewrites a Slack JSON document so it names no one: the
// workspace, bot, channel and every other ID are replaced wherever they
// appear in a string, a list of messages keeps only the sandbox's own
// texts, and what slackDropKeys names is removed. Keys come out sorted.
func (s *SlackSandbox) ScrubJSON(b []byte) ([]byte, error) {
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
	v = s.scrubValue(v)
	s.mu.Unlock()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func (s *SlackSandbox) scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubLocked(t)
	case []any:
		for i := range t {
			t[i] = s.scrubValue(t[i])
		}
		return t
	case map[string]any:
		if msgs, ok := t["messages"].([]any); ok {
			t["messages"] = s.ownMessages(msgs)
			if _, has := t["has_more"]; has {
				t["has_more"] = false
			}
		}
		for k, c := range t {
			switch {
			case slackDropKeys[k]:
				delete(t, k)
			case k == "app_id" || k == "api_app_id":
				t[k] = SlackAppID
			default:
				t[k] = s.scrubValue(c)
			}
		}
		return t
	}
	return v
}

// ownMessages keeps the messages of a list that are the sandbox's own: a
// test text by the bot. The rest of a channel's history is not the
// recording's to keep.
func (s *SlackSandbox) ownMessages(msgs []any) []any {
	out := []any{}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text, _ := mm["text"].(string)
		user, _ := mm["user"].(string)
		bot, _ := mm["bot_id"].(string)
		if strings.HasPrefix(text, SlackTextPrefix) && (user == s.BotUserID || (s.BotID != "" && bot == s.BotID)) {
			out = append(out, m)
		}
	}
	return out
}
