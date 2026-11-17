package approvechat_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

type fakeLog struct {
	mu   sync.Mutex
	info []string
}

func (l *fakeLog) Info(msg any, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.info = append(l.info, fmt.Sprint(append([]any{msg}, kv...)...))
}
func (l *fakeLog) Error(msg any, kv ...any) { l.Info(msg, kv...) }
func (l *fakeLog) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.info {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

type fakeDesk struct {
	ops     map[string]desk.HeldOperation
	blind   map[model.ActorID]bool // actors that may not read
	denied  map[model.ActorID]bool // actors that may not decide
	decided []string
	byCorr  map[string]bool
	calls   int
}

func (f *fakeDesk) Get(_ context.Context, a model.ActorID, id string) (desk.HeldOperation, error) {
	op, ok := f.ops[id]
	if !ok || f.blind[a] {
		return desk.HeldOperation{}, model.NewError(model.NotFound, "resource not found")
	}
	return op, nil
}

func (f *fakeDesk) Decide(_ context.Context, a model.ActorID, r desk.DecideRequest) (desk.HeldDecided, error) {
	f.calls++
	if f.denied[a] {
		return desk.HeldDecided{}, model.NewError(model.Unauthorized, "denied")
	}
	if f.byCorr == nil {
		f.byCorr = map[string]bool{}
	}
	replay := f.byCorr[r.Correlation]
	f.byCorr[r.Correlation] = true
	if !replay {
		f.decided = append(f.decided, fmt.Sprintf("%s:%s:%s:%s", a, r.ID, r.Decision, r.Note))
	}
	return desk.HeldDecided{Replayed: replay}, nil
}

type fakePost struct {
	posts []string
	edits []string
	n     int
}

func (p *fakePost) Post(_ context.Context, _ approvechat.Conversation, replyTo, text string) (string, error) {
	p.n++
	p.posts = append(p.posts, replyTo+"|"+text)
	return fmt.Sprintf("m%d", p.n), nil
}
func (p *fakePost) Edit(_ context.Context, _ approvechat.Conversation, m, text string) error {
	p.edits = append(p.edits, m+"|"+text)
	return nil
}

func heldOp() desk.HeldOperation {
	exp := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	return desk.HeldOperation{ID: "op1", Kind: "reply", Target: "conversation/c1", Status: string(model.Held),
		Proposer: "plugin:ops", Reason: "plugin write", Preview: json.RawMessage(`{"text":"secret draft"}`), ExpiresAt: &exp}
}

func cfg(members ...approvechat.Member) approvechat.Config {
	return approvechat.Config{Enabled: true, Conversations: []approvechat.Conversation{
		{Inbox: "in1", Conversation: "cv1", Members: members}}}
}

var (
	alice = approvechat.Member{Identity: "idn-alice", Actor: "uid:1001"}
	bob   = approvechat.Member{Identity: "idn-bob", Actor: "uid:1002"}
)

type rig struct {
	h *approvechat.Handler
	d *fakeDesk
	p *fakePost
	l *fakeLog
}

func newRig(c approvechat.Config) rig {
	d := &fakeDesk{ops: map[string]desk.HeldOperation{"op1": heldOp()}, blind: map[model.ActorID]bool{}, denied: map[model.ActorID]bool{}}
	p, l := &fakePost{}, &fakeLog{}
	return rig{approvechat.New(c, d, d, p, l), d, p, l}
}

func (r rig) held(t *testing.T) string {
	t.Helper()
	r.h.OnHeld(context.Background(), "op1")
	if len(r.p.posts) == 0 {
		t.Fatal("nothing announced")
	}
	return r.p.posts[0]
}

func in(event, who, text, emoji, to string) approvechat.Inbound {
	return approvechat.Inbound{Event: event, Inbox: "in1", Conversation: "cv1", Identity: who, Text: text, Emoji: emoji, To: to}
}

func TestAnnounceShowsPreviewWhenEveryMemberMayRead(t *testing.T) {
	r := newRig(cfg(alice, bob))
	got := r.held(t)
	if !strings.Contains(got, "secret draft") || !strings.Contains(got, "approve") {
		t.Fatalf("announcement = %q", got)
	}
	r.h.OnHeld(context.Background(), "op1") // redelivered event
	if len(r.p.posts) != 1 {
		t.Fatalf("announced %d times", len(r.p.posts))
	}
}

func TestAnnounceWithholdsPreviewFromAChatWithABlindMember(t *testing.T) {
	r := newRig(cfg(alice, bob))
	r.d.blind["uid:1002"] = true
	got := r.held(t)
	if strings.Contains(got, "secret") || strings.Contains(got, "plugin:ops") || strings.Contains(got, "conversation/c1") {
		t.Fatalf("leaked: %q", got)
	}
	if !strings.Contains(got, "op1") || !r.l.has("withheld") {
		t.Fatalf("announcement = %q", got)
	}
}

func TestNothingAnnouncedWhenNoMemberMayRead(t *testing.T) {
	r := newRig(cfg(alice))
	r.d.blind["uid:1001"] = true
	r.h.OnHeld(context.Background(), "op1")
	if len(r.p.posts) != 0 {
		t.Fatalf("posts = %v", r.p.posts)
	}
}

func TestReplyApprovesAsTheMappedActor(t *testing.T) {
	r := newRig(cfg(alice, bob))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-bob", "Approve looks fine", "", "m1"))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1002:op1:approve:looks fine" {
		t.Fatalf("decided = %v", r.d.decided)
	}
	if last := r.p.posts[len(r.p.posts)-1]; !strings.Contains(last, "approved by uid:1002") {
		t.Fatalf("answer = %q", last)
	}
}

func TestReactionsDecide(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "", "👎️", "m1"))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1001:op1:reject:" {
		t.Fatalf("decided = %v", r.d.decided)
	}
	r2 := newRig(cfg(alice))
	r2.held(t)
	r2.h.OnInbound(context.Background(), in("e1", "idn-alice", "", "😀", "m1"))
	if r2.d.calls != 0 {
		t.Fatal("an unrelated emoji decided")
	}
	r2.h.OnInbound(context.Background(), in("e2", "idn-alice", "", "👍", "m1"))
	if len(r2.d.decided) != 1 || !strings.HasSuffix(r2.d.decided[0], "approve:") {
		t.Fatalf("decided = %v", r2.d.decided)
	}
}

