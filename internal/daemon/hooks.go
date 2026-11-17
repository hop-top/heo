package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/plugins"
	"hop.top/uma/model"
)

// HooksScopePrefix starts the scope of each plugin in the hooks
// service's status: plugin:<name>.
const HooksScopePrefix = "plugin:"

// hooksOpenTimeout bounds opening the plugins' subscriptions before the
// service reports ready; a store that does not answer in time leaves
// those plugins degraded and retrying, not the daemon waiting.
const hooksOpenTimeout = 10 * time.Second

// HookOptions is what the hooks service runs plugins' results through.
type HookOptions struct {
	// Apply applies a plugin's result as its actor; nil accepts and
	// drops results.
	Apply hooks.Applier
	// Dead records the events a plugin did not handle; nil takes Store,
	// or logs them when there is none.
	Dead hooks.DeadLetterer
	// Store keeps the dead letters and the attempt counts of events in
	// retry, durably, and is what replay and discard run on. Nil opens a
	// hooks.FileStore in DeadDir.
	Store hooks.Store
	// DeadDir is where that store keeps its files, and each plugin's
	// consumer lineage. Empty is "hook-dead"
	// beside the plugins' directory; with neither, there is no store and
	// dead letters are only logged.
	DeadDir string
}

// hooksSvc runs the configured hook plugins: one durable uma
// subscription each, read as the plugin's own actor, delivered by a
// runner of its own. A plugin that is slow, failing or gone never holds
// another plugin, a worker, a receiver or the bridge: runners share
// nothing but the open store.
type hooksSvc struct {
	h *Host
	s *serviceState

	ready atomic.Bool

	mu      sync.Mutex
	run     *running
	runners []*hooks.Runner
	store   hooks.Store
	jobs    *hooks.Jobs
	cancel  context.CancelFunc
	done    chan struct{}
}

// Hooks returns the hook runner service.
func (h *Host) Hooks() Service { return h.hooks }

func (hs *hooksSvc) Name() string { return NameHooks }

// Validate refuses a plugins section that does not check: names, modes,
// filters, grants and executables, as `config validate` does.
func (hs *hooksSvc) Validate() error {
	var probs []Problem
	for _, p := range plugins.Check(hs.h.viper()) {
		probs = append(probs, Problem{Key: p.Key, Message: p.Message, Warning: p.Warning})
	}
	return problemsError(probs)
}

// Ready reports whether the plugins run on an owned store. A plugin
// that is retrying is ready, and degraded: it never gates the daemon.
func (hs *hooksSvc) Ready() bool {
	hs.mu.Lock()
	r := hs.run
	hs.mu.Unlock()
	return hs.ready.Load() && r != nil && r.storageHealthy()
}

// Start opens the store as its owner, if no runtime service has, and
// starts one runner per plugin: each opens its subscription (at the
// tail when it is new, at its checkpoint when it exists) and reads.
// Failing to open one is not failing to start: that plugin is degraded
// and retries, the others run.
func (hs *hooksSvc) Start(ctx context.Context, ready func()) error {
	if err := hs.Validate(); err != nil {
		return err
	}
	list, _ := plugins.Read(hs.h.viper())
	hs.s.reset()
	r, err := hs.h.acquire(ctx)
	if err != nil {
		hs.s.set(StateFailed, err)
		return err
	}
	hs.mu.Lock()
	hs.store = hs.openStore(r)
	hs.mu.Unlock()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	hs.mu.Lock()
	hs.jobs = hooks.NewJobs(runCtx)
	hs.mu.Unlock()
	done := make(chan struct{})
	var runners []*hooks.Runner
	var wg sync.WaitGroup
	for _, p := range list {
		scope := HooksScopePrefix + p.Name
		runner, err := hs.newRunner(r, p)
		if err != nil {
			hs.s.scope(ScopeStatus{Scope: scope, State: ScopeFailed, Reason: err.Error()})
			r.logError("runtime: hook plugin not started", "plugin", p.Name, "error", err)
			continue
		}
		hs.s.scope(ScopeStatus{Scope: scope, State: ScopeStarting})
		octx, ocancel := context.WithTimeout(ctx, hooksOpenTimeout)
		if err := runner.Start(octx); err != nil {
			r.logError("runtime: hook plugin could not open its subscription yet; retrying", "plugin", p.Name, "error", err)
		}
		ocancel()
		hs.report(runner.Status())
		runners = append(runners, runner)
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner.Run(runCtx)
		}()
	}
	go func() { wg.Wait(); close(done) }()
	hs.mu.Lock()
	hs.run, hs.runners, hs.cancel, hs.done = r, runners, cancel, done
	hs.mu.Unlock()
	hs.ready.Store(true)
	hs.s.set(StateReady, nil)
	r.logInfo("runtime: hook runner running", "plugins", len(runners))
	ready()
	select {
	case <-ctx.Done():
		return nil
	case <-r.Lost():
		hs.ready.Store(false)
		err := r.lostError()
		hs.s.set(StateFailed, err)
		return err
	}
}

