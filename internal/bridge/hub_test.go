package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/heo/internal/bridge"
	"hop.top/kit/go/runtime/bus"
)

const (
	publicTopic bus.Topic = "uma.public.message.created"
	deskTopic   bus.Topic = "uma.desk.message.created"
	healthTopic bus.Topic = "heo.runtime.bridge.degraded"
	kitTopic    bus.Topic = "kit.serve.service.started"
)

// collector gathers the topics a subscriber received.
type collector struct {
	mu     sync.Mutex
	topics []bus.Topic
}

func (c *collector) add(e bus.Event) {
	c.mu.Lock()
	c.topics = append(c.topics, e.Topic)
	c.mu.Unlock()
}

func (c *collector) got() []bus.Topic {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]bus.Topic(nil), c.topics...)
}

func (c *collector) waitFor(t *testing.T, n int) []bus.Topic {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := c.got(); len(got) >= n {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("received %v, want %d topics", c.got(), n)
	return nil
}

func settle() { time.Sleep(50 * time.Millisecond) }

func newHub(t *testing.T, opts ...bridge.HubOption) *bridge.Hub {
	t.Helper()
	h := bridge.NewHub(opts...)
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func publish(t *testing.T, h *bridge.Hub, topics ...bus.Topic) {
	t.Helper()
	for _, topic := range topics {
		if err := h.Publish(context.Background(), bus.NewEvent(topic, "test", map[string]string{"id": "x"})); err != nil {
			t.Fatalf("publish %s: %v", topic, err)
		}
	}
}

// TestPublicSubscriberNeverSeesDeskTopics: a subscriber that is not
// desk scoped receives uma.public.* events and nothing else, whatever
// pattern it subscribes with, wildcards that match everything included;
// a desk scoped subscriber receives both, and the bridge's health and
// kit's lifecycle events.
func TestPublicSubscriberNeverSeesDeskTopics(t *testing.T) {
	h := newHub(t)
	patterns := []string{"#", "uma.#", "uma.*.*.*", "uma.desk.#", "uma.desk.message.created", "*.*.*.*", "heo.#", "kit.#", "uma.*.message.*"}
	pub := map[string]*collector{}
	for _, p := range patterns {
		c := &collector{}
		pub[p] = c
		cancel := h.Subscribe(bridge.ScopePublic, p, c.add)
		defer cancel()
	}
	desk := &collector{}
	defer h.Subscribe(bridge.ScopeDesk, "#", desk.add)()

	publish(t, h, publicTopic, deskTopic, healthTopic, kitTopic)
	settle()
	if got := desk.waitFor(t, 4); len(got) != 4 {
		t.Errorf("desk scope received %v, want all four", got)
	}
	for p, c := range pub {
		for _, topic := range c.got() {
			if topic != publicTopic {
				t.Errorf("public subscriber on %q received %s", p, topic)
			}
		}
	}
	if got := pub["#"].got(); len(got) != 1 {
		t.Errorf("public # received %v, want the public event once", got)
	}
	if got := pub["uma.desk.#"].got(); len(got) != 0 {
		t.Errorf("a public subscriber on the desk namespace received %v", got)
	}
}

// TestUnknownScopeIsPublic: a scope that is not ScopeDesk gets the
// narrowest delivery.
func TestUnknownScopeIsPublic(t *testing.T) {
	h := newHub(t)
	c := &collector{}
	defer h.Subscribe(bridge.Scope(42), "#", c.add)()
	publish(t, h, deskTopic, publicTopic)
	if got := c.waitFor(t, 1); len(got) != 1 || got[0] != publicTopic {
		t.Errorf("received %v", got)
	}
	settle()
	if got := c.got(); len(got) != 1 {
		t.Errorf("received %v", got)
	}
}

// TestSlowSubscriberIsDroppedNotWaitedFor: a subscriber that never
// returns does not slow the publisher or another subscriber: events
// past its queue are dropped and counted, kit's bus semantics are
// best-effort.
func TestSlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	h := newHub(t, bridge.WithQueue(4))
	release := make(chan struct{})
	var started atomic.Int32
	stuck := h.Subscribe(bridge.ScopeDesk, "#", func(bus.Event) {
		started.Add(1)
		<-release
	})
	defer stuck()
	defer close(release)
	fast := &collector{}
	defer h.Subscribe(bridge.ScopeDesk, "#", fast.add)()

	const n = 2000
	done := make(chan struct{})
	go func() {
		for range n {
			publish(t, h, deskTopic)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a stuck subscriber")
	}
	if got := fast.waitFor(t, 1); len(got) == 0 {
		t.Fatal("the healthy subscriber received nothing")
	}
	if h.Dropped() == 0 {
		t.Error("nothing dropped for a subscriber that never returns")
	}
	if started.Load() != 1 {
		t.Errorf("stuck subscriber started %d times", started.Load())
	}
}

// TestCancelStopsDelivery: a cancelled subscription receives no more,
// and cancelling twice is harmless.
func TestCancelStopsDelivery(t *testing.T) {
	h := newHub(t)
	c := &collector{}
	cancel := h.Subscribe(bridge.ScopeDesk, "#", c.add)
	publish(t, h, deskTopic)
	c.waitFor(t, 1)
	cancel()
	cancel()
	publish(t, h, deskTopic, deskTopic)
	settle()
	if got := c.got(); len(got) != 1 {
		t.Errorf("received %v after cancel", got)
	}
}

// TestHubRefusesInvalidTopics: a topic that breaks kit's notation is an
// error at the publisher, not an event nobody matches.
func TestHubRefusesInvalidTopics(t *testing.T) {
	h := newHub(t)
	for _, topic := range []bus.Topic{"uma.public.message", "uma.public.message.created.x", "uma.*.message.created", "UMA.public.m.created", ""} {
		if err := h.Publish(context.Background(), bus.NewEvent(topic, "test", nil)); err == nil {
			t.Errorf("topic %q published", topic)
		}
	}
}

// TestHubIsLazy: building a hub, as every heo process does, touches
// nothing: kit's bus reads KIT_BUS_SINK when it is made, and opens the
// file it names.
func TestHubIsLazy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sink.jsonl")
	t.Setenv(bus.EnvSinkKind, "jsonl")
	t.Setenv(bus.EnvSinkPath, path)
	h := bridge.NewHub()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("NewHub opened the sink")
	}
	publish(t, h, deskTopic)
	if err := h.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // a path the test made in its own temp dir
	if err != nil || !strings.Contains(string(raw), string(deskTopic)) {
		t.Errorf("sink = %q, %v", raw, err)
	}
	if err := h.Publish(context.Background(), bus.NewEvent(deskTopic, "test", nil)); err == nil {
		t.Error("a closed hub accepted an event")
	}
}
