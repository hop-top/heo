package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	umapg "hop.top/uma/store/postgres"
)

// engine is a uma engine of the test's own on db, admitting everything,
// with factories and opts applied: it drives operations into states a
// healthy daemon would not reach.
func (db *testDB) engine(factories []adapter.Factory, opts ...func(*uma.Config)) uma.Engine {
	db.t.Helper()
	st, err := umapg.Open(db.t.Context(), db.pool(), umapg.Options{})
	if err != nil {
		db.t.Fatal(err)
	}
	cfg := uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: factories, Clock: clock{}, IDs: ids{},
	}
	for _, o := range opts {
		o(&cfg)
	}
	e, err := uma.New(cfg)
	if err != nil {
		db.t.Fatal(err)
	}
	db.t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// worker runs one worker of e until the returned stop is called.
func worker(t *testing.T, e uma.Engine, owner string, lease time.Duration) (stop func()) {
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = e.Runtime().Run(ctx, uma.RuntimeCall{
			Principal: model.Principal{Tenant: "default", Actor: "svc:e2e"}, Call: model.Call{Correlation: "e2e"},
		}, uma.WorkerOptions{Owner: owner, Poll: 20 * time.Millisecond, Lease: lease})
	}()
	return func() { cancel(); wg.Wait() }
}

type deliveryJSON struct {
	ID      model.ID
	State   model.DeliveryState
	Failure *model.Error
	Advice  struct {
		Verdict, Next string
		Retry         bool
	}
	Revision model.Revision
	// NextAttemptAt is when uma next works on the delivery, when scheduled.
	NextAttemptAt *time.Time `json:"next_attempt_at"`
}

type retryJSON struct {
	Delivery     deliveryJSON
	Replay       opJSON
	OperationKey string `json:"operation_key"`
}

func (c *cliSite) delivery(op model.ID) deliveryJSON {
	c.t.Helper()
	var d deliveryJSON
	c.json(&d, "delivery", "get", string(op))
	return d
}

// waitDelivery polls `delivery get` until op's state is one of want.
func (c *cliSite) waitDelivery(op model.ID, want ...model.DeliveryState) deliveryJSON {
	c.t.Helper()
	var d deliveryJSON
	eventually(c.t, "delivery "+string(op)+" in "+strings.TrimSpace(strings.Join(statesOf(want), " ")), func() bool {
		d = c.delivery(op)
		for _, s := range want {
			if d.State == s {
				return true
			}
		}
		return false
	})
	return d
}

func statesOf(ss []model.DeliveryState) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = string(s)
	}
	return out
}

