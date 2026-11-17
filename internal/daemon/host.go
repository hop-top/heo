// Package daemon is heo's runtime: the services `heo service start`
// runs beside kit's control socket, on kit's one supervisor.
//
//   - workers run queued uma operations (Runtime.Run), a bounded number
//     at once;
//   - receivers run the persistent receivers of the profile's channels
//     (Runtime.Receive), one owner per provider account;
//   - webhook serves the channels' HTTP receivers (Runtime.Webhook) on
//     its own listener, apart from every control surface;
//   - bridge republishes uma's committed domain events on the bus, from
//     a durable subscription of its own (internal/bridge).
//
// The services share one Host: the profile's data store, opened through
// its storage driver as the store's exclusive owner, and the adapters
// and uma engine composed over it. The first service to start opens
// them, refusing a store whose schema is not this binary's and a store
// another daemon owns; the last to stop drains, closes the engine and
// releases ownership. Naming one service (`heo service start workers`)
// opens them for that service alone.
//
// Shutdown follows kit's ordered stop, the reverse of start: the
// webhook listener stops accepting, receivers stop, then workers drain
// — no new claims, claimed work finishes — within
// services.workers.drain_timeout; then the bridge finishes the batch in
// hand, leaving its subscription at its checkpoint; then the engine
// closes and ownership is released. Work still running at the deadline
// keeps its lease and resumes on the next start.
//
// Readiness is per service and per scope. A service is ready once what
// it needs is acquired. A channel that cannot receive — its adapter is
// missing from this build, it fails to open, or it fails for good — is
// reported by scope (channel:<id>) and leaves the other channels and
// services running. Status returns the whole picture.
//
// Channel changes apply while the daemon runs. Every channel or inbox
// write calls Reconcile once uma answers it. The daemon owns the store,
// so every configuration write passes through it, and uma commits a
// configure call before it returns. Reconcile compares the stored
// channels with the running ones by revision. It stops and starts only
// the channels that changed, and checks their provider accounts again.
// An account conflict then fails that channel on its scope.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/storage"
	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// ServiceActor is the actor the runtime's own calls run as: binding
// receivers and webhooks, listing channels, running workers.
const ServiceActor model.ActorID = "svc:heo-runtime"

// correlation tags the runtime's own calls.
const correlation model.CorrelationID = "heo-runtime"

// Logger is the structured logger the runtime reports to; kit's
// console logger satisfies it.
type Logger interface {
	Info(msg any, keyvals ...any)
	Error(msg any, keyvals ...any)
}

// Storage is the profile's data store, opened and exclusively owned.
type Storage interface {
	// Store is the uma store.
	Store() store.Store
	// Report is the store's versions and compatibility, as opened.
	Report() storage.Report
	// Verify fails once ownership is lost.
	Verify(ctx context.Context) error
	// Close releases ownership and the connection.
	Close(ctx context.Context) error
}

// StorageOpener opens the profile's data store as its exclusive owner,
// refusing a schema other than this binary's.
type StorageOpener func(ctx context.Context) (Storage, error)

// Deps is what the runtime runs on, resolved when the first runtime
// service starts.
type Deps struct {
	// Tenant is the profile's uma tenant.
	Tenant model.TenantID
	// Owner names this daemon in lease and ownership diagnostics, such
	// as heo/default.
	Owner string
	// Storage opens the data store.
	Storage StorageOpener
	// Credentials resolves channel credentials; nil fails operations
	// that need one.
	Credentials ports.Credentials
	// HTTP is adapters' HTTP client; nil fails operations that need one.
	HTTP ports.HTTPClient
	// Adapters builds the adapters this binary runs over the opened
	// store, once per open, before the engine is composed: an adapter
	// keeping state of its own, such as the widget's visitor logs,
	// keeps it in the store the daemon owns. Nil runs none.
	Adapters func(store.Store) (*adapters.Registry, error)
	// Authorizer decides every uma operation: the runtime's own calls
	// as ServiceActor, every command's, and every queued operation a
	// worker dequeues, rechecked for the actor that queued it.
	// Required: the runtime never runs without a policy.
	Authorizer policy.Authorizer
	// Blobs keeps attachment bytes; nil refuses attachment staging
	// and reads (uma: Disabled).
	Blobs ports.BlobStore
	// IdentityVerifiers verify the evidence of identity links, keyed by
	// evidence kind; a kind without one is refused (model.Unsupported),
	// so none leaves no identity linkable.
	IdentityVerifiers map[string]policy.IdentityVerifier
	// Clock and IDs default to the system clock and UUIDv7.
	Clock ports.Clock
	IDs   ports.IDs
	// RequireDurable refuses a store that is not durable.
	RequireDurable bool
	// Held opts the local writes heo holds for a decision (a label
	// create, a conversation transition) into uma's holding; the zero
	// value holds none of them.
	Held uma.HeldPolicy
	// Retention sets how long uma keeps each record family. Only the
	// operation horizon is set: it is how long an operation key finds
	// its operation, and heo does not run uma's purge.
	Retention uma.RetentionPolicy
	// Accounts claims provider accounts across every profile on the
	// host; nil checks duplicates within the profile only.
	Accounts *Accounts
	// OwnershipCheck is how often store ownership is verified; zero
	// takes 10s.
	OwnershipCheck time.Duration
	// ReceiverStatusPoll is how often each receiver's status is read
	// from uma while its receive call runs; zero takes 2s.
	ReceiverStatusPoll time.Duration
	// Phase returns a value in [0,1) for each receiver restart backoff
	// jitter; nil takes math/rand/v2. Tests inject a fixed sequence.
	Phase func() float64
	// Retry bounds uma's provider attempts and its receivers'
	// reconnects; zero fields take uma's defaults.
	Retry uma.RetryPolicy
	// PluginsDir holds each hook plugin's working directory, <name>
	// under it, created owner-only. Empty runs plugins in the daemon's
	// directory.
	PluginsDir string
	// PluginURL is the HTTP surface address plugins are given; empty
	// gives none.
	PluginURL string
	// PluginToken returns a plugin's own bearer token and whether one is
	// minted. Nil starts plugins with no token; set, a plugin with none
	// minted is not started.
	PluginToken func(ctx context.Context, name string) (string, bool, error)
	// Logger receives scope degradation and drain reports; nil drops
	// them.
	Logger Logger
}