func TestExplicitOperationWorksWithoutAnAnnouncement(t *testing.T) {
	r := newRig(cfg(alice))
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "reject #op1 wrong thread", "", "other"))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1001:op1:reject:wrong thread" {
		t.Fatalf("decided = %v", r.d.decided)
	}
}

func TestChatterIsIgnored(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	for i, text := range []string{"approve", "lunch?", "approved by me earlier", ""} {
		to := "other" // not the announcement
		if i == 2 || i == 3 {
			to = "m1"
		}
		r.h.OnInbound(context.Background(), in(fmt.Sprint("e", i), "idn-alice", text, "", to))
	}
	if r.d.calls != 0 || len(r.p.posts) != 1 {
		t.Fatalf("calls=%d posts=%v", r.d.calls, r.p.posts)
	}
}

func TestUnmappedUserIsIgnoredAndLogged(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-mallory", "approve", "", "m1"))
	r.h.OnInbound(context.Background(), in("e2", "idn-mallory", "", "👍", "m1"))
	if r.d.calls != 0 || len(r.p.posts) != 1 {
		t.Fatalf("calls=%d posts=%v", r.d.calls, r.p.posts)
	}
	if !r.l.has("unmapped user ignored") || !r.l.has("idn-mallory") {
		t.Fatalf("log = %v", r.l.info)
	}
}

