package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

// These tests run the modal verbs' desk against the real slack.chat
// adapter in heo's runtime, with the workers at their default poll (a
// second): the interaction is a signed press on the channel's webhook,
// the effect is queued through the desk, and the workers' call reaches the
// in-process Slack (slackfake_test.go). Nothing reaches a network.

const (
	modalView = `{"type":"modal","external_id":"deploy-42","title":{"type":"plain_text","text":"Approve"},"submit":{"type":"plain_text","text":"Go"},` +
		`"blocks":[{"type":"input","block_id":"why","label":{"type":"plain_text","text":"Why"},"element":{"type":"plain_text_input","action_id":"why"}}]}`
	modalActor = model.ActorID("uid:1000")
)

// modalSite is a Slack site with workers at the default poll and one room
// conversation.
type modalSite struct {
	*slackSite
	conv model.Conversation
	n    int
}

func newModalSite(t *testing.T) *modalSite {
	t.Helper()
	// The workers' default poll is the thing under test: the rig sets a
	// short one for the other tests.
	s := newSlackSiteTuned(t, fullSlackCreds(t), func(r *rig) { r.v.Set(daemon.KeyWorkersPoll, daemon.DefaultWorkersPoll.String()) }, daemon.NameWorkers)
	if code, body := s.event(slackMessage("Ev1", slackPerson, "deploy 42 is ready", "1760000100.000100")); code != http.StatusOK {
		t.Fatalf("inbound = %d %q", code, body)
	}
	convs := s.conversations()
	if len(convs) != 1 {
		t.Fatalf("conversations = %+v", convs)
	}
	return &modalSite{slackSite: s, conv: convs[0]}
}

// press delivers a signed button press that Slack sent at the given time,
// with trigger as its trigger_id (none when empty), and returns the
// interaction uma recorded.
func (s *modalSite) press(trigger string, at time.Time) model.Interaction {
	s.t.Helper()
	s.n++
	p := map[string]any{
		"type": "block_actions", "api_app_id": "A0000000001",
		"team": map[string]any{"id": slackTeam}, "user": map[string]any{"id": slackPerson, "team_id": slackTeam},
		"container": map[string]any{"type": "message", "message_ts": "1760000100.000100", "channel_id": slackRoom},
		"channel":   map[string]any{"id": slackRoom}, "message": map[string]any{"type": "message", "ts": "1760000100.000100"},
		"response_url": "https://hooks.slack.com/actions/T0000000001/397700885554/96rGlfmibIGlgcZRskXaIFfN",
		"actions":      []any{map[string]any{"action_id": "approve", "type": "button", "value": fmt.Sprint("v", s.n), "action_ts": fmt.Sprintf("1760000201.%06d", s.n)}},
	}
	if trigger != "" {
		p["trigger_id"] = trigger
	}
	b, _ := json.Marshal(p)
	before := len(s.interactions())
	if code, body := s.post("application/x-www-form-urlencoded", url.Values{"payload": {string(b)}}.Encode(), slackSecret, at); code != http.StatusOK || body != "" {
		s.t.Fatalf("press = %d %q", code, body)
	}
	ins := s.interactions()
	if len(ins) != before+1 {
		s.t.Fatalf("interactions = %d after a press, had %d", len(ins), before)
	}
	return ins[len(ins)-1]
}

