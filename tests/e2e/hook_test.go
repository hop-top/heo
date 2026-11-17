package e2e

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// hookRowJSON, hookDeadJSON, hookTestJSON and hookReplayJSON are the
// hook verbs' --format json.
type hookRowJSON struct {
	Plugin       string
	Mode         string
	State        string
	Cursor       string
	Delivered    uint64
	DeadLettered uint64 `json:"dead_lettered"`
	Dead         *int
}

type hookDeadJSON struct {
	Plugin, Event, Kind, State, Class string
	Attempts                          int
}

type hookJobJSON struct {
	ID, Plugin, Kind, State, Error string
	Replay                         *struct {
		Scanned, Delivered, Skipped, Failed int
		Complete                            bool
	}
	Retry *hookDeadJSON
}

type hookTestJSON struct {
	Plugin     string
	Event      string
	Admitted   bool
	Ok         bool
	WouldApply bool `json:"would_apply"`
	Applied    bool
	Operations []struct {
		Verb    string
		Key     string `json:"idempotency_key"`
		Allowed bool
	}
}

// TestHookVerbs drives the release binary on Postgres: a plugin that
// fails for good dead-letters the events of a real conversation, and
// hook list, hook test, hook replay, hook job and hook dead-letter report and act on them through the daemon.
func TestHookVerbs(t *testing.T) {
	s := newDeskSite(t)
	conv := string(s.conv.ID)
	s.stop(s.d)
	bad := s.s.write("plugins/bad.sh", "#!/bin/sh\ncat >/dev/null\nexit 2\n")
	good := s.s.write("plugins/good.sh", fmt.Sprintf(
		"#!/bin/sh\ncat >/dev/null\necho '{\"operations\":[{\"verb\":\"note add\",\"args\":[%q],\"flags\":{\"text\":\"seen\",\"placement\":\"local\"}}]}'\n", conv))
	for _, p := range []string{bad, good} {
		if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // an executable script in a temp dir
			t.Fatal(err)
		}
	}
	cfg, err := os.ReadFile(s.s.path("cfg/heo/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s.s.write("cfg/heo/config.yaml", string(cfg)+fmt.Sprintf(`authorization:
  grants:
    - actor: plugin:bad
      roles: [observer]
    - actor: plugin:good
      roles: [observer, desk]
plugins:
  - name: bad
    command: %s
    filter:
      kinds: [message.created]
  - name: good
    command: %s
    filter:
      kinds: [message.received]
`, bad, good))
	s.heo(0, "plugin", "token", "mint", "bad")
	s.heo(0, "plugin", "token", "mint", "good")
	d := s.s.start(nil, nil, []string{"socket", "receivers", "webhook", "workers", "hooks"},
		"-c", "services.webhook.addr="+s.hook, "-c", "services.hooks.enabled=true", "service", "start", "--enable", "webhook")
	defer s.stop(d)

	// A reply is an event after the plugins' tail: bad dead-letters it.
	s.json(&replyJSON{}, "message", "reply", conv, "--text", "hello", "--idempotency-key", "hook-r1")
	var dead []hookDeadJSON
	eventually(t, "the failing plugin's event is a dead letter", func() bool {
		s.json(&dead, "hook", "dead-letter", "list", "--plugin", "bad")
		return len(dead) == 1
	})
	if dead[0].State != "dead" || dead[0].Class != "permanent" || dead[0].Plugin != "bad" || dead[0].Kind != "message.created" {
		t.Errorf("dead letter %+v", dead[0])
	}
	ev := dead[0].Event

	var rows []hookRowJSON
	s.json(&rows, "hook", "list")
	if len(rows) != 2 || rows[0].Plugin != "bad" || rows[0].State != "ready" || rows[0].DeadLettered != 1 || rows[0].Dead == nil || *rows[0].Dead != 1 ||
		rows[1].Plugin != "good" || rows[1].Dead == nil || *rows[1].Dead != 0 {
		t.Errorf("hook list = %+v", rows)
	}

	// test: the good plugin answers and nothing is applied; the failing
	// one is reported and the verb fails.
	var tr hookTestJSON
	s.json(&tr, "hook", "test", "good", "--event", ev)
	if !tr.Admitted || !tr.Ok || !tr.WouldApply || tr.Applied || len(tr.Operations) != 1 || tr.Operations[0].Verb != "note add" ||
		!tr.Operations[0].Allowed || tr.Operations[0].Key == "" {
		t.Errorf("hook test good = %+v", tr)
	}
	r := s.heo(1, "hook", "test", "bad", "--event", ev, "--format", "json")
	if !strings.Contains(r.stdout, `"ok": false`) || !strings.Contains(r.stdout, "permanent") {
		t.Errorf("hook test bad stdout:\n%s", r.stdout)
	}
	wantStderr(t, s.heo(3, "hook", "test", "good", "--event", "no-such-event"), "NOT_FOUND")

	// replay: the good plugin's result has an operation, the case that
	// deadlocks when run inside the command. A dry run queues nothing; the
	// replay is queued and answers at once; the job finishes, and the
	// note exists once, however often the log is replayed.
	notes := func() int {
		var p pageJSON[model.Message]
		s.json(&p, "note", "list", "--conversation", conv)
		return len(p.Items)
	}
	var dry struct{ Effects []struct{ Target string } }
	s.json(&dry, "hook", "replay", "good", "--from", "beginning", "--dry-run")
	if len(dry.Effects) == 0 || notes() != 0 {
		t.Fatalf("replay --dry-run = %+v, notes %d", dry, notes())
	}
	for range 2 {
		var job hookJobJSON
		s.json(&job, "hook", "replay", "good", "--from", "beginning")
		if job.State != "queued" || job.ID == "" || job.Kind != "replay" {
			t.Fatalf("replay = %+v", job)
		}
		eventually(t, "the replay job finished", func() bool {
			s.json(&job, "hook", "job", "get", job.ID)
			return job.State == "done" || job.State == "failed"
		})
		if job.State != "done" || job.Replay == nil || job.Replay.Delivered != 1 || job.Replay.Failed != 0 || !job.Replay.Complete {
			t.Fatalf("replay job = %+v %+v", job, job.Replay)
		}
		if n := notes(); n != 1 {
			t.Fatalf("%d notes after a replay, want the operation applied once", n)
		}
	}
	wantStderr(t, s.heo(3, "hook", "job", "get", "no-such-job"), "NOT_FOUND")

	// retry: bad fails again, so its job fails and the letter stays dead.
	var rj hookJobJSON
	s.json(&rj, "hook", "dead-letter", "retry", "bad", ev)
	eventually(t, "the retry job finished", func() bool {
		s.json(&rj, "hook", "job", "get", rj.ID)
		return rj.State == "done" || rj.State == "failed"
	})
	if rj.State != "failed" || rj.Retry == nil || rj.Retry.State != "dead" || rj.Retry.Attempts < 2 {
		t.Errorf("retry job = %+v %+v", rj, rj.Retry)
	}
	var rplan struct {
		Effects []struct{ Kind, Target string }
	}
	s.json(&rplan, "hook", "dead-letter", "retry", "bad", ev, "--dry-run")
	if len(rplan.Effects) != 2 {
		t.Errorf("retry --dry-run = %+v", rplan)
	}

	// drop: a dry run shows the plan, the drop discards the letter, and a
	// second drop is a conflict. (The destructive tier is rate limited, so
	// the calls are few; the confirmation gate has its own test.)
	var plan struct {
		Effects  []struct{ Kind, Target string }
		Warnings []string
	}
	s.json(&plan, "hook", "dead-letter", "drop", "bad", ev, "--dry-run")
	if len(plan.Effects) != 1 || len(plan.Warnings) != 0 {
		t.Errorf("drop --dry-run = %+v", plan)
	}
	s.heo(0, "hook", "dead-letter", "drop", "bad", ev, "--confirm=yes")
	s.json(&dead, "hook", "dead-letter", "list", "--plugin", "bad", "--state", "discarded")
	if len(dead) != 1 || dead[0].Event != ev {
		t.Errorf("discarded list = %+v", dead)
	}
	wantStderr(t, s.heo(4, "hook", "dead-letter", "drop", "bad", ev, "--confirm=yes"), "CONFLICT")
	wantStderr(t, s.heo(2, "hook", "dead-letter", "list", "--state", "nope"), "USAGE")
}

// manualPlugin returns the example plugin of docs/manual/hooks.md, so the
// manual's script is the one under test.
func manualPlugin(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../docs/manual/hooks.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), "<!-- example:plugin.sh -->\n```sh\n")
	if !ok {
		t.Fatal("hooks.md has no example:plugin.sh block")
	}
	script, _, ok := strings.Cut(rest, "```")
	if !ok {
		t.Fatal("example:plugin.sh block is not closed")
	}
	return script
}

// TestHookPluginModes drives the manual's example plugin on Postgres in
// both process modes at once: a visitor's message is received, each
// plugin labels the conversation (conversation label) and leaves a local
// note (note add, the draft stand-in; provider drafts are not an
// operation yet), and replaying the log to each plugin applies nothing
// twice.
func TestHookPluginModes(t *testing.T) {
	s := newDeskSite(t)
	conv := string(s.conv.ID)
	modes := []string{"exec", "stream"}
	for _, m := range modes {
		s.heo(0, "label", "create", "seen-"+m)
	}
	s.stop(s.d)
	script := s.s.write("plugins/triage.sh", manualPlugin(t))
	if err := os.Chmod(script, 0o700); err != nil { //nolint:gosec // an executable script in a temp dir
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(s.s.path("cfg/heo/config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var plugins, grants strings.Builder
	for _, m := range modes {
		fmt.Fprintf(&plugins, "  - name: %s\n    command: %s\n    mode: %s\n    filter:\n      kinds: [message.received]\n", m, script, m)
		fmt.Fprintf(&grants, "    - actor: plugin:%s\n      roles: [observer, desk]\n", m)
	}
	s.s.write("cfg/heo/config.yaml", string(cfg)+"authorization:\n  grants:\n"+grants.String()+"plugins:\n"+plugins.String())
	for _, m := range modes {
		s.heo(0, "plugin", "token", "mint", m)
	}
	d := s.s.start(nil, nil, []string{"socket", "receivers", "webhook", "workers", "hooks"},
		"-c", "services.webhook.addr="+s.hook, "-c", "services.hooks.enabled=true", "service", "start", "--enable", "webhook")
	defer s.stop(d)

	if code, body := postJSON(t, s.route+"/messages", `{"id":"m-2","text":"again"}`, "Authorization", "Bearer "+s.token); code != http.StatusAccepted {
		t.Fatalf("visitor message = %d %s", code, body)
	}
	state := func() (labelled map[string]int, notes int) {
		labelled = map[string]int{}
		for _, m := range modes {
			var p pageJSON[model.Conversation]
			s.json(&p, "conversation", "list", "--label", "seen-"+m)
			labelled[m] = len(p.Items)
		}
		var p pageJSON[model.Message]
		s.json(&p, "note", "list", "--conversation", conv)
		return labelled, len(p.Items)
	}
	eventually(t, "both plugins labelled and noted the conversation", func() bool {
		l, n := state()
		return l["exec"] == 1 && l["stream"] == 1 && n == 2
	})

	// Replay each plugin's log from the beginning, twice. The log holds
	// the visitor's first message, which predates the plugins' tail and
	// was never delivered: the first replay handles it (one more note per
	// plugin) and repeats the message the plugins already handled under
	// its first keys, which changes nothing. The second replay is a no-op
	// as a whole.
	replay := func() {
		for _, m := range modes {
			var job hookJobJSON
			s.json(&job, "hook", "replay", m, "--from", "beginning")
			eventually(t, m+" replay job finished", func() bool {
				s.json(&job, "hook", "job", "get", job.ID)
				return job.State == "done" || job.State == "failed"
			})
			if job.State != "done" || job.Replay == nil || job.Replay.Delivered != 2 || job.Replay.Failed != 0 {
				t.Fatalf("%s replay = %+v %+v", m, job, job.Replay)
			}
		}
	}
	replay()
	if l, n := state(); l["exec"] != 1 || l["stream"] != 1 || n != 4 {
		t.Errorf("after replay labelled %v, %d notes; want one label each and four notes", l, n)
	}
	replay()
	if l, n := state(); l["exec"] != 1 || l["stream"] != 1 || n != 4 {
		t.Errorf("after a second replay labelled %v, %d notes; want no change", l, n)
	}
	var rows []hookRowJSON
	s.json(&rows, "hook", "list")
	for _, r := range rows {
		if r.DeadLettered != 0 {
			t.Errorf("%s dead-lettered %d", r.Plugin, r.DeadLettered)
		}
	}
}