func TestReplayedApprovalDecidesOnce(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	if r.d.calls != 1 || len(r.d.decided) != 1 {
		t.Fatalf("calls=%d decided=%v", r.d.calls, r.d.decided)
	}
	if len(r.p.posts) != 2 { // announcement, one answer
		t.Fatalf("posts = %v", r.p.posts)
	}
}

func TestDecisionReplayedByUmaIsSilent(t *testing.T) {
	// A second event (a new ID) after a decision: the operation is no
	// longer held, so the answer is polite and nothing is decided.
	r := newRig(cfg(alice, bob))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	op := heldOp()
	op.Status, op.Decision, op.DecidedBy = "queued", "approved", "uid:1001"
	r.d.ops["op1"] = op
	r.h.OnInbound(context.Background(), in("e2", "idn-bob", "reject", "", "m1"))
	if r.d.calls != 1 || !strings.Contains(r.p.posts[len(r.p.posts)-1], "already approved") {
		t.Fatalf("calls=%d posts=%v", r.d.calls, r.p.posts)
	}
}

func TestExpiredOperationIsRefusedPolitely(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	op := heldOp()
	op.Status, op.Decision = "expired", string(model.HeldExpired)
	r.d.ops["op1"] = op
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	if r.d.calls != 0 {
		t.Fatal("decided an expired operation")
	}
	if last := r.p.posts[len(r.p.posts)-1]; !strings.Contains(last, "expired") {
		t.Fatalf("answer = %q", last)
	}
}

func TestMappedUserWithoutTheRightIsRefused(t *testing.T) {
	r := newRig(cfg(alice, bob))
	r.d.denied["uid:1002"] = true
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-bob", "approve", "", "m1"))
	if len(r.d.decided) != 0 {
		t.Fatalf("decided = %v", r.d.decided)
	}
	if last := r.p.posts[len(r.p.posts)-1]; !strings.Contains(last, "may not decide") {
		t.Fatalf("answer = %q", last)
	}
}

func TestPluginActorNeverDecidesFromChat(t *testing.T) {
	// Configuration refuses a plugin mapping; the handler refuses it too
	// when handed one, before anything is read or decided.
	plug := approvechat.Member{Identity: "idn-bot", Actor: "plugin:ops"}
	r := newRig(cfg(alice, plug))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-bot", "approve", "", "m1"))
	r.h.OnInbound(context.Background(), in("e2", "idn-bot", "", "👍", "m1"))
	if r.d.calls != 0 {
		t.Fatal("plugin actor decided")
	}
	if last := r.p.posts[len(r.p.posts)-1]; !strings.Contains(last, "plugin actor may not") {
		t.Fatalf("answer = %q", last)
	}
}

func TestClosedEventsEditTheAnnouncementWithoutPreview(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	op := heldOp()
	op.Status, op.Decision, op.DecidedBy = "queued", "approved", "uid:1001"
	r.d.ops["op1"] = op
	r.h.OnClosed(context.Background(), "op1")
	if len(r.p.edits) != 1 || !strings.Contains(r.p.edits[0], "approved by uid:1001") || strings.Contains(r.p.edits[0], "secret") {
		t.Fatalf("edits = %v", r.p.edits)
	}
	r.h.OnClosed(context.Background(), "op1")
	if len(r.p.edits) != 1 {
		t.Fatalf("closed twice edits = %v", r.p.edits)
	}
	// A reply to the closed announcement is no longer an announcement.
	r.h.OnInbound(context.Background(), in("e9", "idn-alice", "approve", "", "m1"))
	if r.d.calls != 0 {
		t.Fatal("decided from a closed announcement")
	}
}

