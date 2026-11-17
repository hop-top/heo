package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	xrr "hop.top/xrr"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/xrrx"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// Cassettes of github.issues through the real heo binary. In replay (the
// default) the daemon answers from committed cassettes and the webhook
// payloads come from committed fixtures, so nothing reaches GitHub and no
// credential is needed. Recording is the one live step: a private sandbox
// repository, behind HEO_RECORD_LIVE=1 and XRR_MODE=record, never in CI;
// see docs/DEVELOPING.md. The daemon is told the placeholder repository
// and account only; internal/xrrx translates between those and the real
// ones, and the fixtures are written from the real answers GitHub gave,
// scrubbed by the same rules as the cassettes.

const (
	// ghWebhookKey signs the webhook fixtures: a test value, not a credential.
	ghWebhookKey = "e2e-github-webhook-secret" // #nosec G101 -- test value
	// ghReplayToken stands in for the sandbox credential in replay: the
	// cassettes record no Authorization header, so any value replays.
	ghReplayToken = "replay-only-not-a-credential" // #nosec G101 -- test value
	// ghFixtures is where the webhook payloads live.
	ghFixtures = "testdata/github"
)

// ghLive is the recording's side of the sandbox: nil in replay.
type ghLive struct {
	g      *xrrx.GitHubSandbox
	c      *xrrx.Client
	token  string
	number int
}

// openLive returns the live sandbox when this test is recording, nil in
// replay, and skips a recording that was not asked to go live.
func openLive(t *testing.T) *ghLive {
	t.Helper()
	switch xrrModeOf(t) {
	case xrr.ModePassthrough:
		t.Skip("github.issues is replayed or recorded, never passed through to GitHub")
	case xrr.ModeReplay:
		return nil
	}
	if os.Getenv(xrrx.EnvLive) != "1" {
		t.Skipf("recording github.issues is live: set %s=1 and %s (docs/DEVELOPING.md)", xrrx.EnvLive, xrrx.EnvGitHubRepo)
	}
	if err := xrrx.CheckLive(os.Getenv); err != nil {
		t.Fatal(err)
	}
	g, err := xrrx.NewGitHubSandbox(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	token := os.Getenv(xrrx.EnvGitHubToken)
	if token == "" {
		t.Fatalf("%s is not set", xrrx.EnvGitHubToken)
	}
	return &ghLive{g: g, c: g.Client(token, nil), token: token}
}

func (l *ghLive) must(t *testing.T, method, p string, body any) json.RawMessage {
	t.Helper()
	out, err := l.c.Do(t.Context(), method, p, body)
	if err != nil {
		t.Fatalf("sandbox %s %s: %v", method, p, err)
	}
	return out
}

// ghFixture is a webhook payload fixture: read in replay, written from the
// real answer in record.
func ghFixture(t *testing.T, name string, record func() any) []byte {
	t.Helper()
	p := filepath.Join(ghFixtures, name+".json")
	if record != nil {
		b, err := json.MarshalIndent(record(), "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(ghFixtures, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: the test's own fixture
	if err != nil {
		t.Fatalf("no fixture %s: record with `make record-github` (docs/DEVELOPING.md): %v", p, err)
	}
	return b
}

// payload is the webhook body for an issue event: the action, the issue
// as GitHub's API gave it, the (placeholder) repository and the sender.
func payload(action string, issue, comment json.RawMessage) map[string]any {
	var iss map[string]any
	_ = json.Unmarshal(issue, &iss)
	out := map[string]any{
		"action": action, "issue": iss, "sender": iss["user"],
		"repository": map[string]any{
			"id": 9100100, "node_id": xrrx.NodeIDPlaceholder, "name": "sandbox", "full_name": xrrx.SandboxRepo, "private": true,
			"owner": map[string]any{"login": xrrx.SandboxOwner, "id": 9100003, "node_id": xrrx.NodeIDPlaceholder, "type": "User"},
		},
	}
	if comment != nil {
		var c map[string]any
		_ = json.Unmarshal(comment, &c)
		out["comment"] = c
	}
	return out
}

// ghSite is a daemon with a github.issues channel on the placeholder
// repository, feeding it webhook fixtures and recording or replaying its
// calls to GitHub.
type ghSite struct {
	*cliSite
	d       *daemon
	hookURL string
	live    *ghLive
	channel model.Channel
	hookSeq int
}

func newGHSite(t *testing.T, live *ghLive) *ghSite {
	t.Helper()
	c := newCLISite(t, "  all:\n    rate_limit:\n      enabled: false\n")
	token := ghReplayToken
	var env []string
	if live != nil {
		token = live.token
		env = live.g.ChildEnv()
	}
	secret := func(ref, purpose, v string) {
		b, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": v})
		if err != nil {
			t.Fatal(err)
		}
		c.s.write("secrets/uma/default/"+ref+"@"+purpose, string(b))
	}
	secret("gh", github.CredentialPurposeAPI, token)
	secret("gh", github.CredentialPurposeWebhook, ghWebhookKey)
	s := &ghSite{cliSite: c, live: live}
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	s.d = c.s.start(append(c.s.xrrEnv("daemon"), env...), nil, []string{"socket", "receivers", "webhook", "workers"}, args...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", s.d.log())
		}
	})
	cfg := fmt.Sprintf(`{"replies":true,"account_id":%d}`, xrrx.SandboxAccountID)
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(github.Type), "--key", "gh", "--configuration", cfg, "--credential-ref", "gh")
	s.channel = ch.Channel
	c.heo(0, "inbox", "create", "--channel", "gh", "--key", "issues", "--destination", xrrx.SandboxRepo, "--mode", model.ModePerWorkItem)
	s.hookURL = "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	return s
}

