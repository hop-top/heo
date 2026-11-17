package daemon_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/storage"
	"hop.top/kit/go/console/serve"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var allowAll = policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: true}, nil
})

// memStorage is an owned in-memory store whose ownership a test can
// revoke.
type memStorage struct {
	st     store.Store
	lost   atomic.Bool
	closes atomic.Int32
	// view, when set, is the store the daemon sees in place of st.
	view store.Store
}

func (m *memStorage) Store() store.Store {
	if m.view != nil {
		return m.view
	}
	return m.st
}
func (m *memStorage) Report() storage.Report {
	return storage.Report{Versions: storage.Versions{Driver: "memory"}, Compat: storage.CompatOK}
}

func (m *memStorage) Verify(context.Context) error {
	if m.lost.Load() {
		return errors.New("lock session gone")
	}
	return nil
}

func (m *memStorage) Close(context.Context) error {
	m.closes.Add(1)
	return nil
}

// rig is one profile's runtime over an in-memory store, plus a seeding
// engine on the same store that configures what a command would.
type rig struct {
	t        *testing.T
	tenant   model.TenantID
	fake     *adapterstest.Factory
	storage  *memStorage
	v        *viper.Viper
	host     *daemon.Host
	seed     uma.Engine
	registry *adapters.Registry
	extra    []adapters.Adapter
	// compose, when set, builds adapters over the store the daemon
	// opened, beside the fake; the seed engine still runs extra.
	compose func(store.Store) ([]adapters.Adapter, error)
	// opened records the store each open composed the adapters over.
	opened []store.Store
	deps   daemon.Deps
	n      int
	// hub is the bus the event bridge publishes on.
	hub *bridge.Hub
}

type rigOption func(*rig)

// withoutAdapters runs the daemon with no adapter registered.
func withoutAdapters() rigOption {
	return func(r *rig) { r.registry = adapters.NewRegistry() }
}

// withAccounts claims accounts under dir.
func withAccounts(dir string) rigOption {
	return func(r *rig) { r.deps.Accounts = daemon.NewAccounts(dir) }
}

// withCredentials resolves channel credentials through c.
func withCredentials(c ports.Credentials) rigOption {
	return func(r *rig) { r.deps.Credentials = c }
}

// withHTTP gives adapters h as their HTTP client.
func withHTTP(h ports.HTTPClient) rigOption {
	return func(r *rig) { r.deps.HTTP = h }
}

// withSet sets a configuration key.
func withSet(key string, val any) rigOption {
	return func(r *rig) { r.v.Set(key, val) }
}

func newRig(t *testing.T, tenant model.TenantID, opts ...rigOption) *rig {
	t.Helper()
	fake := adapterstest.New()
	reg := adapters.NewRegistry()
	if err := reg.Register(fake.Adapter()); err != nil {
		t.Fatal(err)
	}
	r := &rig{
		t: t, tenant: tenant, fake: fake, registry: reg,
		storage: &memStorage{st: memory.New(memory.Options{})},
		v:       viper.New(),
		hub:     bridge.NewHub(),
	}
	hub := r.hub
	t.Cleanup(func() { _ = hub.Close(context.Background()) })
	for k, val := range daemon.Defaults() {
		r.v.SetDefault(k, val)
	}
	r.v.Set(daemon.KeyWorkersPoll, "10ms")
	r.v.Set(daemon.KeyWebhookAddr, "127.0.0.1:0")
	// Staggered starts are the stagger tests' to ask for.
	r.v.Set(daemon.KeyReceiversStartSpread, "0s")
	r.deps = daemon.Deps{
		Tenant: tenant, Owner: "heo/" + string(tenant),
		Storage:        func(context.Context) (daemon.Storage, error) { return r.storage, nil },
		Authorizer:     allowAll,
		OwnershipCheck: 20 * time.Millisecond,
	}
	for _, o := range opts {
		o(r)
	}
	factories := []adapter.Factory{fake}
	for _, a := range r.extra {
		if r.compose == nil {
			if err := r.registry.Register(a); err != nil {
				t.Fatal(err)
			}
		}
		factories = append(factories, a.Factory)
	}
	var openMu sync.Mutex
	r.deps.Adapters = func(st store.Store) (*adapters.Registry, error) {
		openMu.Lock()
		r.opened = append(r.opened, st)
		openMu.Unlock()
		if r.compose == nil {
			return r.registry, nil
		}
		list, err := r.compose(st)
		if err != nil {
			return nil, err
		}
		reg := adapters.NewRegistry()
		for _, a := range append([]adapters.Adapter{fake.Adapter()}, list...) {
			if err := reg.Register(a); err != nil {
				return nil, err
			}
		}
		return reg, nil
	}
	// The seed engine submits what the daemon's workers run, so it knows
	// the extension effects the adapters ship.
	var effects []adapter.ExtensionDefinition
	for _, a := range r.extra {
		effects = append(effects, a.Extensions...)
	}
	seed, err := uma.New(uma.Config{
		Store: r.storage.st, Authorizer: allowAll,
		Adapters: factories, Extensions: effects,
		Clock: sysClock{}, IDs: &seqIDs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.seed = seed
	t.Cleanup(func() { _ = seed.Close(context.Background()) })
	r.host = daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
	})
	return r
}

