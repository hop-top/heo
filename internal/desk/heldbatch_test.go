package desk_test

import (
	"fmt"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// holdClass holds a reply the plugin writes to conv, proposed as the
// trust ladder does: the trust key and labels of verb:class in the
// conversation's inbox, held for reason.
func holdClass(t *testing.T, w *world, conv model.Conversation, id, verb, class, reason string) model.Operation {
	t.Helper()
	s := w.as(plugin)
	p, err := s.PlanReply(t.Context(), string(conv.Ref.ID), "hi "+id, model.OperationID(id))
	if err != nil {
		t.Fatal(err)
	}
	key, labels := desk.TrustKey(plugin, "", string(conv.Ref.Inbox.ID), verb, class)
	p.Request.Hold = &model.HoldProposal{Reason: reason, TrustKey: key, TrustLabels: labels,
		Preview: &model.Extension{Name: desk.PreviewExtension, Version: 1, Data: []byte(`{}`)}}
	acc, err := s.Reply(t.Context(), p)
	if err != nil || acc.Operation.State != model.Held {
		t.Fatalf("hold %s: %v, %v", id, acc.Operation.State, err)
	}
	return acc.Operation
}

const training = "training: trash 0 of 5 clean approvals"

// batchWorld is a world with three conversations in the local inbox, a
// trash held for each, and a fourth in the provider inbox.
func batchWorld(t *testing.T) (*world, []model.Operation) {
	w := newWorld(t)
	var ops []model.Operation
	for i, who := range []string{"ada", "carol", "dave"} {
		conv := w.local
		if who != "ada" {
			conv = w.start(w.local.Ref.Inbox, who)
		}
		ops = append(ops, holdClass(t, w, conv, fmt.Sprintf("b-%d", i), "state", "trash", training))
	}
	return w, ops
}

func trustOf(t *testing.T, w *world, conv model.Conversation, op model.Operation) model.TrustEntry {
	t.Helper()
	in, err := w.as(plugin).FindInbox(t.Context(), string(conv.Ref.Inbox.ID))
	if err != nil {
		t.Fatal(err)
	}
	e, err := w.client(service).Operations().Trust(t.Context(), model.TrustLookup{Call: call, Channel: in.Channel, Key: op.Held.TrustKey})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func sel(w *world) desk.HeldSelector {
	return desk.HeldSelector{Class: "trash", Inbox: string(w.local.Ref.Inbox.ID)}
}

func TestSelectHeldMatchesClassAndMailbox(t *testing.T) {
	w, ops := batchWorld(t)
	// Another class in the same mailbox and the same class in another
	// mailbox are not matched.
	holdClass(t, w, w.start(w.local.Ref.Inbox, "erin"), "o-1", "state", "archive", "training: archive 0 of 3 clean approvals")
	holdClass(t, w, w.remote, "o-2", "state", "trash", training)

	got, err := w.as(agent).SelectHeld(t.Context(), sel(w), desk.BatchRules{}, desk.DefaultBatch)
	if err != nil || len(got.Items) != len(ops) || got.More || len(got.Skipped) != 0 {
		t.Fatalf("select = %+v, %v; want the %d trashes of the local inbox", got, err, len(ops))
	}
	want := map[string]bool{}
	for _, o := range ops {
		want[string(o.Ref.ID)] = true
	}
	for _, h := range got.Items {
		if !want[h.ID] {
			t.Errorf("matched %s: not a trash of the local inbox", h.ID)
		}
	}
	// A proposer narrows; one that proposed nothing matches nothing.
	if got, err := w.as(agent).SelectHeld(t.Context(), desk.HeldSelector{Class: "trash", Inbox: sel(w).Inbox, Proposer: string(plugin2)}, desk.BatchRules{}, 10); err != nil || len(got.Items) != 0 {
		t.Errorf("proposer plugin2 = %+v, %v", got, err)
	}
	if got, err := w.as(agent).SelectHeld(t.Context(), desk.HeldSelector{Class: "trash", Inbox: sel(w).Inbox, Proposer: string(plugin)}, desk.BatchRules{}, 10); err != nil || len(got.Items) != len(ops) {
		t.Errorf("proposer plugin = %+v, %v", got, err)
	}
}

func TestSelectHeldRefusesEmptyAndPlugin(t *testing.T) {
	w, _ := batchWorld(t)
	s := w.as(agent)
	for _, f := range []desk.HeldSelector{{}, {Class: "trash"}, {Inbox: sel(w).Inbox}, {Class: " ", Inbox: sel(w).Inbox}, {Proposer: string(plugin)}} {
		_, err := s.SelectHeld(t.Context(), f, desk.BatchRules{}, 10)
		wantCode(t, err, model.Validation, "selector")
	}
	for _, max := range []int{0, -1, desk.MaxBatch + 1} {
		_, err := s.SelectHeld(t.Context(), sel(w), desk.BatchRules{}, max)
		wantCode(t, err, model.Validation, "max")
	}
	_, err := w.as(plugin).SelectHeld(t.Context(), sel(w), desk.BatchRules{}, 10)
	wantCode(t, err, model.Unauthorized, "plugin")
}

func TestSelectHeldCapMarksMore(t *testing.T) {
	w, ops := batchWorld(t)
	got, err := w.as(agent).SelectHeld(t.Context(), sel(w), desk.BatchRules{}, len(ops)-1)
	if err != nil || len(got.Items) != len(ops)-1 || !got.More {
		t.Fatalf("select past the cap = %d items, more %v, %v", len(got.Items), got.More, err)
	}
	got, err = w.as(agent).SelectHeld(t.Context(), sel(w), desk.BatchRules{}, len(ops))
	if err != nil || len(got.Items) != len(ops) || got.More {
		t.Fatalf("select at the cap = %d items, more %v, %v", len(got.Items), got.More, err)
	}
}

// The always-held rules exclude an operation from a batch: each, alone.
func TestSelectHeldAlwaysHeldRulesExclude(t *testing.T) {
	cases := []struct {
		name, class, reason, why string
		rules                    func(inbox string) desk.BatchRules
		carry                    bool
	}{
		{name: "cc added", class: "trash", reason: "reply adds recipients (cc)", why: "always"},
		{name: "reply-to differs", class: "trash", reason: "Reply-To differs from From", why: "always"},
		{name: "unattended send", class: "reply:send", reason: "unattended send", why: "always"},
		{name: "unattended send, config", class: "reply:send", reason: training,
			rules: func(string) desk.BatchRules { return desk.BatchRules{Unattended: true} }, why: "unattended"},
		{name: "approval-always", class: "trash", reason: training,
			rules: func(in string) desk.BatchRules { return desk.BatchRules{ApprovalAlways: []string{in}} }, why: "approval-always"},
		{name: "approval-always, recorded", class: "trash", reason: "mailbox marked approval-always", why: "always"},
		{name: "supervised week", class: "trash", reason: "supervised first week", why: "always"},
		{name: "unsubscribe, always held", class: "unsubscribe", reason: trustcfg.UnsubscribeReason, why: "irreversible"},
		{name: "unsubscribe, even recorded as training", class: "unsubscribe", reason: "training: unsubscribe 0 of 5 clean approvals",
			why: "irreversible"},
		{name: "trust unreadable", class: "trash", reason: "trust unreadable: boom", why: "always"},
		{name: "carry-over", class: "trash", reason: training, carry: true, why: "carried over"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			verb, class := "state", tc.class
			if class == "reply:send" {
				verb, class = "reply", "send"
			}
			if class == "unsubscribe" {
				verb, class = "unsubscribe", ""
			}
			op := holdClass(t, w, w.local, "x-1", verb, class, tc.reason)
			if tc.carry {
				w.carryOver(op)
			}
			var rules desk.BatchRules
			inbox := string(w.local.Ref.Inbox.ID)
			if tc.rules != nil {
				rules = tc.rules(inbox)
			}
			got, err := w.as(agent).SelectHeld(t.Context(), desk.HeldSelector{Class: tc.class, Inbox: inbox}, rules, 10)
			if err != nil || len(got.Items) != 0 || len(got.Skipped) != 1 || !strings.Contains(got.Skipped[0].Why, tc.why) {
				t.Fatalf("select = %+v, %v; want it skipped (%s)", got, err, tc.why)
			}
		})
	}
	// Control: the same operation with no rule against it is matched.
	w := newWorld(t)
	holdClass(t, w, w.local, "x-1", "state", "trash", training)
	if got, err := w.as(agent).SelectHeld(t.Context(), desk.HeldSelector{Class: "trash", Inbox: string(w.local.Ref.Inbox.ID)}, desk.BatchRules{}, 10); err != nil || len(got.Items) != 1 {
		t.Fatalf("control = %+v, %v", got, err)
	}
	// HoldAll's reason is the ladder-off default: batchable.
	w = newWorld(t)
	holdClass(t, w, w.local, "x-2", "state", "trash", "plugin write: held for a decision")
	if got, err := w.as(agent).SelectHeld(t.Context(), desk.HeldSelector{Class: "trash", Inbox: string(w.local.Ref.Inbox.ID)}, desk.BatchRules{}, 10); err != nil || len(got.Items) != 1 {
		t.Fatalf("hold-all control = %+v, %v", got, err)
	}
}

// A trash held under never_auto is still approved in a batch: the ceiling
// is asking in batches, not asking each. Its reason is the ladder's own.
func TestSelectHeldBatchesNeverAutoClasses(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.NeverAuto = []string{"trash"}
	reason := cfg.Decide(trustcfg.Input{Verb: "state", Class: "trash", Counts: trustcfg.Counts{Found: true, Clean: 99}}).Reason
	w := newWorld(t)
	holdClass(t, w, w.local, "n-1", "state", "trash", reason)
	s := w.as(agent)
	got, err := s.SelectHeld(t.Context(), sel(w), desk.BatchRules{}, 10)
	if err != nil || len(got.Items) != 1 || len(got.Skipped) != 0 {
		t.Fatalf("select = %+v, %v; want the never-auto trash batchable", got, err)
	}
	if _, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: got.Items[0].ID, Decision: desk.DecideApprove,
		Revision: model.Revision(got.Items[0].Revision), Batch: desk.NewBatchID()}); err != nil {
		t.Fatalf("batch approve: %v", err)
	}
}

