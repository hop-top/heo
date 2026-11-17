package control

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/authz"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// AnnotationDaemon marks a command that runs in the daemon: one that
// needs the profile's runtime. The CLI sends it over the control
// socket; every served surface admits it only for a caller the
// transport verified (see Gate).
const AnnotationDaemon = "heo/daemon"

// MarkDaemon annotates c as daemon-backed.
func MarkDaemon(c *cobra.Command) {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[AnnotationDaemon] = "true"
}

// IsDaemon reports whether c is daemon-backed.
func IsDaemon(c *cobra.Command) bool {
	return c != nil && c.Annotations[AnnotationDaemon] == "true"
}

// Caller is who a daemon-side command acts for, decided by the daemon:
// the actor the transport verified and the profile's tenant.
type Caller struct {
	Actor   model.ActorID
	Tenant  model.TenantID
	Surface cmdsurface.Surface
}

// Principal is the uma principal the command calls the runtime as.
func (c Caller) Principal() model.Principal {
	return model.Principal{Tenant: c.Tenant, Actor: c.Actor}
}

// Actor returns the actor meta's transport verified: the control
// socket's peer (uid:<n>) or the subject of the credential an HTTP
// surface accepted. A caller only claimed, or established by the
// transport without a name (an owner-only socket without peer
// authentication, MCP over stdio), has no actor: UNAUTHORIZED, exit 5.
func Actor(meta cmdsurface.Meta) (model.ActorID, error) {
	if meta.Established != cmdsurface.EstablishedVerified || meta.Caller == "" {
		e := output.UnauthorizedError(fmt.Sprintf(
			"no verified caller on the %s surface: daemon commands act only for a caller the transport verified",
			surfaceName(meta.Surface)))
		e.SuggestedFix = "use the control socket with peer authentication, or an authenticated HTTP surface"
		return "", e
	}
	actor := model.ActorID(meta.Caller)
	if err := actor.Validate(); err != nil {
		e := output.UnauthorizedError("the verified caller is not a valid actor: " + err.Error())
		return "", e
	}
	return actor, nil
}

// CallerOf returns the caller of an admitted invocation: its verified
// actor and the profile's tenant. The tenant meta carries, claimed or
// verified, is never used: one daemon serves one tenant.
func CallerOf(meta cmdsurface.Meta, tenant model.TenantID) (Caller, error) {
	actor, err := Actor(meta)
	if err != nil {
		return Caller{}, err
	}
	return Caller{Actor: actor, Tenant: tenant, Surface: meta.Surface}, nil
}

// Relays reports whether meta is an MCP call over stdio: established by
// the spawn alone, with no name. Such a process forwards daemon
// commands over the control socket, where the daemon verifies the
// spawning user as the socket's peer.
func Relays(meta cmdsurface.Meta) bool {
	return meta.Surface == cmdsurface.SurfaceMCP && meta.Established == cmdsurface.EstablishedTransport
}

// Route is where a daemon-backed command runs.
type Route int

const (
	// Forward sends the command over the control socket.
	Forward Route = iota
	// Local runs it in this process, the daemon.
	Local
	// Refuse runs it nowhere: a served call reached a process that is
	// not the daemon and cannot relay it.
	Refuse
)

// RouteOf decides where a daemon-backed command runs. A command typed
// at the terminal is forwarded. A served one runs here when this
// process serves the control socket: it is the daemon. Otherwise only
// an MCP stdio call is forwarded, as a relay; any other served call
// is refused rather than re-sent under this process's identity.
func RouteOf(ctx context.Context, servesControl bool) (Route, cmdsurface.Meta) {
	meta, served := cmdsurface.AdmittedMeta(ctx)
	return Decide(meta, served, servesControl), meta
}

// Decide is RouteOf's rule for an invocation that is served or not,
// with meta its admitted metadata.
func Decide(meta cmdsurface.Meta, served, servesControl bool) Route {
	switch {
	case !served:
		return Forward
	case servesControl:
		return Local
	case Relays(meta):
		return Forward
	default:
		return Refuse
	}
}

// Admitter decides whether a verified actor may reach the runtime at
// all, and why not when it may not. heo's authorization policy is the
// one admitter; the same policy decides every uma operation after.
type Admitter func(actor model.ActorID) (ok bool, reason string)

// Gate is the permission gate every served surface applies to
// daemon-backed commands, the same on each: a caller the transport
// verified is admitted when admit admits its actor, an MCP stdio call
// is admitted to be relayed when this process is not the daemon (the
// daemon verifies and admits the spawning user), and anything else is
// denied. Other commands pass through to the deciders after it.
//
// It decides who may reach the runtime at all; what each actor may do
// there is decided again, by the same policy, for every uma call.
func Gate(servesControl func() bool, admit Admitter) cmdsurface.PermissionFunc {
	return func(_ context.Context, meta cmdsurface.Meta, leaf *cmdsurface.Leaf) cmdsurface.PermissionDecision {
		if leaf == nil || !IsDaemon(leaf.Cmd) {
			return cmdsurface.PermissionDecision{Allowed: true}
		}
		if actor, err := Actor(meta); err == nil {
			// A plugin authenticates by its token on an HTTP surface; a
			// uid the socket verified never names one, and a plugin
			// actor on the socket is refused whatever the policy says.
			if authz.IsPlugin(actor) && meta.Surface == cmdsurface.SurfaceSocket {
				return cmdsurface.PermissionDecision{Reason: ReasonPluginOnSocket}
			}
			if ok, reason := admit(actor); !ok {
				return cmdsurface.PermissionDecision{Reason: reason}
			}
			return cmdsurface.PermissionDecision{Allowed: true}
		}
		if Relays(meta) && !servesControl() {
			return cmdsurface.PermissionDecision{Allowed: true}
		}
		return cmdsurface.PermissionDecision{Reason: ReasonUnverified}
	}
}

// ReasonPluginOnSocket is the gate's refusal of a plugin actor on the
// control socket.
const ReasonPluginOnSocket = "heo.control.plugin_on_socket"

// ReasonUnverified is the gate's refusal reason.
const ReasonUnverified = "heo.control.unverified_caller"

func surfaceName(s cmdsurface.Surface) string {
	if s == "" {
		return "unknown"
	}
	return string(s)
}
