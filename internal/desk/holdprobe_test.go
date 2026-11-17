package desk_test

import (
	"context"
	"strings"
	"testing"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// holdProbe submits a work item write as a plugin actor with a hold
// proposal set directly on its command, whatever heo's holdable table
// says: it probes what uma does with one.
func holdProbe(t *testing.T, req *model.Command, submit func(desk.Service) (model.Acceptance, error), w *ticketWorld) model.Acceptance {
	t.Helper()
	req.Hold = proposal()
	acc, err := submit(w.asKnowing(plugin))
	if err != nil {
		t.Fatalf("held submission: %v", err)
	}
	return acc
}

// TestWorkItemActIsHeldByUma probes uma's store, as the holdable table
// requires of every action it lists: a plugin actor's work item act with a
// hold proposal is stored held with nothing written; approving it applies
// it exactly once, a second approve is a replay; rejecting it applies
// nothing. It holds for a status the provider owns (nothing is queued
// until approval) and for fields uma owns (nothing changes until
// approval).
func TestWorkItemActIsHeldByUma(t *testing.T) {
	ctx := t.Context()
	dec := func(w *ticketWorld) desk.Service { return w.as(agent) }

	t.Run("provider status", func(t *testing.T) {
		w := newTicketWorld(t)
		issue := w.issue()
		before := w.queued()
		propose := func(key string) model.Acceptance {
			p, err := w.asKnowing(plugin).PlanStatus(ctx, convID(issue), "closed", "", "", model.OperationID(key), channels.Submit)
			if err != nil {
				t.Fatal(err)
			}
			return holdProbe(t, &p.Request.Command, func(s desk.Service) (model.Acceptance, error) {
				_, acc, err := s.SetStatus(ctx, p)
				return acc, err
			}, w)
		}
		acc := propose("hp-rej")
		if acc.Operation.State != model.Held || w.queued() != before {
			t.Fatalf("hold = %v, queued %d", acc.Operation.State, w.queued()-before)
		}
		r, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject})
		if err != nil || r.Status != string(model.Rejected) || w.queued() != before {
			t.Fatalf("reject = %+v, %v, queued %d", r, err, w.queued()-before)
		}
		acc = propose("hp-app")
		if acc.Operation.State != model.Held || w.queued() != before {
			t.Fatalf("hold = %v, queued %d", acc.Operation.State, w.queued()-before)
		}
		a, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
		if err != nil || a.Replayed || w.queued() != before+1 {
			t.Fatalf("approve = %+v, %v, queued %d", a, err, w.queued()-before)
		}
		again, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
		if err != nil || !again.Replayed || w.queued() != before+1 {
			t.Fatalf("second approve = %+v, %v, queued %d", again, err, w.queued()-before)
		}
	})

	// uma has no hold for a write of fields it owns: it refuses the
	// proposal, so heo refuses it first, in HoldUpdate.
	t.Run("local fields are not held", func(t *testing.T) {
		w := newTicketWorld(t)
		gl := w.seedGitLabIssue(model.WorkItemFieldSubject)
		f, _ := desk.ParseFields([]string{"subject=held?"})
		p, err := w.asKnowing(plugin).PlanUpdate(ctx, convID(gl), f, "", "hp-loc", channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		p.Act.Hold = proposal()
		if _, _, err := w.asKnowing(plugin).Update(ctx, p); !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), "cannot be held") {
			t.Fatalf("uma's answer to a held local update = %v, want unsupported: cannot be held", err)
		}
		p.Act.Hold = nil
		s, _ := holding(w.asKnowing(plugin), nil)
		if _, err := s.HoldUpdate(ctx, &p, "p"); !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), "cannot be held yet") {
			t.Fatalf("HoldUpdate of a local update = %v", err)
		}
		if p.Act.Hold != nil {
			t.Error("a refused hold set a proposal")
		}
	})

	t.Run("local status is not held", func(t *testing.T) {
		w := newTicketWorld(t)
		gl := w.seedGitLabIssue(model.WorkItemFieldStatus)
		s, _ := holding(w.asKnowing(plugin), nil)
		p, err := s.PlanStatus(ctx, convID(gl), "resolved", "", "", "hp-ls", channels.Submit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.HoldStatus(ctx, &p, "p"); !model.IsCode(err, model.Unsupported) || !strings.Contains(err.Error(), "cannot be held yet") {
			t.Fatalf("HoldStatus of a local status = %v", err)
		}
		// A person is never held, and commits it.
		if _, _, err := w.asKnowing(agent).SetStatus(ctx, p); err != nil {
			t.Fatalf("a person's local status: %v", err)
		}
	})

	t.Run("provider update effect", func(t *testing.T) {
		w := newTicketWorld(t)
		issue := w.issue()
		before := w.queued()
		propose := func(key string) model.Acceptance {
			f, _ := desk.ParseFields([]string{"milestone=3"})
			p, err := w.asKnowing(plugin).PlanUpdate(ctx, convID(issue), f, "", model.OperationID(key), channels.Submit)
			if err != nil {
				t.Fatal(err)
			}
			return holdProbe(t, &p.Effect.Command, func(s desk.Service) (model.Acceptance, error) {
				_, acc, err := s.Update(ctx, p)
				return acc, err
			}, w)
		}
		acc := propose("hp-rej")
		if acc.Operation.State != model.Held || w.queued() != before {
			t.Fatalf("hold = %v, queued %d", acc.Operation.State, w.queued()-before)
		}
		r, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideReject})
		if err != nil || r.Status != string(model.Rejected) || w.queued() != before {
			t.Fatalf("reject = %+v, %v, queued %d", r, err, w.queued()-before)
		}
		acc = propose("hp-app")
		a, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
		if err != nil || a.Replayed || w.queued() != before+1 {
			t.Fatalf("approve = %+v, %v, queued %d", a, err, w.queued()-before)
		}
		again, err := dec(w).DecideHeld(ctx, desk.DecideRequest{ID: string(acc.Operation.Ref.ID), Decision: desk.DecideApprove})
		if err != nil || !again.Replayed || w.queued() != before+1 {
			t.Fatalf("second approve = %+v, %v, queued %d", again, err, w.queued()-before)
		}
	})
}