// withDesk runs fn with the desk of actor, wired as the daemon's commands
// wire it: the registry's own adapter, effects and offline validation,
// and the runtime's nudge.
func (s *modalSite) withDesk(fn func(desk.Service)) {
	s.t.Helper()
	err := s.r.host.Use(s.t.Context(), func(rt daemon.Runtime) error {
		client, err := rt.Engine.For(model.Principal{Tenant: s.r.tenant, Actor: modalActor})
		if err != nil {
			return err
		}
		slackAdapter, _ := rt.Adapters.Lookup(model.TypeSlackChat)
		fn(desk.Service{
			Client: client, Tenant: s.r.tenant, Actor: modalActor,
			Sees:    func(model.Audience) bool { return true },
			Permits: func(model.ActorID, string) bool { return true },
			Adapter: func(context.Context, model.ChannelRef) (adapter.Descriptor, error) {
				return slackAdapter.Factory.Describe(), nil
			},
			Effects: func(context.Context, model.ChannelRef) ([]adapter.ExtensionDefinition, error) {
				return slackAdapter.Extensions, nil
			},
			ValidateAction: func(_ context.Context, _ model.ChannelRef, aud model.Audience, a model.Extension) ([]model.ValidationIssue, error) {
				return slackAdapter.Factory.(adapter.ActionValidator).ValidateAction(aud, a), nil
			},
			Kick: rt.Nudge,
		})
		return nil
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// open queues an open or push in answer to in and returns its acceptance.
func (s *modalSite) open(effect string, in model.Interaction, view, key string) (acc model.Acceptance, err error) {
	s.t.Helper()
	s.withDesk(func(d desk.Service) {
		var p desk.ModalPlan
		if p, err = d.PlanModal(s.t.Context(), effect, string(in.Ref.ID), "", json.RawMessage(view), model.OperationID(key), channels.Submit); err != nil {
			return
		}
		acc, err = d.SubmitModal(s.t.Context(), p)
	})
	return acc, err
}

// update queues an update of the view ref names.
func (s *modalSite) update(ref desk.ViewRef, view, key string) (acc model.Acceptance, err error) {
	s.t.Helper()
	s.withDesk(func(d desk.Service) {
		var p desk.ModalPlan
		if p, err = d.PlanViewUpdate(s.t.Context(), string(s.inbox.ID), ref, json.RawMessage(view), model.OperationID(key), channels.Submit); err != nil {
			return
		}
		acc, err = d.SubmitModal(s.t.Context(), p)
	})
	return acc, err
}

// viewOf is the Slack view ID an operation reports as a provider
// reference, "" when it reports none.
func viewOf(op model.Operation) string {
	for _, r := range op.ProviderReferences {
		if r.Namespace == slack.Namespace && r.Kind == slack.RefKindView {
			return r.ID
		}
	}
	return ""
}

// TestSlackModalVerbs: a modal is opened from a recorded press through
// heo's desk, the workers and the real adapter: views.open carries the
// press's own trigger_id and the view with the bot token, the opened
// view's ID comes back as a provider reference of the operation, and an
// update by that ID, an update by the view's external_id and a push
// each reach their method. A retry under the same key sends nothing
// again. A press whose trigger_id has stopped working, a redacted press
// and a press with no trigger_id are refused before anything is queued;
// an attempt that reaches the adapter late is refused there, with no
// call; and a lost answer leaves the operation unknown and is never sent
// again.
func TestSlackModalVerbs(t *testing.T) {
	s := newModalSite(t)
	api := s.api

	// Open.
	in := s.press("398738663015.47445629121.open", time.Now())
	acc, err := s.open(slack.ExtensionViewsOpen, in, modalView, "open-1")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	op := s.settled(acc)
	if op.State != model.ProviderAccepted {
		t.Fatalf("open = %s %+v", op.State, op.Failure)
	}
	calls := api.called("views.open")
	if len(calls) != 1 || calls[0].Auth != "Bearer "+slackToken || calls[0].Body["trigger_id"] != "398738663015.47445629121.open" {
		t.Fatalf("views.open sent %+v, want one call with the press's trigger_id and the bot token", calls)
	}
	if v, _ := calls[0].Body["view"].(map[string]any); v["type"] != "modal" || v["external_id"] != "deploy-42" {
		t.Errorf("views.open carried view %v", calls[0].Body["view"])
	}
	viewID := viewOf(op)
	if !strings.HasPrefix(viewID, "V") {
		t.Fatalf("the open reports provider references %+v, want the view's ID", op.ProviderReferences)
	}

	// A retry under the same key is the same operation and sends nothing.
	again, err := s.open(slack.ExtensionViewsOpen, in, modalView, "open-1")
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retry = %+v, %v; want the same operation, replayed", again, err)
	}
	time.Sleep(1500 * time.Millisecond) // a worker poll and a half
	if n := len(api.called("views.open")); n != 1 {
		t.Fatalf("views.open called %d times after a replay", n)
	}

	// Update by the view ID the open reported, then by external_id.
	next := strings.Replace(modalView, "Approve", "Approved", 1)
	up, err := s.update(desk.ViewRef{ViewID: viewID, Hash: "156772938.1827394"}, next, "update-1")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if op := s.settled(up); op.State != model.ProviderAccepted {
		t.Fatalf("update = %s %+v", op.State, op.Failure)
	}
	byID := api.called("views.update")
	if len(byID) != 1 || byID[0].Body["view_id"] != viewID || byID[0].Body["hash"] != "156772938.1827394" || byID[0].Body["external_id"] != nil {
		t.Fatalf("views.update sent %+v, want the view ID and the hash", byID)
	}
	up2, err := s.update(desk.ViewRef{ExternalID: "deploy-42"}, next, "update-2")
	if err != nil {
		t.Fatalf("update by external_id: %v", err)
	}
	if op := s.settled(up2); op.State != model.ProviderAccepted {
		t.Fatalf("update by external_id = %s %+v", op.State, op.Failure)
	}
	if byExt := api.called("views.update"); len(byExt) != 2 || byExt[1].Body["external_id"] != "deploy-42" || byExt[1].Body["view_id"] != nil {
		t.Fatalf("views.update by external_id sent %+v", byExt)
	}
	// Slack does not know the view: the operation ends rejected, not found.
	gone, err := s.update(desk.ViewRef{ViewID: "V9999999999"}, next, "update-3")
	if err != nil {
		t.Fatalf("update of an unknown view: %v", err)
	}
	if op := s.settled(gone); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.NotFound {
		t.Fatalf("update of an unknown view = %s %+v, want rejected, not found", op.State, op.Failure)
	}

	// Push, on a press of the modal's own.
	inner := s.press("398738663015.47445629121.push", time.Now())
	pushed, err := s.open(slack.ExtensionViewsPush, inner, modalView, "push-1")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if op := s.settled(pushed); op.State != model.ProviderAccepted || !strings.HasPrefix(viewOf(op), "V") {
		t.Fatalf("push = %s %+v %+v", op.State, op.Failure, op.ProviderReferences)
	}
	if push := api.called("views.push"); len(push) != 1 || push[0].Body["trigger_id"] != "398738663015.47445629121.push" {
		t.Fatalf("views.push sent %+v", push)
	}

	// Refused before anything is queued: a press from the past, a redacted
	// press, a press with no trigger_id.
	calls0, ops0 := api.total(), len(s.operations())
	stale := s.press("398738663015.47445629121.old1", time.Now().Add(-time.Minute))
	_, err = s.open(slack.ExtensionViewsOpen, stale, modalView, "open-stale")
	if !model.IsCode(err, model.DeadlineExpired) {
		t.Fatalf("a press from a minute ago: %v, want deadline_expired", err)
	}
	red := s.press("398738663015.47445629121.redd", time.Now())
	if _, err := s.r.client().Privacy().Redact(t.Context(), model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "test"}, Operation: "redact", ExpectedRevision: red.Revision},
		Target:  red.Ref.Ref(), Interaction: &red.Ref, Reason: "test",
	}); err != nil {
		t.Fatalf("redact: %v", err)
	}
	_, err = s.open(slack.ExtensionViewsOpen, red, modalView, "open-redacted")
	if !model.IsCode(err, model.Conflict) {
		t.Fatalf("a redacted press: %v, want conflict", err)
	}
	bare := s.press("", time.Now())
	_, err = s.open(slack.ExtensionViewsOpen, bare, modalView, "open-bare")
	if !model.IsCode(err, model.Validation) || !strings.Contains(err.Error(), "trigger_id") {
		t.Fatalf("a press with no trigger_id: %v, want a validation error naming it", err)
	}
	if api.total() != calls0 || len(s.operations()) != ops0 {
		t.Errorf("refused attempts made %d calls and queued %d operations", api.total()-calls0, len(s.operations())-ops0)
	}

	// An attempt that reaches the adapter after expires_at is refused
	// there, with no call: submitted past heo's own check, as a worker
	// delayed past the window would find it.
	late := s.press("398738663015.47445629121.late", time.Now())
	data, _ := json.Marshal(map[string]any{
		"trigger_id": "398738663015.47445629121.late", "expires_at": time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano),
		"view": json.RawMessage(modalView),
	})
	lateAcc, err := s.r.client().Extensions().Execute(t.Context(), model.ExecuteExtension{
		Command: s.r.cmd("late"), Target: late.Ref.Conversation.Ref(), Audience: model.Audience{Visibility: model.Internal},
		Action: model.Extension{Name: slack.ExtensionViewsOpen, Version: slack.ExtensionVersion, Data: data},
	})
	if err != nil {
		t.Fatalf("late attempt: %v", err)
	}
	opens := len(api.called("views.open"))
	if op := s.settled(lateAcc); op.State != model.Rejected || op.Failure == nil || op.Failure.Code != model.DeadlineExpired {
		t.Fatalf("a late attempt = %s %+v, want rejected, deadline_expired", op.State, op.Failure)
	}
	if len(api.called("views.open")) != opens {
		t.Error("a late attempt called Slack")
	}

	// A lost answer: the request was written and Slack's answer never came.
	// The trigger_id may be spent, so it is never sent again.
	lost := s.press("398738663015.47445629121.lost", time.Now())
	api.loseAnswer("views.open")
	lostAcc, err := s.open(slack.ExtensionViewsOpen, lost, modalView, "open-lost")
	if err != nil {
		t.Fatalf("open with a lost answer: %v", err)
	}
	if op := s.waitOperation(lostAcc.Operation.Ref, model.Unknown, model.Failed, model.Rejected, model.ProviderAccepted); op.State != model.Unknown {
		t.Fatalf("a lost answer = %s %+v, want unknown", op.State, op.Failure)
	}
	sent := len(api.called("views.open"))
	api.loseAnswer("")
	time.Sleep(3500 * time.Millisecond) // several polls, and the retry delays of a transient failure
	if got := len(api.called("views.open")); got != sent {
		t.Errorf("a lost answer was sent again: %d calls, had %d", got, sent)
	}
	if op := s.r.operation(lostAcc.Operation.Ref); op.State != model.Unknown {
		t.Errorf("the operation moved to %s after the lost answer", op.State)
	}
}

