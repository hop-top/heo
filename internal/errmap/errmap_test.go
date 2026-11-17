package errmap

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
	"hop.top/uma/model"
)

// TestUmaErrorExitMap: every uma code renders with its envelope code,
// exit code and retry class; a uma code added later without a row
// fails here.
func TestUmaErrorExitMap(t *testing.T) {
	want := map[model.ErrorCode]struct {
		code string
		exit int
	}{
		model.Validation: {"VALIDATION", 2}, model.NotFound: {"NOT_FOUND", 3},
		model.Unauthorized: {"UNAUTHORIZED", 5}, model.Conflict: {"CONFLICT", 4},
		model.Unsupported: {"UNSUPPORTED", 2}, model.Disabled: {"DISABLED", 4},
		model.Unavailable: {"UNAVAILABLE", 6}, model.RateLimited: {"PROVIDER_RATE_LIMITED", 6},
		model.Transient: {"TRANSIENT", 6}, model.Permanent: {"PROVIDER_PERMANENT", 1},
		model.OutcomeUnknown: {"OUTCOME_UNKNOWN", 8}, model.CursorExpired: {"CURSOR_EXPIRED", 4},
		model.SchemaMismatch: {"SCHEMA_MISMATCH", 1}, model.DeadlineExpired: {"DEADLINE_EXPIRED", 1},
	}
	for code, w := range want {
		if !code.Valid() {
			t.Errorf("%s is not a uma code", code)
		}
		e := Of(fmt.Errorf("wrapped: %w", model.NewError(code, "safe text")))
		if e.Code != w.code || e.ExitCode != w.exit || e.Message != "safe text" {
			t.Errorf("%s → %+v, want %s/%d", code, e, w.code, w.exit)
		}
		if e.Transience == "" {
			t.Errorf("%s has no transience", code)
		}
	}
	if len(umaClasses) != len(want) {
		t.Errorf("umaClasses has %d rows, want %d", len(umaClasses), len(want))
	}
	if e := Of(errors.New("plain")); e.Code != output.CodeGeneric || e.ExitCode != 1 {
		t.Errorf("plain error → %+v", e)
	}
	if e := Of(context.Canceled); e.ExitCode != output.ExitTransient {
		t.Errorf("cancel → %+v", e)
	}
	in := output.ConflictError("x")
	if Of(in) != in {
		t.Error("a kit error is re-wrapped")
	}
}

// TestWireRefusalMap: every refusal code the control socket answers
// renders with an envelope code and exit code; none is a success.
func TestWireRefusalMap(t *testing.T) {
	want := map[string]int{
		socket.CodeNotFound: 3, socket.CodeNotEnabled: 5, socket.CodeNotInvocable: 2,
		socket.CodeBlocked: 5, socket.CodeDenied: 5, socket.CodeUnauthenticated: 5,
		socket.CodeRateLimited: 6, socket.CodeQuotaExceeded: 6, socket.CodeOverloaded: 6,
		socket.CodeDeadlineExceeded: 6, socket.CodeConflict: 4, socket.CodeInvalid: 2,
		socket.CodeInternal: 1, "SOMETHING_NEW": 1,
	}
	for code, exit := range want {
		e := Wire(socket.Error{Code: code, Message: "m"})
		if e.ExitCode != exit || e.Message != "m" || e.Transience == "" {
			t.Errorf("%s → %+v, want exit %d", code, e, exit)
		}
	}
	if e := Wire(socket.Error{Code: socket.CodeRateLimited, RetryAfterMs: 1500}); e.SuggestedFix != "retry after 1.5s" {
		t.Errorf("retry hint = %q", e.SuggestedFix)
	}
}

// A read refused as too large keeps its envelope (the provider's
// permanent failure) and says what to do instead.
func TestTooLargeSuggestsTheProvider(t *testing.T) {
	e := Of(model.NewReasonError(model.Permanent, model.TooLarge, "message is over the read bound"))
	if e.Code != "PROVIDER_PERMANENT" || e.ExitCode != output.ExitGeneric || e.SuggestedFix == "" {
		t.Fatalf("too large → %+v", e)
	}
	if plain := Of(model.NewError(model.Permanent, "x")); plain.SuggestedFix != "" {
		t.Fatalf("a permanent failure without the reason got a fix: %+v", plain)
	}
}
