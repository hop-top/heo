package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/heo/internal/approvechat"
	"hop.top/heo/internal/authz"
)

// ApprovalChatConsumer names the approval chat service's subscription
// in uma: its own, apart from the bridge's and from any `event watch`.
const ApprovalChatConsumer = "heo-approval-chat"

// ApprovalChatScope is the service's scope in its status: its
// subscription.
const ApprovalChatScope = "consumer:" + ApprovalChatConsumer

// approvalChatOpenTimeout bounds opening the subscription before the
// service reports ready; a store that does not answer in time leaves the
// service degraded and retrying, not the daemon waiting.
const approvalChatOpenTimeout = 10 * time.Second

// approvalChatSvc connects held_operations.chat to the daemon: it reads
// uma's event log as a durable subscription of its own, as its own
// actor (authz.ApprovalChatActor), announces held operations to the
// configured conversations, closes the announcements and turns the
// replies and reactions of mapped members into decisions made as the
// actor each is mapped to. It runs nothing, and opens no store, while
// held_operations.chat is not enabled.
type approvalChatSvc struct {
	h *Host
	s *serviceState

	ready atomic.Bool
	// idle is set while the service has nothing to run: the
	// configuration leaves it off.
	idle atomic.Bool

	mu     sync.Mutex
	run    *running
	cancel context.CancelFunc
	done   chan struct{}
}

// ApprovalChat returns the approval chat service.
func (h *Host) ApprovalChat() Service { return h.chat }

func (c *approvalChatSvc) Name() string { return NameApprovalChat }

// Validate checks held_operations.chat.
func (c *approvalChatSvc) Validate() error {
	cfg, err := approvechat.Load(c.h.viper())
	if err != nil {
		return err
	}
	return refuseSelf(cfg)
}

// refuseSelf refuses a configuration that maps a member to the
// service's own actor: the announcement's sender never decides.
func refuseSelf(cfg approvechat.Config) error {
	for _, cv := range cfg.Conversations {
		for _, m := range cv.Members {
			if m.Actor == authz.ApprovalChatActor {
				return fmt.Errorf("%s: %s: the approval chat's own actor may not decide a held operation", approvechat.Key, cv.Key())
			}
		}
	}
	return nil
}

// Ready reports whether the service runs on an owned store, or is idle.
func (c *approvalChatSvc) Ready() bool {
	if !c.ready.Load() {
		return false
	}
	if c.idle.Load() {
		return true
	}
	c.mu.Lock()
	r := c.run
	c.mu.Unlock()
	return r != nil && r.storageHealthy()
}

// Start opens the store as its owner, if no runtime service has, opens
// the subscription (at the tail when it is new, at its checkpoint when
// it exists, so announcements missed while the daemon was down are made
// on the next start) and starts reading. Failing to open it is not
// failing to start: the service is degraded and retries.
func (c *approvalChatSvc) Start(ctx context.Context, ready func()) error {
	cfg, err := approvechat.Load(c.h.viper())
	if err == nil {
		err = refuseSelf(cfg)
	}
	if err != nil {
		c.s.set(StateFailed, err)
		return err
	}
	c.s.reset()
	if !cfg.Enabled {
		c.idle.Store(true)
		c.ready.Store(true)
		c.s.set(StateReady, nil)
		ready()
		<-ctx.Done()
		return nil
	}
	c.idle.Store(false)
	r, err := c.h.acquire(ctx)
	if err != nil {
		c.s.set(StateFailed, err)
		return err
	}
	chat, err := newChatRunner(r, cfg, c.report)
	if err != nil {
		_ = c.h.release(context.WithoutCancel(ctx))
		c.s.set(StateFailed, err)
		return err
	}
	c.s.scope(ScopeStatus{Scope: ApprovalChatScope, State: ScopeStarting})
	octx, ocancel := context.WithTimeout(ctx, approvalChatOpenTimeout)
	if err := chat.open(octx); err != nil {
		r.logError("runtime: approval chat could not open its subscription yet; retrying", "error", err)
	}
	ocancel()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	c.mu.Lock()
	c.run, c.cancel, c.done = r, cancel, done
	c.mu.Unlock()
	go func() {
		defer close(done)
		chat.loop(runCtx)
	}()
	c.ready.Store(true)
	c.s.set(StateReady, nil)
	r.logInfo("runtime: approval chat running", "consumer", ApprovalChatConsumer, "conversations", len(cfg.Conversations))
	ready()
	select {
	case <-ctx.Done():
		return nil
	case <-r.Lost():
		c.ready.Store(false)
		err := r.lostError()
		c.s.set(StateFailed, err)
		return err
	}
}

// report records the service's state as its scope's: ready while it
// reads, waiting while it retries.
func (c *approvalChatSvc) report(failure string) {
	sc := ScopeStatus{Scope: ApprovalChatScope, State: ScopeReady}
	if failure != "" {
		sc.State, sc.Reason = ScopeWaiting, "retrying after "+failure
	}
	c.s.scope(sc)
}

// Stop ends the read: a batch in hand is handled and acknowledged,
// within ctx; the subscription is left open at its checkpoint, so the
// next start resumes where this one stopped.
func (c *approvalChatSvc) Stop(ctx context.Context) error {
	c.ready.Store(false)
	c.mu.Lock()
	r, cancel, done := c.run, c.cancel, c.done
	c.run, c.cancel, c.done = nil, nil, nil
	c.mu.Unlock()
	if r == nil {
		c.s.set(StateStopped, nil)
		return nil
	}
	c.s.set(StateStopping, nil)
	cancel()
	var errs []error
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, errors.New("approval chat did not return before the stop deadline; "+
			"its batch in hand is redelivered on the next start"))
	}
	if err := c.h.release(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		c.s.set(StateFailed, err)
		r.logError("runtime: approval chat stopped uncleanly", "error", err)
		return err
	}
	c.s.set(StateStopped, nil)
	r.logInfo("runtime: approval chat stopped")
	return nil
}
