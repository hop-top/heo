package daemon_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/daemon"
	"hop.top/kit/go/console/serve"
	"hop.top/uma/model"
)

// TestUseNeedsAnOpenRuntime: a daemon command reaches the engine and the
// adapters only while a runtime service holds the store open; before
// any starts, and after the last stops, Use refuses with ErrNotRunning
// and never opens the store itself.
func TestUseNeedsAnOpenRuntime(t *testing.T) {
	r := newRig(t, "acme")
	ctx := context.Background()
	called := false
	err := r.host.Use(ctx, func(daemon.Runtime) error { called = true; return nil })
	if !errors.Is(err, daemon.ErrNotRunning) || called {
		t.Fatalf("Use before start = %v (called %v), want ErrNotRunning", err, called)
	}
	if len(r.opened) != 0 {
		t.Fatalf("Use opened the store %d times", len(r.opened))
	}

	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	err = r.host.Use(ctx, func(rt daemon.Runtime) error {
		if _, ok := rt.Adapters.Lookup(adapterstest.Type); !ok {
			t.Error("runtime adapters lack the fake")
		}
		c, err := rt.Engine.For(model.Principal{Tenant: "acme", Actor: "uid:1"})
		if err != nil {
			return err
		}
		_, err = c.Configuration().Channels(ctx, model.ListRequest{Call: model.Call{Correlation: "t"}, Page: model.PageRequest{Limit: 1}})
		return err
	})
	if err != nil {
		t.Fatalf("Use while running: %v", err)
	}
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if err := r.host.Use(ctx, func(daemon.Runtime) error { return nil }); !errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("Use after stop = %v, want ErrNotRunning", err)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times, want once", n)
	}
}

// TestUseHoldsTheRuntimeOpen: the last service stopping while a command
// runs leaves the store open until the command returns.
func TestUseHoldsTheRuntimeOpen(t *testing.T) {
	r := newRig(t, "acme")
	s := r.run(5*time.Second, daemon.NameWorkers)
	s.ready(daemon.NameWorkers)
	inUse, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.host.Use(context.Background(), func(daemon.Runtime) error {
			close(inUse)
			<-finish
			return nil
		})
	}()
	<-inUse
	if res := s.stop(); res.Outcome != serve.OutcomeCleanStop {
		t.Fatalf("outcome %s: %v", res.Outcome, res.Err)
	}
	if n := r.storage.closes.Load(); n != 0 {
		t.Fatalf("store closed while a command used it")
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := r.storage.closes.Load(); n != 1 {
		t.Errorf("store closed %d times after the command, want once", n)
	}
}
