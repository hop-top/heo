package hooks_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/kit/go/runtime/bus"

	"hop.top/heo/internal/hooks"
	"hop.top/uma/model"
)

type recorder struct {
	mu sync.Mutex
	ev []bus.Event
}

func (r *recorder) Publish(_ context.Context, e bus.Event) error {
	r.mu.Lock()
	r.ev = append(r.ev, e)
	r.mu.Unlock()
	return nil
}

func (r *recorder) topics() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.ev {
		out = append(out, string(e.Topic))
	}
	return out
}

func (r *recorder) health(i int) hooks.Health {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ev[i].Payload.(hooks.Health)
}

func fetcher(evs ...model.Event) hooks.Fetch {
	return func(_ context.Context, id model.ID) (model.Event, error) {
		for _, e := range evs {
			if e.ID == id {
				return e, nil
			}
		}
		return model.Event{}, model.NewError(model.NotFound, "no event")
	}
}

func TestFileStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := hooks.NewFileStore(dir)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	idx := 1
	ev := event("e1", "c1")
	dl := hooks.DeadLetter{Plugin: "p", Consumer: "heo-hook-p", Event: ev, Attempts: 3, Reason: "boom", Class: hooks.ClassTimeout,
		FailureIndex: &idx, FirstAt: t0, At: t0.Add(time.Minute)}
	if err := s.Record(ctx, dl); err != nil {
		t.Fatal(err)
	}
	dl2 := dl
	dl2.Event = event("e0", "c1")
	dl2.FirstAt = t0.Add(-time.Hour)
	if err := s.Record(ctx, dl2); err != nil {
		t.Fatal(err)
	}
	other := dl
	other.Plugin = "q"
	if err := s.Record(ctx, other); err != nil {
		t.Fatal(err)
	}

	// A fresh store on the same directory sees it all: it is durable.
	s = hooks.NewFileStore(dir)
	got, err := s.Get(ctx, "p", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != hooks.StateDead || got.Attempts != 3 || got.Class != hooks.ClassTimeout || got.Message != "boom" ||
		got.FailureIndex == nil || *got.FailureIndex != 1 || got.Kind != model.EventMessageCreated || got.Event != "e1" ||
		!got.FirstAt.Equal(t0) || !got.LastAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("record %+v", got)
	}
	list, err := s.List(ctx, "p", "")
	if err != nil || len(list) != 2 || list[0].Event != "e0" || list[1].Event != "e1" {
		t.Errorf("list of p: %+v %v", list, err)
	}
	if all, _ := s.List(ctx, "", ""); len(all) != 3 {
		t.Errorf("list of all: %d", len(all))
	}
	if _, err := s.Get(ctx, "p", "nope"); !errors.Is(err, hooks.ErrNoDeadLetter) {
		t.Errorf("err %v", err)
	}

	// Resolve moves a dead record once.
	res, err := s.Resolve(ctx, "p", "e1", hooks.StateDiscarded, t0.Add(time.Hour))
	if err != nil || res.State != hooks.StateDiscarded || !res.ResolvedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("resolve %+v %v", res, err)
	}
	if _, err := s.Resolve(ctx, "p", "e1", hooks.StateReplayed, t0); err == nil {
		t.Error("a discarded letter was resolved again")
	}
	if dead, _ := s.List(ctx, "p", hooks.StateDead); len(dead) != 1 || dead[0].Event != "e0" {
		t.Errorf("dead list %+v", dead)
	}

	// A failed replay is the same record, first attempt kept, counted.
	again := dl
	again.Attempts, again.At, again.FirstAt, again.Replay = 4, t0.Add(2*time.Hour), t0.Add(2*time.Hour), true
	if err := s.Record(ctx, again); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, "p", "e1")
	if got.State != hooks.StateDead || got.Attempts != 4 || got.Replays != 1 || !got.FirstAt.Equal(t0) || !got.ResolvedAt.IsZero() {
		t.Errorf("after a failed replay: %+v", got)
	}

	// Names that are not file names are refused or escaped, never walked.
	if err := s.Record(ctx, hooks.DeadLetter{Plugin: "..", Event: ev}); err == nil {
		t.Error("a plugin named .. was accepted")
	}
	if err := s.Record(ctx, hooks.DeadLetter{Plugin: "../x", Event: ev}); err != nil {
		t.Errorf("escaped plugin name: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x")); err == nil {
		t.Error("a record was written outside the store")
	}
	if err := s.Save(ctx, "p", "a/b", hooks.AttemptState{Attempts: 1}); err != nil {
		t.Errorf("event ID with a slash: %v", err)
	}
	if a, _ := s.Load(ctx, "p", "a/b"); a.Attempts != 1 {
		t.Errorf("escaped ID did not round trip: %+v", a)
	}
}

