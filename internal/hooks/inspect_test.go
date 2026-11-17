package hooks_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"

	"hop.top/heo/internal/hooks"
	"hop.top/uma/model"
)

// fakeDispatch refuses verbs it lists and counts what it was asked to
// run.
type fakeDispatch struct {
	mu      sync.Mutex
	refuse  map[string]string
	ran     []hooks.Call
	failRun error
}

func (f *fakeDispatch) Check(c hooks.Call) error {
	if why, ok := f.refuse[c.Verb]; ok {
		return errors.New(why)
	}
	return nil
}

func (f *fakeDispatch) Dispatch(_ context.Context, _ model.ActorID, c hooks.Call) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, c)
	return f.failRun
}

func (f *fakeDispatch) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.ran) }

func opsExec(ops ...hooks.Operation) fnExec {
	return func(context.Context, hooks.Envelope) (hooks.Result, error) {
		return hooks.Result{Operations: ops, Message: "m"}, nil
	}
}

// TestTestShowsOperationsAndRefusalsAndAppliesNothing: a test run reads
// the stored event as the plugin, runs the plugin, and reports each
// operation with the key it would be applied under and whether Check
// would refuse it; the dispatcher never runs.
func TestTestShowsOperationsAndRefusalsAndAppliesNothing(t *testing.T) {
	ev := event("e1", "c1")
	d := &fakeDispatch{refuse: map[string]string{"plugin token mint": "hosts itself"}}
	ok := hooks.Operation{Verb: "message note", Args: []string{"c1"}, Flags: map[string]any{"text": "hi"}}
	bad := hooks.Operation{Verb: "plugin token mint", Args: []string{"x"}}
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), Fetch: fetcher(ev),
		Apply: &hooks.OpApplier{Dispatch: d}, Exec: opsExec(ok, bad),
	})
	rep, err := r.Test(context.Background(), "e1")
	if err != nil {
		t.Fatal(err)
	}
	if d.count() != 0 {
		t.Fatalf("a test applied %d operations", d.count())
	}
	if !rep.Admitted || !rep.Ok || rep.Applied || rep.Message != "m" || len(rep.Operations) != 2 {
		t.Fatalf("report %+v", rep)
	}
	if o := rep.Operations[0]; !o.Allowed || o.Refusal != "" || o.Key != hooks.OperationKey("p", "e1", ok) || o.Verb != "message note" {
		t.Errorf("op 0 %+v", o)
	}
	if o := rep.Operations[1]; o.Allowed || o.Refusal != "hosts itself" {
		t.Errorf("op 1 %+v", o)
	}
	if rep.WouldApply {
		t.Error("a result with a refused operation would be rejected whole")
	}
}

func TestTestWouldApplyWhenEveryOperationPasses(t *testing.T) {
	ev := event("e1", "c1")
	d := &fakeDispatch{}
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), Fetch: fetcher(ev),
		Apply: &hooks.OpApplier{Dispatch: d}, Exec: opsExec(hooks.Operation{Verb: "message note"}),
	})
	rep, err := r.Test(context.Background(), "e1")
	if err != nil || !rep.WouldApply || !rep.Ok {
		t.Fatalf("%+v %v", rep, err)
	}
	if d.count() != 0 {
		t.Errorf("a test dispatched %d operations", d.count())
	}
}

// TestTestReportsAFailingPlugin: the plugin's failure is the report's,
// with its class, not an error of the verb.
func TestTestReportsAFailingPlugin(t *testing.T) {
	ev := event("e1", "c1")
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), Fetch: fetcher(ev),
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) {
			return hooks.Result{}, hooks.Permanent(errors.New("bad"))
		}),
	})
	rep, err := r.Test(context.Background(), "e1")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Ok || rep.Failure == nil || rep.Failure.Class != hooks.ClassPermanent || !rep.Failure.Permanent || rep.Failure.Message != "bad" {
		t.Errorf("%+v", rep)
	}
}

// TestTestDoesNotRunForAnEventThePluginCannotSee: the audience rule and
// the filter apply as they do to a delivery, and the plugin is not run.
func TestTestDoesNotRunForAnEventThePluginCannotSee(t *testing.T) {
	ev := event("e1", "c1")
	var runs int
	exec := fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { runs++; return hooks.Result{}, nil })
	hidden := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Fetch: fetcher(ev), Exec: exec,
		Sees: func(model.Audience) bool { return false }})
	rep, err := hidden.Test(context.Background(), "e1")
	if err != nil || rep.Admitted || rep.Skipped == "" || runs != 0 {
		t.Fatalf("audience: %+v %v runs=%d", rep, err, runs)
	}
	filtered := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Fetch: fetcher(ev), Exec: exec,
		Match: func(context.Context, model.Event) (bool, error) { return false, nil }})
	rep, err = filtered.Test(context.Background(), "e1")
	if err != nil || rep.Admitted || rep.Skipped == "" || runs != 0 {
		t.Fatalf("filter: %+v %v runs=%d", rep, err, runs)
	}
	noFetch := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Exec: exec})
	if _, err := noFetch.Test(context.Background(), "e1"); err == nil {
		t.Error("test without an event reader")
	}
	if _, err := hidden.Test(context.Background(), "nope"); err == nil {
		t.Error("test of an event uma does not have")
	}
}

