package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// operationJSON is `operation get` / `operation watch` output.
type operationJSON struct {
	ID       model.ID
	State    model.DeliveryState
	Terminal bool
	Message  *model.MessageRef
	Failure  *model.Error
	Revision model.Revision
	Key      string
	Kind     string
	Target   string
}

type operationRowJSON struct {
	Operation, State, Key, Kind, Target, Updated string
}

type operationPageJSON struct {
	Items    []operationRowJSON
	Next     string
	Complete bool
}

// noHistoryFile fails when heo keeps a client-side operation history:
// uma's ledger is the only record of what was submitted.
func (c *cliSite) noHistoryFile() {
	c.t.Helper()
	_ = filepath.WalkDir(c.s.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "operations") && strings.HasSuffix(d.Name(), ".jsonl") {
			c.t.Errorf("a client-side operation history was written: %s", path)
		}
		return nil
	})
}

// operationRows lists the operations of `operation list args`, following
// next until the listing is complete: uma's page may be short and still
// carry a cursor, so a test reads them all.
func (c *cliSite) operationRows(args ...string) []operationRowJSON {
	c.t.Helper()
	var rows []operationRowJSON
	cursor := ""
	for i := 0; i < 50; i++ {
		a := append([]string{"operation", "list"}, args...)
		if cursor != "" {
			a = append(a, "--cursor", cursor)
		}
		var p operationPageJSON
		c.json(&p, a...)
		rows = append(rows, p.Items...)
		if p.Complete || p.Next == "" {
			return rows
		}
		cursor = p.Next
	}
	c.t.Fatalf("operation list %v: no end after 50 pages", args)
	return nil
}

func rowKeys(rows []operationRowJSON) string {
	ks := make([]string, len(rows))
	for i, r := range rows {
		ks[i] = r.Key
	}
	return strings.Join(ks, ",")
}

func (c *cliSite) operation(args ...string) operationJSON {
	c.t.Helper()
	var o operationJSON
	c.json(&o, append([]string{"operation", "get"}, args...)...)
	return o
}

// asyncRun runs heo in the background: wait returns its result.
func (c *cliSite) asyncRun(args ...string) (wait func() result) {
	var (
		r  result
		wg sync.WaitGroup
	)
	wg.Add(1)
	go func() { defer wg.Done(); r = c.s.run(nil, args...) }()
	return func() result { wg.Wait(); return r }
}

// lastLine is the last line of s, decoded as an operation.
func lastLine(t *testing.T, s string) operationJSON {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(s), "\n")
	var o operationJSON
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &o); err != nil {
		t.Fatalf("watch line %q: %v", lines[len(lines)-1], err)
	}
	return o
}