// The reasons the ladder holds for are what batchableReason pins: only
// its training count is batchable.
func TestLadderReasonsMatchBatchRules(t *testing.T) {
	cfg := trustcfg.Defaults()
	cfg.Unattended = true
	cfg.ApprovalAlways = []string{"vip"}
	cfg.Supervised = trustcfg.Supervised{Enabled: true, Days: 7}
	in := trustcfg.Input{Verb: "state", Class: "trash", Channel: "in1", Now: time.Unix(1_700_000_000, 0)}
	base := func(mod func(*trustcfg.Input)) string {
		i := in
		mod(&i)
		d := cfg.Decide(i)
		if !d.Hold {
			t.Fatalf("not held: %+v", i)
		}
		return d.Reason
	}
	h := func(reason string) desk.HeldOperation {
		return desk.HeldOperation{Reason: reason, TrustLabels: map[string]string{"verb": "state", "class": "trash", "channel": "in1"}}
	}
	cfg.Supervised.Enabled = false
	if why := desk.BatchRefusal(h(base(func(*trustcfg.Input) {})), desk.BatchRules{}); why != "" {
		t.Errorf("the ladder's training reason is refused: %s", why)
	}
	for name, mod := range map[string]func(*trustcfg.Input){
		"cc":       func(i *trustcfg.Input) { i.AddedCC = true },
		"reply-to": func(i *trustcfg.Input) { i.ReplyToDiffers = true },
		"carried":  func(i *trustcfg.Input) { i.CarriedOver = true },
		"send":     func(i *trustcfg.Input) { i.Verb, i.Class = "reply", "send" },
		"unsubscribe": func(i *trustcfg.Input) {
			i.Verb, i.Class = "unsubscribe", ""
			i.Counts = trustcfg.Counts{Found: true, Clean: 99}
		},
		"always-held": func(i *trustcfg.Input) { i.Channel = "vip" },
	} {
		if why := desk.BatchRefusal(h(base(mod)), desk.BatchRules{}); why == "" {
			t.Errorf("%s: the ladder's always-held reason %q is batchable", name, base(mod))
		}
	}
	cfg.Supervised.Enabled = true
	if why := desk.BatchRefusal(h(base(func(*trustcfg.Input) {})), desk.BatchRules{}); why == "" {
		t.Error("the supervised week's reason is batchable")
	}
}

