package desk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"hop.top/uma/model"
)

// DefaultConsumer names the subscription `event watch` opens when the
// caller names none.
const DefaultConsumer = "heo-cli"

// Starts of a new subscription.
const (
	StartTail      = model.StartTail
	StartBeginning = model.StartBeginning
)

// watchBatch bounds the events one poll returns.
const watchBatch = 100

// WatchStart is where a watch begins: the caller's subscription named
// Consumer, resumed at its checkpoint when it is open, else created or
// reopened at Cursor when set, at From (StartTail or StartBeginning)
// otherwise.
type WatchStart struct {
	Consumer string
	Cursor   model.Cursor
	From     string
	// Filter narrows the subscription by kind and inbox, which uma
	// applies; it is set when the subscription is created and kept
	// with it. The zero value watches everything the caller may read.
	Filter model.Filter
}

// GapError refuses a watch asked to resume at a cursor its open
// subscription is not at: starting there would skip or repeat the
// events between the two positions, silently.
type GapError struct {
	Consumer string
	At       model.Cursor
	Asked    model.Cursor
}

func (e *GapError) Error() string {
	return fmt.Sprintf("subscription %s is open at checkpoint %s, not at the cursor %s: "+
		"resuming there would skip or repeat the events between them", e.Consumer, e.At, e.Asked)
}

// Unwrap is the uma conflict the gap is.
func (e *GapError) Unwrap() error { return model.NewError(model.Conflict, e.Error()) }

// OpenWatch opens the caller's subscription named w.Consumer under
// operation op, or returns it as it is when it is open: an open
// subscription keeps its checkpoint. A cursor the open subscription is
// not at is *GapError; a cursor behind event retention is uma's
// cursor_expired.
func (s Service) OpenWatch(ctx context.Context, w WatchStart, op model.OperationID) (model.Subscription, error) {
	consumer := w.Consumer
	if consumer == "" {
		consumer = DefaultConsumer
	}
	req := model.OpenSubscription{
		Command: model.Command{Call: call(), Operation: op}, Consumer: consumer, Filter: w.Filter,
		Start: w.From, PayloadMajor: model.PayloadVersion1,
	}
	if w.Cursor != "" {
		req.Start, req.After = model.StartCheckpoint, w.Cursor
	} else if req.Start == "" {
		req.Start = StartTail
	}
	res, err := s.Client.Events().Open(ctx, req)
	if err != nil {
		return model.Subscription{}, err
	}
	sub := res.Subscription
	if w.Cursor != "" && sub.Checkpoint != w.Cursor {
		return sub, &GapError{Consumer: consumer, At: sub.Checkpoint, Asked: w.Cursor}
	}
	return sub, nil
}

// Batch is one leased batch of a watch: the events the caller may see,
// and the token that acknowledges the whole scanned range, empty when
// nothing was scanned.
type Batch struct {
	Events []model.Event
	Token  string
}

// PollWatch leases the next batch of the subscription sub, waiting up
// to wait for one. Every event passes the caller's audience rule again,
// one by one, whatever uma returned: content that is not public never
// reaches a caller who is not a desk actor. An event dropped here stays
// within the batch's scanned range, so acknowledging the batch passes
// it, as uma passes the events it skips.
func (s Service) PollWatch(ctx context.Context, sub model.SubscriptionRef, wait time.Duration) (Batch, error) {
	b, err := s.Client.Events().Poll(ctx, model.Poll{Call: call(), Subscription: sub, Limit: watchBatch, Wait: wait})
	if err != nil {
		return Batch{}, err
	}
	out := Batch{Events: make([]model.Event, 0, len(b.Events)), Token: b.AckToken}
	for _, ev := range b.Events {
		if s.seesAudience(ev.Audience) {
			out.Events = append(out.Events, ev)
		}
	}
	return out, nil
}

// AckWatch acknowledges the batch token, moving the subscription's
// checkpoint past it. Its operation derives from the token, so an
// acknowledgement retried after a lost answer replays.
func (s Service) AckWatch(ctx context.Context, sub model.SubscriptionRef, token string) (model.Subscription, error) {
	res, err := s.Client.Events().Ack(ctx, model.Acknowledge{
		Command: model.Command{Call: call(), Operation: AckOperation(token)}, Subscription: sub, Token: token,
	})
	return res.Subscription, err
}

