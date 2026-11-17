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

// discordIdentityKeys are the keys under which a Discord answer names a
// user, and discordIdentityListKeys those that hold a list of them. Such
// an object keeps its ID, whether it is a bot and the bot's own username
// and nothing else: the names, avatars, banners and flags of an account
// are not the recording's to keep.
var (
	discordIdentityKeys     = map[string]bool{"author": true, "user": true}
	discordIdentityListKeys = map[string]bool{"mentions": true}
	discordKeptKeys         = map[string]bool{"id": true, "bot": true, "system": true, "username": true}
	// discordDropKeys are removed wherever they appear: a member's nick,
	// roles and join date, and the account's names.
	discordDropKeys = map[string]bool{
		"member": true, "global_name": true, "display_name": true, "nick": true, "avatar": true, "discriminator": true,
		"banner": true, "accent_color": true, "avatar_decoration_data": true, "clan": true, "primary_guild": true,
		"collectibles": true, "display_name_styles": true,
	}
	// discordAccountKeys hold the ID of an account, a server or a channel:
	// replaced by a placeholder.
	discordAccountKeys = map[string]bool{
		"application_id": true, "guild_id": true, "channel_id": true, "webhook_id": true, "parent_id": true, "owner_id": true,
		"user_id": true, "bot_user_id": true, "author_id": true, "recipient_id": true,
	}
)

// discordNameKeyRe is a key that names a person: none may be left in an
// answer.
var discordNameKeyRe = regexp.MustCompile(`"(?:global_name|display_name|nick|discriminator|avatar)"\s*:`)

// scrubResponse is resp as a recording keeps it: the application, the bot,
// the server, the channel and every other ID replaced, the names dropped,
// churning headers removed. An answer that still names what the sandbox
// exists to keep out is not handed on.
func (s *DiscordSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
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

// idIn is the pattern of id standing alone among other characters.
func idIn(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^0-9])` + regexp.QuoteMeta(id) + `(?:$|[^0-9])`)
}

// residue fails when text still names what the sandbox exists to keep out.
func (s *DiscordSandbox) residue(text string) error {
	fail := func(what string) error {
		return fmt.Errorf("xrrx: Discord sandbox: the answer still holds %s after the scrub, not handed on", what)
	}
	if s.Token != "" && strings.Contains(text, s.Token) {
		return fail("the bot token")
	}
	if m := discordTokenRe.FindStringSubmatch(s.Token); m != nil {
		for _, part := range []string{m[1], m[3]} {
			if strings.Contains(text, part) {
				return fail("the bot token")
			}
		}
	}
	for _, id := range []string{s.ApplicationID, s.BotUserID, s.GuildID, s.ChannelID} {
		if id != "" && idIn(id).MatchString(text) {
			return fail("the application, the bot, the server or the channel")
		}
	}
	if discordTokenAnyRe.MatchString(text) {
		return fail("a token")
	}
	if discordNameKeyRe.MatchString(text) {
		return fail("a name")
	}
	return nil
}

// scrubString replaces the token and the IDs of the application, the bot,
// the server, the channel and every other account in in.
func (s *DiscordSandbox) scrubString(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubLocked(in)
}

func (s *DiscordSandbox) scrubLocked(in string) string {
	if s.Token != "" {
		in = strings.ReplaceAll(in, s.Token, "<token>")
	}
	if m := discordTokenRe.FindStringSubmatch(s.Token); m != nil {
		in = strings.ReplaceAll(in, m[3], "<token>")
		in = strings.ReplaceAll(in, m[1], "<token>")
	}
	for _, r := range []struct{ real, placeholder string }{
		{s.ChannelID, DiscordChannelID}, {s.GuildID, DiscordGuildID}, {s.BotUserID, DiscordBotUserID}, {s.ApplicationID, DiscordApplicationID},
	} {
		if r.real != "" {
			in = regexp.MustCompile(`(^|[^0-9])`+regexp.QuoteMeta(r.real)+`($|[^0-9])`).ReplaceAllString(in, "${1}"+r.placeholder+"${2}")
		}
	}
	return in
}

// mapID is the placeholder of a real ID found under key: the sandbox's own
// IDs, and a stable one of the same kind for anyone else. An application
// that is its own bot user (as newer ones are) reads as the application
// under application_id and as the bot everywhere else.
func (s *DiscordSandbox) mapID(id, key string) string {
	switch id {
	case s.ApplicationID:
		if key == "application_id" || id != s.BotUserID {
			return DiscordApplicationID
		}
		return DiscordBotUserID
	case s.BotUserID:
		return DiscordBotUserID
	case s.GuildID:
		return DiscordGuildID
	case s.ChannelID:
		return DiscordChannelID
	case DiscordApplicationID, DiscordBotUserID, DiscordGuildID, DiscordChannelID, DiscordPerson:
		return id
	}
	if s.others == nil {
		s.others = map[string]string{}
	}
	if p, ok := s.others[id]; ok {
		return p
	}
	p := strconv.Itoa(discordOtherBase + len(s.others))
	s.others[id] = p
	return p
}

// ScrubJSON rewrites a Discord JSON document so it names no one: the
// token and the IDs of the application, the bot, the server, the channel
// and every other account are replaced wherever they appear, an object
// naming a user keeps only its ID, whether it is a bot and the bot's own
// username, and a member is dropped. Keys come out sorted.
func (s *DiscordSandbox) ScrubJSON(b []byte) ([]byte, error) {
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

func (s *DiscordSandbox) scrubValue(v any, key string) any {
	switch t := v.(type) {
	case string:
		if discordAccountKeys[key] && discordSnowflakeRe.MatchString(t) {
			return s.mapID(t, key)
		}
		return s.scrubLocked(t)
	case []any:
		for i := range t {
			if discordIdentityListKeys[key] {
				if o, ok := t[i].(map[string]any); ok {
					t[i] = s.scrubIdentity(o)
					continue
				}
			}
			t[i] = s.scrubValue(t[i], key)
		}
		return t
	case map[string]any:
		if discordIdentityKeys[key] {
			return s.scrubIdentity(t)
		}
		for k, c := range t {
			if discordDropKeys[k] {
				delete(t, k)
				continue
			}
			t[k] = s.scrubValue(c, k)
		}
		return t
	}
	return v
}

// scrubIdentity is a user object as a recording keeps it.
func (s *DiscordSandbox) scrubIdentity(o map[string]any) map[string]any {
	out := map[string]any{}
	for k, c := range o {
		if !discordKeptKeys[k] {
			continue
		}
		switch k {
		case "id":
			if id, ok := c.(string); ok && discordSnowflakeRe.MatchString(id) {
				out[k] = s.mapID(id, k)
			}
		case "username":
			// Only the bot's own name is kept, as its placeholder.
			if id, _ := o["id"].(string); id == s.BotUserID {
				out[k] = DiscordBotUsername
			}
		default:
			out[k] = c
		}
	}
	return out
}
