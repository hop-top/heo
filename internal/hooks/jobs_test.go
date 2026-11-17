package hooks_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/hooks"
	"hop.top/uma/model"
)

func waitJob(t *testing.T, j *hooks.Jobs, id string) hooks.Job {
	t.Helper()
	var got hooks.Job
	eventually(t, "job finished", func() bool {
		got, _ = j.Get(id)
		return got.State == hooks.JobDone || got.State == hooks.JobFailed
	})
	return got
}

// TestJobsRunOffTheCallersGoroutine: Submit returns a queued job at once,
// the work runs on its own goroutine, and a plugin's jobs run one at a
// time.
func TestJobsRunOffTheCallersGoroutine(t *testing.T) {
	j := hooks.NewJobs(context.Background())
	release := make(chan struct{})
	var mu sync.Mutex
	running, peak := 0, 0
	work := func(context.Context) (hooks.JobResult, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return hooks.JobResult{}, nil
	}
	a := j.Submit(hooks.Job{Plugin: "p", Kind: hooks.JobRetry}, work)
	b := j.Submit(hooks.Job{Plugin: "p", Kind: hooks.JobReplay}, work)
	if a.State != hooks.JobQueued || a.ID == "" || a.ID == b.ID {
		t.Fatalf("submitted %+v %+v", a, b)
	}
	close(release)
	waitJob(t, j, a.ID)
	waitJob(t, j, b.ID)
	if peak != 1 {
		t.Errorf("%d jobs of one plugin ran at once", peak)
	}
}

func TestJobsRecordTheOutcome(t *testing.T) {
	j := hooks.NewJobs(context.Background())
	rep := &hooks.ReplayReport{Delivered: 2}
	ok := waitJob(t, j, j.Submit(hooks.Job{Plugin: "p", Kind: hooks.JobReplay}, func(context.Context) (hooks.JobResult, error) {
		return hooks.JobResult{Replay: rep}, nil
	}).ID)
	if ok.State != hooks.JobDone || ok.Replay == nil || ok.Replay.Delivered != 2 || ok.FinishedAt.IsZero() {
		t.Errorf("done %+v", ok)
	}
	bad := waitJob(t, j, j.Submit(hooks.Job{Plugin: "q", Kind: hooks.JobRetry}, func(context.Context) (hooks.JobResult, error) {
		return hooks.JobResult{}, errors.New("boom")
	}).ID)
	if bad.State != hooks.JobFailed || bad.Error != "boom" {
		t.Errorf("failed %+v", bad)
	}
	if _, ok := j.Get("nope"); ok {
		t.Error("found a job that was never submitted")
	}
}

// TestLedgerEnqueueRefusesWhatCannotRun: a letter that is not dead or a
// plugin that is not running is refused at once, not as a failed job; a
// retry runs to a replayed record.
func TestLedgerEnqueue(t *testing.T) {
	ctx := context.Background()
	ev := event("e1", "c1")
	store := hooks.NewFileStore(t.TempDir())
	_ = store.Record(ctx, hooks.DeadLetter{Plugin: "p", Consumer: "c", Event: ev, Attempts: 1, Class: hooks.ClassCrash, FirstAt: time.Now(), At: time.Now()})
	r := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), Store: store, Fetch: fetcher(ev),
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil })})
	led := &hooks.Ledger{Store: store, Jobs: hooks.NewJobs(ctx),
		Runner: func(n string) (*hooks.Runner, bool) { return r, n == "p" }}
	if _, err := led.EnqueueRetry(ctx, "other", "e1"); err == nil {
		t.Error("enqueued a retry for a plugin that is not running")
	}
	if _, err := led.EnqueueRetry(ctx, "p", "nope"); !errors.Is(err, hooks.ErrNoDeadLetter) {
		t.Errorf("err %v", err)
	}
	job, err := led.EnqueueRetry(ctx, "p", "e1")
	if err != nil || job.State != hooks.JobQueued || job.Event != "e1" {
		t.Fatalf("%+v %v", job, err)
	}
	done := waitJob(t, led.Jobs, job.ID)
	if done.State != hooks.JobDone || done.Retry == nil || done.Retry.State != hooks.StateReplayed {
		t.Errorf("job %+v", done)
	}
	if _, err := led.EnqueueRetry(ctx, "p", "e1"); err == nil {
		t.Error("enqueued a retry of a letter that is no longer dead")
	}
	r2 := hooks.New(hooks.Options{Plugin: plugin("p", 1, 1), Source: newLog(), List: pager(ev),
		Sees: func(model.Audience) bool { return true },
		Exec: fnExec(func(context.Context, hooks.Envelope) (hooks.Result, error) { return hooks.Result{}, nil })})
	led.Runner = func(string) (*hooks.Runner, bool) { return r2, true }
	rj, err := led.EnqueueReplay(ctx, "p", "0", 10)
	if err != nil {
		t.Fatal(err)
	}
	if d := waitJob(t, led.Jobs, rj.ID); d.Replay == nil || d.Replay.Delivered != 1 {
		t.Errorf("replay job %+v", d)
	}
	if _, err := led.EnqueueReplay(ctx, "p", "", 10); err == nil {
		t.Error("enqueued a replay with no position")
	}
}