func TestOtherConversationsAreIgnored(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	i := in("e1", "idn-alice", "approve #op1", "", "m1")
	i.Conversation = "elsewhere"
	r.h.OnInbound(context.Background(), i)
	if r.d.calls != 0 {
		t.Fatal("decided from an unconfigured conversation")
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	c := cfg(alice)
	c.Enabled = false
	r := newRig(c)
	r.h.OnHeld(context.Background(), "op1")
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve #op1", "", ""))
	if len(r.p.posts) != 0 || r.d.calls != 0 {
		t.Fatal("disabled handler acted")
	}
}

func TestBareReplyToTheAnnouncementDecides(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("m-reply-1", "idn-alice", "reject too risky", "", "m1"))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1001:op1:reject:too risky" {
		t.Fatalf("decided = %v", r.d.decided)
	}
}

// TestBareReplyElsewhereIsIgnored: the verb alone decides only as a
// reply to an announcement; a reply to any other message, or none, is
// conversation.
func TestBareReplyElsewhereIsIgnored(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "someone-elses-message"))
	r.h.OnInbound(context.Background(), in("e2", "idn-alice", "approve", "", ""))
	if r.d.calls != 0 {
		t.Fatal("a bare reply to another message decided")
	}
}

// TestBareReplyFromAnUnmappedIdentityIsIgnoredAndLogged: the right
// message and the right words, from nobody mapped.
func TestBareReplyFromAnUnmappedIdentityIsIgnoredAndLogged(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-mallory", "approve", "", "m1"))
	if r.d.calls != 0 {
		t.Fatal("an unmapped identity decided")
	}
	if !r.l.has("unmapped user ignored") {
		t.Fatal("the ignored reply was not logged")
	}
	if len(r.p.posts) != 1 {
		t.Fatalf("an unmapped sender got an answer: %v", r.p.posts)
	}
}

// TestReplyNamingAnotherOperationIsIgnored: a reply to the announcement
// of one operation decides that one alone.
func TestReplyNamingAnotherOperationIsIgnored(t *testing.T) {
	r := newRig(cfg(alice))
	other := heldOp()
	other.ID = "op2"
	r.d.ops["op2"] = other
	r.held(t)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve #op2", "", "m1"))
	if r.d.calls != 0 {
		t.Fatal("a reply to op1's announcement decided op2")
	}
	// Said outside a reply, the same words still decide op2.
	r.h.OnInbound(context.Background(), in("e2", "idn-alice", "approve #op2", "", ""))
	if len(r.d.decided) != 1 || !strings.Contains(r.d.decided[0], ":op2:") {
		t.Fatalf("decided = %v", r.d.decided)
	}
}

// TestOwnMessageNeverDecides: what heo or a system actor wrote is not a
// member's reply, even from a mapped identity.
func TestOwnMessageNeverDecides(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	i := in("e1", "idn-alice", "approve", "", "m1")
	i.Own = true
	r.h.OnInbound(context.Background(), i)
	if r.d.calls != 0 {
		t.Fatal("an own message decided")
	}
}

// TestAReplyDecidesOnce: the same message handled twice (a redelivery),
// or the same message after a decision, applies one decision.
func TestAReplyDecidesOnce(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	for range 3 {
		r.h.OnInbound(context.Background(), in("m-reply-1", "idn-alice", "approve", "", "m1"))
	}
	if r.d.calls != 1 || len(r.d.decided) != 1 {
		t.Fatalf("calls = %d, decided = %v", r.d.calls, r.d.decided)
	}
}

// TestReplyToAnAlreadyDecidedOperationAnswersPolitely: decided, but its
// announcement not yet closed.
func TestReplyToAnAlreadyDecidedOperationAnswersPolitely(t *testing.T) {
	r := newRig(cfg(alice))
	r.held(t)
	op := heldOp()
	op.Status, op.Decision, op.DecidedBy = "queued", "rejected", "uid:1001"
	r.d.ops["op1"] = op
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	if r.d.calls != 0 {
		t.Fatal("decided a decided operation")
	}
	if last := r.p.posts[len(r.p.posts)-1]; !strings.Contains(last, "already rejected") {
		t.Fatalf("answer = %q", last)
	}
}

