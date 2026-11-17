// Package errmap is heo's one map from failures to kit's structured
// error envelope: a uma error by its code, and a refusal the control
// socket answered by its wire code. Every command renders through it,
// so an error class has one envelope code, one exit code and one retry
// class whichever command, process or surface reports it.
package errmap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
	"hop.top/uma/model"
)

// ExitOutcomeUnknown is heo's own exit code for an operation whose
// provider outcome is unknown: never 0 (it may not have happened) and
// never 6 (retrying blindly may repeat it).
const ExitOutcomeUnknown = 8

// class is the envelope an error code renders as.
type class struct {
	code       string
	exit       int
	transience string
}

// CodeProviderPermission is the envelope code of a provider account
// lacking a permission a verb needs, where a command or a capability
// view surfaces it synchronously. It exits like a denied caller
// (UNAUTHORIZED, 5): refused, but the remedy is the provider's grant.
const CodeProviderPermission = "PROVIDER_PERMISSION"

// umaClasses maps every uma error code onto heo's envelope code, exit
// code and retry class: the kit standard class where uma's code is that
// class, a uma-derived code otherwise.
var umaClasses = map[model.ErrorCode]class{
	model.Validation:      {"VALIDATION", output.ExitUsage, output.TransiencePermanent},
	model.NotFound:        {output.CodeNotFound, output.ExitNotFound, output.TransiencePermanent},
	model.Unauthorized:    {output.CodeUnauthorized, output.ExitUnauthorized, output.TransiencePermanent},
	model.Conflict:        {output.CodeConflict, output.ExitConflict, output.TransiencePermanent},
	model.Unsupported:     {"UNSUPPORTED", output.ExitUsage, output.TransiencePermanent},
	model.Disabled:        {"DISABLED", output.ExitConflict, output.TransiencePermanent},
	model.Unavailable:     {"UNAVAILABLE", output.ExitTransient, output.TransienceTransient},
	model.RateLimited:     {"PROVIDER_RATE_LIMITED", output.ExitTransient, output.TransienceTransient},
	model.Transient:       {output.CodeTransient, output.ExitTransient, output.TransienceTransient},
	model.Permanent:       {"PROVIDER_PERMANENT", output.ExitGeneric, output.TransiencePermanent},
	model.OutcomeUnknown:  {"OUTCOME_UNKNOWN", ExitOutcomeUnknown, output.TransienceUnknown},
	model.CursorExpired:   {"CURSOR_EXPIRED", output.ExitConflict, output.TransiencePermanent},
	model.SchemaMismatch:  {"SCHEMA_MISMATCH", output.ExitGeneric, output.TransiencePermanent},
	model.DeadlineExpired: {"DEADLINE_EXPIRED", output.ExitGeneric, output.TransiencePermanent},
}

// Of renders err as kit's structured error: a uma error by its code, a
// cancellation as transient, anything else as GENERIC. An error that
// already is one passes through.
func Of(err error) *output.Error {
	if err == nil {
		return nil
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return output.TransientError(err.Error())
	}
	var me *model.Error
	if !errors.As(err, &me) {
		return &output.Error{Code: output.CodeGeneric, Message: err.Error(),
			ExitCode: output.ExitGeneric, Transience: output.TransienceUnknown}
	}
	c, ok := umaClasses[me.Code]
	if !ok {
		c = class{output.CodeGeneric, output.ExitGeneric, output.TransienceUnknown}
	}
	e := &output.Error{Code: c.code, Message: me.SafeMessage, ExitCode: c.exit, Transience: c.transience}
	if me.Correlation != "" {
		e.Cause = "correlation=" + string(me.Correlation)
	}
	if me.Reason == model.TooLarge {
		// A read over its bound is refused whole, never cut: say so, and
		// where the whole thing is.
		e.SuggestedFix = "the read is over its size bound and is never truncated: open the message at its provider instead"
	}
	return e
}

// wireClasses maps the control socket's refusal codes. A refusal means
// the command never ran: the daemon turned the request away.
var wireClasses = map[string]class{
	// The daemon's tree has no such command: it runs another heo
	// version than this client.
	socket.CodeNotFound:        {output.CodeNotFound, output.ExitNotFound, output.TransiencePermanent},
	socket.CodeNotEnabled:      {output.CodeUnauthorized, output.ExitUnauthorized, output.TransiencePermanent},
	socket.CodeNotInvocable:    {output.CodeUsage, output.ExitUsage, output.TransiencePermanent},
	socket.CodeBlocked:         {output.CodeUnauthorized, output.ExitUnauthorized, output.TransiencePermanent},
	socket.CodeDenied:          {output.CodeUnauthorized, output.ExitUnauthorized, output.TransiencePermanent},
	socket.CodeUnauthenticated: {output.CodeUnauthorized, output.ExitUnauthorized, output.TransiencePermanent},
	// Throttling at the daemon is transient, as a provider's is: kit's
	// own RATE_LIMITED (64) stays the local --max-ops budget.
	socket.CodeRateLimited:      {output.CodeTransient, output.ExitTransient, output.TransienceTransient},
	socket.CodeQuotaExceeded:    {output.CodeTransient, output.ExitTransient, output.TransienceTransient},
	socket.CodeOverloaded:       {output.CodeTransient, output.ExitTransient, output.TransienceTransient},
	socket.CodeDeadlineExceeded: {output.CodeTransient, output.ExitTransient, output.TransienceTransient},
	socket.CodeConflict:         {output.CodeConflict, output.ExitConflict, output.TransiencePermanent},
	socket.CodeInvalid:          {output.CodeUsage, output.ExitUsage, output.TransiencePermanent},
	socket.CodeInternal:         {output.CodeGeneric, output.ExitGeneric, output.TransienceUnknown},
}

// Wire renders a refusal the control socket answered. An unknown code
// is GENERIC. The wire code stays readable in Cause.
func Wire(e socket.Error) *output.Error {
	c, ok := wireClasses[e.Code]
	if !ok {
		c = class{output.CodeGeneric, output.ExitGeneric, output.TransienceUnknown}
	}
	out := &output.Error{Code: c.code, Message: e.Message, ExitCode: c.exit, Transience: c.transience,
		Cause: "daemon refused the request: " + e.Code}
	switch e.Code {
	case socket.CodeNotFound:
		out.SuggestedFix = "the daemon runs another heo version: restart it on this one"
	case socket.CodeBlocked, socket.CodeDenied, socket.CodeNotEnabled:
		out.SuggestedFix = "ask the installation owner to permit it; do not retry unchanged"
	}
	if e.RetryAfterMs > 0 {
		out.SuggestedFix = fmt.Sprintf("retry after %s", time.Duration(e.RetryAfterMs)*time.Millisecond)
	}
	return out
}
