package daemon_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/hooks/hookstest"
	"hop.top/heo/internal/plugins"
	"hop.top/uma/model"
)

// crashSource is uma's event service with a crash switch: while Close is
// refused, a close never reaches uma, as when the process dies after the
// new consumer is open and before the old one is closed.
type crashSource struct {
	hooks.Source
	noClose atomic.Bool
}

func (c *crashSource) Close(ctx context.Context, ref model.Lookup[model.SubscriptionRef], cmd model.Command) (model.Acceptance, error) {
	if c.noClose.Load() {
		return model.Acceptance{}, errors.New("crash before close")
	}
	return c.Source.Close(ctx, ref, cmd)
}

// failSave is a lineage store that refuses its nth save: a crash after
// the new consumer is open and before the handover is recorded.
type failSave struct {
	hooks.LineageStore
	n, at atomic.Int32
}

func (f *failSave) SaveLineage(ctx context.Context, plugin string, l hooks.Lineage) error {
	if f.at.Load() != 0 && f.n.Add(1) >= f.at.Load() {
		return errors.New("crash before save")
	}
	return f.LineageStore.SaveLineage(ctx, plugin, l)
}

type handoverRig struct {
	*hooksRig
	t    *testing.T
	dir  string
	mu   sync.Mutex
	seen map[string]bool
}

func (h *handoverRig) delivered() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.seen) }

// runner runs a plugin with kinds over uma's real event service until
// the returned stop is called.
func (h *handoverRig) runner(kinds []string, wrap func(hooks.Source) hooks.Source, lin hooks.LineageStore) (stop func()) {
	h.t.Helper()
	p := plugins.Plugin{Name: "p", Command: "x", Mode: plugins.ModeExec, Timeout: time.Second, Filter: plugins.Filter{Kinds: kinds},
		Retry: plugins.Retry{MaxAttempts: 2, Backoff: plugins.Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond}}}
	ctx, cancel := context.WithCancel(h.t.Context())
	done := make(chan struct{})
	err := h.host.Use(ctx, func(rt daemon.Runtime) error {
		c, err := rt.Engine.For(model.Principal{Tenant: h.tenant, Actor: p.Actor()})
		if err != nil {
			return err
		}
		var src hooks.Source = c.Events()
		if wrap != nil {
			src = wrap(src)
		}
		if lin == nil {
			lin = hooks.NewFileStore(h.dir)
		}
		r := hooks.New(hooks.Options{
			Plugin: p, Source: src, Lineage: lin, Poll: 5 * time.Millisecond, RetryMin: time.Millisecond,
			Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
				h.mu.Lock()
				h.seen[e.EventID] = true
				h.mu.Unlock()
				return hooks.Result{}, nil
			}),
		})
		_ = r.Start(ctx) // a failure is retried by Run
		go func() { defer close(done); r.Run(ctx) }()
		return nil
	})
	if err != nil {
		cancel()
		h.t.Fatal(err)
	}
	return func() { cancel(); <-done }
}

type fnExec func(context.Context, hooks.Envelope) (hooks.Result, error)

func (f fnExec) Run(ctx context.Context, e hooks.Envelope) (hooks.Result, error) { return f(ctx, e) }
func (fnExec) Close() error                                                      { return nil }