// operations lists the extension operations queued.
func (s *modalSite) operations() []model.Operation {
	s.t.Helper()
	var out []model.Operation
	req := model.ListRequest{Call: model.Call{Correlation: "test"}, Page: model.PageRequest{Limit: 100}}
	for {
		p, err := s.r.client().Operations().List(s.t.Context(), req)
		if err != nil {
			s.t.Fatal(err)
		}
		for _, op := range p.Items {
			if op.Key != nil && op.Key.Kind == model.ActionExtensionExecute {
				out = append(out, op)
			}
		}
		if p.Complete || p.Next == "" {
			return out
		}
		req.Page.After = p.Next
	}
}

// TestSlackModalBudget: the three seconds of a trigger_id are also spent in
// heo, and the workers' idle poll is a second. Submitting an open wakes the
// workers, so the time from the submission to the call to Slack is the
// adapter's own, not the poll's: every one of these, at every phase of the
// poll, reaches Slack in a fraction of it. Without the wake the same
// submissions spread evenly over the second.
func TestSlackModalBudget(t *testing.T) {
	s := newModalSite(t)
	var lat []time.Duration
	for i := 0; i < 10; i++ {
		time.Sleep(time.Duration(97*(i+1)) % 1000 * time.Millisecond) // vary the phase against the poll
		in := s.press(fmt.Sprintf("1337.4242.t%d", i), time.Now())
		start := time.Now()
		acc, err := s.open(slack.ExtensionViewsOpen, in, modalView, fmt.Sprintf("budget-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if op := s.settled(acc); op.State != model.ProviderAccepted {
			t.Fatalf("open %d = %s %+v", i, op.State, op.Failure)
		}
		calls := s.api.called("views.open")
		if len(calls) != i+1 {
			t.Fatalf("views.open calls = %d after %d opens", len(calls), i+1)
		}
		lat = append(lat, calls[i].At.Sub(start))
	}
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	t.Logf("poll %s: submit to views.open min %s median %s max %s", daemon.DefaultWorkersPoll, lat[0], lat[len(lat)/2], lat[len(lat)-1])
	if worst := lat[len(lat)-1]; worst > daemon.DefaultWorkersPoll/2 {
		t.Errorf("the slowest open reached Slack %s after it was submitted, with the poll at %s: the workers were not woken", worst, daemon.DefaultWorkersPoll)
	}
}

// TestSlackModalUnwokenWorkersWaitOutThePoll documents why the wake
// exists: effects queued without it wait for the workers' poll.
func TestSlackModalUnwokenWorkersWaitOutThePoll(t *testing.T) {
	s := newModalSite(t)
	var worst time.Duration
	for i := 0; i < 6; i++ {
		time.Sleep(time.Duration(173*(i+1)) % 1000 * time.Millisecond)
		start := time.Now()
		data, _ := json.Marshal(map[string]any{
			"trigger_id": fmt.Sprintf("1337.4242.q%d", i), "expires_at": start.Add(slack.TriggerLifetime).UTC().Format(time.RFC3339Nano),
			"view": json.RawMessage(modalView),
		})
		acc, err := s.r.client().Extensions().Execute(t.Context(), model.ExecuteExtension{
			Command: s.r.cmd(fmt.Sprintf("quiet-%d", i)), Target: s.conv.Ref.Ref(), Audience: model.Audience{Visibility: model.Internal},
			Action: model.Extension{Name: slack.ExtensionViewsOpen, Version: slack.ExtensionVersion, Data: data},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.settled(acc)
		calls := s.api.called("views.open")
		worst = max(worst, calls[len(calls)-1].At.Sub(start))
	}
	t.Logf("without a wake: slowest %s of poll %s", worst, daemon.DefaultWorkersPoll)
	if worst < daemon.DefaultWorkersPoll/4 {
		t.Errorf("effects queued without a wake all reached Slack within %s: the budget test proves nothing", worst)
	}
}