// AckOperation is the operation acknowledging token.
func AckOperation(token string) model.OperationID {
	sum := sha256.Sum256([]byte(token))
	return model.OperationID("ack-" + hex.EncodeToString(sum[:16]))
}

// seesAudience reports whether the caller may read content under a.
func (s Service) seesAudience(a model.Audience) bool {
	if s.Sees == nil {
		return a.Visibility == model.Public
	}
	return s.Sees(a)
}

// EventFilter narrows an event list. Inbox names an inbox by key or ID;
// Kinds are event kinds. uma honours these two filters on its event log
// and rejects any other, so heo offers no more.
type EventFilter struct {
	Inbox string
	Kinds []string
}

// ListStart is where an event list reads: Cursor, a position an earlier
// page or watch printed, or From (StartBeginning, the oldest retained
// event, or StartTail, the head) when Cursor is empty. Exactly one is
// set.
type ListStart struct {
	From   string
	Cursor model.Cursor
}

// ListEvents reads one page of the events the caller may read, at
// start, narrowed by f. limit bounds the events uma scans for the page:
// the events the caller may not read count against it and are dropped,
// so a page may be short or empty and still move on. The page's Next is
// the position to continue at, also on an empty page, and Complete says
// the read reached the head. Nothing is written: no subscription is
// opened or moved.
//
// uma authorizes the call and every event it scans; heo passes each
// event through the caller's audience rule again, as PollWatch does,
// whatever uma returned. A position behind event retention is uma's
// cursor_expired, never a list that starts at what is left.
func (s Service) ListEvents(ctx context.Context, f EventFilter, start ListStart, limit int) (model.Page[model.Event], error) {
	req := model.ListEvents{Call: call(), Filter: model.Filter{Kinds: f.Kinds}, Limit: limit}
	switch {
	case start.Cursor != "" && start.From != "":
		return model.Page[model.Event]{}, model.NewError(model.Validation, "an event list starts at a cursor or at beginning or tail, not both")
	case start.Cursor != "":
		req.Start, req.After = model.StartCheckpoint, start.Cursor
	case start.From == StartBeginning || start.From == StartTail:
		req.Start = start.From
	default:
		return model.Page[model.Event]{}, model.NewError(model.Validation, fmt.Sprintf(
			"an event list starts at a cursor, beginning or tail; got %q", start.From))
	}
	if f.Inbox != "" {
		in, err := s.FindInbox(ctx, f.Inbox)
		if err != nil {
			return model.Page[model.Event]{}, err
		}
		req.Filter.Inboxes = []model.InboxRef{in.Ref}
	}
	got, err := s.Client.Events().List(ctx, req)
	if err != nil {
		return model.Page[model.Event]{}, err
	}
	out := model.Page[model.Event]{Next: got.Next, Complete: got.Complete, Items: make([]model.Event, 0, len(got.Items))}
	for _, ev := range got.Items {
		if s.seesAudience(ev.Audience) {
			out.Items = append(out.Items, ev)
		}
	}
	return out, nil
}

// GetEvent reads the event with ID id, as uma stored it, when the
// caller may read it. Nothing is written: no subscription is opened or
// moved.
//
// uma authorizes the call and the event, and answers an event the caller
// may not read exactly as one that does not exist; heo then passes the
// event through the caller's audience rule, as ListEvents does, and
// answers a refusal the same way. Every way an event is out of reach, a
// missing one included, is one not-found error with one message, so the
// answer never says whether the event is there.
func (s Service) GetEvent(ctx context.Context, id string) (model.Event, error) {
	if err := checkID("event", id); err != nil {
		return model.Event{}, err
	}
	ev, err := s.Client.Events().Get(ctx, model.Lookup[model.EventRef]{
		Call: call(), Ref: model.EventRef{Tenant: s.Tenant, ID: model.ID(id)},
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return model.Event{}, eventNotFound(id)
	case err != nil:
		return model.Event{}, err
	case !s.seesAudience(ev.Audience):
		return model.Event{}, eventNotFound(id)
	}
	return ev, nil
}

func eventNotFound(id string) error {
	return model.NewError(model.NotFound, fmt.Sprintf("event %s: not found", id))
}