// newRunner builds one plugin's runner: its executor, and a uma client
// of its own actor, so that what it may read is what it is granted.
func (hs *hooksSvc) newRunner(r *running, p plugins.Plugin) (*hooks.Runner, error) {
	var ex hooks.Executor
	launch, err := hs.launch(r, p)
	if err != nil {
		return nil, err
	}
	if p.Socket != "" {
		ex = &hooks.SocketProcess{Path: p.Socket, Timeout: p.Timeout, Launch: launch}
	} else if p.Mode == plugins.ModeStream {
		ex = &hooks.StreamProcess{Command: p.Command, Args: p.Args, Timeout: p.Timeout, Launch: launch}
	} else {
		ex = &hooks.ExecProcess{Command: p.Command, Args: p.Args, Timeout: p.Timeout, Launch: launch}
	}
	actor := p.Actor()
	client, err := r.engine.For(model.Principal{Tenant: r.deps.Tenant, Actor: actor})
	if err != nil {
		return nil, err
	}
	// The filter's inboxes and channels are looked up as the plugin: one
	// it may not read cannot be named.
	find := channels.Service{Client: client, Tenant: r.deps.Tenant}
	sees := hookSees(r, model.Principal{Tenant: r.deps.Tenant, Actor: actor})
	hs.mu.Lock()
	store := hs.store
	hs.mu.Unlock()
	o := hooks.Options{
		Plugin: p, Source: client.Events(), Exec: ex, Logger: r.deps.Logger,
		Apply: hs.h.opts.Hooks.Apply, Dead: hs.h.opts.Hooks.Dead, Store: store,
		Resolve:  hookFilter(p, find),
		Match:    hookMatch(p, find),
		Sees:     sees,
		OnChange: hs.report,
		List:     client.Events().List,
		// A replay reads the event as the plugin, under its audience rule.
		Fetch: func(ctx context.Context, id model.ID) (model.Event, error) {
			return desk.Service{Client: client, Tenant: r.deps.Tenant, Sees: sees}.GetEvent(ctx, string(id))
		},
	}
	if l, ok := store.(hooks.LineageStore); ok {
		o.Lineage = l
	}
	if pub := hs.publisher(); pub != nil {
		o.Publisher = pub
	}
	if o.Dead == nil && store == nil {
		o.Dead = logDeadLetters{r: r}
	}
	return hooks.New(o), nil
}

// publisher is where the runners' health events go: the bus the bridge
// publishes on, nil without one.
func (hs *hooksSvc) publisher() bridge.Publisher {
	if hs.h.opts.Hub == nil {
		return nil
	}
	return hs.h.opts.Hub
}