// TestOperationRecoveryByKey drives the e2e build on Postgres with the
// fake adapter: a send whose answer is lost is found again by the key it
// was submitted under, from uma's ledger alone (no history file is kept),
// with or without the kind and target the caller names; rerunning it with
// the same key replays the stored operation, and a changed payload under
// the same key is a conflict. A key used for two writes names both.
// Operations are read by ID, which shows the key they were submitted
// under, and followed with watch, which exits by the state the operation
// ends in; `operation list` shows the caller's operations as uma holds
// them: key, kind, target and state, newest change first, filtered and
// paged.
func TestOperationRecoveryByKey(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-ops"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var inbox model.Inbox
		for _, i := range c.inboxes() {
			if i.Key == "outbox" {
				inbox = i
			}
		}
		send := func(who, text, key string, exit int) result {
			t.Helper()
			return c.heo(exit, "message", "send", "--inbox", "outbox", "--to", "user:"+who, "--text", text, "--idempotency-key", key)
		}
		target := "inbox:" + string(inbox.Ref.ID)

		// The send whose response is lost: only its key is known.
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "private words", "--idempotency-key", "lost-1")
		if sent.Operation.State != model.Queued {
			t.Fatalf("send = %+v", sent)
		}

		// Recovery by the key alone: uma's ledger knows the kind and the
		// target; heo keeps no file of them.
		got := c.operation("--key", "lost-1")
		if got.ID != sent.Operation.ID || got.State != model.Queued || got.Key != "lost-1" ||
			got.Kind != "conversation.start" || got.Target != target {
			t.Fatalf("operation get --key = %+v, want %s queued", got, sent.Operation.ID)
		}
		r := c.heo(0, "operation", "get", "--key", "lost-1")
		for _, want := range []string{string(sent.Operation.ID), "queued", "conversation.start", "lost-1"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text operation get lacks %q:\n%s", want, r.stdout)
			}
		}
		// By ID, the operation shows the key it was submitted under too.
		if byRef := c.operation(string(sent.Operation.ID)); byRef.ID != got.ID || byRef.Key != "lost-1" ||
			byRef.Kind != "conversation.start" || byRef.Target != target {
			t.Fatalf("operation get <ref> = %+v", byRef)
		}
		// Narrowed by kind, target, or both.
		for _, narrow := range [][]string{
			{"--kind", "conversation.start"}, {"--target", target}, {"--kind", "conversation.start", "--target", target},
		} {
			if got := c.operation(append([]string{"--key", "lost-1"}, narrow...)...); got.ID != sent.Operation.ID {
				t.Fatalf("operation get --key lost-1 %v = %+v", narrow, got)
			}
		}
		wantStderr(t, c.heo(3, "operation", "get", "--key", "lost-1", "--kind", "message.reply"), "NOT_FOUND")
		wantStderr(t, c.heo(3, "operation", "get", "--key", "lost-1", "--kind", "message.reply", "--target", target), "NOT_FOUND")
		wantStderr(t, c.heo(3, "operation", "get", "--key", "lost-1", "--target", "inbox:in_other"), "NOT_FOUND")

		// Rerunning the identical command replays; a changed payload
		// under the same key is a conflict.
		var again sendJSON
		c.json(&again, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "private words", "--idempotency-key", "lost-1")
		if !again.Operation.Replayed || again.Operation.ID != sent.Operation.ID {
			t.Fatalf("rerun = %+v, want the stored operation replayed", again)
		}
		wantStderr(t, send("ada", "different words", "lost-1", 4), "CONFLICT")

		// A key never claimed, and usage.
		r = c.heo(3, "operation", "get", "--key", "never-sent")
		wantStderr(t, r, "NOT_FOUND", "never-sent", "never accepted", "--kind")
		wantStderr(t, c.heo(3, "operation", "get", "op_404"), "NOT_FOUND")
		wantStderr(t, c.heo(2, "operation", "get"), "USAGE", "name the operation")
		wantStderr(t, c.heo(2, "operation", "get", "op_1", "--key", "k"), "USAGE", "pass one")
		wantStderr(t, c.heo(2, "operation", "get", "op_1", "--kind", "message.reply"), "USAGE", "narrow --key")
		wantStderr(t, c.heo(2, "operation", "get", "--key", "lost-1", "--kind", "conversation.start", "--target", "inbox"), "USAGE", "--target")

		// A key used for two writes names both, and the narrowing tells
		// them apart.
		for _, a := range []string{"a-1", "a-2", "a-3"} {
			send("p"+a, "hello "+a, a, 0)
		}
		var rep replyJSON
		c.json(&rep, "message", "reply", string(sent.Conversation), "--text", "reply words", "--idempotency-key", "a-2")
		r = c.heo(4, "operation", "get", "--key", "a-2")
		wantStderr(t, r, "CONFLICT", "names 2 operations", "conversation.start", "message.reply", "--kind")
		if got := c.operation("--key", "a-2", "--kind", "message.reply"); got.ID != rep.Operation.ID || got.Kind != "message.reply" {
			t.Fatalf("a-2 as a reply = %+v, want %s", got, rep.Operation.ID)
		}
		if got := c.operation("--key", "a-2", "--kind", "conversation.start"); got.ID == rep.Operation.ID || got.Kind != "conversation.start" {
			t.Fatalf("a-2 as a start = %+v", got)
		}

		// The operations, listed as uma holds them: every row has what
		// recovers it, newest change first. Two configurations, four
		// starts and a reply.
		rows := c.operationRows()
		if len(rows) != 7 || rowKeys(rows[:5]) != "a-2,a-3,a-2,a-1,lost-1" {
			t.Fatalf("operation list = %+v", rows)
		}
		for _, r := range rows {
			if r.Operation == "" || r.State == "" || r.Key == "" || r.Kind == "" || r.Target == "" || r.Updated == "" {
				t.Errorf("operation list row lacks a field: %+v", r)
			}
		}
		if rows[0].Operation != string(rep.Operation.ID) || rows[0].Kind != "message.reply" || rows[0].State != "queued" ||
			rows[0].Target != "conversation:"+string(inbox.Ref.ID)+"/"+string(sent.Conversation) {
			t.Errorf("newest row = %+v, want the queued reply", rows[0])
		}
		if rows[4].Key != "lost-1" || rows[4].Kind != "conversation.start" || rows[4].Target != target ||
			rows[4].Operation != string(sent.Operation.ID) || rows[5].Kind != "inbox.configure" || rows[6].Kind != "channel.configure" {
			t.Errorf("rows = %+v", rows)
		}
		// Filters: kind (one or several), state, inbox, conversation,
		// window.
		if got := rowKeys(c.operationRows("--kind", "conversation.start")); got != "a-3,a-2,a-1,lost-1" {
			t.Errorf("--kind conversation.start = %q", got)
		}
		if got := rowKeys(c.operationRows("--kind", "message.reply")); got != "a-2" {
			t.Errorf("--kind message.reply = %q", got)
		}
		if got := c.operationRows("--kind", "inbox.configure,channel.configure"); len(got) != 2 {
			t.Errorf("--kind inbox.configure,channel.configure = %+v", got)
		}
		if got := c.operationRows("--kind", "message.note"); len(got) != 0 {
			t.Errorf("--kind message.note = %+v", got)
		}
		if got := c.operationRows("--state", "queued"); len(got) != 5 {
			t.Errorf("--state queued = %+v", got)
		}
		if got := c.operationRows("--state", "delivered"); len(got) != 0 {
			t.Errorf("--state delivered = %+v", got)
		}
		if got := rowKeys(c.operationRows("--conversation", string(sent.Conversation))); got != "a-2,lost-1" {
			t.Errorf("--conversation = %q", got)
		}
		if got := c.operationRows("--inbox", "outbox"); len(got) != 5 {
			t.Errorf("--inbox outbox = %+v, want the four starts and the reply (the inbox's configuration targets its channel)", got)
		}
		if got := c.operationRows("--since", "2000-01-01", "--until", "2999-01-01"); len(got) != 7 {
			t.Fatalf("a window around now = %d rows, want 7", len(got))
		}
		if got := c.operationRows("--since", "2999-01-01"); len(got) != 0 {
			t.Fatalf("--since 2999 = %+v", got)
		}
		if got := c.operationRows("--until", "2000-01-01"); len(got) != 0 {
			t.Fatalf("--until 2000 = %+v", got)
		}
		// Paged: every page within the limit, nothing twice, nothing lost.
		var list operationPageJSON
		c.json(&list, "operation", "list", "--limit", "2")
		if len(list.Items) > 2 || (!list.Complete && list.Next == "") {
			t.Fatalf("first page = %+v", list)
		}
		seen := map[string]int{}
		for cursor := ""; ; {
			args := []string{"operation", "list", "--limit", "2"}
			if cursor != "" {
				args = append(args, "--cursor", cursor)
			}
			c.json(&list, args...)
			if len(list.Items) > 2 {
				t.Fatalf("page over the limit: %+v", list)
			}
			for _, it := range list.Items {
				seen[it.Operation]++
			}
			if list.Complete || list.Next == "" {
				break
			}
			cursor = list.Next
		}
		if len(seen) != 7 {
			t.Errorf("paged listing saw %d operations, want 7: %v", len(seen), seen)
		}
		for op, n := range seen {
			if n != 1 {
				t.Errorf("operation %s listed %d times across pages", op, n)
			}
		}
		wantStderr(t, c.heo(2, "operation", "list", "--cursor", "x"), "VALIDATION", "cursor")
		wantStderr(t, c.heo(2, "operation", "list", "--state", "lost"), "VALIDATION", `state "lost"`)
		wantStderr(t, c.heo(3, "operation", "list", "--inbox", "nope"), "NOT_FOUND")
		wantStderr(t, c.heo(2, "operation", "list", "--since", "yesterday"), "USAGE")
		wantStderr(t, c.heo(2, "operation", "list", "--limit", "0"), "USAGE")
		r = c.heo(0, "operation", "list")
		for _, want := range []string{"KEY", "KIND", "STATE", "UPDATED", "lost-1", "a-3", "queued", "conversation.start"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("text operation list lacks %q:\n%s", want, r.stdout)
			}
		}

		// Explicit lookup still works, and a wrong kind or target is
		// not found.
		if got := c.operation("--key", "lost-1", "--kind", "conversation.start", "--target", target); got.ID != sent.Operation.ID {
			t.Fatalf("explicit lookup = %+v", got)
		}
		wantStderr(t, c.heo(3, "operation", "get", "--key", "lost-1", "--kind", "message.reply", "--target", target), "NOT_FOUND")
		wantStderr(t, c.heo(2, "operation", "get", "--key", "lost-1", "--kind", "conversation.start", "--target", "channel:"), "USAGE")

		// heo kept no file of any of it.
		c.noHistoryFile()

		// Watch: a queued operation times out (6, TRANSIENT).
		wantStderr(t, c.heo(6, "operation", "watch", string(sent.Operation.ID), "--timeout", "1500ms"),
			"TRANSIENT", "still queued", "operation watch")
		wantStderr(t, c.heo(3, "operation", "watch", "op_404"), "NOT_FOUND")
		wantStderr(t, c.heo(2, "operation", "watch", string(sent.Operation.ID), "--timeout", "-1s"), "USAGE")

		// A worker of the test's own delivers it while a watch follows.
		fake := adapterstest.New()
		e := c.db.engine([]adapter.Factory{fake}, func(cfg *uma.Config) {
			cfg.Work = &uma.WorkPolicy{MaxClaims: 1, MaxInFlight: 8}
		})
		wait := c.asyncRun("operation", "watch", string(sent.Operation.ID), "--format", "json")
		time.Sleep(1500 * time.Millisecond)
		stop := worker(t, e, "ops", 10*time.Second)
		defer stop()
		res := wait()
		if res.exit != 0 {
			t.Fatalf("watch of a delivered operation: exit %d\nstdout: %s\nstderr: %s", res.exit, res.stdout, res.stderr)
		}
		lines := strings.Split(strings.TrimSpace(res.stdout), "\n")
		if first := lastLine(t, lines[0]); first.State != model.Queued || first.Terminal {
			t.Errorf("first line = %+v, want the queued state", first)
		}
		if end := lastLine(t, res.stdout); (end.State != model.ProviderAccepted && end.State != model.Delivered) || !end.Terminal || end.ID != sent.Operation.ID {
			t.Errorf("last line = %+v", end)
		}
		r = c.heo(0, "operation", "watch", string(sent.Operation.ID))
		if f := strings.Split(strings.TrimSpace(r.stdout), "\t"); len(f) != 4 || f[2] != string(sent.Operation.ID) {
			t.Errorf("text watch row = %q, want time, state, operation, failure", r.stdout)
		}

		// The exit follows the state the operation ends in.
		fake.FailSends(errors.New("connection reset after write"))
		var unknown sendJSON
		c.json(&unknown, "message", "send", "--inbox", "outbox", "--to", "user:bob", "--text", "x", "--idempotency-key", "amb-1")
		c.waitDelivery(unknown.Operation.ID, model.Unknown)
		r = c.heo(8, "operation", "watch", string(unknown.Operation.ID))
		wantStderr(t, r, "OUTCOME_UNKNOWN", "ended outcome_unknown")
		if o := c.operation(string(unknown.Operation.ID)); o.State != model.Unknown || !o.Terminal || o.Failure == nil {
			t.Fatalf("operation get of an unknown outcome = %+v (exit 0: a state is an answer)", o)
		}
		fake.FailSends(model.NewError(model.Permanent, "mailbox gone"))
		var failed sendJSON
		c.json(&failed, "message", "send", "--inbox", "outbox", "--to", "user:cy", "--text", "x", "--idempotency-key", "fail-1")
		c.waitDelivery(failed.Operation.ID, model.Failed)
		r = c.heo(1, "operation", "watch", string(failed.Operation.ID), "--format", "json")
		wantStderr(t, r, "PROVIDER_PERMANENT", "ended failed")
		if end := lastLine(t, r.stdout); end.State != model.Failed || end.Failure == nil || end.Failure.Code != model.Permanent {
			t.Errorf("watch of a failed operation ended on %+v", end)
		}
		stop()
		c.stop(d)
	})
}