func TestFileStoreAttempts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := hooks.NewFileStore(dir)
	if a, err := s.Load(ctx, "p", "e1"); err != nil || a.Attempts != 0 {
		t.Fatalf("empty: %+v %v", a, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.Save(ctx, "p", "e1", hooks.AttemptState{Attempts: 2, FirstAt: now}); err != nil {
		t.Fatal(err)
	}
	if a, _ := hooks.NewFileStore(dir).Load(ctx, "p", "e1"); a.Attempts != 2 || !a.FirstAt.Equal(now) {
		t.Errorf("not durable: %+v", a)
	}
	if err := s.Clear(ctx, "p", "e1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Load(ctx, "p", "e1"); a.Attempts != 0 {
		t.Errorf("not cleared: %+v", a)
	}
	if err := s.Clear(ctx, "p", "e1"); err != nil {
		t.Errorf("clearing nothing: %v", err)
	}
	// Attempt files are not dead letters.
	_ = s.Save(ctx, "p", "e2", hooks.AttemptState{Attempts: 1})
	if l, _ := s.List(ctx, "p", ""); len(l) != 0 {
		t.Errorf("attempts listed as dead letters: %+v", l)
	}
}

// TestDeadLetterRecordsTheFailure: the stored record says what failed,
// how, at which operation, and for how long it was tried; health events
// tell the story in order, by class and never by text.
func TestDeadLetterRecordsTheFailure(t *testing.T) {
	src := newLog(event("e1", "c1"), event("e2", "c1"))
	store := hooks.NewFileStore(t.TempDir())
	pub := &recorder{}
	var calls atomic.Int32
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src, Store: store, Publisher: pub,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			if e.EventID == "e1" {
				calls.Add(1)
			}
			return hooks.Result{}, nil
		}),
		Apply: fnApply(func(_ context.Context, d hooks.Delivery, _ hooks.Result) error {
			if d.Event.ID == "e1" {
				return &hooks.OpError{Index: 1, Verb: "message note", Applied: 1, Err: errors.New("secret stderr text")}
			}
			return nil
		}),
	})
	eventually(t, "both events passed", func() bool { return src.acked() == 2 })
	rec, err := store.Get(context.Background(), "p", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Attempts != 3 || rec.Class != hooks.ClassOperation || rec.FailureIndex == nil || *rec.FailureIndex != 1 || rec.Permanent ||
		rec.FirstAt.IsZero() || rec.LastAt.Before(rec.FirstAt) || rec.State != hooks.StateDead {
		t.Errorf("record %+v", rec)
	}
	if a, _ := store.Load(context.Background(), "p", "e1"); a.Attempts != 0 {
		t.Errorf("attempt count left behind: %+v", a)
	}
	want := []string{"heo.runtime.hooks.retry", "heo.runtime.hooks.retry", "heo.runtime.hooks.dead_lettered"}
	if got := pub.topics(); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("topics %v, want %v", got, want)
	}
	if h := pub.health(0); h.Plugin != "p" || h.Event != "e1" || h.Attempt != 1 || h.MaxAttempts != 3 || h.Class != hooks.ClassOperation || h.RetryInMs < 0 {
		t.Errorf("retry payload %+v", h)
	}
	if h := pub.health(2); h.Attempt != 3 || h.Class != hooks.ClassOperation || h.FailureIndex == nil || *h.FailureIndex != 1 {
		t.Errorf("dead_lettered payload %+v", h)
	}
	if st := r.Status(); st.DeadLettered != 1 || st.Delivered != 1 {
		t.Errorf("status %+v", st)
	}
}