// openStore is the dead-letter store: the one configured, or a file
// store in the configured directory, or beside the plugins' directories.
func (hs *hooksSvc) openStore(r *running) hooks.Store {
	o := hs.h.opts.Hooks
	if o.Store != nil {
		return o.Store
	}
	dir := o.DeadDir
	if dir == "" && r.deps.PluginsDir != "" {
		dir = filepath.Join(filepath.Dir(r.deps.PluginsDir), "hook-dead")
	}
	if dir == "" {
		return nil
	}
	return hooks.NewFileStore(dir)
}

// Ledger returns the dead-letter ledger: list, get, replay and discard
// over the store and the running plugins. Nil while the service has no
// store, that is before it starts or where none is configured.
func (h *Host) Ledger() *hooks.Ledger { return h.hooks.ledger() }

func (hs *hooksSvc) ledger() *hooks.Ledger {
	hs.mu.Lock()
	store, jobs := hs.store, hs.jobs
	hs.mu.Unlock()
	if store == nil {
		return nil
	}
	l := &hooks.Ledger{Store: store, Runner: hs.runner, Jobs: jobs}
	if pub := hs.publisher(); pub != nil {
		l.Publisher = pub
	}
	return l
}

func (hs *hooksSvc) runner(plugin string) (*hooks.Runner, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for _, r := range hs.runners {
		if r.Status().Plugin == plugin {
			return r, true
		}
	}
	return nil, false
}

// launch is how p's process is started: its own directory, the
// surface address, its own token read at each start, stderr to the
// daemon's log. A runtime that wires a token source refuses to start a
// plugin with no token minted: it would run with no way to read, and
// every attempt would be a failure that retries and dead-letters.
func (hs *hooksSvc) launch(r *running, p plugins.Plugin) (hooks.Launch, error) {
	l := hooks.Launch{Name: p.Name, URL: r.deps.PluginURL, Log: r.deps.Logger}
	if r.deps.PluginsDir != "" {
		l.Dir = filepath.Join(r.deps.PluginsDir, p.Name)
	}
	if get := r.deps.PluginToken; get != nil {
		ctx, cancel := context.WithTimeout(context.Background(), hooksOpenTimeout)
		defer cancel()
		_, ok, err := get(ctx, p.Name)
		switch {
		case err != nil:
			return hooks.Launch{}, fmt.Errorf("plugin token: %w", err)
		case !ok:
			return hooks.Launch{}, fmt.Errorf("no token minted for plugin %s: run `heo plugin token mint %s`, then restart the hooks service", p.Name, p.Name)
		}
		l.Token = func(ctx context.Context) (string, error) {
			tok, ok, err := get(ctx, p.Name)
			if err == nil && !ok {
				err = errors.New("token revoked: mint a new one")
			}
			return tok, err
		}
	}
	return l, nil
}

// hookFilter is uma's filter for p, fixed when its subscription opens:
// its kinds and its inboxes as IDs, sorted so the same configuration is
// always the same filter.
func hookFilter(p plugins.Plugin, find channels.Service) hooks.Resolve {
	return func(ctx context.Context) (model.Filter, error) {
		f := model.Filter{Kinds: p.Filter.Canonical().Kinds}
		if len(f.Kinds) == 0 {
			f.Kinds = nil
		}
		for _, key := range p.Filter.Canonical().Inboxes {
			in, err := find.FindInbox(ctx, key)
			if err != nil {
				return model.Filter{}, fmt.Errorf("filter.inboxes: %w", err)
			}
			f.Inboxes = append(f.Inboxes, in.Ref)
		}
		slices.SortFunc(f.Inboxes, func(a, b model.InboxRef) int { return strings.Compare(string(a.ID), string(b.ID)) })
		f.Inboxes = slices.Compact(f.Inboxes)
		return f, nil
	}
}

