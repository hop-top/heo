package e2e

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

type deliveryRowJSON struct {
	Operation, State, Kind, Conversation, Updated, Key, Inbox, Verdict string
}

type deliveryPageJSON struct {
	Items    []deliveryRowJSON
	Next     string
	Complete bool
}

func keysOfRows(rows []deliveryRowJSON) string {
	ks := make([]string, len(rows))
	for i, r := range rows {
		ks[i] = r.Key
	}
	return strings.Join(ks, ",")
}

// sortedDeliveryKeys is keysOfRows in key order: uma lists by the time of
// an operation's latest change, and workers settle concurrently.
func sortedDeliveryKeys(rows []deliveryRowJSON) string {
	if len(rows) == 0 {
		return ""
	}
	ks := strings.Split(keysOfRows(rows), ",")
	sort.Strings(ks)
	return strings.Join(ks, ",")
}

type reconcileJSON struct {
	ID             model.ID
	State          model.DeliveryState
	PreviousState  model.DeliveryState `json:"previous_state"`
	Reconciliation string
	Adapter        *struct {
		Type       string
		Reconciles bool
	}
	ProviderReferences []model.ExternalRef `json:"provider_references"`
	NextAttemptAt      *time.Time          `json:"next_attempt_at"`
	Next               string
}

