package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"hop.top/heo/internal/composition"
	"hop.top/heo/internal/storage"
	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	umapg "hop.top/uma/store/postgres"
)

// sseEvent is one event of a widget event stream.
type sseEvent struct{ id, name, data string }

// widgetEvents opens a widget visitor's event stream at route with
// token, resuming after cursor when it is not empty, and returns its
// events as they arrive.
func widgetEvents(t *testing.T, route, token, cursor string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route+composition.WidgetEventsRoute+"?token="+url.QueryEscape(token), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if cursor != "" {
		req.Header.Set("Last-Event-ID", cursor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("event stream = %d", resp.StatusCode)
	}
	t.Cleanup(func() {
		cancel()
		_ = resp.Body.Close()
	})
	out := make(chan sseEvent, 16)
	go func() {
		defer close(out)
		var ev sseEvent
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					out <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return out
}

// next waits for the stream's next event.
func next(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("event stream ended")
		}
		return ev
	case <-time.After(15 * time.Second):
		t.Fatal("no event within 15s")
		return sseEvent{}
	}
}

// postJSON posts body to url with the given headers and returns the
// status and response body.
func postJSON(t *testing.T, url, body string, header ...string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	_, _ = bufio.NewReader(resp.Body).WriteTo(&b)
	return resp.StatusCode, b.String()
}

// widgetSite is a widget channel and inbox in the default tenant of a
// fresh store, and a sandbox whose daemon serves its webhook.
type widgetSite struct {
	t     *testing.T
	db    *testDB
	s     *sandbox
	hook  string
	route string
}

// newWidgetSite configures the widget channel and its inbox, and writes
// the sandbox's configuration with extra appended (YAML at top level).
func newWidgetSite(t *testing.T, extra string) *widgetSite {
	t.Helper()
	db := newDB(t).migrate()
	op := db.operator("default")
	ch := db.configure("default", model.Channel{
		Key: "widget", Type: widget.Type,
		Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key",
	})["widget"]
	lock := true
	if _, err := op.Configuration().ConfigureInbox(t.Context(), model.ConfigureInbox{
		Command: model.Command{Call: model.Call{Correlation: "e2e"}, Operation: "inbox-web"},
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: "default"}, Channel: model.ChannelRef{Tenant: "default", ID: ch},
			Name: "web", Destinations: []string{"site-1"},
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		},
	}); err != nil {
		t.Fatalf("configure inbox: %v", err)
	}
	s := newSandbox(t)
	s.write("cfg/heo/config.yaml", fmt.Sprintf(
		"credentials:\n  backend: file\n  file:\n    dir: %q\nstorage:\n  postgres:\n    dsn: %q\n"+
			"adapters:\n  widget:\n    retain: 2\nservices:\n  workers:\n    poll: 50ms\n%s",
		s.path("secrets"), db.dsn, extra,
	))
	key, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.write("secrets/uma/default/widget-key", string(key))
	hook := freeAddr(t)
	return &widgetSite{t: t, db: db, s: s, hook: hook, route: "http://" + hook + "/webhooks/" + string(ch)}
}

// start runs the daemon with workers and the webhook listener.
func (w *widgetSite) start() *daemon {
	w.t.Helper()
	d := w.s.start(nil, nil, []string{"workers", "webhook"},
		"-c", "services.webhook.addr="+w.hook, "service", "start", "--enable", "webhook")
	w.t.Cleanup(func() {
		if w.t.Failed() {
			w.t.Logf("daemon log:\n%s", d.log())
		}
	})
	return d
}

// visit opens a visitor session, sends its first message and returns
// the session token and the conversation the daemon filed it in.
func (w *widgetSite) visit() (string, model.ConversationRef) {
	t := w.t
	t.Helper()
	code, body := postJSON(t, w.route+"/session?"+composition.WidgetSiteParam+"=site-1", "{}")
	var sess widget.SessionResponse
	if code != http.StatusOK || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("session = %d %s", code, body)
	}
	if code, body := postJSON(t, w.route+"/messages", `{"id":"m-1","text":"hello"}`, "Authorization", "Bearer "+sess.Token); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d %s", code, body)
	}
	page, err := w.db.operator("default").Conversations().List(t.Context(), model.ListRequest{Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("conversations = %+v, %v; want the visitor's", page.Items, err)
	}
	return sess.Token, page.Items[0].Ref
}

// reply queues a public reply to conv as c's actor and returns its
// operation.
func reply(t *testing.T, c uma.Client, conv model.ConversationRef, text string) model.OperationRef {
	t.Helper()
	acc, err := c.Messages().Reply(t.Context(), model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "e2e"}, Operation: model.OperationID("reply-" + text)},
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		t.Fatalf("reply %s: %v", text, err)
	}
	return acc.Operation.Ref
}