// restarted builds a second handler over the first one's store: a
// daemon that restarted.
func (r rig) restarted(c approvechat.Config, st approvechat.Store) rig {
	n := newRig(c)
	n.d = r.d
	n.h = approvechat.New(c, r.d, r.d, n.p, n.l).WithStore(st)
	return n
}

func TestAnnouncementRefsSurviveARestart(t *testing.T) {
	st := approvechat.NewFileStore(t.TempDir())
	c := cfg(alice)
	r := newRig(c)
	r.h.WithStore(st)
	r.held(t)

	n := r.restarted(c, st)
	// Announced already: the held event redelivered announces nothing.
	n.h.OnHeld(context.Background(), "op1")
	if len(n.p.posts) != 0 {
		t.Fatalf("announced again after a restart: %v", n.p.posts)
	}
	// The old announcement is still edited when its operation closes.
	op := heldOp()
	op.Status, op.Decision, op.DecidedBy = "queued", "approved", "uid:1001"
	n.d.ops["op1"] = op
	n.h.OnClosed(context.Background(), "op1")
	if len(n.p.edits) != 1 || !strings.HasPrefix(n.p.edits[0], "m1|") || !strings.Contains(n.p.edits[0], "approved by uid:1001") {
		t.Fatalf("edits after restart = %v", n.p.edits)
	}
	// Closed once: a third start finds nothing to edit.
	again := r.restarted(c, st)
	again.h.OnClosed(context.Background(), "op1")
	if len(again.p.edits) != 0 {
		t.Fatalf("a closed announcement is edited again: %v", again.p.edits)
	}
}

func TestBareReplyWorksAfterARestartAndDecidesOnce(t *testing.T) {
	st := approvechat.NewFileStore(t.TempDir())
	c := cfg(alice)
	r := newRig(c)
	r.h.WithStore(st)
	r.held(t)
	r.h.OnInbound(context.Background(), in("m-reply-1", "idn-alice", "approve", "", "m1"))
	if len(r.d.decided) != 1 {
		t.Fatalf("decided = %v", r.d.decided)
	}
	// Redelivered to the restarted handler: already handled.
	n := r.restarted(c, st)
	n.h.OnInbound(context.Background(), in("m-reply-1", "idn-alice", "approve", "", "m1"))
	if n.d.calls != 1 {
		t.Fatalf("a handled reply decided again after a restart: calls = %d", n.d.calls)
	}
}

func TestBareReplyToAPreRestartAnnouncementDecides(t *testing.T) {
	st := approvechat.NewFileStore(t.TempDir())
	c := cfg(alice)
	r := newRig(c)
	r.h.WithStore(st)
	r.held(t)
	n := r.restarted(c, st)
	n.h.OnInbound(context.Background(), in("m-reply-2", "idn-alice", "approve", "", "m1"))
	if len(n.d.decided) != 1 {
		t.Fatalf("decided = %v", n.d.decided)
	}
}

func TestStoreOfAnotherConfigurationIsNotTrusted(t *testing.T) {
	st := approvechat.NewFileStore(t.TempDir())
	r := newRig(cfg(alice))
	r.h.WithStore(st)
	r.held(t)
	// Conversation no longer configured: its saved announcement is
	// dropped, so a reply to it decides nothing.
	other := approvechat.Config{Enabled: true, Conversations: []approvechat.Conversation{
		{Inbox: "in1", Conversation: "cv2", Members: []approvechat.Member{alice}}}}
	n := r.restarted(other, st)
	i := in("e1", "idn-alice", "approve", "", "m1")
	i.Conversation = "cv2"
	n.h.OnInbound(context.Background(), i)
	if n.d.calls != 0 {
		t.Fatal("a saved announcement of another conversation decided")
	}
}

func thread(i approvechat.Inbound) approvechat.Inbound { i.Thread = true; return i }