// hookMatch checks filter.channels, which uma's log does not honour: an
// event belongs to the channel of its inbox. An event with no inbox
// belongs to none.
func hookMatch(p plugins.Plugin, find channels.Service) hooks.Match {
	want := p.Filter.Canonical().Channels
	if len(want) == 0 {
		return nil
	}
	var mu sync.Mutex
	var ids map[model.ID]bool
	byInbox := map[model.ID]model.ID{}
	return func(ctx context.Context, ev model.Event) (bool, error) {
		if ev.Inbox == "" {
			return false, nil
		}
		mu.Lock()
		defer mu.Unlock()
		if ids == nil {
			set := map[model.ID]bool{}
			for _, ref := range want {
				ch, err := find.FindChannel(ctx, ref)
				if err != nil {
					return false, fmt.Errorf("filter.channels: %w", err)
				}
				set[ch.Ref.ID] = true
			}
			ids = set
		}
		ch, ok := byInbox[ev.Inbox]
		if !ok {
			in, err := find.FindInbox(ctx, string(ev.Inbox))
			if err != nil {
				return false, err
			}
			ch = in.Channel.ID
			byInbox[ev.Inbox] = ch
		}
		return ids[ch], nil
	}
}

// hookSees is the audience rule of the plugin's actor under the
// runtime's authorization policy, when the policy answers it; without
// one a plugin sees public events only.
func hookSees(r *running, p model.Principal) func(model.Audience) bool {
	type seer interface {
		Sees(model.Principal, model.Audience) bool
	}
	if s, ok := r.deps.Authorizer.(seer); ok {
		return func(a model.Audience) bool { return s.Sees(p, a) }
	}
	return nil
}

// logDeadLetters is the dead-letter record where no store is
// configured: a report to the daemon's log.
type logDeadLetters struct{ r *running }

func (l logDeadLetters) Record(_ context.Context, d hooks.DeadLetter) error {
	l.r.logError("hooks: dead letter", "plugin", d.Plugin, "event", string(d.Event.ID), "kind", d.Event.Kind,
		"attempts", d.Attempts, "class", string(d.Class), "permanent", d.Permanent, "reason", d.Reason)
	return nil
}

// report records a plugin's state as its scope's: ready while it reads
// and delivers, waiting while it retries.
func (hs *hooksSvc) report(st hooks.Status) {
	sc := ScopeStatus{Scope: HooksScopePrefix + st.Plugin, State: ScopeReady}
	switch st.State {
	case hooks.StateStarting:
		sc.State = ScopeStarting
	case hooks.StateDegraded:
		sc.State, sc.Reason = ScopeWaiting, "retrying after "+st.Code
	}
	hs.s.scope(sc)
}

// Stop ends the runners: a delivery in hand is cancelled and its batch
// left unacknowledged, so each subscription stays at its checkpoint and
// the next start reads it again. The store is released as the last
// runtime service stops.
func (hs *hooksSvc) Stop(ctx context.Context) error {
	hs.ready.Store(false)
	hs.mu.Lock()
	r, cancel, done := hs.run, hs.cancel, hs.done
	hs.run, hs.runners, hs.cancel, hs.done = nil, nil, nil, nil
	hs.mu.Unlock()
	if r == nil {
		return nil
	}
	hs.s.set(StateStopping, nil)
	cancel()
	var errs []error
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, errors.New("hook runners did not return before the stop deadline; "+
			"their batches in hand are read again on the next start"))
	}
	if err := hs.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		hs.s.set(StateFailed, err)
		r.logError("runtime: hook runner stopped uncleanly", "error", err)
		return err
	}
	hs.s.set(StateStopped, nil)
	r.logInfo("runtime: hook runner stopped")
	return nil
}

// status returns each plugin's state, nil while the service is not
// running.
func (hs *hooksSvc) status() []hooks.Status {
	hs.mu.Lock()
	runners, run := hs.runners, hs.run
	hs.mu.Unlock()
	if run == nil {
		return nil
	}
	out := make([]hooks.Status, 0, len(runners))
	for _, r := range runners {
		out = append(out, r.Status())
	}
	return out
}
