package desk

import (
	"context"
	"encoding/json"
	"strings"

	"hop.top/uma/model"
)

// staleOf reports whether err is uma's stale-target refusal: model.Conflict
// with the reason model.StaleTarget, which tells it from a revision
// mismatch (model.StaleRevision), also a conflict.
func staleOf(err error) bool {
	return model.IsCode(err, model.Conflict) && model.IsReason(err, model.StaleTarget)
}

// staleMessage is what heo says of a stale refusal.
const staleMessage = "stale: the target changed since the operation was held; re-read it with `operation held show`, or pass --force to decide anyway"

// staleEditMessage is what heo says of a stale refusal of an edit. It
// carries staleMessage's marker so IsStale holds, and no --force: an
// edit has none.
const staleEditMessage = "stale: the target changed since the operation was held; re-read it with `operation held show` and edit again"

// IsStale reports whether err is heo's stale refusal of a decision.
func IsStale(err error) bool {
	return model.IsCode(err, model.Conflict) &&
		(strings.Contains(err.Error(), staleMessage) || strings.Contains(err.Error(), staleEditMessage))
}

// Decision is a verb's decision of a held operation.
type Decision string

// The decisions the held verbs make.
const (
	DecideApprove Decision = "approve"
	DecideReject  Decision = "reject"
	DecideEdit    Decision = "edit"
)

// DecideRequest is one decision of one held operation. Revision is the
// operation revision the decider read; empty reads it now. Correlation
// tags the call (the caller's idempotency key).
type DecideRequest struct {
	ID          string
	Decision    Decision
	Revision    model.Revision
	Force       bool
	Note        string
	Correlation string
	// Batch groups approvals for uma's trust count: approvals of one
	// trust key that share a Batch count as one clean approval. Empty
	// makes the operation its own batch.
	Batch string
}

// HeldDecided is the outcome of a decision: the operation as uma holds
// it after, and what uma says of the decision.
type HeldDecided struct {
	HeldOperation
	Replayed   bool `json:"replayed"   yaml:"replayed"`
	Correction bool `json:"correction" yaml:"correction"`
}

// DecisionRecord is what OnDecided receives for each decision uma wrote.
// It is the point an audit trail attaches to; a replay is never
// reported, as uma wrote nothing.
type DecisionRecord struct {
	Operation  string
	Decision   Decision
	By         model.ActorID
	Note       string
	Forced     bool
	Correction bool
	TrustKey   string
}

// errPluginDecides refuses a plugin actor's decision: proposing grants
// no right to decide, so a plugin never approves or rejects.
func errPluginDecides() error {
	return model.NewError(model.Unauthorized, "a plugin actor may not decide a held operation")
}

// DecideHeld approves or rejects the held operation r.ID as the caller.
// A plugin actor is refused before anything is read. A stale target is
// refused as a conflict (IsStale) unless r.Force. uma answers a decision
// already made as Replayed, writing nothing.
func (s Service) DecideHeld(ctx context.Context, r DecideRequest) (HeldDecided, error) {
	if s.Actor.IsPlugin() {
		return HeldDecided{}, errPluginDecides()
	}
	cur, err := s.GetOperation(ctx, r.ID)
	if err != nil {
		return HeldDecided{}, err
	}
	if _, ok := HeldOf(cur); !ok {
		return HeldDecided{}, model.NewError(model.NotFound, "resource not found")
	}
	rev := r.Revision
	if rev == "" {
		rev = cur.Revision
	}
	corr := Correlation
	if r.Correlation != "" {
		corr = model.CorrelationID(r.Correlation)
	}
	d := model.DecideHeld{
		Call:             model.Call{Correlation: corr},
		Operation:        cur.Ref,
		ExpectedRevision: rev,
		Force:            r.Force,
		Note:             r.Note,
		Batch:            r.Batch,
	}
	var res model.Decided
	switch r.Decision {
	case DecideApprove:
		res, err = s.Client.Operations().Approve(ctx, d)
	case DecideReject:
		res, err = s.Client.Operations().Reject(ctx, d)
	case DecideEdit:
		return HeldDecided{}, model.NewError(model.Validation, "decision: edit takes EditHeld")
	default:
		return HeldDecided{}, model.NewError(model.Validation, "decision: want approve or reject")
	}
	if err != nil {
		if staleOf(err) {
			return HeldDecided{}, model.NewError(model.Conflict, staleMessage)
		}
		return HeldDecided{}, err
	}
	v, _ := HeldOf(res.Operation)
	if !res.Replayed && s.OnDecided != nil {
		s.OnDecided(ctx, DecisionRecord{Operation: r.ID, Decision: r.Decision, By: s.Actor, Note: r.Note,
			Forced: r.Force, Correction: res.Correction, TrustKey: res.TrustKey})
	}
	return HeldDecided{HeldOperation: v, Replayed: res.Replayed, Correction: res.Correction}, nil
}

