package desk

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/kit/go/core/consent"
	"hop.top/uma/model"
)

// The audit trail of a held operation is derived, never stored by heo:
// uma owns the held block (proposal, preview, edits with before and
// after, decision, decider, time) and the held event, and every record
// here is read back from them. A reader sees a record only while uma
// shows it the operation (the read of the operation and its target).

// Audit events: what happened to a held operation.
const (
	AuditProposed = "proposed"
	AuditEdited   = "edited"
	AuditApproved = "approved"
	AuditRejected = "rejected"
	AuditExpired  = "expired"
)

// HeldAuditVersion is the version of HeldAuditRecord's shape.
const HeldAuditVersion = 1

// HeldAuditRecord is one audit record of a held operation: who (Subject),
// when (At), what was shown (Shown, the preview the decider saw) and what
// changed (Before and After of an edit; Applied, the arguments an
// approval applied in place of the proposal).
//
// Capability, Decision and Cause use kit's core/consent audit vocabulary:
// Capability is the operation kind, Decision allow or deny (approved or
// rejected), Cause consent.CauseExpired for an expiry, which is a refusal
// that no one decided. A JSONL line is one record.
type HeldAuditRecord struct {
	V          int                  `json:"v"`
	Operation  string               `json:"operation"`
	Event      string               `json:"event"`
	At         *time.Time           `json:"at,omitempty"`
	Subject    string               `json:"subject,omitempty"`
	Capability consent.Capability   `json:"capability,omitempty"`
	Target     string               `json:"target,omitempty"`
	TrustKey   string               `json:"trust_key,omitempty"`
	Decision   consent.Effect       `json:"decision,omitempty"`
	Cause      consent.RefusalCause `json:"reason,omitempty"`
	Note       string               `json:"note,omitempty"`
	Reason     string               `json:"held_reason,omitempty"`
	OnBehalfOf string               `json:"on_behalf_of,omitempty"`
	Shown      json.RawMessage      `json:"shown,omitempty"`
	Before     json.RawMessage      `json:"before,omitempty"`
	After      json.RawMessage      `json:"after,omitempty"`
	Applied    json.RawMessage      `json:"applied,omitempty"`
}

// HeldAuditOf derives the records of op, oldest first: the proposal, each
// edit, and the decision or expiry. heldAt is when the operation was held
// (its held event), nil when unknown. An operation that was never held has
// none.
func HeldAuditOf(op model.Operation, heldAt *time.Time) []HeldAuditRecord {
	h := op.Held
	if h == nil {
		return nil
	}
	base := HeldAuditRecord{V: HeldAuditVersion, Operation: string(op.Ref.ID), TrustKey: h.TrustKey}
	if op.Key != nil {
		base.Capability = consent.Capability(op.Key.Kind)
		base.Target = string(op.Key.Target.Kind) + "/" + string(op.Key.Target.ID)
	}
	var shown json.RawMessage
	if h.Preview != nil {
		shown = h.Preview.Data
	}
	rec := func(event string, at *time.Time, subject model.ActorID) HeldAuditRecord {
		r := base
		r.Event, r.At, r.Subject = event, at, string(subject)
		return r
	}

	p := rec(AuditProposed, heldAt, h.Proposer)
	p.Reason, p.OnBehalfOf, p.Shown = h.Reason, string(h.OnBehalfOf), shown
	out := []HeldAuditRecord{p}

	for _, e := range h.Edits {
		at := e.At
		r := rec(AuditEdited, &at, e.By)
		r.Before, r.After = e.Before.Data, e.After.Data
		out = append(out, r)
	}

	switch h.Decision {
	case model.HeldApproved, model.HeldEdited:
		r := rec(AuditApproved, h.DecidedAt, h.DecidedBy)
		r.Decision, r.Note, r.Shown = consent.Allow, h.DecisionNote, shown
		if h.Decision == model.HeldEdited && h.EditedArgs != nil {
			r.Applied = h.EditedArgs.Data
		}
		out = append(out, r)
	case model.HeldRejected:
		r := rec(AuditRejected, h.DecidedAt, h.DecidedBy)
		r.Decision, r.Note, r.Shown = consent.Deny, h.DecisionNote, shown
		out = append(out, r)
	case model.HeldExpired:
		r := rec(AuditExpired, h.DecidedAt, "")
		r.Cause = consent.CauseExpired
		out = append(out, r)
	}
	slices.SortStableFunc(out, func(a, b HeldAuditRecord) int {
		switch {
		case a.At == nil || b.At == nil:
			return 0
		case a.At.Before(*b.At):
			return -1
		case b.At.Before(*a.At):
			return 1
		}
		return 0
	})
	return out
}

// heldTimes maps held operation IDs to when each was held, read from the
// held events the caller may read. Best effort: an unreadable or
// retention-truncated log leaves a proposal without a time.
func (s Service) heldTimes(ctx context.Context) map[string]time.Time {
	times := map[string]time.Time{}
	start := ListStart{From: StartBeginning}
	for range 1000 {
		p, err := s.ListEvents(ctx, EventFilter{Kinds: []string{model.EventOperationHeld}}, start, 500)
		if err != nil {
			return times
		}
		for _, ev := range p.Items {
			var pl model.HeldOperationPayload
			if json.Unmarshal(ev.Payload.Data, &pl) == nil && pl.Operation.ID != "" {
				if _, ok := times[string(pl.Operation.ID)]; !ok {
					times[string(pl.Operation.ID)] = ev.OccurredAt
				}
			}
		}
		if p.Complete || p.Next == "" {
			return times
		}
		start = ListStart{Cursor: p.Next}
	}
	return times
}

func heldAtOf(times map[string]time.Time, id model.OperationRef) *time.Time {
	if t, ok := times[string(id.ID)]; ok {
		return &t
	}
	return nil
}

// HeldHistory returns the audit records of the held operation with ID id,
// oldest first. An operation that is not held, or that the caller may not
// read, is model.NotFound, as GetHeld answers it.
func (s Service) HeldHistory(ctx context.Context, id string) ([]HeldAuditRecord, error) {
	op, err := s.GetOperation(ctx, id)
	if err != nil {
		return nil, err
	}
	if op.Held == nil {
		return nil, model.NewError(model.NotFound, "resource not found")
	}
	return HeldAuditOf(op, heldAtOf(s.heldTimes(ctx), op.Ref)), nil
}

// ListHeldHistory returns the audit records of every held operation, in any
// state, that the caller may read and f admits, grouped by operation (newest
// change first) and oldest first within one.
func (s Service) ListHeldHistory(ctx context.Context, f OperationFilter) ([]HeldAuditRecord, error) {
	var ops []model.Operation
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListOperations(ctx, f, page)
		if err != nil {
			return nil, err
		}
		for _, op := range p.Items {
			if op.Held != nil {
				ops = append(ops, op)
			}
		}
		if p.Complete || p.Next == "" {
			break
		}
		page.After = p.Next
	}
	if len(ops) == 0 {
		return nil, nil
	}
	times := s.heldTimes(ctx)
	var out []HeldAuditRecord
	for _, op := range ops {
		out = append(out, HeldAuditOf(op, heldAtOf(times, op.Ref))...)
	}
	return out, nil
}