// TestDeliveryGetAndRetry drives the e2e build on Postgres with the fake
// adapter: a start whose work stalled twice is dead-lettered, and
// `delivery get` says to retry it; the retry previews, replays it under
// its key and the daemon's workers deliver it. An outcome_unknown
// delivery is refused (exit 8) with the reconcile path, a failed one
// (exit 4) with "new --idempotency-key".
func TestDeliveryGetAndRetry(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start("workers")
		ch := c.fakeChannel("fake", adapterstest.Config{Account: "acct"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "room", "--destination", "room-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var in model.Inbox
		for _, i := range c.inboxes() {
			if i.Key == "room" {
				in = i
			}
		}

		fake := adapterstest.New()
		e := c.db.engine([]adapter.Factory{fake}, func(cfg *uma.Config) {
			cfg.Work = &uma.WorkPolicy{MaxClaims: 1, MaxInFlight: 4}
		})
		me, err := e.For(model.Principal{Tenant: "default", Actor: model.ActorID(me())})
		if err != nil {
			t.Fatal(err)
		}
		start := func(who string) model.ID {
			t.Helper()
			acc, err := me.Conversations().Start(t.Context(), model.StartConversation{
				Command:   model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID("start-" + who)},
				Inbox:     in.Ref,
				Recipient: model.ExternalRef{Channel: ch.Ref, Namespace: adapterstest.Namespace, Kind: "user", ID: who},
				Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello " + who}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			return acc.Operation.Ref.ID
		}

		// Dead-lettered: two claims that never finished.
		dead := start("ada")
		if got := c.delivery(dead); got.State != model.Queued || got.Advice.Retry {
			t.Fatalf("queued delivery = %+v", got)
		}
		wantStderr(t, c.heo(4, "delivery", "retry", string(dead)), "CONFLICT", "still working on it", "nothing to retry by hand")
		fake.Stall(true)
		stopA := worker(t, e, "a", 300*time.Millisecond)
		select {
		case <-fake.Stalled():
		case <-time.After(15 * time.Second):
			t.Fatal("no worker claimed the start")
		}
		time.Sleep(400 * time.Millisecond)
		stopB := worker(t, e, "b", 300*time.Millisecond)
		got := c.waitDelivery(dead, model.DeadLettered)
		stopB()
		stopA()
		fake.Stall(false)
		if !got.Advice.Retry || got.Advice.Verdict != "retry" || got.Failure == nil || got.Failure.Code != model.Permanent ||
			!strings.Contains(got.Advice.Next, "heo delivery retry "+string(dead)) {
			t.Fatalf("dead-lettered delivery = %+v", got)
		}
		r := c.heo(0, "delivery", "get", string(dead))
		for _, want := range []string{"dead_lettered", "unfinished claims", "retry          yes", "heo delivery retry"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("delivery get lacks %q:\n%s", want, r.stdout)
			}
		}
		var plan planJSON
		c.json(&plan, "delivery", "retry", string(dead), "--dry-run")
		if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || string(plan.Args["state"]) != `"dead_lettered"` ||
			!strings.Contains(plan.Effects[0].Detail, "twice") || c.delivery(dead).State != model.DeadLettered {
			t.Fatalf("retry preview = %+v, or it wrote", plan)
		}

		// outcome_unknown and failed, from the provider's answers.
		fake.FailSends(errors.New("connection reset after write"))
		unknown := start("bob")
		stop := worker(t, e, "c", 10*time.Second)
		c.waitDelivery(unknown, model.Unknown)
		fake.FailSends(model.NewError(model.Permanent, "mailbox gone"))
		failed := start("cy")
		c.waitDelivery(failed, model.Failed)
		stop()

		r = c.heo(8, "delivery", "retry", string(unknown))
		wantStderr(t, r, "OUTCOME_UNKNOWN", "reconcile", "look for it at the provider", "new --idempotency-key")
		if got := c.delivery(unknown); got.Advice.Retry || got.Advice.Verdict != "reconcile" {
			t.Fatalf("unknown delivery = %+v", got)
		}
		wantStderr(t, c.heo(4, "delivery", "retry", string(failed)), "CONFLICT", "failed finally", "new --idempotency-key")
		wantStderr(t, c.heo(3, "delivery", "get", "op_404"), "NOT_FOUND")

		// The retry: queued again, replayed under its key, delivered by
		// the daemon's workers.
		c.stop(d)
		d = c.start()
		var retried, again retryJSON
		c.json(&retried, "delivery", "retry", string(dead), "--idempotency-key", "retry-1")
		if retried.Replay.State != model.Committed || retried.Replay.Replayed || retried.OperationKey != "retry-1" ||
			retried.Delivery.ID != dead || retried.Delivery.State == model.DeadLettered {
			t.Fatalf("retry = %+v", retried)
		}
		c.json(&again, "delivery", "retry", string(dead), "--idempotency-key", "retry-1")
		if !again.Replay.Replayed || again.Replay.ID != retried.Replay.ID {
			t.Fatalf("retry rerun under its key = %+v, first %+v", again, retried)
		}
		c.waitDelivery(dead, model.ProviderAccepted, model.Delivered)
		wantStderr(t, c.heo(4, "delivery", "retry", string(dead), "--idempotency-key", "retry-2"), "CONFLICT", "nothing to retry")
		c.stop(d)
	})
}

// watchProc is one watch process (`event watch`, `task watch`): its
// stdout lines as they come.
type watchProc struct {
	t     *testing.T
	cmd   *exec.Cmd
	lines chan string
	errb  lockedBuffer
	done  chan struct{}
	exit  int
}

