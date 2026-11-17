package bridge

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"hop.top/kit/go/runtime/bus"
)

// Scope is who a subscriber is.
type Scope int

const (
	// ScopePublic subscribers receive public domain events only
	// (uma.public.*.*). It is the zero value: an unset or unknown scope
	// gets the narrowest delivery.
	ScopePublic Scope = iota
	// ScopeDesk subscribers receive everything on the bus: desk-only
	// domain events, the bridge's health and the daemon's lifecycle.
	ScopeDesk
)

// DefaultQueue is how many events a subscriber may have waiting before
// further ones are dropped for it.
const DefaultQueue = 256

// HubOption configures a Hub.
type HubOption func(*Hub)

// WithQueue sets the events a subscriber may have waiting.
func WithQueue(n int) HubOption {
	return func(h *Hub) {
		if n > 0 {
			h.queue = n
		}
	}
}

// Hub is heo's one bus: kit's in-memory bus, with the audience rule on
// its subscribers and a bounded queue in front of each.
//
// Kit's bus delivers best-effort and in memory, but it does not shield
// the publisher from a slow subscriber: a synchronous handler runs on
// the publisher's goroutine, and an asynchronous one holds one of 256
// slots until it returns, after which Publish waits. Hub subscribers
// are therefore handed events through a queue of their own and dropped
// for when it is full, so a stuck or slow subscriber costs only its own
// events, never the daemon or the other subscribers.
//
// The bus is made on first use: kit reads KIT_BUS_SINK when it makes
// one, and heo processes that never publish should not open the file it
// names. That sink, when an operator sets it, tees every event, desk
// ones included, to a file the operator owns; payloads are references
// only, so the file holds no content.
type Hub struct {
	queue   int
	once    sync.Once
	b       bus.Bus
	closed  atomic.Bool
	dropped atomic.Uint64
}

// NewHub returns a Hub. It touches nothing until it is used.
func NewHub(opts ...HubOption) *Hub {
	h := &Hub{queue: DefaultQueue}
	for _, o := range opts {
		o(h)
	}
	return h
}

func (h *Hub) bus() bus.Bus {
	h.once.Do(func() {
		// Strict: a topic that breaks the notation is the publisher's
		// error, not an event no pattern matches.
		h.b = bus.New(bus.WithEnforce(bus.ModeStrict))
	})
	return h.b
}

// Publish delivers e to the subscribers whose scope may receive it. It
// is kit's serve.Publisher, so the daemon's lifecycle events ride the
// same bus. It never waits for a subscriber.
func (h *Hub) Publish(ctx context.Context, e bus.Event) error {
	if h.closed.Load() {
		return bus.ErrBusClosed
	}
	return h.bus().Publish(ctx, e)
}

// Dropped counts the events dropped because a subscriber's queue was
// full.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

// Close stops the hub. Events published after it are refused.
func (h *Hub) Close(ctx context.Context) error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	// A hub never used has made no bus, and opened no sink.
	h.once.Do(func() {})
	if h.b == nil {
		return nil
	}
	return h.b.Close(ctx)
}

// Subscribe calls fn, in one goroutine of its own, with each event
// matching pattern (kit's patterns: * one segment, # the trailing
// segments) that scope may receive. A public scope receives
// uma.public.* topics only, whatever the pattern; any scope other than
// ScopeDesk is public. The returned function ends the subscription.
//
// The subscription holds at most the hub's queue of events not yet
// handed to fn; past it new events are dropped for this subscription
// alone, and counted in Dropped.
func (h *Hub) Subscribe(scope Scope, pattern string, fn func(bus.Event)) (cancel func()) {
	q := make(chan bus.Event, h.queue)
	done := make(chan struct{})
	unsub := h.bus().Subscribe(pattern, func(_ context.Context, e bus.Event) error {
		if !receives(scope, e.Topic) {
			return nil
		}
		select {
		case q <- e:
		default:
			h.dropped.Add(1)
		}
		return nil
	})
	go func() {
		for {
			select {
			case e := <-q:
				fn(e)
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			unsub()
			close(done)
		})
	}
}

// receives reports whether a subscriber of scope may receive topic t.
// Anything but a public domain topic needs the desk scope.
func receives(scope Scope, t bus.Topic) bool {
	if scope == ScopeDesk {
		return true
	}
	return strings.HasPrefix(string(t), SourceDomain+"."+CategoryPublic+".")
}