// TestRuntimeWidgetEventsSurviveRestart: a visitor's conversation gets
// replies the daemon's workers deliver to the visitor's event stream;
// the daemon stops and starts again, and a stream resuming from its
// cursor receives what was delivered before the restart; a stream whose
// cursor is behind the retained events (adapters.widget.retain) gets a
// reset first. The release build runs it: the replies are queued as the
// test's uid, the daemon's owner, whom the default policy makes a desk
// and operator actor.
func TestRuntimeWidgetEventsSurviveRestart(t *testing.T) {
	site := newWidgetSite(t, "")
	route := site.route
	d := site.start()
	token, conv := site.visit()
	op := site.db.operator("default")
	send := func(text string) {
		t.Helper()
		reply(t, op, conv, text)
	}

	live := widgetEvents(t, route, token, "")
	send("one")
	first := next(t, live)
	send("two")
	second := next(t, live)
	if first.name != widget.EventMessage || !strings.Contains(first.data, `"one"`) ||
		second.name != widget.EventMessage || !strings.Contains(second.data, `"two"`) {
		t.Fatalf("live events = %+v, %+v; want both replies", first, second)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}

	d = site.start()
	if ev := next(t, widgetEvents(t, route, token, first.id)); ev.id != second.id || !strings.Contains(ev.data, `"two"`) {
		t.Fatalf("resumed after %s with %+v, want the reply delivered before the restart (%s)", first.id, ev, second.id)
	}
	// A third reply leaves two events retained: a stream from the start
	// is behind them and resets before the retained ones.
	send("three")
	if ev := next(t, widgetEvents(t, route, token, second.id)); !strings.Contains(ev.data, `"three"`) {
		t.Fatalf("after %s = %+v, want the third reply", second.id, ev)
	}
	fromStart := widgetEvents(t, route, token, "")
	if ev := next(t, fromStart); ev.name != widget.EventReset {
		t.Fatalf("stream from the start = %+v, want a reset (retain 2)", ev)
	}
	if ev := next(t, fromStart); ev.id != second.id {
		t.Errorf("after the reset = %+v, want the oldest retained event %s", ev, second.id)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}

// recipientLogsSchema is the uma schema that added the store's recipient
// logs, where the widget keeps visitor events.
const recipientLogsSchema = 15

// pgDumpInContainer writes a pg_dump for heo's configuration that runs
// the test server's own pg_dump in its container, reaching the server
// on the container's port, and returns its path.
func pgDumpInContainer(t *testing.T, s *sandbox) string {
	t.Helper()
	return pgToolInContainer(t, s, "pg_dump")
}

// pgRestoreInContainer is pgDumpInContainer for pg_restore, which reads
// the dump on its standard input.
func pgRestoreInContainer(t *testing.T, s *sandbox) string {
	t.Helper()
	return pgToolInContainer(t, s, "pg_restore")
}

func pgToolInContainer(t *testing.T, s *sandbox, tool string) string {
	t.Helper()
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker CLI, to run pg_dump in the test server's container: %v", err)
	}
	cli, err := testcontainers.NewDockerClientWithOpts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	dockerHost := cli.DaemonHost()
	_ = cli.Close()
	p := s.write("bin/"+tool, fmt.Sprintf(`#!/bin/sh
set -e
for a; do
	shift
	set -- "$@" "$(printf '%%s' "$a" | sed 's#@%s/#@localhost:5432/#')"
done
DOCKER_HOST=%q exec %q exec -i %s %s "$@"
`, pg.host, dockerHost, docker, pg.ctr.GetContainerID(), tool))
	if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // G302: the test's own executable
		t.Fatal(err)
	}
	return p
}

// TestRuntimeStoreBeforeWidgetLogsMigrates: a store at the schema
// before recipient logs is refused at start with the way forward, and
// `heo storage migrate` brings it to this binary's schema, after which
// the daemon starts.
func TestRuntimeStoreBeforeWidgetLogsMigrates(t *testing.T) {
	if umapg.SchemaVersion < recipientLogsSchema {
		t.Fatalf("uma schema %d predates recipient logs (%d)", umapg.SchemaVersion, recipientLogsSchema)
	}
	db := newDB(t).migrate()
	db.downgradeTo(recipientLogsSchema - 1)
	s := newSandbox(t)
	s.useStore(db)
	r := s.run(nil, "service", "start")
	s.want(r, 1, "service", "start")
	for _, want := range []string{
		fmt.Sprintf("store schema version %d is older than %d", recipientLogsSchema-1, umapg.SchemaVersion),
		"run `heo storage migrate`",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}

	pgDump := pgDumpInContainer(t, s)
	args := []string{
		"storage", "migrate", "--format", "json", "-c", "storage.postgres.pg_dump=" + pgDump,
		"-c", "storage.backup.dir=" + s.path("backups"),
	}
	r = s.run(nil, args...)
	s.want(r, 0, args...)
	var m struct {
		From, To int64
		Backup   *struct{ Schema int64 }
	}
	if err := json.Unmarshal([]byte(lastJSON(r.stdout)), &m); err != nil {
		t.Fatalf("%v: %s", err, r.stdout)
	}
	if m.From != recipientLogsSchema-1 || m.To != umapg.SchemaVersion || m.Backup == nil || m.Backup.Schema != recipientLogsSchema-1 {
		t.Fatalf("migrate = %+v, want %d → %d after a backup", m, recipientLogsSchema-1, umapg.SchemaVersion)
	}
	r = s.run(nil, "storage", "status", "--format", "json")
	s.want(r, 0, "storage", "status")
	if !strings.Contains(r.stdout, `"compat": "`+string(storage.CompatOK)+`"`) {
		t.Errorf("status after migrate:\n%s", r.stdout)
	}
	d := s.start(nil, nil, []string{"workers", "receivers"}, "service", "start")
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}
