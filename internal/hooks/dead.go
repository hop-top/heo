package hooks

import (
	"context"
	"errors"
	"fmt"
	"time"

	"hop.top/uma/model"
)

// DeadState is where a dead letter stands.
type DeadState string

// Dead letter states.
const (
	// StateDead: the plugin did not handle the event and nobody has
	// replayed or discarded it.
	StateDead DeadState = "dead"
	// StateReplayed: a replay handled the event; the record stays as
	// history.
	StateReplayed DeadState = "replayed"
	// StateDiscarded: an operator gave the event up; the record stays as
	// history.
	StateDiscarded DeadState = "discarded"
)

// ErrNoDeadLetter is returned for a plugin and event with no dead
// letter.
var ErrNoDeadLetter = errors.New("no such dead letter")

// DeadRecord is a dead letter as a Store keeps it. It holds references
// only, never an event's content: replay reads the event again from
// uma, as the plugin's actor, so redaction and audience apply to it as
// they do to any delivery.
type DeadRecord struct {
	Plugin   string    `json:"plugin" yaml:"plugin"`
	Consumer string    `json:"consumer" yaml:"consumer"`
	State    DeadState `json:"state" yaml:"state"`
	// Event is uma's event ID; Kind, Resource and Inbox say what it was
	// about.
	Event      string    `json:"event" yaml:"event"`
	Kind       string    `json:"kind" yaml:"kind"`
	Resource   string    `json:"resource,omitempty" yaml:"resource,omitempty"`
	Inbox      string    `json:"inbox,omitempty" yaml:"inbox,omitempty"`
	RecordedAt time.Time `json:"recorded_at,omitzero" yaml:"recorded_at,omitempty"`
	// Attempts counts every attempt made, across restarts and replays.
	Attempts int `json:"attempts" yaml:"attempts"`
	// Class and Message are the last attempt's failure class and text.
	Class   Class  `json:"class" yaml:"class"`
	Message string `json:"message" yaml:"message"`
	// FailureIndex is the index of the result's operation that failed,
	// when one did.
	FailureIndex *int `json:"failure_index,omitempty" yaml:"failure_index,omitempty"`
	// Permanent is set when the plugin said so, unset when it ran out
	// of attempts.
	Permanent bool `json:"permanent" yaml:"permanent"`
	// FirstAt is when the event was first attempted, LastAt when the
	// last attempt failed; ResolvedAt when it was replayed or discarded.
	FirstAt    time.Time `json:"first_attempt_at" yaml:"first_attempt_at"`
	LastAt     time.Time `json:"last_attempt_at" yaml:"last_attempt_at"`
	ResolvedAt time.Time `json:"resolved_at,omitzero" yaml:"resolved_at,omitempty"`
	// Replays counts the replays that failed again.
	Replays int `json:"replays" yaml:"replays"`
}

func newRecord(d DeadLetter) DeadRecord {
	ev := d.Event
	return DeadRecord{
		Plugin: d.Plugin, Consumer: d.Consumer, State: StateDead,
		Event: string(ev.ID), Kind: ev.Kind, Resource: string(ev.Resource.Kind) + ":" + string(ev.Resource.ID),
		Inbox: string(ev.Inbox), RecordedAt: ev.RecordedAt,
		Attempts: d.Attempts, Class: d.Class, Message: d.Reason, FailureIndex: d.FailureIndex, Permanent: d.Permanent,
		FirstAt: d.FirstAt, LastAt: d.At,
	}
}

// merge is r on top of an earlier record for the same event: the first
// attempt time stays, and a replay that failed again is counted.
func (r DeadRecord) merge(prev DeadRecord, replay bool) DeadRecord {
	if !prev.FirstAt.IsZero() && (r.FirstAt.IsZero() || prev.FirstAt.Before(r.FirstAt)) {
		r.FirstAt = prev.FirstAt
	}
	r.Replays = prev.Replays
	if replay {
		r.Replays++
	}
	return r
}

// AttemptState is what survives a restart about an event in retry: how
// many attempts were made and when the first was.
type AttemptState struct {
	Attempts int       `json:"attempts"`
	FirstAt  time.Time `json:"first_attempt_at"`
}

// Attempts keeps the attempt count of events in retry, so a restart
// does not give an event a fresh budget. uma's redelivery of an
// unacknowledged batch carries no count of its own to rely on.
type Attempts interface {
	// Load returns the state of plugin's event, zero when none.
	Load(ctx context.Context, plugin, event string) (AttemptState, error)
	Save(ctx context.Context, plugin, event string, s AttemptState) error
	// Clear forgets the event: it was handled or dead-lettered.
	Clear(ctx context.Context, plugin, event string) error
}

// Store is the durable per-plugin dead-letter list and attempt counts.
// Record is an upsert: a record for an event already held keeps its
// first attempt time (and counts a failed replay when the letter says
// it was one), and one that was replayed or discarded is dead again.
type Store interface {
	DeadLetterer
	Attempts
	// List returns plugin's records in state (all when empty), oldest
	// first by first attempt, then event ID. An empty plugin lists
	// every plugin's.
	List(ctx context.Context, plugin string, state DeadState) ([]DeadRecord, error)
	// Get returns the record, or ErrNoDeadLetter.
	Get(ctx context.Context, plugin, event string) (DeadRecord, error)
	// Resolve moves a dead record to StateReplayed or StateDiscarded at
	// the time given. It returns ErrNoDeadLetter, or an error naming the
	// state when the record was not dead.
	Resolve(ctx context.Context, plugin, event string, state DeadState, at time.Time) (DeadRecord, error)
}

func notDead(r DeadRecord) error {
	return fmt.Errorf("dead letter %s/%s is %s, not dead", r.Plugin, r.Event, r.State)
}

// Fetch reads an event by ID as the plugin's actor, so that its
// audience rule applies: replay never hands a plugin what it could not
// have read the first time.
type Fetch func(ctx context.Context, id model.ID) (model.Event, error)
