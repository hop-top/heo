package xrrx

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// teamsRehearsal is the in-memory Bot Connector a rehearsal answers from:
// the messages the recording sent, their text and reactions.
type teamsRehearsal struct {
	sends int
	msgs  map[string]*teamsRehearsed
}

type teamsRehearsed struct {
	text      string
	reactions map[string]bool
	deleted   bool
}

// canned is the answer a rehearsal gives instead of the Bot Connector's:
// the documented shapes of the few calls a recording may make, over a
// personal chat that holds only what the recording sent. It names the real
// bot, tenant, service, user and chat, and a name and an object ID for the
// user, so the scrub that follows has something to do. It is a stand-in
// for debugging a recording's path before it sends anything; nothing it
// says is Microsoft's.
func (s *TeamsSandbox) canned(op teamsOp, req *http.Request) (*http.Response, error) {
	var args map[string]any
	if req.GetBody != nil {
		rc, _ := req.GetBody()
		raw, _ := io.ReadAll(rc)
		args, _ = decodeObject(raw)
	}
	text, _ := args["text"].(string)
	segs := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	activity, reaction := "", ""
	for i, seg := range segs {
		if seg == "activities" && i+1 < len(segs) {
			activity = segs[i+1]
		}
		if seg == "reactions" && i+1 < len(segs) {
			reaction = segs[i+1]
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rehearse.msgs == nil {
		s.rehearse.msgs = map[string]*teamsRehearsed{}
	}
	if s.known == nil {
		s.known = map[string]bool{}
	}
	msgs := s.rehearse.msgs
	failed := func(status int, code string) (*http.Response, error) {
		return cannedTeams(status, map[string]any{"error": map[string]any{"code": code, "message": code + " in chat " + s.ConversationID}}), nil
	}
	ok := func(v any) (*http.Response, error) { return cannedTeams(http.StatusOK, v), nil }
	switch op {
	case teamsCreate:
		return ok(map[string]any{
			"id": s.ConversationID, "activityId": nil, "serviceUrl": s.ServiceURL,
			"members": []any{map[string]any{"id": s.UserID, "name": "Rehearsal User", "aadObjectId": "11111111-2222-4333-8444-555555555555"}},
		})
	case teamsSend:
		if s.rehearse.sends >= s.Max {
			return nil, &scrubbedErr{msg: "xrrx: Teams sandbox: the cap of " + strconv.Itoa(s.Max) + " sends is spent, send refused"}
		}
		s.rehearse.sends++
		id := strconv.Itoa(1_760_000_000_000 + s.rehearse.sends)
		msgs[id] = &teamsRehearsed{text: text, reactions: map[string]bool{}}
		s.known[id] = true
		return ok(map[string]any{"id": id})
	}
	m := msgs[activity]
	if m == nil || m.deleted {
		return failed(http.StatusNotFound, "ActivityNotFoundInConversation")
	}
	switch op {
	case teamsUpdate:
		m.text = text
	case teamsDelete:
		m.deleted = true
	case teamsReact:
		m.reactions[reaction] = true
	case teamsUnreact:
		delete(m.reactions, reaction)
	}
	return ok(map[string]any{"id": activity})
}

func cannedTeams(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(string(b))),
	}
}
