package e2e

import (
	"encoding/json"
	"strings"
	"time"

	"hop.top/uma/model"
)

// settleLimit bounds how long settle waits for an operation to end.
const settleLimit = 20 * time.Second

// settle waits until operation op has settled (provider_accepted,
// delivered, committed, failed or rejected) and returns it.
//
// A provider verb (`message reply`, `message edit`, `message delete`,
// `reaction add`) returns once its operation is queued. The adapter's own
// side effect, such as the widget's live event, happens inside the worker's
// call, before the operation settles. The message record (its revision,
// reactions, deleted flag) is written in the transaction that settles the
// operation, so a read or a revision-guarded verb issued on the event
// races that commit. Wait here for the operation instead of for the event.
//
// d, when not nil, supplies the daemon log for a failure message.
func (s *sandbox) settle(d *daemon, op model.ID) opJSON {
	s.t.Helper()
	deadline := time.Now().Add(settleLimit)
	var last result
	var got opJSON
	for {
		last = s.run(nil, "operation", "get", string(op), "--format", "json")
		if last.exit == 0 && json.Unmarshal([]byte(last.stdout), &got) == nil {
			switch got.State {
			case model.ProviderAccepted, model.Delivered, model.Committed, model.Failed, model.Rejected:
				return got
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.t.Fatalf("operation %s not settled after %s: state %q; last `operation get` exit %d\nstdout: %s\nstderr: %s%s",
		op, settleLimit, got.State, last.exit, last.stdout, last.stderr, daemonLog(d))
	return got
}

// daemonLog is d's log laid out for a failure message, empty without d.
func daemonLog(d *daemon) string {
	if d == nil {
		return ""
	}
	return "\ndaemon log:\n" + strings.TrimSpace(d.log())
}

// settleAccepted is settle for an operation that must end successfully:
// the provider accepted it, or delivered it.
func (s *sandbox) settleAccepted(d *daemon, op model.ID) opJSON {
	s.t.Helper()
	got := s.settle(d, op)
	if got.State != model.ProviderAccepted && got.State != model.Delivered && got.State != model.Committed {
		s.t.Fatalf("operation %s settled in %q, want it accepted%s", op, got.State, daemonLog(d))
	}
	return got
}