// EditRequest is an edit of one held operation: Args replaces the stored
// intent's data (its extension name and version stay), and the edit is
// approved as edited. Revision is the operation revision the editor
// read; empty reads it now. CorrectedKeys names the trust keys the edit
// corrects; empty is the operation's own.
type EditRequest struct {
	ID            string
	Args          json.RawMessage
	CorrectedKeys []string
	Revision      model.Revision
	Note          string
	Correlation   string
}

// EditHeld edits the held operation r.ID and approves it as edited, as
// the caller. A plugin actor is refused before anything is read. A stale
// target is refused as a conflict (IsStale); an edit never forces past
// it, since the editor read the draft before the target moved. uma
// checks the arguments against the stored intent.
func (s Service) EditHeld(ctx context.Context, r EditRequest) (HeldDecided, error) {
	if s.Actor.IsPlugin() {
		return HeldDecided{}, errPluginDecides()
	}
	cur, err := s.GetOperation(ctx, r.ID)
	if err != nil {
		return HeldDecided{}, err
	}
	if _, ok := HeldOf(cur); !ok {
		return HeldDecided{}, model.NewError(model.NotFound, "resource not found")
	}
	if cur.Held.Intent == nil {
		return HeldDecided{}, model.NewError(model.Unsupported, "the held operation stored no intent to edit")
	}
	if len(r.Args) == 0 || !json.Valid(r.Args) {
		return HeldDecided{}, model.NewError(model.Validation, "args: want a JSON document")
	}
	rev := r.Revision
	if rev == "" {
		rev = cur.Revision
	}
	corr := Correlation
	if r.Correlation != "" {
		corr = model.CorrelationID(r.Correlation)
	}
	if len(r.CorrectedKeys) == 0 {
		r.CorrectedKeys = nil // uma: a non-nil empty list corrects no key
	}
	e := model.EditHeld{
		DecideHeld:    model.DecideHeld{Call: model.Call{Correlation: corr}, Operation: cur.Ref, ExpectedRevision: rev, Note: r.Note},
		Args:          model.Extension{Name: cur.Held.Intent.Name, Version: cur.Held.Intent.Version, Data: r.Args},
		CorrectedKeys: r.CorrectedKeys,
	}
	res, err := s.Client.Operations().Edit(ctx, e)
	if err != nil {
		if staleOf(err) {
			return HeldDecided{}, model.NewError(model.Conflict, staleEditMessage)
		}
		return HeldDecided{}, err
	}
	v, _ := HeldOf(res.Operation)
	if !res.Replayed && s.OnDecided != nil {
		s.OnDecided(ctx, DecisionRecord{Operation: r.ID, Decision: DecideEdit, By: s.Actor, Note: r.Note,
			Correction: res.Correction, TrustKey: res.TrustKey})
	}
	return HeldDecided{HeldOperation: v, Replayed: res.Replayed, Correction: res.Correction}, nil
}
