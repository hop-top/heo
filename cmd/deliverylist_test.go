package cmd

import (
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

func TestReconcileReportsWhatUmaHolds(t *testing.T) {
	a := newApp()
	next := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	unknown := model.Operation{
		Ref: model.OperationRef{ID: "op_9"}, State: model.Unknown, Revision: "r2", NextAttemptAt: &next,
		Failure:            model.NewError(model.OutcomeUnknown, "unknown"),
		ProviderReferences: []model.ExternalRef{{Kind: "message", ID: "m-1"}},
	}
	plan := func(op model.Operation, r *desk.Reconciler) desk.ReconcilePlan {
		return desk.ReconcilePlan{Operation: op, Reconciler: r}
	}
	reconciles := &desk.Reconciler{Adapter: "mail.imap", Reconciles: true, Settle: 90 * time.Second}

	// The adapter answered and settled it.
	got := unknown
	got.State, got.Revision, got.NextAttemptAt = model.Delivered, "r3", nil
	got.ProviderReferences = []model.ExternalRef{{Kind: "message", ID: "m-2"}}
	out := a.reconcileOf(plan(unknown, reconciles), got)
	if out.Reconciliation != reconcileSettled || out.State != model.Delivered || out.PreviousState != model.Unknown ||
		out.Adapter == nil || !out.Adapter.Reconciles || out.Revision != "r3" || !strings.Contains(out.Next, "nothing to do") {
		t.Fatalf("settled = %+v", out)
	}

	// Failed for good: settled, and says to send it again.
	got.State, got.Failure = model.Failed, model.NewError(model.Permanent, "mailbox gone")
	out = a.reconcileOf(plan(unknown, reconciles), got)
	if out.Reconciliation != reconcileSettled || out.State != model.Failed || !strings.Contains(out.Next, "new --idempotency-key") {
		t.Fatalf("failed = %+v", out)
	}

	// No record, past the settle window: not applied, sent again then.
	got = unknown
	got.State = model.RetryPending
	out = a.reconcileOf(plan(unknown, reconciles), got)
	if out.Reconciliation != reconcileNotApplied || out.NextAttemptAt == nil ||
		!strings.Contains(out.Next, "not applied") || !strings.Contains(out.Next, "2026-10-12T09:30:00Z") {
		t.Fatalf("retry_pending = %+v", out)
	}

	// An answer that proved nothing: still unknown, the next attempt shown.
	out = a.reconcileOf(plan(unknown, reconciles), unknown)
	if out.Reconciliation != reconcilePending || out.State != model.Unknown || out.Adapter == nil || out.Adapter.Settle != "1m30s" {
		t.Fatalf("pending = %+v", out)
	}
	for _, want := range []string{"proved nothing", "2026-10-12T09:30:00Z", "delivery reconcile op_9", "1m30s", "stays outcome_unknown"} {
		if !strings.Contains(out.Next, want) {
			t.Errorf("next lacks %q:\n%s", want, out.Next)
		}
	}
	// With nothing scheduled it says so rather than invent a time.
	unscheduled := unknown
	unscheduled.NextAttemptAt = nil
	out = a.reconcileOf(plan(unscheduled, nil), unscheduled)
	if out.Reconciliation != reconcilePending || out.NextAttemptAt != nil || out.Adapter != nil ||
		!strings.Contains(out.Next, "no attempt scheduled") {
		t.Fatalf("unscheduled = %+v", out)
	}

	// Any other state is not asked, and says what to do.
	for st, verdict := range map[model.DeliveryState]string{
		model.Queued: "in progress", model.Delivered: "nothing to do", model.Failed: "final",
		model.DeadLettered: "delivery retry op_9", model.RetryPending: "uma retries",
	} {
		op := model.Operation{Ref: model.OperationRef{ID: "op_9"}, State: st}
		out = a.reconcileOf(plan(op, nil), op)
		if out.Reconciliation != reconcileNotNeeded || out.State != st || !strings.Contains(out.Next, verdict) {
			t.Errorf("%s = %+v, want not_needed with %q", st, out, verdict)
		}
	}
}

func TestReconcilePlanAsksNothing(t *testing.T) {
	a := newApp()
	unknown := model.Operation{Ref: model.OperationRef{ID: "op_9"}, State: model.Unknown}

	p := a.reconcilePlan(desk.ReconcilePlan{Operation: unknown,
		Reconciler: &desk.Reconciler{Adapter: "mail.imap", Reconciles: true, Settle: time.Minute}})
	if len(p.Effects) != 1 || p.Effects[0].Target != "operation:op_9" || !strings.Contains(p.Effects[0].Detail, "mail.imap") ||
		!strings.Contains(p.Effects[0].Detail, "nothing is sent") || len(p.Warnings) != 1 {
		t.Fatalf("plan = %+v", p)
	}
	// An adapter that declares no reconcile: uma would refuse; no effect.
	p = a.reconcilePlan(desk.ReconcilePlan{Operation: unknown, Reconciler: &desk.Reconciler{Adapter: "widget"}})
	if len(p.Effects) != 0 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "UNSUPPORTED") {
		t.Fatalf("plan without a reconciler = %+v", p)
	}
	// A delivery that is not outcome_unknown: nothing to run.
	p = a.reconcilePlan(desk.ReconcilePlan{Operation: model.Operation{Ref: model.OperationRef{ID: "op_9"}, State: model.Delivered}})
	if p.Effects == nil || len(p.Effects) != 0 || len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "nothing to reconcile") {
		t.Fatalf("plan of a delivered one = %+v", p)
	}
}