func (s *sandbox) watch(args ...string) *watchProc {
	return s.watchOf([]string{"event", "watch"}, args...)
}

// watchOf starts the watch command path with args.
func (s *sandbox) watchOf(path []string, args ...string) *watchProc {
	s.t.Helper()
	p := &watchProc{t: s.t, lines: make(chan string, 4096), done: make(chan struct{})}
	p.cmd = exec.Command(heoBin, append(append([]string{}, path...), args...)...) //nolint:gosec // runs the binary TestMain built
	p.cmd.Dir = s.path("cwd")
	for k, v := range s.env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	out, err := p.cmd.StdoutPipe()
	if err != nil {
		s.t.Fatal(err)
	}
	p.cmd.Stderr = &p.errb
	if err := p.cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			p.lines <- sc.Text()
		}
		close(p.lines)
		err := p.cmd.Wait()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.exit = ee.ExitCode()
		}
		close(p.done)
	}()
	s.t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// until reads events until match accepts one, and returns every event
// read.
func (p *watchProc) until(match func(model.Event) bool) []model.Event {
	p.t.Helper()
	var out []model.Event
	timeout := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				<-p.done
				p.t.Fatalf("watch ended (exit %d) before the event:\n%s", p.exit, p.errb.String())
			}
			var ev model.Event
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				p.t.Fatalf("watch line %q: %v", line, err)
			}
			out = append(out, ev)
			if match(ev) {
				return out
			}
		case <-timeout:
			p.t.Fatalf("no matching event in 30s; read %d:\n%s", len(out), p.errb.String())
		}
	}
}

// quiet reads lines until none came for d.
func (p *watchProc) quiet(d time.Duration) []string {
	var out []string
	for {
		select {
		case line, ok := <-p.lines:
			if !ok {
				return out
			}
			out = append(out, line)
		case <-time.After(d):
			return out
		}
	}
}

// interrupt sends SIGINT and returns the exit code and stderr.
func (p *watchProc) interrupt() (int, string) {
	p.t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGINT); err != nil {
		p.t.Fatal(err)
	}
	go func() {
		for range p.lines {
		}
	}()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		p.t.Fatalf("watch did not stop:\n%s", p.errb.String())
	}
	return p.exit, p.errb.String()
}

// checkpointOf reads the checkpoint a stopped watch printed.
func checkpointOf(t *testing.T, stderr string) string {
	t.Helper()
	_, rest, ok := strings.Cut(stderr, " at checkpoint ")
	if !ok {
		t.Fatalf("no checkpoint in %q", stderr)
	}
	cp, _, _ := strings.Cut(rest, ";")
	return cp
}

func idsOf(evs []model.Event) map[model.ID]bool {
	out := map[model.ID]bool{}
	for _, ev := range evs {
		out[ev.ID] = true
	}
	return out
}