// pager is an event list over a fixed log: positions are indexes.
func pager(evs ...model.Event) func(context.Context, model.ListEvents) (model.Page[model.Event], error) {
	return pagerOf(100, evs...)
}

// pagerOf is pager with pages of at most size events.
func pagerOf(size int, evs ...model.Event) func(context.Context, model.ListEvents) (model.Page[model.Event], error) {
	return func(_ context.Context, r model.ListEvents) (model.Page[model.Event], error) {
		start := 0
		if r.Start == model.StartCheckpoint {
			n, err := strconv.Atoi(string(r.After))
			if err != nil {
				return model.Page[model.Event]{}, model.NewError(model.Validation, "bad cursor")
			}
			start = n
		}
		end := min(len(evs), start+min(max(r.Limit, 1), size))
		return model.Page[model.Event]{Items: evs[start:end], Next: model.Cursor(strconv.Itoa(end)), Complete: end == len(evs)}, nil
	}
}

func TestReplayFromWalksTheLogFromACursor(t *testing.T) {
	evs := []model.Event{event("e0", "c0"), event("e1", "c1"), event("e2", "c2"), event("e3", "c3")}
	var seen []string
	var mu sync.Mutex
	store := hooks.NewFileStore(t.TempDir())
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), List: pager(evs...), Store: store,
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			mu.Lock()
			seen = append(seen, e.EventID)
			mu.Unlock()
			if e.EventID == "e2" {
				return hooks.Result{}, errors.New("boom")
			}
			return hooks.Result{}, nil
		}),
		Sees:  func(model.Audience) bool { return true },
		Match: func(_ context.Context, ev model.Event) (bool, error) { return ev.ID != "e3", nil },
	})
	rep, err := r.ReplayFrom(context.Background(), "1", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != "e1" || seen[1] != "e2" {
		t.Errorf("plugin saw %v, want e1 and e2 (the cursor skips e0, the filter e3)", seen)
	}
	if rep.Delivered != 1 || rep.Failed != 1 || rep.Skipped != 1 || rep.Scanned != 3 || !rep.Complete || rep.Next != "4" {
		t.Errorf("report %+v", rep)
	}
	// A failure is a dead letter, retryable like any other.
	if rec, err := store.Get(context.Background(), "p", "e2"); err != nil || rec.State != hooks.StateDead {
		t.Errorf("dead letter %+v %v", rec, err)
	}
}

// TestReplayFromLimitLeavesACursorToContinue: a limit stops after that
// many events and Next resumes exactly after the last one.
func TestReplayFromLimitLeavesACursorToContinue(t *testing.T) {
	evs := []model.Event{event("e0", "c0"), event("e1", "c1"), event("e2", "c2")}
	var seen []string
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), List: pager(evs...), Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			seen = append(seen, e.EventID)
			return hooks.Result{}, nil
		}),
	})
	rep, err := r.ReplayFrom(context.Background(), "0", 2, false)
	if err != nil || rep.Delivered != 2 || rep.Complete || rep.Next != "2" {
		t.Fatalf("%+v %v", rep, err)
	}
	rep, err = r.ReplayFrom(context.Background(), string(rep.Next), 2, false)
	if err != nil || rep.Delivered != 1 || !rep.Complete || len(seen) != 3 {
		t.Fatalf("%+v %v seen %v", rep, err, seen)
	}
}

// TestReplayFromDryRunRunsNothing: it lists what would be replayed.
func TestReplayFromDryRunRunsNothing(t *testing.T) {
	evs := []model.Event{event("e0", "c0"), event("e1", "c1")}
	var runs int
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), List: pager(evs...), Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { runs++; return hooks.Result{}, nil }),
	})
	rep, err := r.ReplayFrom(context.Background(), "0", 10, true)
	if err != nil || runs != 0 || !rep.DryRun || len(rep.Events) != 2 || rep.Events[0].Outcome != hooks.ReplayWould {
		t.Fatalf("%+v %v runs=%d", rep, err, runs)
	}
}

func TestReplayFromNeedsAListerAndACursor(t *testing.T) {
	exec := fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil })
	if _, err := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Exec: exec}).ReplayFrom(context.Background(), "1", 1, false); err == nil {
		t.Error("replay without an event lister")
	}
	r := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), List: pager(), Exec: exec})
	if _, err := r.ReplayFrom(context.Background(), "", 1, false); err == nil {
		t.Error("replay without a cursor")
	}
}

// TestReplayFromFollowsPages: a log read in pages of one event is
// walked to its end, each page continuing at the last one's next.
func TestReplayFromFollowsPages(t *testing.T) {
	evs := []model.Event{event("e0", "c0"), event("e1", "c1"), event("e2", "c2")}
	var seen []string
	r := hooks.New(hooks.Options{
		Plugin: plugin("p", 1, 1), Source: newLog(), List: pagerOf(1, evs...), Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(_ context.Context, e hooks.Envelope) (hooks.Result, error) {
			seen = append(seen, e.EventID)
			return hooks.Result{}, nil
		}),
	})
	rep, err := r.ReplayFrom(context.Background(), hooks.FromBeginning, 10, false)
	if err != nil || rep.Delivered != 3 || !rep.Complete || len(seen) != 3 || seen[0] != "e0" || seen[2] != "e2" {
		t.Fatalf("%+v %v seen %v", rep, err, seen)
	}
}
