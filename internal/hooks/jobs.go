package hooks

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// JobState is where a job stands.
type JobState string

// Job states.
const (
	JobQueued  JobState = "queued"
	JobRunning JobState = "running"
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
)

// Job kinds.
const (
	JobRetry  = "retry"
	JobReplay = "replay"
)

// maxJobs bounds the finished jobs kept; the oldest go first.
const maxJobs = 200

// JobResult is what a finished job produced: a retry's record, a
// replay's report.
type JobResult struct {
	Retry  *DeadRecord
	Replay *ReplayReport
}

// Job is one retry or replay handed to the daemon. It runs on its own
// goroutine, outside the command that queued it: running it inside the
// command would deadlock, since the operations a plugin returns run
// through the command tree the command holds. Jobs live in memory and
// end with the daemon.
type Job struct {
	ID     string   `json:"id" yaml:"id"`
	Plugin string   `json:"plugin" yaml:"plugin"`
	Kind   string   `json:"kind" yaml:"kind"`
	State  JobState `json:"state" yaml:"state"`
	// Event is a retry's event; From and Limit a replay's range.
	Event string `json:"event,omitempty" yaml:"event,omitempty"`
	From  string `json:"from,omitempty" yaml:"from,omitempty"`
	Limit int    `json:"limit,omitempty" yaml:"limit,omitempty"`
	// Error is why a failed job failed.
	Error      string        `json:"error,omitempty" yaml:"error,omitempty"`
	Retry      *DeadRecord   `json:"retry,omitempty" yaml:"retry,omitempty"`
	Replay     *ReplayReport `json:"replay,omitempty" yaml:"replay,omitempty"`
	QueuedAt   time.Time     `json:"queued_at" yaml:"queued_at"`
	StartedAt  time.Time     `json:"started_at,omitzero" yaml:"started_at,omitempty"`
	FinishedAt time.Time     `json:"finished_at,omitzero" yaml:"finished_at,omitempty"`
}

// Jobs runs and remembers jobs. A plugin's jobs run one at a time.
type Jobs struct {
	ctx context.Context

	mu    sync.Mutex
	seq   int
	byID  map[string]*Job
	order []string
	lanes map[string]*sync.Mutex
}

// NewJobs returns a Jobs whose work ends with ctx.
func NewJobs(ctx context.Context) *Jobs {
	return &Jobs{ctx: ctx, byID: map[string]*Job{}, lanes: map[string]*sync.Mutex{}}
}

// Submit queues work as a job of j's plugin and kind and returns it,
// queued, at once.
func (s *Jobs) Submit(j Job, work func(context.Context) (JobResult, error)) Job {
	s.mu.Lock()
	s.seq++
	j.ID = "hj-" + strconv.FormatInt(time.Now().Unix(), 36) + "-" + strconv.Itoa(s.seq)
	j.State, j.QueuedAt = JobQueued, time.Now().UTC()
	jp := &j
	s.byID[j.ID] = jp
	s.order = append(s.order, j.ID)
	for len(s.order) > maxJobs {
		old := s.byID[s.order[0]]
		if old != nil && (old.State == JobQueued || old.State == JobRunning) {
			break
		}
		delete(s.byID, s.order[0])
		s.order = s.order[1:]
	}
	lane := s.lanes[j.Plugin]
	if lane == nil {
		lane = &sync.Mutex{}
		s.lanes[j.Plugin] = lane
	}
	snap := *jp
	s.mu.Unlock()
	go func() {
		lane.Lock()
		defer lane.Unlock()
		s.update(jp, func(j *Job) { j.State, j.StartedAt = JobRunning, time.Now().UTC() })
		res, err := safely(s.ctx, work)
		s.update(jp, func(j *Job) {
			j.FinishedAt = time.Now().UTC()
			j.Retry, j.Replay = res.Retry, res.Replay
			if err != nil {
				j.State, j.Error = JobFailed, err.Error()
				return
			}
			j.State = JobDone
		})
	}()
	return snap
}

func safely(ctx context.Context, work func(context.Context) (JobResult, error)) (res JobResult, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return work(ctx)
}

func (s *Jobs) update(j *Job, f func(*Job)) {
	s.mu.Lock()
	f(j)
	s.mu.Unlock()
}

// Get returns a copy of the job.
func (s *Jobs) Get(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.byID[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// EnqueueRetry queues a retry of plugin's dead letter. It refuses at once
// what could not run: a plugin that is not running, no such letter, one
// that is not dead.
func (l *Ledger) EnqueueRetry(ctx context.Context, plugin, event string) (Job, error) {
	r, ok := l.runner(plugin)
	if !ok {
		return Job{}, fmt.Errorf("plugin %q is not running: its dead letters cannot be replayed", plugin)
	}
	rec, err := l.Store.Get(ctx, plugin, event)
	if err != nil {
		return Job{}, err
	}
	if rec.State != StateDead {
		return Job{}, notDead(rec)
	}
	return l.submit(Job{Plugin: plugin, Kind: JobRetry, Event: event}, func(c context.Context) (JobResult, error) {
		rec, err := r.Replay(c, event)
		return JobResult{Retry: &rec}, err
	})
}

// EnqueueReplay queues a replay of plugin's events from a log position.
func (l *Ledger) EnqueueReplay(_ context.Context, plugin, from string, limit int) (Job, error) {
	r, ok := l.runner(plugin)
	if !ok {
		return Job{}, fmt.Errorf("plugin %q is not running: its events cannot be replayed", plugin)
	}
	if from == "" || limit < 1 {
		return Job{}, errors.New("a replay needs --from (a log position or \"" + FromBeginning + "\") and a limit of 1 or more")
	}
	return l.submit(Job{Plugin: plugin, Kind: JobReplay, From: from, Limit: limit}, func(c context.Context) (JobResult, error) {
		rep, err := r.ReplayFrom(c, from, limit, false)
		return JobResult{Replay: &rep}, err
	})
}

func (l *Ledger) submit(j Job, work func(context.Context) (JobResult, error)) (Job, error) {
	if l.Jobs == nil {
		return Job{}, errors.New("no job runner: the hooks service is not running")
	}
	return l.Jobs.Submit(j, work), nil
}

// Job returns a job by ID.
func (l *Ledger) Job(id string) (Job, bool) {
	if l.Jobs == nil {
		return Job{}, false
	}
	return l.Jobs.Get(id)
}