// Options configures a Host.
type Options struct {
	// Viper returns the resolved configuration, read for services.*
	// when a service validates or starts.
	Viper func() *viper.Viper
	// Resolve builds Deps from the loaded profile configuration.
	Resolve func(ctx context.Context) (Deps, error)
	// Hub is the bus the event bridge publishes on; without one the
	// bridge service does not start.
	Hub *bridge.Hub
	// Hooks is what the hook runner applies plugin results with and
	// records dead letters in.
	Hooks HookOptions
}

// Host is the state the runtime services share. Its zero value is not
// usable; use New.
type Host struct {
	opts Options

	mu   sync.Mutex
	refs int
	run  *running
	svcs map[string]*serviceState
	// bridge is the event bridge service, hooks the hook runner.
	bridge *bridgeSvc
	// chat is the approval chat service.
	chat  *approvalChatSvc
	hooks *hooksSvc
	// last is the storage state once the store is closed, kept for
	// status.
	last StorageStatus

	// abort ends when the operator gives up on a drain (AbortDrain).
	abort   context.Context
	abortFn context.CancelFunc
}

// running is one open store and engine.
type running struct {
	deps      Deps
	st        Storage
	adapters  *adapters.Registry
	engine    uma.Engine
	principal model.Principal
	// wake paces the workers' idle wait; a nudge ends it.
	wake *wakeWaiter

	// owned is when store ownership was taken, host the machine taking
	// it.
	owned time.Time
	host  string

	mu       sync.Mutex
	healthy  bool
	lostErr  error
	lost     chan struct{}
	verified time.Time

	stopMonitor context.CancelFunc
	monitorDone chan struct{}

	// planMu guards the inbound plan, the account claims taken for it
	// (by account, held until close or until a reconcile stops their
	// channel) and the services running it.
	planMu   sync.Mutex
	planDone bool
	plan     *inboundPlan
	planErr  error
	claims   map[string]*Claim
	users    []planUser
}

// New returns a Host.
func New(o Options) *Host {
	h := &Host{opts: o, svcs: map[string]*serviceState{}, last: StorageStatus{State: StorageClosed}}
	h.abort, h.abortFn = context.WithCancel(context.Background())
	for _, n := range []string{NameWorkers, NameReceivers, NameWebhook, NameBridge, NameApprovalChat, NameHooks} {
		h.svcs[n] = &serviceState{name: n, state: StateNotStarted}
	}
	h.bridge = &bridgeSvc{h: h, s: h.svcs[NameBridge]}
	h.chat = &approvalChatSvc{h: h, s: h.svcs[NameApprovalChat]}
	h.hooks = &hooksSvc{h: h, s: h.svcs[NameHooks]}
	return h
}

// AbortDrain gives up on the drain: a drain in progress, or one that
// begins later, stops waiting for claimed work, which is abandoned to
// its lease. The daemon calls it on the second shutdown signal.
func (h *Host) AbortDrain() { h.abortFn() }

// viper returns the configuration, an empty one when none is wired.
func (h *Host) viper() *viper.Viper {
	if h.opts.Viper != nil {
		if v := h.opts.Viper(); v != nil {
			return v
		}
	}
	v := viper.New()
	for k, val := range Defaults() {
		v.SetDefault(k, val)
	}
	return v
}