func newHandoverRig(t *testing.T) *handoverRig {
	t.Helper()
	r := newHooksRig(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:p": {authz.RoleObserver}}, r.plugin("p", logPath(t), hookstest.ModeOK, nil))
	hold := r.run(5*time.Second, daemon.NameBridge)
	hold.ready(daemon.NameBridge)
	return &handoverRig{hooksRig: r, t: t, dir: t.TempDir(), seen: map[string]bool{}}
}

// tailCheckpoint is where a consumer opened now at the tail would stand.
func (h *handoverRig) tailCheckpoint(consumer string, kinds []string) model.Cursor {
	h.t.Helper()
	var cp model.Cursor
	err := h.host.Use(h.t.Context(), func(rt daemon.Runtime) error {
		c, err := rt.Engine.For(model.Principal{Tenant: h.tenant, Actor: "plugin:p"})
		if err != nil {
			return err
		}
		res, err := c.Events().Open(h.t.Context(), model.OpenSubscription{
			Command:  model.Command{Call: model.Call{Correlation: "test"}, Operation: model.OperationID("probe-" + consumer)},
			Consumer: consumer, Filter: model.Filter{Kinds: kinds}, Start: model.StartTail, PayloadMajor: model.PayloadVersion1,
		})
		cp = res.Subscription.Checkpoint
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return cp
}

var (
	kindsOld = []string{"message.created"}
	kindsNew = []string{"message.created", "conversation.changed"}
)

func (h *handoverRig) settle(want int) {
	h.t.Helper()
	eventually(h.t, "delivery", func() bool { return h.delivered() >= want })
	time.Sleep(50 * time.Millisecond)
}

// TestHandoverOverRealStore: a changed filter, with events committed
// while the plugin is down, loses nothing; the old consumer is closed
// (a consumer left open would keep its old checkpoint behind the tail).
func TestHandoverOverRealStore(t *testing.T) {
	h := newHandoverRig(t)
	stop := h.runner(kindsOld, nil, nil)
	h.start("a", "one")
	h.settle(1)
	per := h.delivered()
	stop()
	h.start("b", "two")
	h.start("c", "three")
	stop = h.runner(kindsNew, nil, nil)
	defer stop()
	h.settle(3 * per)
	eventually(t, "lineage cleared", func() bool {
		l, err := hooks.NewFileStore(h.dir).LoadLineage(context.Background(), "p")
		return err == nil && len(l.Retiring) == 0 && len(l.Active) == 1
	})
	oldName := plugins.ConsumerName("p", plugins.Filter{Kinds: kindsOld})
	stop()
	// Reopening a closed consumer revives it at the start asked for: the
	// tail. An open one keeps its checkpoint, behind the tail.
	if got, tail := h.tailCheckpoint(oldName, kindsOld), h.tailCheckpoint("probe-tail", kindsOld); got != tail {
		t.Errorf("old consumer still open at %q, tail is %q", got, tail)
	}
}

// TestHandoverCrashBeforeClose: the process dies after the new consumer
// is open and before the old is closed; the next start loses nothing,
// and closes the old.
func TestHandoverCrashBeforeClose(t *testing.T) {
	h := newHandoverRig(t)
	stop := h.runner(kindsOld, nil, nil)
	h.start("a", "one")
	h.settle(1)
	per := h.delivered()
	stop()
	h.start("b", "two")

	crash := &crashSource{}
	crash.noClose.Store(true)
	stop = h.runner(kindsNew, func(s hooks.Source) hooks.Source { crash.Source = s; return crash }, nil)
	h.settle(2 * per)
	l, _ := hooks.NewFileStore(h.dir).LoadLineage(context.Background(), "p")
	if len(l.Retiring) != 1 {
		t.Errorf("a close that failed left %+v", l)
	}
	stop()

	h.start("c", "three")
	stop = h.runner(kindsNew, nil, nil)
	defer stop()
	h.settle(3 * per)
	eventually(t, "the old consumer closed at the next start", func() bool {
		l, err := hooks.NewFileStore(h.dir).LoadLineage(context.Background(), "p")
		return err == nil && len(l.Retiring) == 0
	})
}

// TestHandoverCrashBeforeRecord: the process dies after the new consumer
// is open and before the handover is recorded; the next start finds both
// consumers tracked and loses nothing.
func TestHandoverCrashBeforeRecord(t *testing.T) {
	h := newHandoverRig(t)
	stop := h.runner(kindsOld, nil, nil)
	h.start("a", "one")
	h.settle(1)
	per := h.delivered()
	stop()
	h.start("b", "two")

	// The first save writes the new consumer as active; the second would
	// record the handover, and is the one that crashes.
	fs := &failSave{LineageStore: hooks.NewFileStore(h.dir)}
	fs.at.Store(2)
	stop = h.runner(kindsNew, nil, fs)
	time.Sleep(100 * time.Millisecond)
	stop()
	if got := h.delivered(); got != per {
		t.Fatalf("delivered %d before the handover was recorded, want %d", got, per)
	}
	h.start("c", "three")
	stop = h.runner(kindsNew, nil, nil)
	defer stop()
	h.settle(3 * per)
	eventually(t, "the old consumer closed", func() bool {
		l, err := hooks.NewFileStore(h.dir).LoadLineage(context.Background(), "p")
		return err == nil && len(l.Retiring) == 0 && len(l.Active) == 1
	})
}
