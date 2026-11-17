package daemon

import (
	"context"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
)

// Receiving is how the daemon runs one way a channel receives: the state
// of its scope in the receivers service (way receive: the adapter's
// persistent receiver, such as a Discord gateway) or the webhook service
// (way receive_http: the channel's route), and, for a receiver, who holds
// its receiver lease and what uma's receive call is doing. The scope
// follows uma's status of the call (receiverView), a few seconds behind
// at most; the call's own fields are read when asked.
type Receiving struct {
	// State is a scope state (starting, ready, waiting, failed), or
	// not_running when no service of this daemon runs the way for the
	// channel.
	State  string
	Reason string
	// Since is when the scope entered State; zero for not_running.
	Since time.Time
	// Path is the route on the webhook listener.
	Path string
	// Owner and Until are the receiver lease's holder and expiry; empty
	// unless way is receive and a receive call holds the lease.
	Owner string
	Until time.Time
	// Phase, Link, Failures, LastFailure and RetryAt are what uma's
	// receive call this daemon runs for the channel is doing: running,
	// retrying or ended; the link its adapter reports (connecting,
	// connected or degraded, empty for an adapter that reports none); the
	// consecutive connections that accepted nothing; the error code of
	// the last of them; and, while retrying, when the next connection
	// opens. They are empty for a lease another process holds, which
	// shows Owner and Until alone, and once the call has returned.
	Phase       string
	Link        string
	Failures    int
	LastFailure string
	RetryAt     time.Time
	// Detail is, while Link is degraded, the adapter's stable code for
	// what is failing (google.mail.push_denied); empty otherwise.
	Detail string
}

// StateNotRunning is Receiving.State when the daemon runs nothing for
// the way.
const StateNotRunning = "not_running"

// Receiving returns how the daemon runs way (receive or receive_http)
// for channel ref.
func (h *Host) Receiving(ctx context.Context, ref model.ChannelRef, way model.Capability) Receiving {
	name := NameReceivers
	if way == model.CapabilityReceiveHTTP {
		name = NameWebhook
	}
	var svc ServiceStatus
	for _, s := range h.Status().Services {
		if s.Name == name {
			svc = s
		}
	}
	if svc.State == StateNotStarted || svc.State == "" {
		return Receiving{State: StateNotRunning, Reason: "the " + name + " service is not running in this daemon"}
	}
	scope := scopeName(model.Channel{Ref: ref})
	for _, sc := range svc.Scopes {
		if sc.Scope != scope {
			continue
		}
		out := Receiving{State: string(sc.State), Reason: sc.Reason, Since: sc.Since, Path: sc.Path}
		if way == model.CapabilityReceive {
			st := h.receiverStatus(ctx, ref)
			out.Owner, out.Until = st.Owner, st.Until
			out.Phase, out.Link, out.Failures = string(st.Phase), string(st.Link), st.Failures
			out.LastFailure, out.RetryAt, out.Detail = string(st.Reason), st.RetryAt, st.Detail
		}
		return out
	}
	return Receiving{State: StateNotRunning, Reason: "the " + name + " service runs nothing for this channel"}
}

// receiverStatus is uma's status of the channel's receiver: the lease's
// holder and expiry and, when this daemon holds it, what the receive call
// is doing. It is one read of the store and of the call's memory; empty
// when the daemon holds no store open or the status cannot be read.
func (h *Host) receiverStatus(ctx context.Context, ref model.ChannelRef) uma.ReceiverStatus {
	r, done, err := h.hold(ctx)
	if err != nil {
		return uma.ReceiverStatus{}
	}
	defer done()
	st, err := r.engine.Runtime().ReceiverStatus(ctx, r.call(), ref)
	if err != nil {
		return uma.ReceiverStatus{}
	}
	return st
}
