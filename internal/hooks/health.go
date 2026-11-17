package hooks

import (
	"context"

	"hop.top/kit/go/runtime/bus"

	"hop.top/heo/internal/bridge"
)

// Health topics: heo.runtime.hooks.<action>, beside the bridge's
// heo.runtime.bridge.<action>, for desk scoped subscribers only. Each
// reports one event's fate with one plugin, so a consumer can alert on
// a plugin that retries or dead-letters.
const (
	sourceHealth   = "heo"
	categoryHealth = "runtime"
	objectHealth   = "hooks"
	// emitter names the hook runner on every event it publishes.
	emitter = "heo.hooks"

	// ActionRetry: an attempt failed and another is scheduled.
	ActionRetry = "retry"
	// ActionDeadLettered: the event was recorded as a dead letter and
	// the plugin's cursor moved on.
	ActionDeadLettered = "dead_lettered"
	// ActionRecovered: an event that had failed was handled.
	ActionRecovered = "recovered"
	// ActionReplayed: a dead letter was replayed and handled.
	ActionReplayed = "replayed"
	// ActionDiscarded: a dead letter was given up on.
	ActionDiscarded = "discarded"
)

// HealthTopic is the topic of a hook health event action.
func HealthTopic(action string) bus.Topic {
	return bus.Topic(sourceHealth + "." + categoryHealth + "." + objectHealth + "." + action)
}

// Health is a hook health event's payload: references, the failure
// class and counts, never the failure's text, which may carry a
// plugin's stderr.
type Health struct {
	Plugin   string `json:"plugin"`
	Consumer string `json:"consumer"`
	// Event is uma's event ID.
	Event string `json:"event"`
	// Attempt is the attempt the event is about, MaxAttempts the
	// plugin's budget.
	Attempt     int `json:"attempt,omitempty"`
	MaxAttempts int `json:"max_attempts,omitempty"`
	// Class is the failure class; empty on recovered and replayed.
	Class Class `json:"class,omitempty"`
	// FailureIndex is the result operation that failed, when one did.
	FailureIndex *int `json:"failure_index,omitempty"`
	// RetryInMs is the jittered wait before the next attempt, on retry.
	RetryInMs int64 `json:"retry_in_ms,omitempty"`
}

// emit publishes a health event; a bus that refuses it is not the
// runner's to report on the bus.
func emit(ctx context.Context, pub bridge.Publisher, action string, h Health) {
	if pub == nil {
		return
	}
	_ = pub.Publish(context.WithoutCancel(ctx), bus.NewEvent(HealthTopic(action), emitter, h))
}