// TestRecoveredHealth: an event that failed and was then handled is
// reported as recovered, and one handled first time is not reported.
func TestRecoveredHealth(t *testing.T) {
	src := newLog(event("e1", "c1"), event("e2", "c1"))
	pub := &recorder{}
	var n atomic.Int32
	run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src, Publisher: pub,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			if e.EventID == "e1" && n.Add(1) == 1 {
				return hooks.Result{}, &hooks.Failure{Class: hooks.ClassTimeout, Reason: "timed out"}
			}
			return hooks.Result{}, nil
		}),
	})
	eventually(t, "acknowledged", func() bool { return src.acked() == 2 })
	got := pub.topics()
	if len(got) != 2 || got[0] != "heo.runtime.hooks.retry" || got[1] != "heo.runtime.hooks.recovered" {
		t.Fatalf("topics %v", got)
	}
	if h := pub.health(0); h.Class != hooks.ClassTimeout {
		t.Errorf("retry payload %+v", h)
	}
	if h := pub.health(1); h.Attempt != 2 || h.Class != "" {
		t.Errorf("recovered payload %+v", h)
	}
}

// TestAttemptsSurviveRestart: the attempts an event used before the
// daemon stopped count after it starts; the budget is not renewed.
func TestAttemptsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	store := hooks.NewFileStore(t.TempDir())
	first := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := store.Save(ctx, "p", "e1", hooks.AttemptState{Attempts: 2, FirstAt: first}); err != nil {
		t.Fatal(err)
	}
	src := newLog(event("e1", "c1"))
	var calls atomic.Int32
	var seenAttempt atomic.Int64
	run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src, Store: store,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			calls.Add(1)
			seenAttempt.Store(int64(e.Attempt))
			return hooks.Result{}, &hooks.Failure{Class: hooks.ClassCrash, Reason: "exit 3"}
		}),
	})
	eventually(t, "dead-lettered and acknowledged", func() bool { return src.acked() == 1 })
	rec, err := store.Get(ctx, "p", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || seenAttempt.Load() != 3 || rec.Attempts != 3 || !rec.FirstAt.Equal(first) || rec.Class != hooks.ClassCrash {
		t.Errorf("calls %d attempt %d record %+v", calls.Load(), seenAttempt.Load(), rec)
	}
}

// TestAttemptWrittenAhead: a retry is counted before it runs, so an
// attempt that takes the daemon down with it is not free.
func TestAttemptWrittenAhead(t *testing.T) {
	ctx := context.Background()
	store := hooks.NewFileStore(t.TempDir())
	src := newLog(event("e1", "c1"))
	var n atomic.Int32
	atSecond := make(chan hooks.AttemptState, 1)
	run(t, hooks.Options{
		Plugin: plugin("p", 1, 3), Source: src, Store: store,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			if n.Add(1) == 2 {
				a, _ := store.Load(ctx, "p", "e1")
				atSecond <- a
			}
			return hooks.Result{}, nil
		}),
		Apply: fnApply(func(context.Context, hooks.Delivery, hooks.Result) error {
			if n.Load() == 1 {
				return errors.New("busy")
			}
			return nil
		}),
	})
	select {
	case a := <-atSecond:
		if a.Attempts != 2 {
			t.Errorf("count while attempt 2 ran: %+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no second attempt")
	}
}