func slackCfg(on bool) approvechat.Config {
	c := cfg(alice)
	c.Conversations[0].SlackThreadReplies = on
	return c
}

// TestSlackThreadReplyDoesNotDecideByDefault: a thread reply, to the
// announcement or to heo's answer in its thread (both arrive as a reply
// to the root), decides nothing and says so in the log.
func TestSlackThreadReplyDoesNotDecideByDefault(t *testing.T) {
	r := newRig(slackCfg(false))
	r.held(t)
	r.h.OnInbound(context.Background(), thread(in("e1", "idn-alice", "approve", "", "m1")))
	if r.d.calls != 0 || len(r.p.posts) != 1 {
		t.Fatalf("a thread reply decided: calls = %d, posts = %v", r.d.calls, r.p.posts)
	}
	if !r.l.has("thread reply does not decide") {
		t.Fatal("not logged")
	}
	// The same reply as a quote reply (not a thread) still decides.
	r.h.OnInbound(context.Background(), in("e2", "idn-alice", "approve", "", "m1"))
	if len(r.d.decided) != 1 {
		t.Fatalf("decided = %v", r.d.decided)
	}
}

func TestSlackThreadReplyDecidesWithTheOption(t *testing.T) {
	r := newRig(slackCfg(true))
	r.held(t)
	r.h.OnInbound(context.Background(), thread(in("e1", "idn-alice", "reject nope", "", "m1")))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1001:op1:reject:nope" {
		t.Fatalf("decided = %v", r.d.decided)
	}
}

// TestExplicitApproveInASlackThreadStillWorks: the explicit form and a
// reaction decide with the option off.
func TestExplicitApproveInASlackThreadStillWorks(t *testing.T) {
	r := newRig(slackCfg(false))
	r.held(t)
	r.h.OnInbound(context.Background(), thread(in("e1", "idn-alice", "approve #op1 ok", "", "m1")))
	if len(r.d.decided) != 1 || r.d.decided[0] != "uid:1001:op1:approve:ok" {
		t.Fatalf("decided = %v", r.d.decided)
	}
	r2 := newRig(slackCfg(false))
	r2.held(t)
	r2.h.OnInbound(context.Background(), in("e2", "idn-alice", "", "👍", "m1"))
	if len(r2.d.decided) != 1 {
		t.Fatalf("reaction decided = %v", r2.d.decided)
	}
}

func closeOp(r rig, decision string) {
	op := heldOp()
	op.Status, op.Decision, op.DecidedBy = "queued", decision, "uid:1001"
	if decision == "expired" {
		op.Status, op.DecidedBy = "expired", ""
	}
	r.d.ops["op1"] = op
	r.h.OnClosed(context.Background(), "op1")
}

// TestReplyAfterCloseIsLoggedAndIgnored: nothing is posted, nothing
// decided, whatever the reply says, for a decision and for an expiry.
func TestReplyAfterCloseIsLoggedAndIgnored(t *testing.T) {
	for decision, want := range map[string]string{"approved": "already decided", "expired": "expired"} {
		t.Run(decision, func(t *testing.T) {
			r := newRig(cfg(alice))
			r.held(t)
			closeOp(r, decision)
			posts := len(r.p.posts)
			for i, text := range []string{"approve", "reject no", "approve #op1"} {
				r.h.OnInbound(context.Background(), in(fmt.Sprintf("late%d", i), "idn-alice", text, "", "m1"))
			}
			r.h.OnInbound(context.Background(), in("late-r", "idn-alice", "", "👍", "m1"))
			if r.d.calls != 0 || len(r.p.posts) != posts {
				t.Fatalf("calls = %d, posts = %v", r.d.calls, r.p.posts)
			}
			if !r.l.has(want+"; reply ignored") || !r.l.has("op1") || !r.l.has("late0") {
				t.Fatalf("log = %v", r.l.info)
			}
			// Chatter in a reply to it is not worth a line.
			n := len(r.l.info)
			r.h.OnInbound(context.Background(), in("chat", "idn-alice", "thanks", "", "m1"))
			if len(r.l.info) != n {
				t.Fatalf("chatter logged: %v", r.l.info[n:])
			}
		})
	}
}

