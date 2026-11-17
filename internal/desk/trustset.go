package desk

import (
	"context"
	"fmt"
	"time"

	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// TrustWriter is the part of uma's operations a manual trust change
// uses.
type TrustWriter interface {
	Trust(ctx context.Context, req model.TrustLookup) (model.TrustEntry, error)
	ResetTrust(ctx context.Context, req model.ResetTrust) (model.TrustEntry, error)
	ImportTrust(ctx context.Context, req model.ImportTrust) (model.TrustEntry, error)
}

// Outcomes of a manual trust change.
const (
	TrustSetCount     = "set"
	TrustSetReset     = "reset"
	TrustSetUnchanged = "unchanged"
)

// TrustSetRequest is a person's change of one trust key's clean count:
// to Count, or back to zero (Reset). Exactly one is given.
type TrustSetRequest struct {
	Key   string
	Count *int64
	Reset bool
	Note  string
	// DryRun reports what the change would do and writes nothing.
	DryRun bool
}

// TrustSetResult is what a manual change did. It is the audit record
// of the change: who, when, the key, the count before and after and the
// note. It counts as neither an approval nor a correction.
type TrustSetResult struct {
	Key     string   `json:"key"               yaml:"key"`
	Outcome string   `json:"outcome"           yaml:"outcome"`
	DryRun  bool     `json:"dry_run"           yaml:"dry_run"`
	Actor   string   `json:"actor"             yaml:"actor"`
	At      string   `json:"at"                yaml:"at"`
	Note    string   `json:"note,omitempty"    yaml:"note,omitempty"`
	Before  TrustRow `json:"before"          yaml:"before"`
	After   TrustRow `json:"after"           yaml:"after"`
}

// SetTrust moves the clean count of the key req names on channel ch, as
// a person: a plugin actor is refused before anything is read. uma has
// no write that sets a count, so a count above the stored one is an
// import of it, and a count below is a reset first and an import of it;
// the totals uma keeps (approvals, corrections, last correction) are
// never lowered, and a count above the stored approvals raises that
// total to it. Neither is an approval nor a correction of the key.
//
// A class under never_auto takes a count below its threshold only (ask
// each, or in batches); at or above it is refused. unsubscribe takes
// none. w writes as the runtime: the caller's right to do this is the
// desk or operator role, checked before.
func (s Service) SetTrust(ctx context.Context, w TrustWriter, cfg trustcfg.Config, ch model.ChannelRef,
	req TrustSetRequest, now time.Time,
) (TrustSetResult, error) {
	if s.Actor.IsPlugin() {
		return TrustSetResult{}, model.NewError(model.Unauthorized, "a plugin actor may not change trust")
	}
	if req.Key == "" {
		return TrustSetResult{}, model.NewError(model.Validation, "key: required")
	}
	if (req.Count == nil) == !req.Reset {
		return TrustSetResult{}, model.NewError(model.Validation, "name one change: --count N or --reset")
	}
	if req.Count != nil && *req.Count < 0 {
		return TrustSetResult{}, model.NewError(model.Validation, fmt.Sprintf("count: %d is negative", *req.Count))
	}
	cur, err := w.Trust(ctx, model.TrustLookup{Call: call(), Channel: ch, Key: req.Key})
	if model.IsCode(err, model.NotFound) {
		return TrustSetResult{}, model.NewError(model.NotFound, fmt.Sprintf(
			"trust key %q has no counter on this inbox: a key is counted from its first decision or import", req.Key))
	}
	if err != nil {
		return TrustSetResult{}, err
	}
	before := TrustRowOf(cfg, cur, now)
	want := int64(0)
	if req.Count != nil {
		want = *req.Count
	}
	if err := checkTrustSet(cfg, before, want); err != nil {
		return TrustSetResult{}, err
	}
	res := TrustSetResult{Key: req.Key, DryRun: req.DryRun, Actor: string(s.Actor), At: now.UTC().Format(time.RFC3339),
		Note: req.Note, Before: before, Outcome: TrustSetCount}
	if req.Reset {
		res.Outcome = TrustSetReset
	}
	if want == cur.Clean {
		res.Outcome = TrustSetUnchanged
	}
	after := cur
	switch {
	case res.Outcome == TrustSetUnchanged:
	case req.DryRun:
		after.Clean = want
		if want > after.Approvals {
			after.Approvals = want
		}
	default:
		if after, err = s.writeTrustCount(ctx, w, ch, cur, want); err != nil {
			return TrustSetResult{}, err
		}
	}
	res.After = TrustRowOf(cfg, after, now)
	return res, nil
}

// checkTrustSet refuses a count the ladder's ceilings do not allow.
func checkTrustSet(cfg trustcfg.Config, row TrustRow, want int64) error {
	if row.Verb == trustcfg.UnsubscribeClass {
		return model.NewError(model.Validation, "unsubscribe is irreversible and never earns trust: there is no count to set")
	}
	if cfg.IsNeverAuto(row.Verb, row.Class) && row.Threshold != nil && want >= int64(*row.Threshold) {
		return model.NewError(model.Validation, fmt.Sprintf(
			"count %d: the class is under never_auto and stops at asking in batches, below its threshold of %d", want, *row.Threshold))
	}
	return nil
}

// writeTrustCount makes the clean count of cur want in uma, and reads it
// back: a count that did not land (a decision counted meanwhile) is a
// conflict, not a silent success.
func (s Service) writeTrustCount(ctx context.Context, w TrustWriter, ch model.ChannelRef, cur model.TrustEntry, want int64) (model.TrustEntry, error) {
	if want < cur.Clean || want == 0 {
		if _, err := w.ResetTrust(ctx, model.ResetTrust{Call: call(), Channel: ch, Key: cur.Key}); err != nil {
			return model.TrustEntry{}, err
		}
	}
	if want > 0 {
		imp := model.ImportTrust{Call: call(), Channel: ch, Key: cur.Key, Clean: want,
			Corrections: cur.Corrections, LastCorrectionAt: cur.LastCorrectionAt}
		if err := imp.Validate(); err != nil {
			return model.TrustEntry{}, err
		}
		if _, err := w.ImportTrust(ctx, imp); err != nil {
			return model.TrustEntry{}, err
		}
	}
	got, err := w.Trust(ctx, model.TrustLookup{Call: call(), Channel: ch, Key: cur.Key})
	if err != nil {
		return model.TrustEntry{}, err
	}
	if got.Clean != want {
		return model.TrustEntry{}, model.NewError(model.Conflict, fmt.Sprintf(
			"the count is %d, not %d: a decision was counted meanwhile; read it again and retry", got.Clean, want))
	}
	return got, nil
}
