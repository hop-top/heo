package desk

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// HeldOperation is what heo shows of an operation a plugin actor proposed
// for a human decision: uma's held block flattened into the verb output.
// uma owns the state; this is a view, never a second copy. Preview and
// EditedArgs are the extension payloads as uma holds them.
type HeldOperation struct {
	ID         string          `json:"id" yaml:"id"`
	Kind       string          `json:"kind" yaml:"kind"`
	Target     string          `json:"target,omitempty" yaml:"target,omitempty"`
	Status     string          `json:"status" yaml:"status"`
	Proposer   string          `json:"proposer" yaml:"proposer"`
	OnBehalfOf string          `json:"on_behalf_of,omitempty" yaml:"on_behalf_of,omitempty"`
	Reason     string          `json:"reason,omitempty" yaml:"reason,omitempty"`
	Preview    json.RawMessage `json:"preview,omitempty" yaml:"preview,omitempty"`
	// Intent is the arguments an approval applies unless edited, as uma
	// stored them when the operation was held: what `held edit` is
	// seeded from.
	Intent      json.RawMessage   `json:"intent,omitempty" yaml:"intent,omitempty"`
	TrustKey    string            `json:"trust_key,omitempty" yaml:"trust_key,omitempty"`
	TrustLabels map[string]string `json:"trust_labels,omitempty" yaml:"trust_labels,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty" yaml:"expires_at,omitempty"`
	// CarriedOverAt is when an expiry sweep first found the operation
	// still held a full window after it was held.
	CarriedOverAt *time.Time      `json:"carried_over_at,omitempty" yaml:"carried_over_at,omitempty"`
	Decision      string          `json:"decision,omitempty" yaml:"decision,omitempty"`
	DecidedBy     string          `json:"decided_by,omitempty" yaml:"decided_by,omitempty"`
	DecidedAt     *time.Time      `json:"decided_at,omitempty" yaml:"decided_at,omitempty"`
	EditedArgs    json.RawMessage `json:"edited_args,omitempty" yaml:"edited_args,omitempty"`
	Edits         []HeldEditView  `json:"edits,omitempty" yaml:"edits,omitempty"`
	Revision      string          `json:"revision" yaml:"revision"`
	UpdatedAt     time.Time       `json:"updated_at" yaml:"updated_at"`
}

// HeldEditView is one edit of a held operation's arguments.
type HeldEditView struct {
	By     string          `json:"by" yaml:"by"`
	At     time.Time       `json:"at" yaml:"at"`
	Before json.RawMessage `json:"before,omitempty" yaml:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty" yaml:"after,omitempty"`
}

// HeldOf maps an operation uma returned to heo's held view. ok is false
// for an operation that was never held.
func HeldOf(op model.Operation) (HeldOperation, bool) {
	h := op.Held
	if h == nil {
		return HeldOperation{}, false
	}
	v := HeldOperation{
		ID: string(op.Ref.ID), Status: string(op.State), Revision: string(op.Revision), UpdatedAt: op.UpdatedAt,
		Proposer: string(h.Proposer), OnBehalfOf: string(h.OnBehalfOf), Reason: h.Reason,
		TrustKey: h.TrustKey, TrustLabels: h.TrustLabels, ExpiresAt: h.ExpiresAt,
		Decision: string(h.Decision), DecidedBy: string(h.DecidedBy), DecidedAt: h.DecidedAt,
	}
	if op.Key != nil {
		v.Kind = op.Key.Kind
		v.Target = string(op.Key.Target.Kind) + "/" + string(op.Key.Target.ID)
	}
	if h.Preview != nil {
		v.Preview = h.Preview.Data
	}
	if h.Intent != nil {
		v.Intent = h.Intent.Data
	}
	v.CarriedOverAt = h.CarriedOverAt
	if h.EditedArgs != nil {
		v.EditedArgs = h.EditedArgs.Data
	}
	for _, e := range h.Edits {
		v.Edits = append(v.Edits, HeldEditView{By: string(e.By), At: e.At, Before: e.Before.Data, After: e.After.Data})
	}
	return v, true
}

// GetHeld returns the held operation with record ID id, as uma shows it
// to the caller: a reader of the operation and of its target only. An
// operation that is not held, or that the caller may not read, is
// model.NotFound; uma answers both alike.
func (s Service) GetHeld(ctx context.Context, id string) (HeldOperation, error) {
	op, err := s.GetOperation(ctx, id)
	if err != nil {
		return HeldOperation{}, err
	}
	v, ok := HeldOf(op)
	if !ok {
		return HeldOperation{}, model.NewError(model.NotFound, "resource not found")
	}
	return v, nil
}

// ListHeld returns one page of the operations uma holds for a decision
// that the caller may read, newest change first. f.States defaults to
// held; a decided or expired proposal is listed with its other states.
func (s Service) ListHeld(ctx context.Context, f OperationFilter, page model.PageRequest) (model.Page[HeldOperation], error) {
	if len(f.States) == 0 {
		f.States = []string{string(model.Held)}
	}
	p, err := s.ListOperations(ctx, f, page)
	if err != nil {
		return model.Page[HeldOperation]{}, err
	}
	out := model.Page[HeldOperation]{Next: p.Next, Complete: p.Complete}
	for _, op := range p.Items {
		if v, ok := HeldOf(op); ok {
			out.Items = append(out.Items, v)
		}
	}
	return out, nil
}