// TestReplay: a dead letter is retried from the CLI side through the
// ledger: the event is read again, run with the attempt after the last,
// and the record resolves; the keys stay those of the first run.
func TestReplay(t *testing.T) {
	ctx := context.Background()
	ev := event("e1", "c1")
	store := hooks.NewFileStore(t.TempDir())
	pub := &recorder{}
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	var keys []string
	var attempts []int
	src := newLog(ev)
	r := run(t, hooks.Options{
		Plugin: plugin("p", 1, 1), Source: src, Store: store, Publisher: pub, Fetch: fetcher(ev),
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) {
			return hooks.Result{Operations: []hooks.Operation{{Verb: "message note", Args: []string{"x"}}}}, nil
		}),
		Apply: fnApply(func(_ context.Context, d hooks.Delivery, res hooks.Result) error {
			mu.Lock()
			defer mu.Unlock()
			keys = append(keys, hooks.OperationKey(d.Plugin, d.Event.ID, res.Operations[0]))
			attempts = append(attempts, d.Attempt)
			if fail.Load() {
				return &hooks.OpError{Index: 0, Verb: "message note", Err: errors.New("busy")}
			}
			return nil
		}),
	})
	eventually(t, "dead-lettered", func() bool { return src.acked() == 1 })
	led := &hooks.Ledger{Store: store, Runner: func(n string) (*hooks.Runner, bool) { return r, n == "p" }, Publisher: pub}

	// Still failing: the record stays dead, with the new attempt.
	rec, err := led.Replay(ctx, "p", "e1")
	if err == nil || rec.State != hooks.StateDead || rec.Attempts != 2 || rec.Replays != 1 || rec.Class != hooks.ClassOperation {
		t.Fatalf("failed replay: %+v %v", rec, err)
	}
	// Fixed: handled, resolved, reported.
	fail.Store(false)
	rec, err = led.Replay(ctx, "p", "e1")
	if err != nil || rec.State != hooks.StateReplayed || rec.ResolvedAt.IsZero() {
		t.Fatalf("replay: %+v %v", rec, err)
	}
	mu.Lock()
	if len(keys) != 3 || keys[0] != keys[1] || keys[1] != keys[2] || attempts[2] != 3 {
		t.Errorf("keys %v attempts %v", keys, attempts)
	}
	mu.Unlock()
	got := pub.topics()
	if got[len(got)-1] != "heo.runtime.hooks.replayed" {
		t.Errorf("topics %v", got)
	}
	// Only a dead letter is replayed.
	if _, err := led.Replay(ctx, "p", "e1"); err == nil {
		t.Error("a replayed letter was replayed again")
	}
	if _, err := led.Replay(ctx, "p", "nope"); !errors.Is(err, hooks.ErrNoDeadLetter) {
		t.Errorf("err %v", err)
	}
	if _, err := led.Replay(ctx, "other", "e1"); err == nil {
		t.Error("replayed through a plugin that is not running")
	}
}

func TestReplayNeedsTheEvent(t *testing.T) {
	ctx := context.Background()
	store := hooks.NewFileStore(t.TempDir())
	_ = store.Record(ctx, hooks.DeadLetter{Plugin: "p", Event: event("e1", "c1"), Attempts: 1, Class: hooks.ClassCrash, FirstAt: time.Now(), At: time.Now()})
	noFetch := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Store: store, Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil })})
	if _, err := noFetch.Replay(ctx, "e1"); err == nil {
		t.Error("replay without an event reader")
	}
	gone := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Store: store, Fetch: fetcher(), Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil })})
	if _, err := gone.Replay(ctx, "e1"); err == nil {
		t.Error("replay of an event uma no longer has")
	}
	if rec, _ := store.Get(ctx, "p", "e1"); rec.State != hooks.StateDead {
		t.Errorf("record %+v", rec)
	}
}

func TestDiscard(t *testing.T) {
	ctx := context.Background()
	store := hooks.NewFileStore(t.TempDir())
	pub := &recorder{}
	_ = store.Record(ctx, hooks.DeadLetter{Plugin: "p", Consumer: "heo-hook-p", Event: event("e1", "c1"), Attempts: 5, Class: hooks.ClassTimeout, FirstAt: time.Now(), At: time.Now()})
	led := &hooks.Ledger{Store: store, Publisher: pub}
	rec, err := led.Discard(ctx, "p", "e1")
	if err != nil || rec.State != hooks.StateDiscarded {
		t.Fatalf("%+v %v", rec, err)
	}
	if got := pub.topics(); len(got) != 1 || got[0] != "heo.runtime.hooks.discarded" {
		t.Errorf("topics %v", got)
	}
	if _, err := led.Discard(ctx, "p", "e1"); err == nil {
		t.Error("discarded twice")
	}
	if l, _ := led.List(ctx, "p", hooks.StateDead); len(l) != 0 {
		t.Errorf("a discarded letter is still dead: %+v", l)
	}
	if l, _ := led.List(ctx, "", ""); len(l) != 1 || l[0].State != hooks.StateDiscarded {
		t.Errorf("history: %+v", l)
	}
}