func TestClosedRefsSurviveARestart(t *testing.T) {
	st := approvechat.NewFileStore(t.TempDir())
	c := cfg(alice)
	r := newRig(c)
	r.h.WithStore(st)
	r.held(t)
	closeOp(r, "rejected")
	n := r.restarted(c, st)
	n.h.OnInbound(context.Background(), in("late", "idn-alice", "approve", "", "m1"))
	if n.d.calls != 0 || len(n.p.posts) != 0 || !n.l.has("already decided; reply ignored") {
		t.Fatalf("calls = %d, posts = %v, log = %v", n.d.calls, n.p.posts, n.l.info)
	}
	// Closing again edits nothing: the closed ref is not an open one.
	n.h.OnClosed(context.Background(), "op1")
	if len(n.p.edits) != 0 {
		t.Fatalf("edits = %v", n.p.edits)
	}
}

// TestClosedRefsArePrunedAfterRetention: past retention a reply is
// plainly not an answer to anything, in memory and on load, and the
// saved file no longer holds the ref.
func TestClosedRefsArePrunedAfterRetention(t *testing.T) {
	dir := t.TempDir()
	st := approvechat.NewFileStore(dir)
	c := cfg(alice)
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := newRig(c)
	r.h.WithClock(clock).WithStore(st)
	r.held(t)
	closeOp(r, "approved")

	now = now.Add(approvechat.ClosedRetention - time.Hour)
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	if !r.l.has("already decided") {
		t.Fatal("a closed ref inside retention is forgotten")
	}
	// A restart inside retention keeps it.
	kept := r.restarted(c, st)
	kept.h.WithClock(clock)
	if s, _ := st.Load(); len(s.Announcements) != 1 {
		t.Fatalf("saved = %+v", s)
	}

	now = now.Add(2 * time.Hour)
	r.h.OnInbound(context.Background(), in("e2", "idn-alice", "approve", "", "m1"))
	if r.l.has("e2") {
		t.Fatalf("a closed ref past retention still answers: %v", r.l.info)
	}
	// Load drops it; a later save leaves it out of the file.
	late := newRig(c)
	late.h = approvechat.New(c, r.d, r.d, late.p, late.l).WithClock(clock).WithStore(st)
	late.h.OnInbound(context.Background(), in("e3", "idn-alice", "approve", "", "m1"))
	if late.l.has("e3") || late.d.calls != 0 {
		t.Fatalf("pruned on load failed: %v", late.l.info)
	}
	late.h.OnHeld(context.Background(), "op1") // announces again, saves
	if s, _ := st.Load(); len(s.Announcements) != 1 || s.Announcements[0].Closed != "" {
		t.Fatalf("saved = %+v, want the new open ref only", s)
	}
}

// TestOldFormatStateLoads: a file of open announcements only, as before
// closed refs existed.
func TestOldFormatStateLoads(t *testing.T) {
	dir := t.TempDir()
	old := `{"announcements":[{"conversation":"in1/cv1","message":"m1","operation":"op1"}],"handled":["e0"]}`
	if err := os.WriteFile(filepath.Join(dir, "approval-chat.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newRig(cfg(alice))
	r.h.WithStore(approvechat.NewFileStore(dir))
	r.h.OnInbound(context.Background(), in("e1", "idn-alice", "approve", "", "m1"))
	if len(r.d.decided) != 1 {
		t.Fatalf("an old-format open ref does not decide: %v", r.l.info)
	}
	r.h.OnInbound(context.Background(), in("e0", "idn-alice", "approve", "", "m1"))
	if r.d.calls != 1 {
		t.Fatal("an old-format handled event decided again")
	}
}
