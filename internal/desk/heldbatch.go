package desk

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"hop.top/heo/internal/trustcfg"
	"hop.top/uma/model"
)

// Batch approval: a person approves the held operations that match a
// selector (a verb class in a mailbox, optionally one proposer) in one
// call. uma counts the approvals of one trust key that share a batch id
// as one clean approval, so a run names one batch for every operation it
// approves: approving fifty trashes never graduates the class by itself.

// MaxBatch is the most operations one selector run approves, whatever
// the caller asks: a larger match is narrowed, not approved.
const MaxBatch = 500

// DefaultBatch is how many operations a selector run approves unless the
// caller raises the cap (up to MaxBatch): more matches than the cap are
// refused, never truncated.
const DefaultBatch = 50

// HeldSelector picks held operations. Class and Inbox are both required:
// a selector with either empty matches nothing and is refused, so a run
// never approves "everything".
type HeldSelector struct {
	// Class is the verb class the trust ladder counts under, such as
	// trash, move_to or reply:send (trustcfg.Class).
	Class string
	// Inbox is the mailbox: an inbox key or ID.
	Inbox string
	// Proposer narrows to one plugin actor's operations; empty is any.
	Proposer string
}

// Validate refuses an empty selector.
func (f HeldSelector) Validate() error {
	switch {
	case strings.TrimSpace(f.Class) == "":
		return model.NewError(model.Validation, "selector: a verb class is required, such as trash")
	case strings.TrimSpace(f.Inbox) == "":
		return model.NewError(model.Validation, "selector: a mailbox (inbox) is required")
	}
	return nil
}

// BatchRules are the always-held rules a selector run honours beyond what
// an operation records about itself: they are read from the current
// configuration, so a rule added after an operation was held still
// excludes it.
type BatchRules struct {
	// ApprovalAlways lists the mailboxes (inbox IDs) whose every write is
	// held: none is approved in a batch.
	ApprovalAlways []string
	// Unattended: a send is always held, so none is approved in a batch.
	Unattended bool
}

// Skipped is an operation that matches the selector but is never
// approved in a batch, and why.
type Skipped struct {
	Operation string `json:"operation" yaml:"operation"`
	Why       string `json:"why"       yaml:"why"`
}

// HeldSelection is what a selector matched.
type HeldSelection struct {
	// Items are the operations a run approves, newest change first.
	Items []HeldOperation
	// Skipped are the matches that always-held rules exclude.
	Skipped []Skipped
	// More is true when matches beyond the cap exist.
	More bool
}

// trainingReasonPrefix starts the reason the ladder gives a write it holds
// for lack of clean approvals (trustcfg.Config.Decide); a test pins it.
const trainingReasonPrefix = "training:"

// batchableReason reports whether the reason an operation was held is one
// a batch may approve: the ladder's training count, a never_auto class
// (capped at asking in batches), or the hold-everything default. Every
// other reason is an always-held rule (cc added, Reply-To differs,
// unattended send, approval-always, carry-over, the supervised week) or a failure to read trust, and is never batched. A reason heo
// does not know excludes the operation: a new rule is safe by default.
func batchableReason(reason string) bool {
	return strings.HasPrefix(reason, trainingReasonPrefix) || strings.HasPrefix(reason, trustcfg.NeverAutoReasonPrefix) ||
		reason == holdAllReason
}

// BatchRefusal says why h is never approved in a batch, "" when it may be.
func BatchRefusal(h HeldOperation, rules BatchRules) string {
	channel := h.TrustLabels["channel"]
	switch {
	case h.CarriedOverAt != nil:
		return "carried over from an earlier batch"
	case slices.Contains(rules.ApprovalAlways, channel):
		return "mailbox marked approval-always"
	case rules.Unattended && classOf(h) == "reply:send":
		return "unattended send"
	case classOf(h) == trustcfg.UnsubscribeClass:
		return "unsubscribe is irreversible: approve it one operation at a time"
	case !batchableReason(h.Reason):
		return "held by a rule that always needs a decision: " + h.Reason
	}
	return ""
}

func classOf(h HeldOperation) string {
	return trustcfg.Class(h.TrustLabels["verb"], h.TrustLabels["class"])
}

// SelectHeld lists the held operations that match f and may be approved
// in a batch, at most max of them (More says there are more). A plugin
// actor is refused before anything is read. The listing is the caller's:
// uma shows an operation only to a reader of it and of its target, so a
// selector never matches what the caller may not read.
func (s Service) SelectHeld(ctx context.Context, f HeldSelector, rules BatchRules, max int) (HeldSelection, error) {
	if s.Actor.IsPlugin() {
		return HeldSelection{}, errPluginDecides()
	}
	if err := f.Validate(); err != nil {
		return HeldSelection{}, err
	}
	if max < 1 || max > MaxBatch {
		return HeldSelection{}, model.NewError(model.Validation, fmt.Sprintf("max: want 1 to %d", MaxBatch))
	}
	of := OperationFilter{Inbox: f.Inbox, States: []string{string(model.Held)}}
	if f.Proposer != "" {
		of.Actor = model.ActorID(f.Proposer)
	}
	var out HeldSelection
	page := model.PageRequest{Limit: scanPage}
	for {
		p, err := s.ListHeld(ctx, of, page)
		if err != nil {
			return HeldSelection{}, err
		}
		for _, h := range p.Items {
			if h.Status != string(model.Held) || classOf(h) != strings.ToLower(f.Class) {
				continue
			}
			if f.Proposer != "" && h.Proposer != f.Proposer {
				continue
			}
			if why := BatchRefusal(h, rules); why != "" {
				out.Skipped = append(out.Skipped, Skipped{Operation: h.ID, Why: why})
				continue
			}
			if len(out.Items) == max {
				out.More = true
				return out, nil
			}
			out.Items = append(out.Items, h)
		}
		if p.Complete || p.Next == "" {
			return out, nil
		}
		page.After = p.Next
	}
}

// NewBatchID returns the batch id of one selector run: a time-ordered
// UUIDv7, so batches sort by when they ran.
func NewBatchID() string {
	return uuid.Must(uuid.NewV7()).String()
}