// deliver posts body to the channel's webhook as GitHub would: signed
// under the webhook secret, retried while the daemon has not mounted the
// channel's route yet.
func (s *ghSite) deliver(event string, body []byte) {
	s.t.Helper()
	s.hookSeq++
	delivery := fmt.Sprintf("d-%s-%d", event, s.hookSeq)
	m := hmac.New(sha256.New, []byte(ghWebhookKey))
	m.Write(body)
	sig := "sha256=" + hex.EncodeToString(m.Sum(nil))
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.hookURL, bytes.NewReader(body))
		if err != nil {
			s.t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(github.HeaderEvent, event)
		req.Header.Set(github.HeaderDelivery, delivery)
		req.Header.Set(github.HeaderSignature, sig)
		resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback webhook of the daemon the test started
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusServiceUnavailable {
				s.t.Fatalf("webhook %s = %d", event, resp.StatusCode)
			}
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("webhook %s: the channel's route never answered (%v)", event, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// issue is the one work item the site holds, read through its conversation.
func (s *ghSite) issue() workItemJSON {
	s.t.Helper()
	var p pageJSON[conversationItemJSON]
	s.json(&p, "conversation", "list", "--work-item")
	if len(p.Items) != 1 {
		s.t.Fatalf("work items = %+v", p.Items)
	}
	return s.workItem(string(p.Items[0].Ref.ID))
}

// message is the message of the issue's conversation whose text is text.
func (s *ghSite) message(text string) model.Message {
	s.t.Helper()
	var found model.Message
	eventually(s.t, "message "+text, func() bool {
		for _, m := range s.issue().Messages {
			if desk.Text(m.Content) == text {
				found = m
				return true
			}
		}
		return false
	})
	return found
}

// accepted waits for op to be taken by the provider.
func (s *ghSite) accepted(op opJSON) {
	s.t.Helper()
	if d := s.waitDelivery(op.ID, model.ProviderAccepted, model.Delivered, model.Failed); d.State == model.Failed {
		s.t.Fatalf("operation %s failed: %+v", op.ID, d.Failure)
	}
}

// pause lets GitHub's one-second timestamps move on between two changes
// of one object, when recording.
func (s *ghSite) pause() {
	if s.live != nil {
		time.Sleep(1200 * time.Millisecond)
	}
}

// TestXRRGitHubIssuesProviderVerbs: github.issues through the real heo
// binary. A description and a comment arrive as GitHub's webhooks
// (fixtures written from real answers), the issue is listed and read, and
// then, each one a request the daemon's workers send to GitHub and the
// cassettes hold: a reaction on the comment, a reply, the edit and the
// delete of that reply, the removal of the reaction, and the issue closed
// as completed, reopened and closed as not planned. In record mode the
// sandbox is read back after each change to show GitHub did it. Previews
// and a verb the adapter does not declare send nothing, and with replies
// switched off a queued edit ends unsupported with no request: the
// cassette directory does not grow. In replay nothing reaches GitHub.
func TestXRRGitHubIssuesProviderVerbs(t *testing.T) {
	withE2EBinary(t, func() {
		live := openLive(t)
		var issueRaw, commentRaw json.RawMessage
		var base string
		if live != nil {
			t.Cleanup(func() { live.cleanup(t) })
			issueRaw = live.must(t, http.MethodPost, "/repos/"+xrrx.SandboxRepo+"/issues", map[string]string{
				"title": "heo adapter check", "body": "A synthetic issue for a recorded adapter test."})
			var n struct{ Number int }
			_ = json.Unmarshal(issueRaw, &n)
			live.number = n.Number
			base = "/repos/" + xrrx.SandboxRepo + "/issues/" + strconv.Itoa(n.Number)
			commentRaw = live.must(t, http.MethodPost, base+"/comments", map[string]string{"body": "A synthetic comment."})
			live.recordAudit(t)
		}
		opened := ghFixture(t, "issues-opened", recordIf(live, func() any { return payload("opened", issueRaw, nil) }))
		commented := ghFixture(t, "issue_comment-created", recordIf(live, func() any { return payload("created", issueRaw, commentRaw) }))

		s := newGHSite(t, live)
		s.deliver("issues", opened)
		s.deliver("issue_comment", commented)

		iv := s.issue()
		if iv.WorkItem.Status != model.WorkItemOpen || iv.WorkItem.External.ParentID != xrrx.SandboxRepo || len(iv.Messages) != 2 {
			t.Fatalf("issue = %+v", iv)
		}
		conv := string(iv.Conversation.Ref.ID)
		theirs := s.message("A synthetic comment.")
		var commentID struct{ ID int64 }
		_ = json.Unmarshal(commentRaw, &commentID)

		// The channel's verb table, offline: what github.issues declares.
		table := s.heo(0, "channel", "get", "gh").stdout
		for _, want := range []string{
			"verb message edit         declared (edit)", "verb message delete       declared (delete)", "verb reaction add         declared (react)",
			"verb conversation status  declared (work_item_act)", "verb conversation tag     unsupported: not_declared (extension_execute)",
			"verb history fetch        declared (history)", "verb receipt send         unsupported: not_declared (receipt)",
		} {
			if !tableHas(table, want) {
				t.Errorf("channel get lacks %q:\n%s", want, table)
			}
		}

		// The same table in JSON: every row carries a limits object; the
		// declared work item and effect verbs carry what github.issues
		// declares for them.
		var cv struct {
			Verbs []map[string]json.RawMessage `json:"verbs"`
		}
		s.json(&cv, "channel", "get", "gh")
		limits := map[string]map[string]json.RawMessage{}
		for _, row := range cv.Verbs {
			var name string
			_ = json.Unmarshal(row["verb"], &name)
			var l map[string]json.RawMessage
			if err := json.Unmarshal(row["limits"], &l); err != nil || row["limits"] == nil {
				t.Fatalf("verb %s: limits = %s, want an object", name, row["limits"])
			}
			limits[name] = l
		}
		if limits["conversation status"]["transitions"] == nil || limits["conversation update"]["effects"] == nil || len(limits["conversation tag"]) != 0 {
			t.Errorf("channel get limits = %v", limits)
		}

		// Previews and refusals send nothing: no operation, no request.
		held, sent := s.ledger(), s.cassettes()
		edit := []string{"message", "edit", string(theirs.Ref.ID), "--revision", string(theirs.Revision), "--text", "preview only"}
		s.heo(0, append(edit, "--dry-run")...)
		s.heo(0, "conversation", "status", conv, "--set", "closed", "--dry-run")
		wantStderr(t, s.heo(2, "message", "send", "--inbox", "issues", "--to", "user:grace", "--text", "hi"),
			"UNSUPPORTED", "cannot start a conversation", "start_conversation")
		wantStderr(t, s.heo(2, "conversation", "status", conv, "--set", "pending", "--dry-run"), "UNSUPPORTED", "declares no work item transition")
		if n := s.ledger(); n != held || s.cassettes() != sent {
			t.Fatalf("a preview or a refusal wrote: operations %d, was %d; requests %d, was %d", n, held, s.cassettes(), sent)
		}

		// React to the comment, and read the reaction back.
		var rr reactJSON
		s.json(&rr, "reaction", "add", "--message", string(theirs.Ref.ID), "👍", "--idempotency-key", "gh-react")
		s.accepted(rr.Operation)
		if l := s.reactions(theirs); len(l) != 1 || l[0].Emoji != "👍" || l[0].Count != 1 {
			t.Fatalf("reactions after the add = %+v", l)
		}
		live.expectReactions(t, base, commentID.ID, 1)

		// Reply, edit, delete.
		var reply replyJSON
		s.json(&reply, "message", "reply", conv, "--text", "On it.", "--idempotency-key", "gh-reply")
		s.accepted(reply.Operation)
		mine := s.message("On it.")
		live.expectComments(t, base, "A synthetic comment.", "On it.")
		s.pause()

		var ed providerOpJSON
		s.json(&ed, "message", "edit", string(mine.Ref.ID), "--revision", string(mine.Revision), "--text", "Fixed in the next release.", "--idempotency-key", "gh-edit")
		s.accepted(ed.Operation)
		mine = s.message("Fixed in the next release.")
		live.expectComments(t, base, "A synthetic comment.", "Fixed in the next release.")

		var del providerOpJSON
		s.json(&del, "message", "delete", string(mine.Ref.ID), "--revision", string(mine.Revision), "--confirm=yes", "--idempotency-key", "gh-delete")
		s.accepted(del.Operation)
		var gone model.Message
		s.json(&gone, "message", "get", string(mine.Ref.ID))
		if !gone.Deleted {
			t.Errorf("the deleted reply is not a tombstone: %+v", gone)
		}
		live.expectComments(t, base, "A synthetic comment.")

		var rm reactJSON
		s.json(&rm, "reaction", "remove", "--message", string(theirs.Ref.ID), "👍", "--idempotency-key", "gh-unreact")
		s.accepted(rm.Operation)
		if l := s.reactions(theirs); len(l) != 0 {
			t.Errorf("reactions after the removal = %+v", l)
		}
		live.expectReactions(t, base, commentID.ID, 0)

		// Close, reopen, close as not planned. GitHub's answer to the
		// change does not settle it: its webhook does (the fixtures are the
		// issue as GitHub then held it).
		transition := func(state, reason, fixture string, wantStatus, wantRaw string) {
			cur := s.issue()
			args := []string{"conversation", "status", conv, "--set", state, "--revision", string(cur.WorkItem.Revision), "--idempotency-key", "gh-" + fixture}
			if reason != "" {
				args = append(args, "--reason", reason)
			}
			var out statusJSON
			s.json(&out, args...)
			if out.WorkItem.Status != cur.WorkItem.Status {
				t.Fatalf("the output claims the change: %+v", out)
			}
			s.accepted(out.Operation)
			s.pause()
			var fresh json.RawMessage
			if live != nil {
				fresh = live.must(t, http.MethodGet, base, nil)
				live.expectIssue(t, fresh, wantRaw)
			}
			action := map[string]string{"closed": "closed", "open": "reopened"}[state]
			s.deliver("issues", ghFixture(t, fixture, recordIf(live, func() any { return payload(action, fresh, nil) })))
			eventually(t, "issue "+wantRaw, func() bool {
				w := s.issue().WorkItem
				return w.Status == wantStatus && w.RawStatus == wantRaw
			})
		}
		transition("closed", "", "issues-closed", model.WorkItemClosed, "closed:completed")
		transition("open", "", "issues-reopened", model.WorkItemOpen, "open:reopened")
		transition("closed", "not_planned", "issues-closed-not-planned", model.WorkItemClosed, "closed:not_planned")

		// Replies switched off: the verbs report it, and an edit queued
		// anyway ends unsupported without a request.
		s.updateChannel(s.channel, fmt.Sprintf(`{"account_id":%d}`, xrrx.SandboxAccountID))
		off := s.heo(0, "channel", "get", "gh").stdout
		for _, want := range []string{"verb message edit         unsupported: config (edit)", "verb reaction add         unsupported: config (react)", "replies is off"} {
			if !tableHas(off, want) {
				t.Errorf("channel get with replies off lacks %q:\n%s", want, off)
			}
		}
		sent = s.cassettes()
		var queued providerOpJSON
		s.json(&queued, "message", "edit", string(theirs.Ref.ID), "--revision", string(s.message("A synthetic comment.").Revision), "--text", "not sent", "--idempotency-key", "gh-edit-off")
		d := s.waitDelivery(queued.Operation.ID, model.Failed, model.ProviderAccepted, model.Delivered)
		if d.State != model.Failed || d.Failure == nil || !strings.Contains(d.Failure.Error(), "unsupported") {
			t.Fatalf("an edit with replies off = %+v", d)
		}
		if s.cassettes() != sent {
			t.Errorf("an edit with replies off sent a request")
		}
		live.expectComments(t, base, "A synthetic comment.")
	})
}

// recordIf is the fixture recorder when live, nil in replay.
func recordIf(l *ghLive, f func() any) func() any {
	if l == nil {
		return nil
	}
	return f
}

// cleanup closes the issue and deletes the comments it holds: all of them
// are the test's own, on an issue it created.
func (l *ghLive) cleanup(t *testing.T) {
	t.Helper()
	if l.number == 0 {
		return
	}
	base := "/repos/" + xrrx.SandboxRepo + "/issues/" + strconv.Itoa(l.number)
	var cs []struct{ ID int64 }
	if b, err := l.c.Do(context.Background(), http.MethodGet, base+"/comments", nil); err == nil {
		_ = json.Unmarshal(b, &cs)
	}
	for _, c := range cs {
		if _, err := l.c.Do(context.Background(), http.MethodDelete, "/repos/"+xrrx.SandboxRepo+"/issues/comments/"+strconv.FormatInt(c.ID, 10), nil); err != nil {
			t.Logf("cleanup: %v", err)
		}
	}
	if _, err := l.c.Do(context.Background(), http.MethodPatch, base, map[string]string{"state": "closed"}); err != nil {
		t.Logf("cleanup: %v", err)
	}
}

// recordAudit audits the cassettes and fixtures this recording wrote, for
// the identity only the recording knows, when the test ends.
func (l *ghLive) recordAudit(t *testing.T) {
	t.Cleanup(func() {
		found, err := xrrx.AuditWith("testdata", l.g.Deny()...)
		if err != nil {
			t.Errorf("audit: %v", err)
		}
		for _, f := range found {
			t.Errorf("recording is not clean: %s", f)
		}
	})
}

// expectReactions checks, in record mode, that GitHub holds n reactions on
// comment id.
func (l *ghLive) expectReactions(t *testing.T, base string, id int64, n int) {
	t.Helper()
	if l == nil {
		return
	}
	var rs []struct{ Content string }
	if err := json.Unmarshal(l.must(t, http.MethodGet, "/repos/"+xrrx.SandboxRepo+"/issues/comments/"+strconv.FormatInt(id, 10)+"/reactions", nil), &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs) != n {
		t.Errorf("GitHub holds %d reactions on the comment, want %d", len(rs), n)
	}
}

// expectComments checks, in record mode, that the issue holds comments
// whose bodies start with wants, in order (the echo marker aside).
func (l *ghLive) expectComments(t *testing.T, base string, wants ...string) {
	t.Helper()
	if l == nil {
		return
	}
	var cs []struct{ Body string }
	if err := json.Unmarshal(l.must(t, http.MethodGet, base+"/comments", nil), &cs); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, strings.SplitN(c.Body, "\n\n<!--", 2)[0])
	}
	if strings.Join(got, "|") != strings.Join(wants, "|") {
		t.Errorf("GitHub holds comments %q, want %q", got, wants)
	}
}