// Services returns the runtime services in registration order: the
// event bridge, approval chat, workers, receivers, webhook, hooks. Kit
// starts them in that order and stops them in reverse, so the plugins stop
// first, then ingress before workers drain, and the approval chat, whose
// replies and edits the workers deliver, and the bridge, which reads what
// they commit, stop last.
func (h *Host) Services() []Service {
	return []Service{h.Bridge(), h.ApprovalChat(), h.Workers(), h.Receivers(), h.Webhook(), h.Hooks()}
}

// acquire opens the store and engine on first use and counts a user.
func (h *Host) acquire(ctx context.Context) (*running, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.run != nil {
		h.refs++
		return h.run, nil
	}
	if h.opts.Resolve == nil {
		return nil, errors.New("runtime: no dependencies configured")
	}
	deps, err := h.opts.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	r, err := open(ctx, deps)
	if err != nil {
		h.last = StorageStatus{State: StorageFailed, Error: err.Error()}
		return nil, err
	}
	h.run = r
	h.refs = 1
	return r, nil
}

// ErrNotRunning is Use's refusal when no runtime service holds the data
// store open in this process.
var ErrNotRunning = errors.New("runtime not running: no runtime service holds the data store open")

// Runtime is the open runtime a daemon command acts through: the uma
// engine over the store the daemon owns, and the adapters composed over
// it.
type Runtime struct {
	Engine   uma.Engine
	Adapters *adapters.Registry

	// principal is the runtime's own: what a bounded import binds the
	// channel's sink as.
	principal model.Principal
	// wake paces the workers' idle wait.
	wake *wakeWaiter
}

// Nudge wakes the daemon's idle workers at once instead of at their next
// poll (services.workers.poll, a second by default). A command whose
// effect must reach its provider within seconds calls it after queueing
// the operation.
func (r Runtime) Nudge() {
	if r.wake != nil {
		r.wake.nudge()
	}
}

// Import runs one bounded history import (uma Runtime.Import): the
// requester, a desk or operator actor, must hold channel.import on the
// channel or conversation, and the runtime's own actor, which is the
// one that receives on channels, accepts each message through the
// channel's sink as ingress would. Only a daemon command reaches it:
// the daemon owns the open runtime. On a failure the report holds what
// was done before it.
func (r Runtime) Import(ctx context.Context, req uma.ImportRequest) (uma.ImportReport, error) {
	return r.Engine.Runtime().Import(ctx,
		uma.RuntimeCall{Principal: r.principal, Call: model.Call{Correlation: correlation}}, req)
}

// Use runs fn with the open runtime, holding it open until fn returns:
// a runtime service stopping meanwhile leaves the store open for fn, and
// the last user to finish closes it. Use never opens the store itself —
// only a runtime service starting does — and fails with ErrNotRunning
// when none holds it open.
func (h *Host) Use(ctx context.Context, fn func(Runtime) error) error {
	r, done, err := h.hold(ctx)
	if err != nil {
		return err
	}
	defer done()
	return fn(Runtime{Engine: r.engine, Adapters: r.adapters, principal: r.principal, wake: r.wake})
}

// hold counts a user of the open run, never opening it, and returns
// the call that drops that user; ErrNotRunning when no run is open.
func (h *Host) hold(ctx context.Context) (*running, func(), error) {
	h.mu.Lock()
	r := h.run
	if r == nil {
		h.mu.Unlock()
		return nil, nil, ErrNotRunning
	}
	h.refs++
	h.mu.Unlock()
	return r, func() { _ = h.release(context.WithoutCancel(ctx)) }, nil
}

// release drops a user; the last one closes the engine and releases the
// store.
func (h *Host) release(ctx context.Context) error {
	h.mu.Lock()
	if h.run == nil || h.refs == 0 {
		h.mu.Unlock()
		return nil
	}
	h.refs--
	if h.refs > 0 {
		h.mu.Unlock()
		return nil
	}
	r := h.run
	h.run = nil
	rep := r.st.Report()
	h.last = StorageStatus{State: StorageClosed, Report: &rep}
	h.mu.Unlock()
	return r.close(ctx)
}