// TestDeliveryListAndReconcile drives the e2e build on Postgres with the
// fake adapter in the ambiguous-send scenario: deliveries are listed from
// uma's ledger (outbound kinds only: the channel and inbox configurations
// the caller also submitted are not deliveries) by state, inbox,
// conversation and the time of their latest change; an outcome_unknown one is
// listed, and `delivery reconcile` asks the adapter's provider now. Where
// the adapter declares no reconcile uma refuses (UNSUPPORTED, exit 2);
// where the provider cannot be asked the error comes back and nothing
// changes; a lookup that finds the message settles the delivery
// (exit 0); one that proves nothing leaves it outcome_unknown with its
// next attempt (exit 8); one that finds no record makes it retry_pending
// and uma sends it again. A viewer is refused before the provider is
// asked, --dry-run asks nothing, and `delivery retry` keeps refusing an
// unknown or failed one.
func TestDeliveryListAndReconcile(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-dl"})
		for _, in := range [][]string{{"outbox", "out-1"}, {"alerts", "alerts-1"}} {
			c.heo(0, "inbox", "create", "--channel", "fake", "--key", in[0], "--destination", in[1],
				"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		}
		var outbox model.Inbox
		for _, i := range c.inboxes() {
			if i.Key == "outbox" {
				outbox = i
			}
		}
		send := func(inbox, who, key string) sendJSON {
			t.Helper()
			var s sendJSON
			c.json(&s, "message", "send", "--inbox", inbox, "--to", "user:"+who, "--text", "hi "+who, "--idempotency-key", key)
			return s
		}
		list := func(args ...string) deliveryPageJSON {
			t.Helper()
			var p deliveryPageJSON
			c.json(&p, append([]string{"delivery", "list"}, args...)...)
			return p
		}

		// Healthy sends, and a reply to the first conversation.
		ada := send("outbox", "ada", "d-ada")
		cy := send("alerts", "cy", "d-cy")
		var reply replyJSON
		c.json(&reply, "message", "reply", string(ada.Conversation), "--text", "again", "--idempotency-key", "d-reply")

		// Queued: every write is listed with its state.
		if p := list(); keysOfRows(p.Items) != "d-reply,d-cy,d-ada" || !p.Complete {
			t.Fatalf("list = %+v", p)
		}
		if got := keysOfRows(list("--state", "queued").Items); got != "d-reply,d-cy,d-ada" {
			t.Errorf("--state queued = %q", got)
		}
		if got := list("--state", "delivered,outcome_unknown").Items; len(got) != 0 {
			t.Errorf("--state delivered = %+v", got)
		}

		// A worker of the test's own delivers them. stopWorker waits out
		// any claim a stop abandoned: a worker cancelled while it holds an
		// unknown outcome's lease leaves it until the term ends, and a
		// reconcile then would rightly be refused (see
		// TestDeliveryReconcileWhileWorkerHoldsLease).
		fake := adapterstest.New()
		fake.EnableReconcile()
		e := c.db.engine([]adapter.Factory{fake}, func(cfg *uma.Config) {
			cfg.Work = &uma.WorkPolicy{MaxClaims: 10, MaxInFlight: 8}
			cfg.Retry = uma.RetryPolicy{MaxAttempts: 5, BaseDelay: 300 * time.Millisecond, MaxDelay: 300 * time.Millisecond}
		})
		stop := worker(t, e, "dl-a", 2*time.Second)
		for _, op := range []model.ID{ada.Operation.ID, cy.Operation.ID, reply.Operation.ID} {
			c.waitDelivery(op, model.ProviderAccepted, model.Delivered)
		}
		c.stopWorker(stop)

		// An ambiguous send, after a mark: the connection drops after the
		// request.
		time.Sleep(150 * time.Millisecond)
		mark := time.Now().UTC()
		time.Sleep(150 * time.Millisecond)
		fake.FailSends(errors.New("connection reset after write"))
		bob := send("outbox", "bob", "d-bob")
		stop = worker(t, e, "dl-b", 2*time.Second)
		c.waitDelivery(bob.Operation.ID, model.Unknown)
		c.stopWorker(stop)
		fake.FailSends(nil)

		// The listing: by state, inbox, conversation, window, and paged.
		unknown := list("--state", "outcome_unknown")
		if len(unknown.Items) != 1 || unknown.Items[0].Operation != string(bob.Operation.ID) || unknown.Items[0].Verdict != "reconcile" ||
			unknown.Items[0].Kind != "conversation.start" || unknown.Items[0].Inbox != string(outbox.Ref.ID) {
			t.Fatalf("--state outcome_unknown = %+v", unknown)
		}
		if got := sortedDeliveryKeys(list("--state", "delivered,provider_accepted").Items); got != "d-ada,d-cy,d-reply" {
			t.Errorf("--state delivered,provider_accepted = %q", got)
		}
		if got := keysOfRows(list("--inbox", "alerts").Items); got != "d-cy" {
			t.Errorf("--inbox alerts = %q", got)
		}
		if got := sortedDeliveryKeys(list("--inbox", string(outbox.Ref.ID)).Items); got != "d-ada,d-bob,d-reply" {
			t.Errorf("--inbox <id> = %q", got)
		}
		if got := sortedDeliveryKeys(list("--conversation", string(ada.Conversation)).Items); got != "d-ada,d-reply" {
			t.Errorf("--conversation = %q", got)
		}
		if got := keysOfRows(list("--conversation", string(bob.Conversation)).Items); got != "d-bob" {
			t.Errorf("--conversation of a start = %q", got)
		}
		if got := keysOfRows(list("--inbox", "alerts", "--state", "outcome_unknown").Items); got != "" {
			t.Errorf("--inbox alerts --state outcome_unknown = %q", got)
		}
		// The window is the time of an operation's latest change: bob's
		// send and its unknown outcome are after the mark, the others'
		// delivery before it.
		if got := keysOfRows(list("--since", mark.Format(time.RFC3339Nano)).Items); got != "d-bob" {
			t.Errorf("--since = %q", got)
		}
		if got := sortedDeliveryKeys(list("--until", mark.Format(time.RFC3339Nano)).Items); got != "d-ada,d-cy,d-reply" {
			t.Errorf("--until = %q", got)
		}
		// Paged: uma's pages may be short, so follow next to the end.
		seen := map[string]int{}
		var p deliveryPageJSON
		for cursor, pages := "", 0; ; pages++ {
			args := []string{"--limit", "3"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			p = list(args...)
			if len(p.Items) > 3 || pages > 10 {
				t.Fatalf("page %d = %+v", pages, p)
			}
			for _, it := range p.Items {
				seen[it.Key]++
			}
			if p.Complete || p.Next == "" {
				break
			}
			cursor = p.Next
		}
		if len(seen) != 4 || seen["d-ada"] != 1 || seen["d-bob"] != 1 || seen["d-cy"] != 1 || seen["d-reply"] != 1 {
			t.Fatalf("paged listing = %v, want each of the four deliveries once", seen)
		}
		if first := list("--limit", "3").Items; len(first) == 0 || first[0].Updated == "" || first[0].Key != "d-bob" {
			t.Fatalf("newest change first = %+v, want d-bob", first)
		}
		wantStderr(t, c.heo(2, "delivery", "list", "--state", "lost"), "VALIDATION", `state "lost"`)
		wantStderr(t, c.heo(3, "delivery", "list", "--inbox", "nope"), "NOT_FOUND")
		wantStderr(t, c.heo(2, "delivery", "list", "--until", "soon"), "USAGE")
		r := c.heo(0, "delivery", "list", "--state", "outcome_unknown")
		for _, want := range []string{"OPERATION", "STATE", "KIND", "CONVERSATION", "outcome_unknown", string(bob.Operation.ID), "d-bob"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text delivery list lacks %q:\n%s", want, r.stdout)
			}
		}

		// asDaemon restarts the daemon under the fake adapter's reconcile
		// mode and the roles the profile's owner holds.
		asDaemon := func(mode, owner string) {
			t.Helper()
			c.stop(d)
			if mode == "" {
				delete(c.s.env, "HEO_E2E_FAKE_RECONCILE")
			} else {
				c.s.env["HEO_E2E_FAKE_RECONCILE"] = mode
			}
			c.s.write("cfg/heo/profiles/default.yaml", owner)
			d = c.start("workers")
		}
		var rec reconcileJSON
		reconcile := func(exit int, id model.ID, args ...string) result {
			t.Helper()
			return c.heo(exit, append([]string{"delivery", "reconcile", string(id), "--format", "json"}, args...)...)
		}

		// Where the adapter declares no reconcile: uma refuses, nothing
		// will do it, and heo says so. A preview refuses the same way
		// (TestReconcilePreviewAgreesWithSubmission holds the two to one
		// answer) and asks nothing.
		r = reconcile(2, bob.Operation.ID, "--dry-run")
		wantStderr(t, r, "UNSUPPORTED", "nothing will reconcile it", "new --idempotency-key")
		if strings.Contains(r.stdout, `"kind"`) {
			t.Errorf("dry-run of a reconcile the adapter cannot do planned an effect:\n%s", r.stdout)
		}
		r = reconcile(2, bob.Operation.ID)
		wantStderr(t, r, "UNSUPPORTED", "nothing will reconcile it", "new --idempotency-key")

		// Declared, but the provider cannot be asked: the error, and
		// nothing is recorded. A preview shows what would be asked.
		asDaemon("1", "")
		r = reconcile(0, bob.Operation.ID, "--dry-run")
		for _, want := range []string{`"kind": "update"`, "operation:" + string(bob.Operation.ID), "nothing is sent", "fake"} {
			if !strings.Contains(strings.ReplaceAll(r.stdout, `":"`, `": "`), want) {
				t.Errorf("dry-run reconcile lacks %q:\n%s", want, r.stdout)
			}
		}
		r = reconcile(6, bob.Operation.ID)
		wantStderr(t, r, "UNAVAILABLE", "nothing was recorded")
		if got := c.delivery(bob.Operation.ID); got.State != model.Unknown || got.Advice.Retry {
			t.Fatalf("a failed reconcile changed the delivery: %+v", got)
		}

		// A caller without the role is refused before the provider is
		// asked, though this lookup would have found the message.
		asDaemon("found", "authorization:\n  owner: [viewer]\n")
		wantStderr(t, reconcile(5, bob.Operation.ID), "UNAUTHORIZED")
		wantStderr(t, reconcile(5, bob.Operation.ID, "--dry-run"), "UNAUTHORIZED")
		asDaemon("found", "authorization:\n  owner: [external]\n")
		wantStderr(t, reconcile(5, bob.Operation.ID), "UNAUTHORIZED")
		// Each role that may reconcile can on its own: the desk (which
		// submitted the write) here, the operator below.
		asDaemon("found", "authorization:\n  owner: [desk]\n")
		if got := c.delivery(bob.Operation.ID); got.State != model.Unknown {
			t.Fatalf("a refused reconcile settled the delivery: %+v", got)
		}

		// The provider holds the message: the delivery settles, with the
		// provider's reference, and the command succeeds. Repeating it is
		// safe.
		r = reconcile(0, bob.Operation.ID)
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "settled" || rec.State != model.ProviderAccepted || rec.PreviousState != model.Unknown ||
			len(rec.ProviderReferences) == 0 || rec.Adapter == nil || !rec.Adapter.Reconciles {
			t.Fatalf("reconcile that finds the message = %+v", rec)
		}
		r = c.heo(0, "delivery", "reconcile", string(bob.Operation.ID))
		for _, want := range []string{"reconciliation  not_needed", "provider_accepted"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text delivery reconcile of a settled one lacks %q:\n%s", want, r.stdout)
			}
		}
		if got := c.delivery(bob.Operation.ID); got.State != model.ProviderAccepted || got.Advice.Retry {
			t.Fatalf("after reconcile: %+v", got)
		}
		if len(list("--state", "outcome_unknown").Items) != 0 {
			t.Error("a reconciled delivery is still listed as outcome_unknown")
		}
		if got := keysOfRows(list("--state", "provider_accepted,delivered").Items); !strings.Contains(got, "d-bob") {
			t.Errorf("--state delivered lacks the reconciled one: %q", got)
		}

		// A state that is not an unknown outcome needs none (exit 0).
		r = reconcile(0, ada.Operation.ID)
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "not_needed" || !strings.Contains(rec.Next, "nothing to do") {
			t.Fatalf("reconcile of a delivered one = %+v", rec)
		}
		wantStderr(t, c.heo(3, "delivery", "reconcile", "op_404"), "NOT_FOUND")

		// A lookup that proves nothing: exit 8, still unknown, and the
		// next attempt is shown.
		fake.FailSends(errors.New("connection reset after write"))
		eve := send("outbox", "eve", "d-eve")
		stop = worker(t, e, "dl-d", 2*time.Second)
		c.waitDelivery(eve.Operation.ID, model.Unknown)
		c.stopWorker(stop)
		fake.FailSends(nil)
		asDaemon("unproven", "authorization:\n  owner: [operator]\n")
		r = reconcile(8, eve.Operation.ID)
		wantStderr(t, r, "OUTCOME_UNKNOWN", "still outcome_unknown", "proved nothing")
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "pending" || rec.State != model.Unknown || rec.NextAttemptAt == nil ||
			!strings.Contains(rec.Next, "proved nothing") || !strings.Contains(rec.Next, rec.NextAttemptAt.UTC().Format(time.RFC3339)) {
			t.Fatalf("reconcile that proves nothing = %+v", rec)
		}
		r = c.heo(8, "delivery", "reconcile", string(eve.Operation.ID))
		for _, want := range []string{"reconciliation  pending", "next attempt", "outcome_unknown"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text delivery reconcile lacks %q:\n%s", want, r.stdout)
			}
		}
		if got := c.delivery(eve.Operation.ID); got.State != model.Unknown || got.NextAttemptAt == nil {
			t.Fatalf("delivery get of an unknown one = %+v: want its next attempt", got)
		}
		wantStderr(t, c.heo(8, "delivery", "retry", string(eve.Operation.ID)), "OUTCOME_UNKNOWN", "delivery reconcile")
		wantStderr(t, c.heo(4, "delivery", "retry", string(bob.Operation.ID)), "CONFLICT", "nothing to retry")

		// The provider holds no record: not applied, so uma sends it
		// again, and the retry delivers it.
		asDaemon("missing", "")
		r = reconcile(0, eve.Operation.ID)
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "not_applied" || rec.State != model.RetryPending || rec.NextAttemptAt == nil {
			t.Fatalf("reconcile that finds no record = %+v", rec)
		}
		stop = worker(t, e, "dl-e", 2*time.Second)
		if got := c.waitDelivery(eve.Operation.ID, model.ProviderAccepted, model.Delivered); got.State == model.Unknown {
			t.Fatalf("after the retry: %+v", got)
		}
		c.stopWorker(stop)

		// A final failure is an answer too: reconcile reports it (exit
		// 0), and the way on is a new operation.
		fake.FailSends(model.NewError(model.Permanent, "mailbox gone"))
		zed := send("outbox", "zed", "d-zed")
		stop = worker(t, e, "dl-f", 2*time.Second)
		defer stop()
		c.waitDelivery(zed.Operation.ID, model.Failed)
		if got := keysOfRows(list("--state", "failed").Items); got != "d-zed" {
			t.Errorf("--state failed = %q", got)
		}
		wantStderr(t, c.heo(4, "delivery", "retry", string(zed.Operation.ID)), "CONFLICT", "failed finally", "new --idempotency-key")
		r = reconcile(0, zed.Operation.ID)
		mustJSON(t, r.stdout, &rec)
		if rec.Reconciliation != "not_needed" || !strings.Contains(rec.Next, "new --idempotency-key") {
			t.Fatalf("reconcile of a failed one = %+v", rec)
		}
		stop()
		c.stop(d)
	})
}
