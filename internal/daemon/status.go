package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/storage"
	"hop.top/uma"
	"hop.top/uma/store"
)

// ServiceState is where one runtime service stands.
type ServiceState string

// Service states.
const (
	// StateNotStarted: the service has not started in this process.
	StateNotStarted ServiceState = "not_started"
	// StateStarting: acquiring what it needs.
	StateStarting ServiceState = "starting"
	// StateReady: accepting work, every scope healthy.
	StateReady ServiceState = "ready"
	// StateDegraded: accepting work, some scope is not.
	StateDegraded ServiceState = "degraded"
	// StateStopping: draining.
	StateStopping ServiceState = "stopping"
	// StateStopped: stopped cleanly.
	StateStopped ServiceState = "stopped"
	// StateFailed: failed to start, or failed while running.
	StateFailed ServiceState = "failed"
)

// ScopeState is where one scope of a service stands.
type ScopeState string

// Scope states.
const (
	// ScopeStarting: the scope is not up yet, such as a receiver whose
	// listener has not bound.
	ScopeStarting ScopeState = "starting"
	// ScopeReady: the scope works: a worker runs; a channel's receiver
	// serves on its bound listener or, for an adapter whose receiver binds
	// no listener heo sees, its receive call has started.
	ScopeReady ScopeState = "ready"
	// ScopeWaiting: the scope waits for a resource, such as a receiver
	// lease a stopped daemon still holds, and retries on its own. It is
	// not receiving yet.
	ScopeWaiting ScopeState = "waiting"
	// ScopeDegraded: the scope still works but part of it is broken: a
	// channel's receiver that still receives, by polling, while part of
	// what it relies on fails (push or change notifications, a missing
	// credential for them; Code, Reason).
	ScopeDegraded ScopeState = "degraded"
	// ScopeFailed: the scope stopped; the rest of the service runs.
	ScopeFailed ScopeState = "failed"
)