// A selector never matches what the caller may not read.
func TestSelectHeldNeverMatchesUnreadable(t *testing.T) {
	w, ops := batchWorld(t)
	hidden := ops[1].Key.Target.ID
	w.gate.hide = map[model.ID]bool{hidden: true}
	got, err := w.as(agent).SelectHeld(t.Context(), sel(w), desk.BatchRules{}, 10)
	if err != nil || len(got.Items) != len(ops)-1 {
		t.Fatalf("select = %d items, %v; want %d", len(got.Items), err, len(ops)-1)
	}
	for _, h := range got.Items {
		if h.ID == string(ops[1].Ref.ID) {
			t.Errorf("matched %s, an operation the caller may not read", h.ID)
		}
	}
}

// One batch id counts once per trust key: three approvals of one key
// move the clean count by one; without a batch, by three.
func TestBatchApprovalCountsOncePerTrustKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		batch string
		clean int64
	}{{"one batch", desk.NewBatchID(), 1}, {"own batches", "", 3}} {
		t.Run(tc.name, func(t *testing.T) {
			w, ops := batchWorld(t)
			s := w.as(agent)
			sl, err := s.SelectHeld(t.Context(), sel(w), desk.BatchRules{}, 10)
			if err != nil || len(sl.Items) != 3 {
				t.Fatal(sl, err)
			}
			for _, h := range sl.Items {
				if _, err := s.DecideHeld(t.Context(), desk.DecideRequest{ID: h.ID, Decision: desk.DecideApprove, Revision: model.Revision(h.Revision), Batch: tc.batch}); err != nil {
					t.Fatal(err)
				}
			}
			if e := trustOf(t, w, w.local, ops[0]); e.Clean != tc.clean || e.Approvals != tc.clean {
				t.Fatalf("clean = %d, approvals %d; want %d", e.Clean, e.Approvals, tc.clean)
			}
		})
	}
}

func TestNewBatchIDIsAUUIDv7(t *testing.T) {
	a, b := desk.NewBatchID(), desk.NewBatchID()
	for _, id := range []string{a, b} {
		u, err := uuid.Parse(id)
		if err != nil || u.Version() != 7 {
			t.Fatalf("batch id %q is not a UUIDv7: %v", id, err)
		}
		if err := model.ValidateTrustBatch(id); err != nil {
			t.Fatal(err)
		}
	}
	if a == b {
		t.Errorf("two runs share batch id %s", a)
	}
}

// carryOver has uma's expiry sweep mark op carried over: it stays held a
// full sweep window.
func (w *world) carryOver(op model.Operation) {
	w.t.Helper()
	stop := sweeper(w, time.Minute)
	defer stop()
	w.clock.Advance(2 * time.Minute)
	for range 400 {
		if cur, err := w.as(agent).GetOperation(w.t.Context(), string(op.Ref.ID)); err == nil && cur.Held.CarriedOverAt != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.t.Fatalf("operation %s was never carried over", op.Ref.ID)
}
