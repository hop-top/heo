package adapterstest

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Connection is one connection of a scripted receiver, handed to the
// function ScriptReceive installs.
type Connection struct {
	// N counts the receiver connections the fake has opened for the
	// channel, from 1: a connection uma's supervisor opens again after a
	// failure counts again.
	N int
	// Report hands the host the link: true when the connection is up,
	// false when it is lost or being re-established. It does nothing
	// unless the fake reports its link (ReportLink): an adapter that
	// implements no adapter.ReceiverReporter.
	Report func(connected bool)
	// Accept has the channel accept in through the receiver's sink, as an
	// inbound message does: the first one a connection gets accepted
	// ends the failures counted before it.
	Accept func(ctx context.Context, in Inject) error
}

// ScriptReceive makes every receiver connection run fn instead of
// waiting for its context to end: fn returns the failure that ends the
// connection (uma's supervisor retries a retryable one) or blocks until
// ctx ends. Call it before the engine starts.
func (f *Factory) ScriptReceive(fn func(ctx context.Context, c Connection) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = fn
}

// ReportLink makes the fake's instances implement
// adapter.ReceiverReporter, as the Discord gateway, Slack Socket Mode,
// Telegram polling and IMAP receivers do; without it they implement
// nothing of the kind. Call it before the engine starts.
func (f *Factory) ReportLink() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = true
}

// SlowOpen makes every open of an instance take d, as a provider that
// is slow to dial or to authenticate does: the time before the instance
// exists, in which a receiver cannot have a link to report.
func (f *Factory) SlowOpen(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openDelay = d
}

// reporting is an instance whose receiver reports its link.
type reporting struct{ *instance }

var _ adapter.ReceiverReporter = reporting{}

// ReportReceiver keeps report for the connection's script.
func (r reporting) ReportReceiver(report func(bool)) {
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	r.link = report
}

// runScript runs the scripted receiver connection n over sink.
func (i *instance) runScript(ctx context.Context, sink adapter.Sink, n int, fn func(context.Context, Connection) error) error {
	i.f.mu.Lock()
	link := i.link
	i.f.mu.Unlock()
	if link == nil {
		link = func(bool) {}
	}
	return fn(ctx, Connection{N: n, Report: link, Accept: func(ctx context.Context, in Inject) error {
		env, err := i.envelope(ctx, sink, in)
		if err != nil {
			return err
		}
		_, err = sink.Accept(ctx, model.Ingress{Call: model.Call{Correlation: "fake-" + model.CorrelationID(in.Event)}, Envelope: env})
		return err
	}})
}

// configured is the receiver connection the channel's configuration
// asks for: it fails with Config.FailReceive, else reports Config.Link,
// then waits for its context to end.
func (i *instance) configured(ctx context.Context, c Connection) error {
	if code := i.cfg.FailReceive; code != "" {
		c.Report(false)
		wait, _ := time.ParseDuration(i.cfg.RetryAfter)
		return &model.Error{Code: code, SafeMessage: "fake receiver failure", RetryAfter: wait}
	}
	if i.cfg.Link == "connected" {
		c.Report(true)
	}
	<-ctx.Done()
	return ctx.Err()
}

// Stagger marks the adapter Staggered: the daemon spreads the first
// start of its receivers. Call it before Adapter.
func (f *Factory) Stagger() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staggered = true
}