// expectIssue checks, in record mode, the issue's state and state reason
// as GitHub holds them.
func (l *ghLive) expectIssue(t *testing.T, raw json.RawMessage, want string) {
	t.Helper()
	var i struct {
		State       string
		StateReason string `json:"state_reason"`
	}
	if err := json.Unmarshal(raw, &i); err != nil {
		t.Fatal(err)
	}
	if got := i.State + ":" + i.StateReason; got != want {
		t.Errorf("GitHub holds the issue %s, want %s", got, want)
	}
}

// reactions lists what uma holds for m.
func (s *ghSite) reactions(m model.Message) []model.MessageReaction {
	s.t.Helper()
	var l reactionListJSON
	s.json(&l, "reaction", "list", "--message", string(m.Ref.ID))
	return l.Reactions
}

// cassettes counts the requests the daemon's cassette directory holds.
func (s *ghSite) cassettes() int {
	s.t.Helper()
	n := 0
	ents, _ := os.ReadDir(filepath.Join("testdata", "cassettes", s.t.Name(), "daemon"))
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".req.yaml") {
			n++
		}
	}
	return n
}

// ledger counts the operations uma holds: what a preview or a refused
// verb must leave as it was.
func (s *ghSite) ledger() int {
	s.t.Helper()
	c := s.db.operator("default")
	req := model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 100}}
	n := 0
	for {
		p, err := c.Operations().List(s.t.Context(), req)
		if err != nil {
			s.t.Fatal(err)
		}
		n += len(p.Items)
		if p.Complete || p.Next == "" {
			return n
		}
		req.Page.After = p.Next
	}
}
