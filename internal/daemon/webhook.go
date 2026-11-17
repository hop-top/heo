package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"hop.top/kit/go/console/serve"
	"hop.top/kit/go/transport/api"
	"hop.top/uma/model"
)

// WebhookPrefix is where a channel's HTTP receiver is mounted on the
// webhook listener: <prefix>/<channel id>. The channel ID routes the
// request only; the adapter verifies every request it accepts.
const WebhookPrefix = "/webhooks/"

// webhook serves the HTTP receivers of every channel that receives
// over HTTP (its adapter declares receive_http and the channel's
// capability status does not report it disabled), on a listener of its own: never on a control
// surface. It also answers /healthz and /readyz for the runtime.
type webhook struct {
	h *Host
	s *serviceState

	ready atomic.Bool

	mu   sync.Mutex
	run  *running
	srv  *http.Server
	addr string
	// cfg, ctx and routes are the start's configuration, the context
	// channels mount under, and the channel routes; routes is nil while
	// the service does not run.
	cfg    WebhookConfig
	ctx    context.Context
	routes *channelRoutes
}

// Webhook returns the webhook service.
func (h *Host) Webhook() Service { return &webhook{h: h, s: h.svcs[NameWebhook]} }

func (wh *webhook) Name() string { return NameWebhook }

func (wh *webhook) Validate() error {
	_, probs := ReadWebhook(wh.h.viper())
	return problemsError(probs)
}

// Ready reports whether the listener serves on an owned store.
func (wh *webhook) Ready() bool {
	wh.mu.Lock()
	r := wh.run
	wh.mu.Unlock()
	return wh.ready.Load() && r != nil && r.storageHealthy()
}

// Addr is the bound listen address.
func (wh *webhook) Addr() string {
	wh.mu.Lock()
	defer wh.mu.Unlock()
	return wh.addr
}

// Start mounts each receiving channel's handler and binds the listener.
// A channel whose handler cannot be mounted answers 503 and is
// reported by scope; the others serve. While it runs, a reconcile
// mounts, replaces and removes channels' routes as channels change.
func (wh *webhook) Start(ctx context.Context, ready func()) error {
	cfg, probs := ReadWebhook(wh.h.viper())
	if err := problemsError(probs); err != nil {
		return err
	}
	wh.s.reset()
	r, err := wh.h.acquire(ctx)
	if err != nil {
		wh.s.set(StateFailed, err)
		return err
	}
	routes := &channelRoutes{byID: map[model.ID]*mounted{}}
	wh.mu.Lock()
	wh.cfg, wh.ctx, wh.routes = cfg, ctx, routes
	wh.mu.Unlock()
	fail := func(err error) error {
		wh.mu.Lock()
		wh.routes = nil
		wh.mu.Unlock()
		routes.clear()
		_ = wh.h.release(context.WithoutCancel(ctx))
		wh.s.set(StateFailed, err)
		return err
	}
	if err := r.attach(ctx, wh); err != nil {
		return fail(err)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		r.detach(wh)
		return fail(fmt.Errorf("listen %s: %w", cfg.Addr, err))
	}
	mux := http.NewServeMux()
	mux.Handle(WebhookPrefix, routes)
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	handler := api.HealthRoutes(mux, api.HealthConfig{
		Checks: wh.h.readinessChecks(ctx),
		Detail: isLoopback(host),
	})
	// Requests run under a context the shutdown cancels, so a
	// long-lived response, such as a widget event stream, ends when
	// the listener stops instead of holding the stop to its deadline.
	reqCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	srv := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: webhookReadHeaderTimeout,
		ReadTimeout:       webhookReadTimeout,
		WriteTimeout:      webhookWriteTimeout,
		IdleTimeout:       webhookIdleTimeout,
	}
	srv.RegisterOnShutdown(cancelRequests)
	wh.mu.Lock()
	wh.run, wh.srv, wh.addr = r, srv, ln.Addr().String()
	wh.mu.Unlock()
	wh.s.setAddr(ln.Addr().String())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	wh.ready.Store(true)
	wh.s.set(StateReady, nil)
	ready()
	select {
	case <-ctx.Done():
		return nil
	case err := <-served:
		cancelRequests()
		wh.ready.Store(false)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		wh.s.set(StateFailed, err)
		return err
	case <-r.Lost():
		wh.ready.Store(false)
		err := r.lostError()
		wh.s.set(StateFailed, err)
		return err
	}
}