// ScopeStatus is one scope's state: a channel for receivers and
// webhook, a worker for workers.
type ScopeStatus struct {
	// Scope names it, such as channel:<id> or worker:2.
	Scope string `json:"scope" yaml:"scope"`
	// Key is the channel's key, when set.
	Key string `json:"key,omitempty" yaml:"key,omitempty"`
	// Type is the channel's adapter type.
	Type   string     `json:"type,omitempty" yaml:"type,omitempty"`
	State  ScopeState `json:"state" yaml:"state"`
	Reason string     `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Code is the adapter's stable code for what degrades a receiver
	// that still receives (google.mail.push_denied); empty otherwise.
	Code string `json:"code,omitempty" yaml:"code,omitempty"`
	// Path and URL are a webhook route: the path on the listener and,
	// with services.webhook.public_base_url set, the URL providers are
	// pointed at.
	Path  string    `json:"path,omitempty" yaml:"path,omitempty"`
	URL   string    `json:"url,omitempty" yaml:"url,omitempty"`
	Since time.Time `json:"since" yaml:"since"`
}

// ServiceStatus is one runtime service's state.
type ServiceStatus struct {
	Name  string       `json:"name" yaml:"name"`
	State ServiceState `json:"state" yaml:"state"`
	// Error is why the service failed.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
	// Address is where the service listens, when it does.
	Address string        `json:"address,omitempty" yaml:"address,omitempty"`
	Scopes  []ScopeStatus `json:"scopes,omitempty" yaml:"scopes,omitempty"`
}

// StorageState is where the data store stands.
type StorageState string

// Storage states.
const (
	// StorageClosed: not open in this process.
	StorageClosed StorageState = "closed"
	// StorageReady: open, owned and compatible.
	StorageReady StorageState = "ready"
	// StorageLost: ownership was lost while open.
	StorageLost StorageState = "lost"
	// StorageFailed: opening failed.
	StorageFailed StorageState = "failed"
)

// StorageStatus is the data store's state.
type StorageStatus struct {
	State  StorageState    `json:"state" yaml:"state"`
	Report *storage.Report `json:"report,omitempty" yaml:"report,omitempty"`
	Error  string          `json:"error,omitempty" yaml:"error,omitempty"`
}

// OwnerStatus names the holder of the data store's ownership: this
// daemon, while a runtime service holds the store open.
type OwnerStatus struct {
	// Owner is the name the daemon took ownership under, such as
	// heo/default: what a refused second daemon is told.
	Owner string `json:"owner" yaml:"owner"`
	PID   int    `json:"pid" yaml:"pid"`
	Host  string `json:"host,omitempty" yaml:"host,omitempty"`
	// Since is when ownership was taken.
	Since time.Time `json:"since" yaml:"since"`
	// VerifiedAt is when ownership was last verified still held; zero
	// before the first periodic check.
	VerifiedAt time.Time `json:"verified_at,omitzero" yaml:"verified_at,omitempty"`
}

// Status is the runtime's state: what `service status` reports for the
// running daemon.
type Status struct {
	// Ready is the readiness verdict: the store is ready and every
	// started runtime service is ready or degraded.
	Ready   bool          `json:"ready" yaml:"ready"`
	Storage StorageStatus `json:"storage" yaml:"storage"`
	// Owner is the store's owner, nil while the store is closed.
	Owner    *OwnerStatus    `json:"owner,omitempty" yaml:"owner,omitempty"`
	Services []ServiceStatus `json:"services" yaml:"services"`
	// Bridge is the event bridge's state, nil while it does not run.
	Bridge *BridgeStatus `json:"bridge,omitempty" yaml:"bridge,omitempty"`
	// Hooks is each plugin's subscription: cursor, lag and last error;
	// nil while the hook runner does not run.
	Hooks []hooks.Status `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// Status returns a snapshot of the runtime's state.
func (h *Host) Status() Status {
	h.mu.Lock()
	r := h.run
	st := h.last
	svcs := make([]*serviceState, 0, len(h.svcs))
	for _, n := range []string{NameWorkers, NameReceivers, NameWebhook, NameBridge, NameApprovalChat, NameHooks} {
		svcs = append(svcs, h.svcs[n])
	}
	h.mu.Unlock()
	var owner *OwnerStatus
	if r != nil {
		rep := r.st.Report()
		st = StorageStatus{State: StorageReady, Report: &rep}
		if !r.storageHealthy() {
			st.State = StorageLost
			if err := r.lostError(); err != nil {
				st.Error = err.Error()
			}
		}
		owner = r.ownerStatus()
	}
	out := Status{Storage: st, Owner: owner, Ready: true, Bridge: h.bridge.status(), Hooks: h.hooks.status()}
	started := false
	for _, s := range svcs {
		ss := s.snapshot()
		out.Services = append(out.Services, ss)
		switch ss.State {
		case StateNotStarted:
		case StateReady, StateDegraded:
			started = true
		default:
			started = true
			out.Ready = false
		}
	}
	if started && st.State != StorageReady {
		out.Ready = false
	}
	return out
}

// QueueStatus is the depth of the queue workers run: the operation
// queue (uma.WorkKindOperation), counted by state in the store.
type QueueStatus struct {
	// Kind is the work kind counted.
	Kind string `json:"kind" yaml:"kind"`
	// Waiting items wait to be claimed: queued operations and those
	// whose retry is scheduled, a deferred retry included.
	Waiting int `json:"waiting" yaml:"waiting"`
	// InFlight items are claimed by a worker of this or a stopped
	// daemon whose lease has not expired.
	InFlight int `json:"in_flight" yaml:"in_flight"`
	// DeadLettered items were quarantined after repeated unfinished
	// claims: their operations are dead_lettered and run again only
	// once replayed.
	DeadLettered int `json:"dead_lettered" yaml:"dead_lettered"`
}

// Queue reads the operation queue's depth from the store the daemon
// owns: one count per state, never the items. It fails with
// ErrNotRunning when no runtime service holds the store open, and holds
// the store open while it reads.
func (h *Host) Queue(ctx context.Context) (QueueStatus, error) {
	r, done, err := h.hold(ctx)
	if err != nil {
		return QueueStatus{}, err
	}
	defer done()
	q := QueueStatus{Kind: uma.WorkKindOperation}
	err = r.st.Store().View(ctx, r.deps.Tenant, func(rd store.Reader) error {
		d, err := rd.WorkDepth(ctx, q.Kind)
		if err != nil {
			return err
		}
		q.Waiting, q.InFlight, q.DeadLettered = d.Pending, d.Claimed, d.Quarantined
		return nil
	})
	if err != nil {
		return QueueStatus{}, err
	}
	return q, nil
}

// StorageReady reports whether the store is open, owned and compatible.
func (h *Host) StorageReady() bool {
	h.mu.Lock()
	r := h.run
	h.mu.Unlock()
	return r != nil && r.storageHealthy()
}

// serviceState is one service's record, safe for concurrent use.
type serviceState struct {
	name string

	mu     sync.Mutex
	state  ServiceState
	err    string
	addr   string
	scopes map[string]ScopeStatus
}

func (s *serviceState) set(state ServiceState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.err = ""
	if err != nil {
		s.err = err.Error()
	}
}

func (s *serviceState) get() ServiceState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *serviceState) setAddr(addr string) {
	s.mu.Lock()
	s.addr = addr
	s.mu.Unlock()
}

// reset clears scopes for a new start.
func (s *serviceState) reset() {
	s.mu.Lock()
	s.scopes = map[string]ScopeStatus{}
	s.err = ""
	s.addr = ""
	s.state = StateStarting
	s.mu.Unlock()
}

func (s *serviceState) scope(sc ScopeStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopes == nil {
		s.scopes = map[string]ScopeStatus{}
	}
	if prev, ok := s.scopes[sc.Scope]; ok && prev.State == sc.State && prev.Reason == sc.Reason && prev.Code == sc.Code {
		sc.Since = prev.Since
	} else {
		sc.Since = time.Now().UTC()
	}
	s.scopes[sc.Scope] = sc
}

// drop removes a scope: its channel no longer runs in the service.
func (s *serviceState) drop(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.scopes, scope)
}

func (s *serviceState) snapshot() ServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := ServiceStatus{Name: s.name, State: s.state, Error: s.err, Address: s.addr}
	degraded := false
	for _, sc := range s.scopes {
		out.Scopes = append(out.Scopes, sc)
		if sc.State != ScopeReady {
			degraded = true
		}
	}
	sort.Slice(out.Scopes, func(i, j int) bool { return out.Scopes[i].Scope < out.Scopes[j].Scope })
	if out.State == StateReady && degraded {
		out.State = StateDegraded
	}
	return out
}
