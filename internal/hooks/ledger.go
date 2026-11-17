package hooks

import (
	"context"
	"fmt"
	"time"

	"hop.top/heo/internal/bridge"
)

// Ledger is what the CLI's dead-letter verbs run on: the Store, and the
// runners that replay. A replay runs in the daemon, where the plugin's
// process, token and actor are.
type Ledger struct {
	Store Store
	// Runner returns the running runner of a plugin.
	Runner func(plugin string) (*Runner, bool)
	// Publisher receives the discarded health event; nil publishes none.
	Publisher bridge.Publisher
	// Jobs runs the retries and replays the CLI queues; nil refuses them.
	Jobs *Jobs
	// Now defaults to time.Now.
	Now func() time.Time
}

// List returns plugin's dead letters in state, oldest first; an empty
// plugin lists every plugin's, an empty state every state.
func (l *Ledger) List(ctx context.Context, plugin string, state DeadState) ([]DeadRecord, error) {
	return l.Store.List(ctx, plugin, state)
}

// Get returns one dead letter, or ErrNoDeadLetter.
func (l *Ledger) Get(ctx context.Context, plugin, event string) (DeadRecord, error) {
	return l.Store.Get(ctx, plugin, event)
}

// Replay runs the dead letter's event through its plugin again (see
// Runner.Replay). The plugin must be running.
func (l *Ledger) Replay(ctx context.Context, plugin, event string) (DeadRecord, error) {
	r, ok := l.runner(plugin)
	if !ok {
		return DeadRecord{}, fmt.Errorf("plugin %q is not running: its dead letters cannot be replayed", plugin)
	}
	return r.Replay(ctx, event)
}

// Discard gives up on a dead letter: it stays listed as discarded, and
// its event is never delivered again.
func (l *Ledger) Discard(ctx context.Context, plugin, event string) (DeadRecord, error) {
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	rec, err := l.Store.Resolve(ctx, plugin, event, StateDiscarded, now())
	if err != nil {
		return rec, err
	}
	emit(ctx, l.Publisher, ActionDiscarded, Health{Plugin: rec.Plugin, Consumer: rec.Consumer, Event: rec.Event, Attempt: rec.Attempts, Class: rec.Class, FailureIndex: rec.FailureIndex})
	return rec, nil
}

func (l *Ledger) runner(plugin string) (*Runner, bool) {
	if l.Runner == nil {
		return nil, false
	}
	return l.Runner(plugin)
}

// Test runs plugin on one stored event and reports its result without
// applying it (see Runner.Test). The plugin must be running.
func (l *Ledger) Test(ctx context.Context, plugin, event string) (TestReport, error) {
	r, ok := l.runner(plugin)
	if !ok {
		return TestReport{}, fmt.Errorf("plugin %q is not running: it cannot be tested", plugin)
	}
	return r.Test(ctx, event)
}

// ReplayFrom delivers plugin's events from a log position again (see
// Runner.ReplayFrom). The plugin must be running.
func (l *Ledger) ReplayFrom(ctx context.Context, plugin, from string, limit int, dry bool) (ReplayReport, error) {
	r, ok := l.runner(plugin)
	if !ok {
		return ReplayReport{}, fmt.Errorf("plugin %q is not running: its events cannot be replayed", plugin)
	}
	return r.ReplayFrom(ctx, from, limit, dry)
}