// rebuildHost builds the rig's host again, after a test replaced the
// hub.
func rebuildHost(r *rig) *daemon.Host {
	return daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
	})
}

type sysClock struct{}

func (sysClock) Now() time.Time { return time.Now() }

// seqIDs allocates UUID-shaped IDs from a counter.
type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (s *seqIDs) New(context.Context) (model.ID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return model.ID(uuidFor(s.n)), nil
}

func uuidFor(n int) string {
	const hex = "0123456789abcdef"
	b := []byte("00000000-0000-7000-8000-000000000000")
	for i := len(b) - 1; n > 0 && i >= 0; i-- {
		if b[i] == '-' {
			continue
		}
		b[i] = hex[n%16]
		n /= 16
	}
	return string(b)
}

func (r *rig) client() uma.Client {
	r.t.Helper()
	c, err := r.seed.For(model.Principal{Tenant: r.tenant, Actor: "operator"})
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

func (r *rig) cmd(op string) model.Command {
	r.n++
	return model.Command{Call: model.Call{Correlation: "test"}, Operation: model.OperationID(op)}
}

// channel configures a fake channel.
func (r *rig) channel(key string, cfg adapterstest.Config) model.ChannelRef {
	r.t.Helper()
	return r.channelOf(model.Channel{Key: key, Type: adapterstest.Type, Configuration: cfg.Configuration()})
}

// channelOf configures ch in the rig's tenant, as a channel command
// would.
func (r *rig) channelOf(ch model.Channel) model.ChannelRef {
	r.t.Helper()
	key := ch.Key
	ch.Ref = model.ChannelRef{Tenant: r.tenant}
	acc, err := r.client().Configuration().ConfigureChannel(r.t.Context(), model.ConfigureChannel{
		Command: r.cmd("channel-" + key),
		Channel: ch,
	})
	if err != nil {
		r.t.Fatalf("configure channel %s: %v", key, err)
	}
	ref, err := acc.Operation.Result.AsChannel()
	if err != nil {
		r.t.Fatal(err)
	}
	return ref
}

// queueSend configures a channel and inbox and queues the first message
// of a conversation: one operation for a worker to run.
func (r *rig) queueSend() model.OperationRef {
	r.t.Helper()
	ctx := r.t.Context()
	ch := r.channel("out", adapterstest.Config{Account: "acct-out"})
	lock := true
	ib, err := r.client().Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: r.cmd("inbox"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: r.tenant}, Channel: ch, Name: "out", Destinations: []string{"desk"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		r.t.Fatalf("configure inbox: %v", err)
	}
	inbox, err := ib.Operation.Result.AsInbox()
	if err != nil {
		r.t.Fatal(err)
	}
	acc, err := r.client().Conversations().Start(ctx, model.StartConversation{
		Command: r.cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: ch, Namespace: adapterstest.Namespace, Kind: "user", ID: "ada"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		r.t.Fatalf("start conversation: %v", err)
	}
	return acc.Operation.Ref
}

func (r *rig) operation(ref model.OperationRef) model.Operation {
	r.t.Helper()
	op, err := r.client().Operations().Get(context.Background(), model.Lookup[model.OperationRef]{
		Call: model.Call{Correlation: "test"}, Ref: ref,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return op
}

// supervised is one kit supervisor run of the rig's services.
type supervised struct {
	t      *testing.T
	cancel context.CancelFunc
	done   chan serve.Result
	svcs   map[string]daemon.Service
}

// run starts names under kit's supervisor, as `heo service start`
// does, with each service's stop budget.
func (r *rig) run(stop time.Duration, names ...string) *supervised {
	r.t.Helper()
	return r.runWith(stop, nil, names...)
}

// runWith is run with the supervisor's options, such as the publisher
// its lifecycle events go to.
func (r *rig) runWith(stop time.Duration, opts []serve.Option, names ...string) *supervised {
	r.t.Helper()
	reg := serve.NewRegistry()
	svcs := map[string]daemon.Service{}
	for _, s := range r.host.Services() {
		reg.Register(s)
		svcs[s.Name()] = s
	}
	configs := map[string]serve.Config{}
	for _, n := range names {
		if err := svcs[n].Validate(); err != nil {
			r.t.Fatalf("validate %s: %v", n, err)
		}
		configs[n] = serve.Config{Enabled: true, StopTimeout: stop}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &supervised{t: r.t, cancel: cancel, done: make(chan serve.Result, 1), svcs: svcs}
	sup := serve.NewSupervisor(reg, serve.SupervisorConfig{ShutdownTimeout: 3 * stop}, opts...)
	go func() { s.done <- sup.Run(ctx, names, configs) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(10 * time.Second):
		}
	})
	return s
}

// ready waits until every named service is ready, or the run ends.
func (s *supervised) ready(names ...string) {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := true
		for _, n := range names {
			all = all && s.svcs[n].Ready()
		}
		if all {
			return
		}
		select {
		case res := <-s.done:
			s.done <- res
			s.t.Fatalf("run ended before %v were ready: %s %v", names, res.Outcome, res.Err)
		default:
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%v never ready", names)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// stop cancels the run, as a signal does, and waits for its result.
func (s *supervised) stop() serve.Result {
	s.t.Helper()
	s.cancel()
	return s.result()
}

func (s *supervised) result() serve.Result {
	s.t.Helper()
	select {
	case res := <-s.done:
		s.done <- res
		return res
	case <-time.After(10 * time.Second):
		s.t.Fatal("run did not end")
		return serve.Result{}
	}
}

func service(st daemon.Status, name string) daemon.ServiceStatus {
	for _, s := range st.Services {
		if s.Name == name {
			return s
		}
	}
	return daemon.ServiceStatus{}
}

func TestEmptyInstallReady(t *testing.T) {
	r := newRig(t, "acme")
	s := r.run(5*time.Second, daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook)
	s.ready(daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook)
	st := r.host.Status()
	if !st.Ready || st.Storage.State != daemon.StorageReady {
		t.Fatalf("status = %+v", st)
	}
	for _, n := range []string{daemon.NameWorkers, daemon.NameReceivers, daemon.NameWebhook} {
		if got := service(st, n).State; got != daemon.StateReady {
			t.Errorf("%s = %s, want ready", n, got)
		}
	}
	addr := s.svcs[daemon.NameWebhook].(serve.Addressed).Addr()
	code, body := get(t, "http://"+addr+"/readyz")
	if code != http.StatusOK {
		t.Errorf("/readyz = %d %s", code, body)
	}
	res := s.stop()
	if res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times, want once, by the last service to stop", n)
	}
	if st := r.host.Status(); st.Storage.State != daemon.StorageClosed || service(st, daemon.NameWorkers).State != daemon.StateStopped {
		t.Errorf("after stop: %+v", st)
	}
}

// TestAdaptersComposedOverOpenedStore: the adapters are built once per
// open, over the store the daemon opened as its owner, so an adapter
// keeping state of its own keeps it there.
func TestAdaptersComposedOverOpenedStore(t *testing.T) {
	r := newRig(t, "acme")
	for range 2 {
		s := r.run(5*time.Second, daemon.NameWorkers, daemon.NameReceivers)
		s.ready(daemon.NameWorkers, daemon.NameReceivers)
		if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
			t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
		}
	}
	if len(r.opened) != 2 || r.opened[0] != r.storage.st || r.opened[1] != r.storage.st {
		t.Fatalf("adapters composed over %v, want the opened store once per open", r.opened)
	}
}

// TestAdaptersRefusedFailsOpen: adapters that cannot be composed over
// the store refuse the start and release the store.
func TestAdaptersRefusedFailsOpen(t *testing.T) {
	r := newRig(t, "acme")
	r.deps.Adapters = func(store.Store) (*adapters.Registry, error) { return nil, errors.New("retain: bad") }
	res := r.run(5*time.Second, daemon.NameWorkers).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "retain: bad") {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times, want once", n)
	}
}

// TestDrainFinishesInFlightWork: a signal during a send stops nothing
// until the send completes; the operation commits, and its context is
// never cancelled.
func TestDrainFinishesInFlightWork(t *testing.T) {
	r := newRig(t, "acme")
	op := r.queueSend()
	r.fake.Hold()
	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	select {
	case <-r.fake.Sending():
	case <-time.After(5 * time.Second):
		t.Fatal("no worker picked the send up")
	}
	s.cancel()
	select {
	case res := <-s.done:
		t.Fatalf("run ended with a send in flight: %s %v", res.Outcome, res.Err)
	case <-time.After(300 * time.Millisecond):
	}
	if got := service(r.host.Status(), daemon.NameWorkers).State; got != daemon.StateStopping {
		t.Errorf("workers = %s while draining, want stopping", got)
	}
	r.fake.Release()
	res := s.result()
	if res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if n := r.fake.Interrupted(); n != 0 {
		t.Errorf("%d sends interrupted by the shutdown", n)
	}
	if got := r.operation(op).State; got != model.ProviderAccepted {
		t.Errorf("operation = %s, want provider_accepted", got)
	}
}

// TestDrainDeadlineAbandonsToLease: work still running at the drain
// deadline is abandoned, and the stop says so instead of exiting clean.
func TestDrainDeadlineAbandonsToLease(t *testing.T) {
	r := newRig(t, "acme", withSet(daemon.KeyWorkersDrainTimeout, "100ms"))
	r.queueSend()
	r.fake.Hold()
	t.Cleanup(r.fake.Release)
	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	<-r.fake.Sending()
	res := s.stop()
	if res.Outcome == serve.OutcomeCleanStop || res.Err == nil ||
		!strings.Contains(res.Err.Message, "drain did not finish within 100ms") {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if got := service(r.host.Status(), daemon.NameWorkers).State; got != daemon.StateFailed {
		t.Errorf("workers = %s, want failed", got)
	}
	if n := r.fake.Interrupted(); n != 1 {
		t.Errorf("%d sends abandoned, want the one in flight", n)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times after an abandoned drain, want once", n)
	}
}

// TestAbortDrainAbandonsInFlightWork: AbortDrain, the second shutdown
// signal, ends a drain that has all of its deadline left: the send in
// flight is interrupted at once and its item stays leased, and the stop
// says why.
func TestAbortDrainAbandonsInFlightWork(t *testing.T) {
	r := newRig(t, "acme", withSet(daemon.KeyWorkersDrainTimeout, "1m"))
	r.queueSend()
	r.fake.Hold()
	t.Cleanup(r.fake.Release)
	s := r.run(2*time.Minute, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	<-r.fake.Sending()
	s.cancel()
	waitFor(t, func() bool { return service(r.host.Status(), daemon.NameWorkers).State == daemon.StateStopping })
	select {
	case res := <-s.done:
		t.Fatalf("run ended with a send in flight before the abort: %s %v", res.Outcome, res.Err)
	case <-time.After(200 * time.Millisecond):
	}
	r.host.AbortDrain()
	var res serve.Result
	select {
	case res = <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain outlasted AbortDrain")
	}
	if res.Outcome == serve.OutcomeCleanStop || res.Err == nil || !strings.Contains(res.Err.Message, "second signal") {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if n := r.fake.Interrupted(); n != 1 {
		t.Errorf("%d sends interrupted, want the one in flight", n)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times after an aborted drain, want once", n)
	}
}

func TestDuplicateAccountRefused(t *testing.T) {
	r := newRig(t, "acme")
	a := r.channel("alpha", adapterstest.Config{Account: "acct-1"})
	b := r.channel("beta", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers)
	res := s.result()
	if res.Outcome != serve.OutcomeStartFailed {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	msg := res.Err.Message
	for _, want := range []string{"receive the same provider account", `"alpha"`, `"beta"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q: %s", want, msg)
		}
	}
	if r.fake.Receiving(a.ID) != 0 || r.fake.Receiving(b.ID) != 0 {
		t.Error("a duplicate account was received")
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times after a refused start, want once", n)
	}
}

func TestAccountClaimedAcrossProfiles(t *testing.T) {
	dir := t.TempDir()
	one := newRig(t, "one", withAccounts(dir))
	two := newRig(t, "two", withAccounts(dir))
	ch1 := one.channel("alpha", adapterstest.Config{Account: "shared"})
	ch2 := two.channel("beta", adapterstest.Config{Account: "shared"})
	two.channel("gamma", adapterstest.Config{Account: "own"})

	s1 := one.run(5*time.Second, daemon.NameReceivers)
	s1.ready(daemon.NameReceivers)
	waitFor(t, func() bool { return one.fake.Receiving(ch1.ID) == 1 })

	res := two.run(5*time.Second, daemon.NameReceivers).result()
	if res.Outcome != serve.OutcomeStartFailed || !strings.Contains(res.Err.Message, "already received by heo/one (pid ") {
		t.Fatalf("second profile: %s %v", res.Outcome, res.Err)
	}
	if two.fake.Receiving(ch2.ID) != 0 {
		t.Error("the second profile received a claimed account")
	}
	if res := s1.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("first profile: %s %v", res.Outcome, res.Err)
	}
	s2 := two.run(5*time.Second, daemon.NameReceivers)
	s2.ready(daemon.NameReceivers)
	waitFor(t, func() bool { return two.fake.Receiving(ch2.ID) == 1 })
}

// TestBrokenChannelDegradesByScope: one channel that fails for good is
// reported by scope; the others receive and the service stays ready.
func TestBrokenChannelDegradesByScope(t *testing.T) {
	r := newRig(t, "acme")
	good := r.channel("good", adapterstest.Config{Account: "acct-1"})
	bad := r.channel("bad", adapterstest.Config{Account: "acct-2", Broken: true})
	s := r.run(5*time.Second, daemon.NameReceivers)
	s.ready(daemon.NameReceivers)
	waitFor(t, func() bool { return r.fake.Receiving(good.ID) == 1 })
	var scope daemon.ScopeStatus
	waitFor(t, func() bool {
		for _, sc := range service(r.host.Status(), daemon.NameReceivers).Scopes {
			if sc.Scope == "channel:"+string(bad.ID) && sc.State == daemon.ScopeFailed {
				scope = sc
				return true
			}
		}
		return false
	})
	if scope.Key != "bad" || !strings.Contains(scope.Reason, "broken") {
		t.Errorf("scope = %+v", scope)
	}
	st := r.host.Status()
	if rs := service(st, daemon.NameReceivers); rs.State != daemon.StateDegraded {
		t.Errorf("receivers = %s, want degraded", rs.State)
	}
	if !st.Ready || !s.svcs[daemon.NameReceivers].Ready() {
		t.Error("one broken channel made the runtime unready")
	}
}

func TestMissingAdapterDegradesByScope(t *testing.T) {
	r := newRig(t, "acme", withoutAdapters())
	ch := r.channel("orphan", adapterstest.Config{Account: "acct-1"})
	s := r.run(5*time.Second, daemon.NameReceivers, daemon.NameWorkers)
	s.ready(daemon.NameReceivers, daemon.NameWorkers)
	rs := service(r.host.Status(), daemon.NameReceivers)
	if rs.State != daemon.StateDegraded || len(rs.Scopes) != 1 ||
		rs.Scopes[0].Scope != "channel:"+string(ch.ID) || !strings.Contains(rs.Scopes[0].Reason, "not in this heo build") {
		t.Errorf("receivers = %+v", rs)
	}
}

func TestOwnershipLostFailsRuntime(t *testing.T) {
	r := newRig(t, "acme")
	s := r.run(5*time.Second, daemon.NameWorkers, daemon.NameWebhook)
	s.ready(daemon.NameWorkers, daemon.NameWebhook)
	addr := s.svcs[daemon.NameWebhook].(serve.Addressed).Addr()
	r.storage.lost.Store(true)
	res := s.result()
	if res.Outcome != serve.OutcomeRuntimeCrash || !strings.Contains(res.Err.Message, "store ownership lost") {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if st := r.host.Status(); st.Ready {
		t.Errorf("status ready after ownership loss: %+v", st)
	}
	if _, err := http.Get("http://" + addr + "/readyz"); err == nil { //nolint:noctx // loopback test
		t.Error("webhook listener still serving after the run failed")
	}
}

func TestWebhookRoutesChannels(t *testing.T) {
	r := newRig(t, "acme", withSet(daemon.KeyWebhookPublicBaseURL, "https://hooks.example.com/heo/"))
	ch := r.channel("web", adapterstest.Config{Account: "acct-web"})
	broken := r.channel("down", adapterstest.Config{Account: "acct-down", Broken: true})
	s := r.run(5*time.Second, daemon.NameWebhook)
	s.ready(daemon.NameWebhook)
	addr := s.svcs[daemon.NameWebhook].(serve.Addressed).Addr()
	base := "http://" + addr
	for path, want := range map[string]int{
		daemon.WebhookPrefix + string(ch.ID):            http.StatusNoContent,
		daemon.WebhookPrefix + string(ch.ID) + "/event": http.StatusNoContent,
		daemon.WebhookPrefix + string(broken.ID):        http.StatusServiceUnavailable,
		daemon.WebhookPrefix + "nosuch":                 http.StatusNotFound,
		"/v1/commands":                                  http.StatusNotFound,
	} {
		resp, err := http.Post(base+path, "application/json", strings.NewReader("{}")) //nolint:noctx // loopback test
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("POST %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	ws := service(r.host.Status(), daemon.NameWebhook)
	if ws.State != daemon.StateDegraded || ws.Address != addr {
		t.Errorf("webhook = %+v", ws)
	}
	for _, sc := range ws.Scopes {
		if sc.Scope == "channel:"+string(ch.ID) &&
			sc.URL != "https://hooks.example.com/heo/webhooks/"+string(ch.ID) {
			t.Errorf("route URL = %q", sc.URL)
		}
	}
	if code, body := get(t, base+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz with one broken channel = %d %s", code, body)
	}
}

func TestWebhookAddressMustBeLoopback(t *testing.T) {
	v := viper.New()
	for k, val := range daemon.Defaults() {
		v.SetDefault(k, val)
	}
	v.Set(daemon.KeyWebhookAddr, "0.0.0.0:8083")
	if _, probs := daemon.ReadWebhook(v); len(probs) != 1 || probs[0].Key != daemon.KeyWebhookAddr {
		t.Errorf("non-loopback without opt-in: %+v", probs)
	}
	v.Set(daemon.KeyWebhookAllowRemote, true)
	if _, probs := daemon.ReadWebhook(v); len(probs) != 0 {
		t.Errorf("non-loopback with opt-in: %+v", probs)
	}
	v.Set(daemon.KeyWebhookPublicBaseURL, "https://user:pw@hooks.example.com")
	if _, probs := daemon.ReadWebhook(v); len(probs) != 1 || probs[0].Key != daemon.KeyWebhookPublicBaseURL {
		t.Errorf("base URL with credentials: %+v", probs)
	}
	v.Set(daemon.KeyWorkersConcurrency, 0)
	if _, probs := daemon.ReadWorkers(v); len(probs) != 1 {
		t.Errorf("zero workers: %+v", probs)
	}
}

// TestReceiversSettleTimeoutBelowReadyTimeout: the settle timeout must
// end before kit's ready timeout, or a channel retrying a transient
// start would fail the whole service instead of degrading it.
func TestReceiversSettleTimeoutBelowReadyTimeout(t *testing.T) {
	v := viper.New()
	for k, val := range daemon.Defaults() {
		v.SetDefault(k, val)
	}
	if c, probs := daemon.ReadReceivers(v); len(probs) != 0 || c.SettleTimeout != daemon.DefaultReceiversSettleTimeout {
		t.Errorf("defaults: %+v %+v", c, probs)
	}
	v.Set(daemon.KeyReceiversSettleTimeout, "30s")
	if _, probs := daemon.ReadReceivers(v); len(probs) != 1 || probs[0].Key != daemon.KeyReceiversSettleTimeout {
		t.Errorf("settle timeout at the default ready timeout: %+v", probs)
	}
	v.Set("services."+daemon.NameReceivers+".ready_timeout", "1m")
	if _, probs := daemon.ReadReceivers(v); len(probs) != 0 {
		t.Errorf("settle timeout below a raised ready timeout: %+v", probs)
	}
	v.Set(daemon.KeyReceiversSettleTimeout, "soon")
	if _, probs := daemon.ReadReceivers(v); len(probs) != 1 {
		t.Errorf("malformed settle timeout: %+v", probs)
	}
}

// TestRuntimeRefusesWithoutPolicy: the runtime never composes an engine
// without an authorization policy: deny by default has no fallback.
func TestRuntimeRefusesWithoutPolicy(t *testing.T) {
	r := newRig(t, "acme")
	r.deps.Authorizer = nil
	s := r.run(time.Second, daemon.NameWorkers)
	res := s.result()
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no authorization policy") {
		t.Fatalf("run without a policy = %s %v", res.Outcome, res.Err)
	}
}

// TestWorkersRecheckThePolicy: a worker reauthorizes a queued
// operation for the actor that queued it, under the daemon's policy,
// before any provider effect: the actor the policy grants gets its
// send, one it does not — queued past the policy, or revoked since —
// never reaches the provider.
func TestWorkersRecheckThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		grants []any
		want   model.DeliveryState
	}{
		{"granted", []any{map[string]any{"actor": "operator", "roles": []any{"desk"}}}, model.ProviderAccepted},
		{"not granted", nil, model.Failed},
		{"read only", []any{map[string]any{"actor": "operator", "roles": []any{"viewer"}}}, model.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, "acme")
			v := viper.New()
			v.Set(authz.KeyOwner, []any{})
			v.Set(authz.KeyGrants, tc.grants)
			p, err := authz.FromConfig(v, "acme", "uid:0", daemon.ServiceActor)
			if err != nil {
				t.Fatal(err)
			}
			r.deps.Authorizer = p
			op := r.queueSend()
			s := r.run(5*time.Second, daemon.NameWorkers)
			s.ready(daemon.NameWorkers)
			var got model.Operation
			waitFor(t, func() bool {
				got = r.operation(op)
				return got.State != model.Queued && got.State != model.RetryPending
			})
			if got.State != tc.want {
				t.Errorf("operation = %s (%+v), want %s", got.State, got.Failure, tc.want)
			}
			if tc.want == model.Failed {
				if got.Failure == nil || got.Failure.Code != model.Unauthorized {
					t.Errorf("failure = %+v, want unauthorized", got.Failure)
				}
				select {
				case req := <-r.fake.Sending():
					t.Errorf("the provider was called: %+v", req)
				default:
				}
			}
			if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
				t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
			}
		})
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // loopback test URL
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStatusBeforeStartReportsStoreClosed: a daemon whose runtime
// services never started reports its store closed, not a blank state.
func TestStatusBeforeStartReportsStoreClosed(t *testing.T) {
	st := daemon.New(daemon.Options{}).Status()
	if st.Storage.State != daemon.StorageClosed {
		t.Errorf("storage state = %q, want %q", st.Storage.State, daemon.StorageClosed)
	}
	for _, s := range st.Services {
		if s.State != daemon.StateNotStarted {
			t.Errorf("%s = %q, want %q", s.Name, s.State, daemon.StateNotStarted)
		}
	}
}
