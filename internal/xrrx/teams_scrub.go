package xrrx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// teamsNameKeys are the keys under which a Bot Connector answer names a
// person or says who they are: none is the recording's to keep.
var teamsNameKeys = map[string]bool{
	"name": true, "givenName": true, "surname": true, "email": true, "userPrincipalName": true,
	"aadObjectId": true, "displayName": true, "objectId": true,
}

var (
	// teamsNameKeyRe is a key that names a person: none may be left in an
	// answer.
	teamsNameKeyRe = regexp.MustCompile(`"(?:name|givenName|surname|email|userPrincipalName|aadObjectId|displayName|objectId)"\s*:`)
	// The accounts and conversations of anyone else, wherever they appear.
	teamsAccountRe = regexp.MustCompile(`\b(2[89]):[A-Za-z0-9_-]{8,}`)
	teamsChatAnyRe = regexp.MustCompile(`\ba:[A-Za-z0-9_-]{8,}`)
	teamsThreadRe  = regexp.MustCompile(`\b19:[A-Za-z0-9_.@-]+`)
	teamsGUIDAnyRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
)

// scrubResponse is resp as a recording keeps it: the bot, the tenant, the
// service, the user, the chat and every other ID replaced, the names
// dropped, churning headers removed. An answer that still names what the
// sandbox exists to keep out is not handed on.
func (s *TeamsSandbox) scrubResponse(resp *http.Response) (*http.Response, error) {
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
func (s *TeamsSandbox) residue(text string) error {
	fail := func(what string) error {
		return fmt.Errorf("xrrx: Teams sandbox: the answer still holds %s after the scrub, not handed on", what)
	}
	low := strings.ToLower(text)
	svc, _ := url.Parse(s.ServiceURL)
	for what, lits := range map[string][]string{
		"the token":   {s.Token},
		"the bot":     {s.AppID},
		"the tenant":  {s.TenantID},
		"the service": {svc.Hostname()},
		"the user":    {s.UserID},
		"the chat":    {s.ConversationID},
	} {
		for _, lit := range lits {
			if lit != "" && strings.Contains(low, strings.ToLower(lit)) {
				return fail(what)
			}
		}
	}
	if teamsJWTAnyRe.MatchString(text) {
		return fail("a token")
	}
	if teamsNameKeyRe.MatchString(text) {
		return fail("a name")
	}
	return nil
}

// scrubString replaces the token, the service, the bot, the tenant, the
// user, the chat and the accounts and conversations of anyone else in in.
func (s *TeamsSandbox) scrubString(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scrubLocked(in)
}

func (s *TeamsSandbox) scrubLocked(in string) string {
	if s.Token != "" {
		in = strings.ReplaceAll(in, s.Token, "<token>")
	}
	svc, _ := url.Parse(s.ServiceURL)
	in = replaceFold(in, strings.TrimSuffix(s.ServiceURL, "/"), strings.TrimSuffix(TeamsServiceURL, "/"))
	in = replaceFold(in, svc.Hostname(), TeamsServiceHost)
	in = strings.ReplaceAll(in, s.ConversationID, TeamsConversationID)
	in = strings.ReplaceAll(in, s.UserID, TeamsUserID)
	in = replaceFold(in, s.AppID, TeamsAppID)
	in = replaceFold(in, s.TenantID, TeamsTenantID)
	in = teamsAccountRe.ReplaceAllStringFunc(in, func(m string) string {
		if m == TeamsUserID || m == "28:"+TeamsAppID {
			return m
		}
		return s.mapOther(m, m[:3]+teamsOtherUserBase)
	})
	in = teamsChatAnyRe.ReplaceAllStringFunc(in, func(m string) string {
		if m == TeamsConversationID {
			return m
		}
		return s.mapOther(m, "a:"+teamsOtherUserBase+"chat-")
	})
	in = teamsThreadRe.ReplaceAllStringFunc(in, func(m string) string { return s.mapOther(m, "19:"+teamsOtherUserBase+"conversation-") })
	return teamsGUIDAnyRe.ReplaceAllStringFunc(in, func(m string) string {
		if strings.EqualFold(m, TeamsAppID) || strings.EqualFold(m, TeamsTenantID) {
			return m
		}
		return s.mapOther(strings.ToLower(m), "guid")
	})
}

// mapOther is the stable placeholder of an account, chat or GUID the
// sandbox does not own, numbered in order of appearance; prefix says what
// kind it is ("guid" for a GUID).
func (s *TeamsSandbox) mapOther(real, prefix string) string {
	if s.others == nil {
		s.others = map[string]string{}
	}
	if p, ok := s.others[real]; ok {
		return p
	}
	n := len(s.others) + 1
	p := fmt.Sprintf("%s%d", prefix, n)
	if prefix == "guid" {
		p = fmt.Sprintf("00000000-0000-4000-8000-%012x", 0xb000+n)
	}
	s.others[real] = p
	return p
}

// ScrubJSON rewrites a Bot Connector JSON document so it names no one: the
// token, the service, the bot, the tenant, the user, the chat and every
// other account, conversation and GUID are replaced wherever they appear,
// and the keys that name a person are dropped. Keys come out sorted.
func (s *TeamsSandbox) ScrubJSON(b []byte) ([]byte, error) {
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

func (s *TeamsSandbox) scrubValue(v any) any {
	switch t := v.(type) {
	case string:
		return s.scrubLocked(t)
	case []any:
		for i := range t {
			t[i] = s.scrubValue(t[i])
		}
		return t
	case map[string]any:
		// Sorted: the placeholders are numbered in the order they are met, and a
		// map ranges in a random one.
		for _, k := range slices.Sorted(maps.Keys(t)) {
			if teamsNameKeys[k] {
				delete(t, k)
				continue
			}
			t[k] = s.scrubValue(t[k])
		}
		return t
	}
	return v
}