// recordPolicy holds everything and records what it was asked.
type recordPolicy struct{ got *[]desk.HoldQuery }

func (r recordPolicy) Decide(_ context.Context, q desk.HoldQuery) (desk.HoldDecision, error) {
	*r.got = append(*r.got, q)
	return desk.HoldDecision{Hold: true, Reason: "probe"}, nil
}

// TestHeldWorkItemWritesCarryTheirTrustClass: a plugin actor's status
// change and provider update are offered to the hold point under the
// verbs status (class: the status) and fields (class: the fields asked
// for, sorted), held, and settled on the operation; a person's are
// never offered.
func TestHeldWorkItemWritesCarryTheirTrustClass(t *testing.T) {
	w := newTicketWorld(t)
	issue := w.issue()
	ctx := t.Context()
	var asked []desk.HoldQuery
	s, events := holding(w.asKnowing(plugin), recordPolicy{&asked})

	sp, err := s.PlanStatus(ctx, convID(issue), "closed", "", "", "hc-1", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.HoldStatus(ctx, &sp, "p")
	if err != nil || h == nil {
		t.Fatalf("HoldStatus = %v, %v", h, err)
	}
	_, acc, err := s.SetStatus(ctx, sp)
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Held {
		t.Fatalf("held status = %v, %v", acc.Operation.State, err)
	}
	f, _ := desk.ParseFields([]string{"milestone=3", `labels=["a"]`})
	up, err := s.PlanUpdate(ctx, convID(issue), f, "", "hc-2", channels.Submit)
	if err != nil {
		t.Fatal(err)
	}
	if h, err = s.HoldUpdate(ctx, &up, "p"); err != nil || h == nil {
		t.Fatalf("HoldUpdate = %v, %v", h, err)
	}
	_, acc, err = s.Update(ctx, up)
	if acc, err = h.Settle(ctx, acc, err); err != nil || acc.Operation.State != model.Held {
		t.Fatalf("held update = %v, %v", acc.Operation.State, err)
	}
	if len(asked) != 2 || asked[0].Verb != "status" || asked[0].Class != "closed" || asked[1].Verb != "fields" || asked[1].Class != "labels,milestone" ||
		asked[0].Action != model.ActionWorkItemAct || asked[0].Channel != string(issue.Ref.Conversation.Inbox.ID) || len(*events) != 2 {
		t.Fatalf("queries = %+v, events %d", asked, len(*events))
	}
	if h, err := w.asKnowing(agent).HoldStatus(ctx, &sp, "p"); h != nil || err != nil {
		t.Errorf("a person's write was offered to the hold point: %v, %v", h, err)
	}
}
