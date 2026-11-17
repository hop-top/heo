package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/uma/model"
)

// EnvelopeVersion is the envelope's and the result's major version.
const EnvelopeVersion = 1

// Envelope is what a plugin receives for one event: references only,
// never content (docs/hook-envelope.schema.json).
type Envelope struct {
	HeoVersion  int            `json:"heo_version"`
	Plugin      string         `json:"plugin"`
	EventID     string         `json:"event_id"`
	Kind        string         `json:"kind"`
	Attempt     int            `json:"attempt"`
	DeliveredAt time.Time      `json:"delivered_at"`
	Event       bridge.Payload `json:"event"`
}

// NewEnvelope is the envelope of ev for plugin, the attempt-th time.
func NewEnvelope(plugin string, ev model.Event, attempt int, now time.Time) Envelope {
	_, p := bridge.Map(ev)
	return Envelope{
		HeoVersion: EnvelopeVersion, Plugin: plugin, EventID: string(ev.ID), Kind: ev.Kind,
		Attempt: attempt, DeliveredAt: now.UTC(), Event: p,
	}
}

// Operation is one step a plugin asks for, in heo's command vocabulary.
type Operation struct {
	Verb           string         `json:"verb"`
	Args           []string       `json:"args,omitempty"`
	Flags          map[string]any `json:"flags,omitempty"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`
}

// Result is what a plugin answers for one envelope
// (docs/hook-result.schema.json).
type Result struct {
	HeoVersion int         `json:"heo_version,omitempty"`
	EventID    string      `json:"event_id,omitempty"`
	Operations []Operation `json:"operations"`
	Message    string      `json:"message,omitempty"`
}

// ParseResult reads a plugin's output. Empty output is no operations.
// The result is held to docs/hook-result.schema.json: no field it does
// not name, an operations list, numbers kept as written.
func ParseResult(b []byte) (Result, error) {
	var r Result
	if len(bytes.TrimSpace(b)) == 0 {
		return r, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return Result{}, fmt.Errorf("result is not JSON: %w", err)
	}
	if _, ok := top["operations"]; !ok {
		return Result{}, errors.New("result has no operations list")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return Result{}, fmt.Errorf("result does not match the schema: %w", err)
	}
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	return r, nil
}

// Failure is why an event was not handled. Permanent failures are not
// retried: the event is dead-lettered at once. Class says what kind of
// failure it was; unset, Classify infers it.
type Failure struct {
	Permanent bool
	Class     Class
	Reason    string
}

func (f *Failure) Error() string { return f.Reason }

// Permanent wraps err as a failure that is not retried.
func Permanent(err error) error {
	return &Failure{Permanent: true, Class: ClassPermanent, Reason: err.Error()}
}

// IsPermanent reports whether err is a permanent failure.
func IsPermanent(err error) bool {
	var f *Failure
	return errors.As(err, &f) && f.Permanent
}

// Executor hands one envelope to a plugin and returns its result, or a
// *Failure. Run is called from several goroutines at once only for a
// plugin whose concurrency is above 1.
type Executor interface {
	Run(ctx context.Context, env Envelope) (Result, error)
	// Close ends any process the executor keeps.
	Close() error
}

// Delivery is what an Applier is given: the event, the plugin that
// answered it and the attempt that did.
type Delivery struct {
	Plugin  string
	Event   model.Event
	Attempt int
	// DryRun is plugins.<name>.dry_run: record what would be applied,
	// apply nothing.
	DryRun bool
}

// Applier applies a plugin's result, in order, as the plugin's actor,
// stopping at the first failure. An error wrapped by Permanent
// dead-letters the event; any other is retried.
type Applier interface {
	Apply(ctx context.Context, d Delivery, r Result) error
}

// DiscardApplier applies nothing: results are accepted and dropped.
type DiscardApplier struct{}

// Apply implements Applier.
func (DiscardApplier) Apply(context.Context, Delivery, Result) error { return nil }

// DeadLetter is the record of an event a plugin did not handle, as the
// runner hands it to a DeadLetterer. A Store keeps it as a DeadRecord,
// references only.
type DeadLetter struct {
	Plugin   string
	Consumer string
	Event    model.Event
	// Attempts counts every attempt made, across restarts and replays.
	Attempts int
	Reason   string
	// Class is the failure class of the last attempt.
	Class Class
	// FailureIndex is the index of the operation that stopped the last
	// attempt, when it was one of the result's operations.
	FailureIndex *int
	// Permanent is set when the plugin said so, unset when it ran out
	// of attempts.
	Permanent bool
	// FirstAt is when the event was first attempted, At when the last
	// attempt failed.
	FirstAt time.Time
	At      time.Time
	// Replay is set when the failure was that of a replay of an event
	// already dead-lettered.
	Replay bool
}

// DeadLetterer records a dead letter. Until it returns nil the event's
// batch is not acknowledged.
type DeadLetterer interface {
	Record(ctx context.Context, d DeadLetter) error
}

// Logger receives a runner's reports; kit's console logger satisfies
// it.
type Logger interface {
	Info(msg any, keyvals ...any)
	Error(msg any, keyvals ...any)
}

// Source is the part of uma's Events service a runner reads with, as
// the plugin's own actor. uma.Events satisfies it.
type Source interface {
	Open(context.Context, model.OpenSubscription) (model.SubscriptionResult, error)
	Poll(context.Context, model.Poll) (model.EventBatch, error)
	Ack(context.Context, model.Acknowledge) (model.SubscriptionResult, error)
	Close(context.Context, model.Lookup[model.SubscriptionRef], model.Command) (model.Acceptance, error)
}