// TestOperationRetentionHorizon: runtime.retention.operations is uma's
// operation horizon, so a valid one starts the daemon and an invalid one
// is a configuration error; heo keeps no operation history of its own
// with a horizon of its own.
func TestOperationRetentionHorizon(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "runtime:\n  retention:\n    operations: 3h\n")
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-prune"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		c.heo(0, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "x", "--idempotency-key", "kept-1")
		if got := c.operationRows("--kind", "conversation.start"); rowKeys(got) != "kept-1" {
			t.Fatalf("list = %+v", got)
		}
		c.noHistoryFile()
		c.stop(d)

		r := c.s.run(nil, "-c", "runtime.retention.operations=soon", "config", "validate", "--format", "json")
		if r.exit != 2 || !strings.Contains(r.stdout, "runtime.retention.operations") {
			t.Errorf("config validate with a bad horizon: exit %d\n%s", r.exit, r.stdout)
		}
	})
}

// TestOperationReadsAreScopedToTheCaller: another desk actor finds
// nothing under a key it did not submit and lists none of the owner's
// operations (the list asks uma for the caller's own, though uma would
// show every operation the actor may read), only its own, though it may
// read an operation by ID like any reader; an external actor may read no
// operation at all. Read commands are served; watch is socket-only.
func TestOperationReadsAreScopedToTheCaller(t *testing.T) {
	withE2EBinary(t, func() {
		const deskB, partner = "uid:4243", "partner-portal"
		c := newCLISite(t, "")
		d := c.start("workers")
		c.fakeChannel("fake", adapterstest.Config{Account: "acct-scope"})
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "outbox", "--destination", "out-1",
			"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
		var sent sendJSON
		c.json(&sent, "message", "send", "--inbox", "outbox", "--to", "user:ada", "--text", "mine", "--idempotency-key", "mine-1")
		var inbox model.Inbox
		for _, i := range c.inboxes() {
			if i.Key == "outbox" {
				inbox = i
			}
		}
		c.stop(d)

		c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  grants:\n"+
			"    - actor: \""+deskB+"\"\n      roles: [desk]\n    - actor: \""+partner+"\"\n      roles: [external]\n")
		auth := c.s.useHTTPAuth()
		api := freeAddr(t)
		d = c.s.start(nil, nil, []string{"socket", "receivers", "api"}, "-c", auth.config,
			"service", "start", "--enable", "api", "--addr", api, "--disable=workers")
		defer func() { c.s.want(result{exit: d.term()}, 0) }()
		x := &surfaces{d: d, auth: auth, api: api}
		get := func(token, route string) (int, map[string]any) {
			t.Helper()
			return x.rest(t, http.MethodGet, route, token, "")
		}
		owner, other, ext := auth.token(me()), auth.token(deskB), auth.token(partner)
		byKey := "/v1/commands/operation/get?key=" + url.QueryEscape("mine-1")

		code, body := get(owner, byKey)
		if code != http.StatusOK || !strings.Contains(asString(body), string(sent.Operation.ID)) {
			t.Fatalf("owner by key = %d %v", code, body)
		}
		// Another desk actor: not found by key (the key is the owner's),
		// and an empty list; by ID it reads, as any reader does.
		code, body = get(other, byKey)
		if code != http.StatusNotFound {
			t.Errorf("other desk actor by key = %d %v, want 404", code, body)
		}
		code, body = get(other, "/v1/commands/operation/get?key=mine-1&kind=conversation.start&target="+url.QueryEscape("inbox:"+string(inbox.Ref.ID)))
		if code != http.StatusNotFound {
			t.Errorf("other desk actor by key, kind and target = %d %v, want 404", code, body)
		}
		code, body = get(other, "/v1/commands/operation/list")
		if code != http.StatusOK || listedKey(body, "mine-1") || strings.Contains(asString(body), string(sent.Operation.ID)) {
			t.Errorf("other desk actor's list = %d %v, must not show the owner's operations", code, body)
		}
		code, body = get(owner, "/v1/commands/operation/list")
		if code != http.StatusOK || !listedKey(body, "mine-1") {
			t.Errorf("owner's list = %d %v", code, body)
		}
		code, body = get(other, "/v1/commands/operation/get?arg="+string(sent.Operation.ID))
		if code != http.StatusOK || !strings.Contains(asString(body), string(sent.Operation.ID)) {
			t.Errorf("other desk actor by ID = %d %v, want the operation: operation.read decides", code, body)
		}
		// A delivery list is the caller's own writes.
		code, body = get(owner, "/v1/commands/delivery/list")
		if code != http.StatusOK || !strings.Contains(asString(body), string(sent.Operation.ID)) {
			t.Errorf("owner's delivery list = %d %v", code, body)
		}
		code, body = get(other, "/v1/commands/delivery/list")
		if code != http.StatusOK || strings.Contains(asString(body), string(sent.Operation.ID)) {
			t.Errorf("other desk actor's delivery list = %d %v, must not show the owner's writes", code, body)
		}
		// The other actor's own write is its own: listed for it, found
		// by its key, and not the owner's.
		code, body = x.rest(t, http.MethodPost, "/v1/commands/message/reply", other,
			`{"args":["`+string(sent.Conversation)+`"],"flags":{"text":"from b","idempotency-key":"b-1"}}`)
		if code >= 300 {
			t.Fatalf("other desk actor's reply = %d %v", code, body)
		}
		code, body = get(other, "/v1/commands/operation/list")
		if code != http.StatusOK || !listedKey(body, "b-1") || listedKey(body, "mine-1") {
			t.Errorf("other desk actor's list after its write = %d %v", code, body)
		}
		code, body = get(other, "/v1/commands/delivery/list")
		if code != http.StatusOK || !listedKey(body, "b-1") || listedKey(body, "mine-1") {
			t.Errorf("other desk actor's delivery list after its write = %d %v", code, body)
		}
		code, body = get(other, "/v1/commands/operation/get?key=b-1")
		if code != http.StatusOK || !strings.Contains(asString(body), "message.reply") {
			t.Errorf("other desk actor by its own key = %d %v", code, body)
		}
		code, body = get(owner, "/v1/commands/operation/list")
		if code != http.StatusOK || listedKey(body, "b-1") {
			t.Errorf("owner's list = %d %v, must not show the other actor's write", code, body)
		}
		code, body = get(owner, "/v1/commands/operation/get?key=b-1")
		if code != http.StatusNotFound {
			t.Errorf("owner by the other actor's key = %d %v, want 404", code, body)
		}
		// External: no operation reads at all.
		for _, route := range []string{byKey, "/v1/commands/operation/list", "/v1/commands/operation/get?arg=" + string(sent.Operation.ID),
			"/v1/commands/delivery/list"} {
			code, body = get(ext, route)
			if code != http.StatusForbidden || !strings.Contains(asString(body), "heo.authz.not_granted") {
				t.Errorf("external %s = %d %v, want 403 not_granted", route, code, body)
			}
		}
		// Reconciling a delivery spends a provider call, so it is a write
		// (POST), and the external actor is refused it too.
		code, body = x.rest(t, http.MethodPost, "/v1/commands/delivery/reconcile", ext,
			`{"args":["`+string(sent.Operation.ID)+`"]}`)
		if code != http.StatusForbidden || !strings.Contains(asString(body), "heo.authz.not_granted") {
			t.Errorf("external delivery reconcile = %d %v, want 403 not_granted", code, body)
		}
		// The terminal (the owner) is unaffected.
		if got := c.operation("--key", "mine-1"); got.ID != sent.Operation.ID {
			t.Fatalf("owner by key after the restart = %+v", got)
		}
	})
}

func mustJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
}

// listedKey reports whether a list response (data.items) has an item whose
// key is exactly key. A substring match on the whole body is not enough:
// random ids in the same body can contain short keys like "b-1".
func listedKey(body map[string]any, key string) bool {
	data, _ := body["data"].(map[string]any)
	items, _ := data["items"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok && fmt.Sprint(m["key"]) == key {
			return true
		}
	}
	return false
}

// TestListedKeyIgnoresIDSubstrings: a random id in the body that happens
// to contain a short key (here "b-1" inside "91ab-12af") is not a listed key.
func TestListedKeyIgnoresIDSubstrings(t *testing.T) {
	body := map[string]any{"data": map[string]any{"items": []any{
		map[string]any{"key": "mine-1", "target": "channel:01a1ea32-3aff-710e-91ab-12af89f614df"},
	}}}
	if !strings.Contains(asString(body), "b-1") {
		t.Fatal("fixture no longer has the substring this test guards against")
	}
	if listedKey(body, "b-1") {
		t.Error(`listedKey found "b-1" inside an id`)
	}
	if !listedKey(body, "mine-1") {
		t.Error(`listedKey missed the listed key "mine-1"`)
	}
}