// startChannels mounts each channel's routes.
func (wh *webhook) startChannels(r *running, chans []inbound) {
	wh.mu.Lock()
	routes, cfg, ctx := wh.routes, wh.cfg, wh.ctx
	wh.mu.Unlock()
	if routes == nil {
		return
	}
	for _, in := range chans {
		wh.mount(ctx, r, cfg, routes, in)
	}
}

// stopChannels unmounts each channel's routes, ending the requests in
// flight on them, and drops its scope.
func (wh *webhook) stopChannels(_ context.Context, chans []inbound) {
	wh.mu.Lock()
	routes := wh.routes
	wh.mu.Unlock()
	for _, in := range chans {
		if routes != nil {
			routes.remove(in.ch.Ref.ID)
		}
		wh.s.drop(scopeName(in.ch))
	}
}

// mount routes one channel's webhook, when it receives over HTTP: the
// adapter's receiver at the channel's route, and any extra routes the
// adapter serves beside it. A channel whose configuration switches
// receive_http off, such as a Telegram bot that polls, is not mounted
// and has no scope; nor is one whose adapter does not currently offer
// receive_http, such as a mailbox with push disabled.
func (wh *webhook) mount(ctx context.Context, r *running, cfg WebhookConfig, routes *channelRoutes, in inbound) {
	if in.disabled[model.CapabilityReceiveHTTP] {
		r.logInfo("runtime: webhook not mounted", "scope", scopeName(in.ch), "reason", "receive_http disabled by the channel's configuration")
		return
	}
	if !in.receives(model.CapabilityReceiveHTTP) {
		// A missing adapter is reported once, by the receivers.
		return
	}
	ch := in.ch
	path := WebhookPrefix + string(ch.Ref.ID)
	sc := channelScope(ch, ScopeReady, "")
	sc.Path = path
	if cfg.PublicBaseURL != "" {
		sc.URL = strings.TrimSuffix(cfg.PublicBaseURL, "/") + path
	}
	var h http.Handler
	err := in.unusable()
	if err == nil {
		h, err = r.engine.Runtime().Webhook(ctx, r.call(), ch.Ref)
		if model.IsCode(err, model.Unsupported) {
			r.logInfo("runtime: webhook not mounted", "scope", sc.Scope, "reason", err)
			return
		}
	}
	extra := map[string]http.Handler{}
	if err == nil && in.ad.Routes != nil {
		extra, err = checkRoutes(in.ad.Routes(ch))
	}
	if err != nil {
		sc.State, sc.Reason = ScopeFailed, err.Error()
		r.logError("runtime: webhook not mounted", "scope", sc.Scope, "error", err)
		h = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		})
		extra = nil
	}
	wh.s.scope(sc)
	mux := http.NewServeMux()
	routed := stripRoute(path, h)
	mux.Handle(path, routed)
	mux.Handle(path+"/", routed)
	for sub, rh := range extra {
		mux.Handle(path+sub, stripRoute(path, rh))
	}
	routes.put(ch.Ref.ID, mux)
}

// channelRoutes dispatches <prefix>/<channel id>[/...] to the routes
// mounted for that channel, which a reconcile replaces or removes one
// channel at a time; an unknown channel is 404. Safe for concurrent
// use.
type channelRoutes struct {
	mu   sync.RWMutex
	byID map[model.ID]*mounted
}

// mounted is one channel's routes and the context its requests run
// under, canceled once the routes are replaced or removed.
type mounted struct {
	h      http.Handler
	ctx    context.Context
	cancel context.CancelFunc
}

// put mounts h for channel id, replacing, and ending the requests of,
// what was mounted for it.
func (cr *channelRoutes) put(id model.ID, h http.Handler) {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:gosec // G118: stored with the mounted route and cancelled when it is replaced
	cr.mu.Lock()
	old := cr.byID[id]
	cr.byID[id] = &mounted{h: h, ctx: ctx, cancel: cancel}
	cr.mu.Unlock()
	if old != nil {
		old.cancel()
	}
}

