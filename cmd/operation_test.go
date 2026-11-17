package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

func TestTerminalErrorMapsTheState(t *testing.T) {
	failure := func(c model.ErrorCode) *model.Error { return model.NewError(c, "boom") }
	for _, tc := range []struct {
		name string
		op   model.Operation
		exit int // 0: success
		code string
	}{
		{"delivered", model.Operation{State: model.Delivered}, 0, ""},
		{"provider_accepted", model.Operation{State: model.ProviderAccepted}, 0, ""},
		{"committed", model.Operation{State: model.Committed}, 0, ""},
		{"dead_lettered", model.Operation{State: model.DeadLettered, Failure: failure(model.Permanent)}, 1, "PROVIDER_PERMANENT"},
		{"failed unauthorized", model.Operation{State: model.Failed, Failure: failure(model.Unauthorized)}, 5, "UNAUTHORIZED"},
		{"rejected validation", model.Operation{State: model.Rejected, Failure: failure(model.Validation)}, 2, "VALIDATION"},
		{"failed without a failure", model.Operation{State: model.Failed}, 1, "PROVIDER_PERMANENT"},
		{"outcome_unknown", model.Operation{State: model.Unknown, Failure: failure(model.OutcomeUnknown)}, 8, "OUTCOME_UNKNOWN"},
		{"outcome_unknown without a failure", model.Operation{State: model.Unknown}, 8, "OUTCOME_UNKNOWN"},
	} {
		tc.op.Ref.ID = "op_1"
		err := terminalError(tc.op)
		if tc.exit == 0 {
			if err != nil {
				t.Errorf("%s: %v, want success", tc.name, err)
			}
			continue
		}
		var oe *output.Error
		if !errors.As(err, &oe) || oe.ExitCode != tc.exit || oe.Code != tc.code || !strings.Contains(oe.Message, "op_1 ended "+string(tc.op.State)) {
			t.Errorf("%s: %v, want exit %d %s naming the state", tc.name, err, tc.exit, tc.code)
		}
	}
}

func TestParseTarget(t *testing.T) {
	got, err := parseTarget("acme", "conversation:in_1/conv_9")
	if err != nil || got != (model.Ref{Tenant: "acme", Kind: model.KindConversation, Inbox: "in_1", ID: "conv_9"}) {
		t.Fatalf("nested = %+v, %v", got, err)
	}
	if got, err = parseTarget("acme", "inbox:in_1"); err != nil || got != (model.Ref{Tenant: "acme", Kind: model.KindInbox, ID: "in_1"}) {
		t.Fatalf("flat = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "inbox", "inbox:", ":x", "conversation:/c", "conversation:in/"} {
		var oe *output.Error
		if _, err := parseTarget("acme", bad); !errors.As(err, &oe) || oe.ExitCode != 2 {
			t.Errorf("parseTarget(%q) = %v, want a usage error", bad, err)
		}
	}
	// A target round-trips through its spelling.
	ref := model.Ref{Tenant: "acme", Kind: model.KindConversation, Inbox: "in_1", ID: "conv_9"}
	if back, _ := parseTarget("acme", targetName(ref)); back != ref {
		t.Errorf("round trip = %+v", back)
	}
}

// follow runs the watch loop over a scripted operation.
func follow(t *testing.T, timeout time.Duration, ctx context.Context, step opStepper) (string, string, error) {
	t.Helper()
	old, oldBase := opPoll, retryBase
	opPoll, retryBase = 5*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { opPoll, retryBase = old, oldBase })
	a := newApp()
	a.r.Viper.Set("format", output.JSON)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err := a.followOperation(ctx, cmd, "op_1", step, timeout, 0)
	return out.String(), errb.String(), err
}

func scripted(states ...operationOut) opStepper {
	i := 0
	return func(context.Context) (operationOut, error) {
		s := states[min(i, len(states)-1)]
		i++
		s.ID = "op_1"
		s.Terminal = s.State != model.Queued && s.State != model.RetryPending
		return s, nil
	}
}

// TestFollowOperationWritesChangesAndEndsOnTerminal: a state is written
// when it changes, not on every read, and the watch ends with the exit
// of the state it reaches.
func TestFollowOperationWritesChangesAndEndsOnTerminal(t *testing.T) {
	out, _, err := follow(t, 0, t.Context(), scripted(
		operationOut{State: model.Queued, Revision: "r1"}, operationOut{State: model.Queued, Revision: "r1"},
		operationOut{State: model.RetryPending, Revision: "r2"}, operationOut{State: model.Delivered, Revision: "r3"},
	))
	if err != nil {
		t.Fatalf("delivered: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"state":"queued"`) || !strings.Contains(lines[2], `"state":"delivered"`) {
		t.Fatalf("lines:\n%s", out)
	}

	_, _, err = follow(t, 0, t.Context(), scripted(operationOut{State: model.Queued},
		operationOut{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "unknown")}))
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != 8 {
		t.Fatalf("outcome_unknown ended with %v, want exit 8", err)
	}
}

func TestFollowOperationTimeoutAndInterrupt(t *testing.T) {
	_, _, err := follow(t, 30*time.Millisecond, t.Context(), scripted(operationOut{State: model.Queued}))
	var oe *output.Error
	if !errors.As(err, &oe) || oe.ExitCode != 6 || !strings.Contains(oe.Message, "still queued") {
		t.Fatalf("timeout = %v, want exit 6 naming the state", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	n := 0
	_, stderr, err := follow(t, 0, ctx, func(context.Context) (operationOut, error) {
		if n++; n == 3 {
			cancel()
		}
		return operationOut{ID: "op_1", State: model.Queued}, nil
	})
	if err != nil || !strings.Contains(stderr, "stopped: operation op_1 was queued") {
		t.Fatalf("interrupt = %v, stderr %q; want success and where it stopped", err, stderr)
	}
}

func TestFollowOperationRetriesTransientRefusals(t *testing.T) {
	n := 0
	out, _, err := follow(t, 0, t.Context(), func(context.Context) (operationOut, error) {
		if n++; n <= 2 {
			return operationOut{}, output.TransientError("rate limited")
		}
		return operationOut{ID: "op_1", State: model.Delivered, Terminal: true}, nil
	})
	if err != nil || n != 3 || !strings.Contains(out, "delivered") {
		t.Fatalf("after %d reads: %v\n%s", n, err, out)
	}
	// Anything else ends the watch.
	_, _, err = follow(t, 0, t.Context(), func(context.Context) (operationOut, error) {
		return operationOut{}, output.NotFoundError("no such operation")
	})
	if !errors.As(err, new(*output.Error)) {
		t.Fatalf("refusal = %v", err)
	}
}