// open opens the store as its owner, then composes the engine over it.
func open(ctx context.Context, d Deps) (*running, error) {
	if err := d.Tenant.Validate(); err != nil {
		return nil, err
	}
	if d.Storage == nil {
		return nil, errors.New("runtime: no storage configured")
	}
	if d.Authorizer == nil {
		return nil, errors.New("runtime: no authorization policy")
	}
	principal := model.Principal{Tenant: d.Tenant, Actor: ServiceActor}
	if d.Clock == nil {
		d.Clock = systemClock{}
	}
	if d.IDs == nil {
		d.IDs = uuidV7{}
	}
	if d.OwnershipCheck <= 0 {
		d.OwnershipCheck = defaultOwnershipCheckPeriod
	}
	st, err := d.Storage(ctx)
	if err != nil {
		return nil, err
	}
	owned := time.Now().UTC()
	reg := adapters.NewRegistry()
	if d.Adapters != nil {
		if reg, err = d.Adapters(st.Store()); err != nil {
			_ = st.Close(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("compose adapters: %w", err)
		}
		if reg == nil {
			reg = adapters.NewRegistry()
		}
	}
	wake := newWakeWaiter()
	engine, err := uma.New(uma.Config{
		Store:             st.Store(),
		Authorizer:        d.Authorizer,
		IdentityVerifiers: d.IdentityVerifiers,
		Adapters:          reg.Factories(),
		Extensions:        reg.Extensions(),
		Credentials:       d.Credentials,
		Blobs:             d.Blobs,
		HTTP:              d.HTTP,
		Clock:             d.Clock,
		IDs:               d.IDs,
		RequireDurable:    d.RequireDurable,
		Retention:         d.Retention,
		Held:              d.Held,
		Retry:             d.Retry,
		Waiter:            wake,
	})
	if err != nil {
		_ = st.Close(context.WithoutCancel(ctx))
		return nil, fmt.Errorf("compose engine: %w", err)
	}
	mctx, stop := context.WithCancel(context.Background())
	r := &running{
		deps: d, st: st, adapters: reg, engine: engine, principal: principal, wake: wake, healthy: true,
		owned: owned, host: hostname(),
		lost: make(chan struct{}), stopMonitor: stop, monitorDone: make(chan struct{}),
		claims: map[string]*Claim{},
	}
	go r.monitor(mctx)
	return r, nil
}

// monitor verifies store ownership periodically; losing it fails every
// runtime service, since another daemon may now own the store.
func (r *running) monitor(ctx context.Context) {
	defer close(r.monitorDone)
	t := time.NewTicker(r.deps.OwnershipCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		vctx, cancel := context.WithTimeout(ctx, r.deps.OwnershipCheck)
		err := r.st.Verify(vctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.mu.Lock()
			r.healthy = false
			r.lostErr = fmt.Errorf("store ownership lost: %w", err)
			close(r.lost)
			r.mu.Unlock()
			r.logError("runtime: store ownership lost", "error", err)
			return
		}
		r.mu.Lock()
		r.verified = time.Now().UTC()
		r.mu.Unlock()
	}
}

// ownerStatus names this daemon as the store's owner.
func (r *running) ownerStatus() *OwnerStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return &OwnerStatus{Owner: r.deps.Owner, PID: os.Getpid(), Host: r.host, Since: r.owned, VerifiedAt: r.verified}
}

// hostname is the machine's name, empty when it cannot be read.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// Lost is closed when store ownership is lost.
func (r *running) Lost() <-chan struct{} { return r.lost }

func (r *running) lostError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lostErr
}

func (r *running) storageHealthy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.healthy
}

// call is the runtime's own call.
func (r *running) call() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: r.principal, Call: model.Call{Correlation: correlation}}
}

// close drains anything still running, closes the engine, gives up the
// run's provider account claims and releases the store, within ctx.
func (r *running) close(ctx context.Context) error {
	var errs []error
	if err := r.engine.Runtime().Drain(ctx, r.call()); err != nil && !model.IsCode(err, model.Unavailable) {
		errs = append(errs, fmt.Errorf("drain: %w", err))
	}
	if err := r.engine.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("close engine: %w", err))
	}
	r.releaseClaims()
	r.stopMonitor()
	<-r.monitorDone
	if err := r.st.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("release store: %w", err))
	}
	return errors.Join(errs...)
}

func (r *running) logInfo(msg string, kv ...any) {
	if r.deps.Logger != nil {
		r.deps.Logger.Info(msg, kv...)
	}
}

func (r *running) logError(msg string, kv ...any) {
	if r.deps.Logger != nil {
		r.deps.Logger.Error(msg, kv...)
	}
}

// channels lists every channel of the tenant the runtime may read.
func (r *running) channels(ctx context.Context) ([]model.Channel, error) {
	c, err := r.engine.For(r.principal)
	if err != nil {
		return nil, err
	}
	var out []model.Channel
	req := model.ListRequest{Call: model.Call{Correlation: correlation}, Page: model.PageRequest{Limit: 100}}
	for {
		page, err := c.Configuration().Channels(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Complete || page.Next == "" {
			return out, nil
		}
		req.Page.After = page.Next
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type uuidV7 struct{}

func (uuidV7) New(context.Context) (model.ID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return model.ID(id.String()), nil
}