// TestEventWatch drives the release binary on Postgres through a real
// widget conversation: a watch streams the events of a reply as NDJSON;
// Ctrl-C acknowledges and exits 0 with the checkpoint; rerunning the
// consumer resumes after it, repeating nothing; another consumer starts
// at that checkpoint; a cursor the open consumer is not at is a gap
// (exit 4); purged history is CURSOR_EXPIRED (exit 4); a viewer never
// sees content that is not public.
func TestEventWatch(t *testing.T) {
	s := newDeskSite(t)
	id := string(s.conv.ID)
	reply := func(text, key string) replyJSON {
		t.Helper()
		var r replyJSON
		s.json(&r, "message", "reply", id, "--text", text, "--idempotency-key", key)
		return r
	}
	created := func(msg model.ID) func(model.Event) bool {
		return func(ev model.Event) bool { return ev.Kind == model.EventMessageCreated && ev.Resource.ID == msg }
	}

	w := s.s.watch("--consumer", "c1", "--from", "beginning", "--format", "json")
	r1 := reply("first reply", "r1")
	first := w.until(created(r1.Message))
	delivery := s.waitDelivery(r1.Operation.ID, model.ProviderAccepted, model.Delivered)
	if delivery.Advice.Retry || delivery.Advice.Verdict != "done" {
		t.Fatalf("delivered reply = %+v", delivery)
	}
	w.until(func(ev model.Event) bool { return ev.Kind == model.EventDeliveryUpdated })
	code, stderr := w.interrupt()
	if code != 0 || !strings.Contains(stderr, "stopped: consumer c1 at checkpoint ") {
		t.Fatalf("interrupted watch: exit %d\n%s", code, stderr)
	}
	stopped := checkpointOf(t, stderr)
	seen := idsOf(first)

	// Resume: only what came after, nothing repeated.
	r2 := reply("second reply", "r2")
	w = s.s.watch("--consumer", "c1", "--format", "json")
	resumed := w.until(created(r2.Message))
	for _, ev := range resumed {
		if seen[ev.ID] {
			t.Fatalf("resumed watch repeated %s %s", ev.Kind, ev.ID)
		}
	}
	code, stderr = w.interrupt()
	if code != 0 {
		t.Fatalf("second watch exit %d\n%s", code, stderr)
	}
	if checkpointOf(t, stderr) == stopped {
		t.Fatal("the checkpoint did not move")
	}

	// Another consumer starts at the first checkpoint.
	w = s.s.watch("--consumer", "c2", "--cursor", stopped, "--format", "json")
	from := w.until(created(r2.Message))
	if idsOf(from)[first[0].ID] {
		t.Fatal("a watch from the checkpoint replayed history before it")
	}
	if code, _ := w.interrupt(); code != 0 {
		t.Fatalf("c2 exit %d", code)
	}

	// A cursor the open consumer is not at: a gap, refused.
	wantStderr(t, s.heo(4, "event", "watch", "--consumer", "c1", "--cursor", stopped), "CONFLICT", "skip or repeat", "--consumer")
	wantStderr(t, s.heo(2, "event", "watch", "--cursor", stopped, "--from", "tail"), "USAGE")

	// Purged past c1's checkpoint: CURSOR_EXPIRED.
	reply("unread reply", "r3")
	long := 1000 * time.Hour
	purger := s.db.engine(nil, func(cfg *uma.Config) {
		cfg.Retention = uma.RetentionPolicy{Events: time.Nanosecond, Operations: long, Ingress: long, Attempts: long,
			Staging: long, Exports: long, Contacts: long}
	})
	if rep, err := purger.Runtime().Purge(t.Context(), uma.RuntimeCall{
		Principal: model.Principal{Tenant: "default", Actor: "svc:e2e"}, Call: model.Call{Correlation: "e2e"},
	}); err != nil || rep.Events == 0 {
		t.Fatalf("purge = %+v, %v", rep, err)
	}
	wantStderr(t, s.heo(4, "event", "watch", "--consumer", "c1"), "CURSOR_EXPIRED", "purged", "--from beginning", "reconcile")
	wantStderr(t, s.heo(4, "event", "watch", "--consumer", "c9", "--cursor", stopped), "CURSOR_EXPIRED")

	// A viewer never sees content that is not public.
	s.json(new(noteJSON), "note", "add", id, "--text", "internal secret", "--placement", "local")
	reply("public after the note", "r4")
	s.stop(s.d)
	s.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: [viewer]\n")
	s.d = s.start()
	for _, format := range []string{"json", "text"} {
		w = s.s.watch("--consumer", "viewer-"+format, "--from", "beginning", "--format", format)
		lines := w.quiet(3 * time.Second)
		if code, stderr := w.interrupt(); code != 0 {
			t.Fatalf("viewer watch exit %d\n%s", code, stderr)
		}
		if len(lines) == 0 {
			t.Fatalf("viewer %s watch saw nothing", format)
		}
		for _, l := range lines {
			if strings.Contains(l, "internal secret") || strings.Contains(l, "internal") || strings.Contains(l, "restricted") {
				t.Fatalf("viewer %s watch shows non-public content: %s", format, l)
			}
		}
	}
	wantStderr(t, s.heo(5, "delivery", "get", string(r1.Operation.ID)), "UNAUTHORIZED")
	s.stop(s.d)
}