func TestDeliveryRowShowsWhereAWriteWent(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	conv := model.ConversationRef{Inbox: model.InboxRef{Tenant: "acme", ID: "in_1"}, ID: "conv_1"}

	// A reply: the conversation and inbox are the target's.
	reply := model.Operation{
		Ref: model.OperationRef{ID: "op_1"}, State: model.Queued, Revision: "r1", UpdatedAt: at,
		Key: &model.OperationKey{Caller: "uid:1", Kind: model.ActionMessageReply, Target: conv.Ref(), ID: "k1"},
	}
	r := deliveryRowOf(reply)
	if r.Operation != "op_1" || r.Conversation != "conv_1" || r.Inbox != "in_1" || r.Verdict != "wait" || r.Key != "k1" ||
		r.Kind != "message.reply" || r.Updated != "2026-10-01T09:00:00Z" {
		t.Errorf("reply row = %+v", r)
	}

	// A start: the target is the inbox; the conversation is the result's.
	start := model.Operation{
		Ref: model.OperationRef{ID: "op_2"}, State: model.Unknown, UpdatedAt: at,
		Key: &model.OperationKey{Caller: "uid:1", Kind: model.ActionConversationStart,
			Target: model.InboxRef{Tenant: "acme", ID: "in_1"}.Ref(), ID: "k2"},
		ResultMessage: &model.MessageRef{Conversation: model.ConversationRef{Inbox: model.InboxRef{ID: "in_1"}, ID: "conv_7"}, ID: "m_1"},
	}
	r = deliveryRowOf(start)
	if r.Conversation != "conv_7" || r.Inbox != "in_1" || r.Verdict != "reconcile" {
		t.Errorf("start row = %+v", r)
	}
	start.ResultMessage = nil
	if r = deliveryRowOf(start); r.Conversation != "-" || r.Inbox != "in_1" {
		t.Errorf("start row without a message = %+v", r)
	}
}

func TestOperationRowShowsWhatRecoversIt(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	op := model.Operation{
		Ref: model.OperationRef{ID: "op_3"}, State: model.Delivered, UpdatedAt: at,
		Key: &model.OperationKey{Caller: "uid:1", Kind: model.ActionMessageReply,
			Target: model.ConversationRef{Inbox: model.InboxRef{Tenant: "acme", ID: "in_1"}, ID: "conv_9"}.Ref(), ID: "send-42"},
	}
	r := operationRowOf(op)
	want := operationRow{Key: "send-42", Kind: "message.reply", Target: "conversation:in_1/conv_9", State: "delivered",
		Operation: "op_3", Updated: "2026-10-01T09:00:00Z"}
	if r != want {
		t.Errorf("row = %+v, want %+v", r, want)
	}
	// The target spells as --target takes it, to recover the operation.
	if _, err := parseTarget("acme", r.Target); err != nil {
		t.Errorf("target %q is not a --target: %v", r.Target, err)
	}
	// An operation shown without a key (none, from uma) is a row without one.
	if r = operationRowOf(model.Operation{Ref: model.OperationRef{ID: "op_4"}, State: model.Queued}); r.Key != "" || r.Kind != "" || r.Operation != "op_4" {
		t.Errorf("row without a key = %+v", r)
	}
	if o := operationDetail(op); o.Key != "send-42" || o.Kind != "message.reply" || o.Target != "conversation:in_1/conv_9" {
		t.Errorf("detail = %+v", o)
	}
}