// remove unmounts channel id and ends the requests in flight on it.
func (cr *channelRoutes) remove(id model.ID) {
	cr.mu.Lock()
	old := cr.byID[id]
	delete(cr.byID, id)
	cr.mu.Unlock()
	if old != nil {
		old.cancel()
	}
}

// clear unmounts every channel.
func (cr *channelRoutes) clear() {
	cr.mu.Lock()
	all := cr.byID
	cr.byID = map[model.ID]*mounted{}
	cr.mu.Unlock()
	for _, m := range all {
		m.cancel()
	}
}

func (cr *channelRoutes) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	id, _, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, WebhookPrefix), "/")
	cr.mu.RLock()
	m := cr.byID[model.ID(id)]
	cr.mu.RUnlock()
	if id == "" || m == nil {
		http.NotFound(w, req)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	defer context.AfterFunc(m.ctx, cancel)()
	m.h.ServeHTTP(w, req.WithContext(ctx))
}

// checkRoutes refuses an adapter route that would leave the channel's
// own: each is one or more "/"-led segments, without "." or "..".
func checkRoutes(routes map[string]http.Handler) (map[string]http.Handler, error) {
	for sub, h := range routes {
		if h == nil || !validRoute(sub) {
			return nil, fmt.Errorf("adapter route %q is not a path under the channel's route", sub)
		}
	}
	return routes, nil
}

func validRoute(sub string) bool {
	if !strings.HasPrefix(sub, "/") || strings.HasSuffix(sub, "/") {
		return false
	}
	for _, seg := range strings.Split(sub[1:], "/") {
		if seg == "" || seg == "." || seg == ".." || strings.ContainsAny(seg, "{}?#% ") {
			return false
		}
	}
	return true
}

// stripRoute hands h the request with the channel's route removed from
// its path, so an adapter sees paths relative to its mount.
func stripRoute(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r2 := req.Clone(req.Context())
		p := strings.TrimPrefix(req.URL.Path, prefix)
		if p == "" {
			p = "/"
		}
		r2.URL.Path = p
		r2.URL.RawPath = ""
		h.ServeHTTP(w, r2)
	})
}

// Stop stops accepting, lets in-flight requests finish within ctx, and
// releases the runtime.
func (wh *webhook) Stop(ctx context.Context) error {
	wh.ready.Store(false)
	wh.mu.Lock()
	r := wh.run
	wh.mu.Unlock()
	if r == nil {
		return nil
	}
	// No reconcile reaches the service from here on.
	r.detach(wh)
	wh.mu.Lock()
	srv := wh.srv
	wh.run, wh.srv, wh.routes = nil, nil, nil
	wh.mu.Unlock()
	wh.s.set(StateStopping, nil)
	var errs []error
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("webhook listener: %w", err))
		}
	}
	if err := wh.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		wh.s.set(StateFailed, err)
		return err
	}
	wh.s.set(StateStopped, nil)
	return nil
}

// readinessChecks are /readyz's inputs: the runtime configuration, the
// store, and each runtime service the run started. A service the run
// did not start is not checked; a degraded one is ready.
func (h *Host) readinessChecks(ctx context.Context) []api.ReadinessCheck {
	view, hasView := serve.RunViewFrom(ctx)
	checks := []api.ReadinessCheck{
		{Name: "config", Ready: func() bool { return len(blocking(Check(h.viper()))) == 0 }},
		{Name: "storage", Ready: h.StorageReady},
	}
	for _, name := range []string{NameWorkers, NameReceivers, NameWebhook, NameBridge, NameApprovalChat, NameHooks} {
		st := h.svcs[name]
		checks = append(checks, api.ReadinessCheck{Name: name, Ready: func() bool {
			if hasView {
				switch view.State(name) {
				case serve.StateNotInRun:
					return true
				case serve.StateReady:
				default:
					return false
				}
			}
			s := st.get()
			return s == StateReady || s == StateNotStarted
		}})
	}
	return checks
}

// blocking returns the problems that are not warnings.
func blocking(probs []Problem) []Problem {
	var out []Problem
	for _, p := range probs {
		if !p.Warning {
			out = append(out, p)
		}
	}
	return out
}
