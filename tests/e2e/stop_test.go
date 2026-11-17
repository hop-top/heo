package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	umapg "hop.top/uma/store/postgres"
)

// Environment the e2e build's fake adapter reads (cmd/adapters_e2e.go).
const (
	fakeSendDelayEnv = "HEO_E2E_FAKE_SEND_DELAY"
	fakeSendLogEnv   = "HEO_E2E_FAKE_SEND_LOG"
)

// queueFakeSend configures a fake channel and an inbox in tenant's
// store and starts a conversation on it, as the test's own uid — the
// daemon's installation owner: one queued send for the daemon's workers.
func (db *testDB) queueFakeSend(tenant model.TenantID, account string) model.OperationRef {
	db.t.Helper()
	ctx := db.t.Context()
	st, err := umapg.Open(ctx, db.pool(), umapg.Options{})
	if err != nil {
		db.t.Fatal(err)
	}
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{adapterstest.New()},
		Clock:    clock{}, IDs: ids{},
	})
	if err != nil {
		db.t.Fatal(err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	c, err := e.For(model.Principal{Tenant: tenant, Actor: model.ActorID(me())})
	if err != nil {
		db.t.Fatal(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID(op)}
	}
	ch, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd("channel-out"), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: tenant}, Key: "out", Type: adapterstest.Type,
		Configuration: adapterstest.Config{Account: account}.Configuration(),
	}})
	if err != nil {
		db.t.Fatalf("configure channel: %v", err)
	}
	chRef, err := ch.Operation.Result.AsChannel()
	if err != nil {
		db.t.Fatal(err)
	}
	lock := true
	ib, err := c.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd("inbox-out"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: tenant}, Channel: chRef, Name: "out", Destinations: []string{"desk"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		db.t.Fatalf("configure inbox: %v", err)
	}
	inbox, err := ib.Operation.Result.AsInbox()
	if err != nil {
		db.t.Fatal(err)
	}
	acc, err := c.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: chRef, Namespace: adapterstest.Namespace, Kind: "user", ID: "ada"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		db.t.Fatalf("start conversation: %v", err)
	}
	return acc.Operation.Ref
}

// sendSteps reads the fake adapter's send log: one "<step> <operation
// id>" line per step.
func sendSteps(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(b), " ", "_"))
}

// count counts step lines for op.
func count(steps []string, step string, op model.OperationRef) int {
	n := 0
	for _, s := range steps {
		if s == step+"_"+string(op.ID) {
			n++
		}
	}
	return n
}

// queueDepth is the queue section of the daemon's `service status`.
func (c *cliSite) queueDepth() (waiting, inFlight, dead float64) {
	c.t.Helper()
	status, d := c.s.serviceStatus().section(c.t, "queue")
	if status != "ok" {
		c.t.Fatalf("queue section = %s %v", status, d)
	}
	return d["waiting"].(float64), d["in_flight"].(float64), d["dead_lettered"].(float64)
}

// stopService runs `service stop` and requires exit 0 and the daemon
// gone, with exit code want.
func (c *cliSite) stopService(d *daemon, want int) {
	c.t.Helper()
	c.heo(0, "service", "stop")
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		c.t.Fatalf("daemon still running after service stop returned:\n%s", d.log())
	}
	if d.exit != want {
		c.t.Fatalf("daemon exit %d, want %d:\n%s", d.exit, want, d.log())
	}
}

// TestServiceStopDuringDeliveryDrains: `service stop` with a delivery
// in flight waits for the drain; the send completes, the operation is
// accepted, the daemon exits 0, and the next run sends nothing again.
// While the send is in flight `service status` counts it in flight.
func TestServiceStopDuringDeliveryDrains(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    drain_timeout: 30s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "2s"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })
		if waiting, inFlight, dead := site.queueDepth(); waiting != 0 || inFlight != 1 || dead != 0 {
			t.Errorf("queue during the send: waiting %v, in flight %v, dead-lettered %v; want 0 1 0", waiting, inFlight, dead)
		}
		site.stopService(d, 0)
		steps := sendSteps(t, sendLog)
		if count(steps, "done", op) != 1 || count(steps, "interrupted", op) != 0 {
			t.Fatalf("send steps after the drain = %v, want it done", steps)
		}
		if !strings.Contains(d.log(), "runtime: workers drained") {
			t.Errorf("no drain reported:\n%s", d.log())
		}
		if got := operationState(t, site.db.operator("default"), op); got.State != model.ProviderAccepted {
			t.Fatalf("operation = %s %+v, want provider_accepted", got.State, got.Failure)
		}
		// The next run finds nothing to send.
		d = site.start()
		time.Sleep(time.Second) // twenty worker polls
		if n := count(sendSteps(t, sendLog), "start", op); n != 1 {
			t.Errorf("sent %d times across the restart, want once", n)
		}
		if waiting, inFlight, dead := site.queueDepth(); waiting != 0 || inFlight != 0 || dead != 0 {
			t.Errorf("queue after the restart: %v %v %v, want empty", waiting, inFlight, dead)
		}
		site.stopService(d, 0)
	})
}

// TestServiceStopDrainDeadlineLeavesLease: a delivery that outlasts the
// drain deadline is abandoned to its lease: the daemon exits 1 and says
// so, and `service stop` still exits 0 once it is gone. The next run
// finds the item still leased, takes it up once the lease expires, and
// settles it without sending it again: the abandoned send may have
// reached the provider, so its operation ends outcome_unknown.
func TestServiceStopDrainDeadlineLeavesLease(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    drain_timeout: 300ms\n    lease: 10s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "1m"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })
		site.stopService(d, 1)
		if !strings.Contains(d.log(), "drain did not finish within 300ms") {
			t.Errorf("abandoned drain not reported:\n%s", d.log())
		}
		if steps := sendSteps(t, sendLog); count(steps, "interrupted", op) != 1 || count(steps, "done", op) != 0 {
			t.Fatalf("send steps after the deadline = %v, want it interrupted", steps)
		}
		// The next run, sends no longer delayed, finds the item leased,
		// then settles it once the lease expires.
		delete(site.s.env, fakeSendDelayEnv)
		d = site.start()
		if waiting, inFlight, _ := site.queueDepth(); waiting != 0 || inFlight != 1 {
			t.Errorf("queue at restart: waiting %v, in flight %v; want the abandoned item still leased", waiting, inFlight)
		}
		eventually(t, "the abandoned item settles", func() bool {
			waiting, inFlight, dead := site.queueDepth()
			return waiting == 0 && inFlight == 0 && dead == 0
		})
		if got := operationState(t, site.db.operator("default"), op); got.State != model.Unknown {
			t.Errorf("operation = %s %+v, want outcome_unknown", got.State, got.Failure)
		}
		if steps := sendSteps(t, sendLog); count(steps, "start", op) != 1 || count(steps, "done", op) != 0 {
			t.Errorf("send steps across the restart = %v, want the one abandoned send only", steps)
		}
		site.stopService(d, 0)
	})
}
