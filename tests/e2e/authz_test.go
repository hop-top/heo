package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// pingAs runs `daemon ping` for profile args and returns the result.
func pingAs(s *sandbox, pre ...string) result {
	s.t.Helper()
	return s.run(nil, append(pre, "daemon", "ping", "--format", "json")...)
}

// wantDenied requires r to be the policy's refusal: UNAUTHORIZED, exit
// 5, naming the reason, with nothing on stdout.
func wantDenied(t *testing.T, r result, reason string) {
	t.Helper()
	if r.exit != 5 || !strings.Contains(r.stderr, "UNAUTHORIZED") || !strings.Contains(r.stderr, reason) || r.stdout != "" {
		t.Fatalf("exit %d, want 5 UNAUTHORIZED (%s)\nstdout: %s\nstderr: %s", r.exit, reason, r.stdout, r.stderr)
	}
}

// operationState waits until op leaves the queue and returns it.
func operationState(t *testing.T, c uma.Client, op model.OperationRef) model.Operation {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := c.Operations().Get(t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "e2e"}, Ref: op})
		if err != nil {
			t.Fatal(err)
		}
		if got.State != model.Queued && got.State != model.RetryPending {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation still %s after 15s", got.State)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestReleaseDaemonDeliversOnlyGrantedReplies: the release daemon, its
// policy configured, delivers an operator's reply end to end through
// the widget to the visitor's event stream. Before the operator is
// granted anything, the daemon refuses its commands (UNAUTHORIZED, exit
// 5) and its workers, rechecking the queued reply for the uid that
// queued it, fail it unauthorized without delivering it.
func TestReleaseDaemonDeliversOnlyGrantedReplies(t *testing.T) {
	// The owner's default roles withdrawn: this uid holds nothing.
	site := newWidgetSite(t, "authorization:\n  owner: []\n")
	d := site.start()
	token, conv := site.visit()
	op := site.db.operator("default")

	wantDenied(t, pingAs(site.s), "heo.authz.no_role")
	denied := operationState(t, op, reply(t, op, conv, "denied"))
	if denied.State != model.Failed || denied.Failure == nil || denied.Failure.Code != model.Unauthorized {
		t.Fatalf("reply queued by an ungranted uid = %s %+v, want failed unauthorized", denied.State, denied.Failure)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}

	// The operator configured by uid, in the profile's file.
	site.s.write("cfg/heo/profiles/default.yaml",
		"authorization:\n  grants:\n    - actor: \""+me()+"\"\n      roles: [desk]\n")
	d = site.start()
	r := pingAs(site.s)
	site.s.want(r, 0, "daemon", "ping")
	if p := decodePing(t, r.stdout); p.Actor != me() {
		t.Errorf("ping actor = %s, want %s", p.Actor, me())
	}
	events := widgetEvents(t, site.route, token, "")
	granted := reply(t, op, conv, "granted")
	ev := next(t, events)
	if ev.name != widget.EventMessage || !strings.Contains(ev.data, `"granted"`) {
		t.Fatalf("visitor's first event = %+v, want the granted reply (the denied one never delivered)", ev)
	}
	if got := operationState(t, op, granted); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Errorf("granted reply = %s %+v", got.State, got.Failure)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}

// TestApsBindingGrantsNoAuthority: an aps-bound profile's daemon serves
// the socket peer's uid as the actor, never the aps profile; binding a
// profile grants nothing (with the owner's role withdrawn, the same uid
// is refused), and a grant naming the profile is refused as
// configuration.
func TestApsBindingGrantsNoAuthority(t *testing.T) {
	s := newSandbox(t)
	s.apsProfile("noor")
	prof := []string{"--profile", "aps:noor"}
	file := "cfg/heo/profiles/aps-noor.yaml"
	startNoor := func() *daemon {
		return s.start(nil, nil, []string{"socket"}, append(prof, "service", "start", noRuntime)...)
	}

	s.write(file, "")
	d := startNoor()
	r := pingAs(s, prof...)
	s.want(r, 0, "daemon", "ping")
	if p := decodePing(t, r.stdout); p.Actor != me() || p.Profile != "aps://profile/noor" {
		t.Errorf("ping = %+v, want actor %s on aps://profile/noor", p, me())
	}
	s.want(result{exit: d.term()}, 0)

	s.write(file, "authorization:\n  owner: []\n")
	d = startNoor()
	wantDenied(t, pingAs(s, prof...), "heo.authz.no_role")
	s.want(result{exit: d.term()}, 0)

	s.write(file, "authorization:\n  owner: []\n  grants:\n    - actor: aps:noor\n      roles: [desk]\n")
	args := append(append([]string{}, prof...), "config", "validate", "--format", "json")
	r = s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stdout, `"key": "authorization.grants[0].actor"`) || !strings.Contains(r.stdout, "grants no authority") {
		t.Errorf("validate:\n%s", r.stdout)
	}
	args = append(append([]string{}, prof...), "service", "start", noRuntime)
	r = s.run(nil, args...)
	s.want(r, 2, args...)
	if !strings.Contains(r.stderr, "authorization.grants[0].actor") || readyRe.MatchString(r.stderr) {
		t.Errorf("start with a profile granted as an actor:\n%s", r.stderr)
	}
}

// TestConfigValidatePolicy: config validate checks the policy offline —
// an unknown role is an error, a grant no enabled surface can reach a
// warning — and service start refuses a policy that does not read.
func TestConfigValidatePolicy(t *testing.T) {
	s := newSandbox(t)
	type issue struct{ Severity, Key, Message string }
	validate := func(exit int) []issue {
		t.Helper()
		r := s.run(nil, "config", "validate", "--format", "json")
		s.want(r, exit, "config", "validate")
		var v struct{ Issues []issue }
		if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
			t.Fatalf("%v\n%s", err, r.stdout)
		}
		return v.Issues
	}
	has := func(issues []issue, sev, key, msg string) bool {
		for _, i := range issues {
			if i.Severity == sev && i.Key == key && strings.Contains(i.Message, msg) {
				return true
			}
		}
		return false
	}

	// Unset, the owner holds desk and operator: the effective default
	// shows as such, and it validates clean.
	owner := func() (value, origin string) {
		t.Helper()
		r := s.run(nil, "config", "get", "authorization.owner", "--format", "json")
		s.want(r, 0, "config", "get", "authorization.owner")
		var e struct{ Value, Origin string }
		if err := json.Unmarshal([]byte(r.stdout), &e); err != nil {
			t.Fatalf("%v\n%s", err, r.stdout)
		}
		return e.Value, e.Origin
	}
	if value, origin := owner(); value != `["desk","operator"]` || origin != "default" {
		t.Errorf("default authorization.owner = %s (%s), want [\"desk\",\"operator\"] (default)", value, origin)
	}
	if issues := validate(0); len(issues) != 0 {
		t.Errorf("default policy issues = %+v", issues)
	}
	s.write("cfg/heo/config.yaml", "authorization:\n  owner: [desk]\n")
	if value, origin := owner(); value != `["desk"]` || origin == "default" {
		t.Errorf("configured authorization.owner = %s (%s), want [\"desk\"] from the file", value, origin)
	}

	s.write("cfg/heo/config.yaml", "authorization:\n  owner: [admin]\n")
	if issues := validate(2); !has(issues, "error", "authorization.owner", `unknown role "admin"`) {
		t.Errorf("issues = %+v", issues)
	}
	r := s.run(nil, "service", "start", noRuntime)
	s.want(r, 2, "service", "start")
	if !strings.Contains(r.stderr, "authorization.owner") || readyRe.MatchString(r.stderr) {
		t.Errorf("start with an unknown role:\n%s", r.stderr)
	}

	s.write("cfg/heo/config.yaml", "authorization:\n  grants:\n    - actor: uid:4242\n      roles: [viewer]\n")
	if issues := validate(0); !has(issues, "warning", "authorization.grants[0].actor", "cannot reach the daemon") {
		t.Errorf("issues = %+v", issues)
	}
}

// TestConfigValidatePlugins: config validate refuses an unknown event
// kind, a plugin with no grant, a command that is not found and a name
// used twice; a granted plugin with a real command validates.
func TestConfigValidatePlugins(t *testing.T) {
	s := newSandbox(t)
	validate := func(exit int) string {
		t.Helper()
		r := s.run(nil, "config", "validate", "--format", "json")
		s.want(r, exit, "config", "validate")
		return r.stdout
	}
	const grant = "authorization:\n  grants:\n    - actor: plugin:triage\n      roles: [desk]\n"
	s.write("cfg/heo/config.yaml", grant+"plugins:\n  - name: triage\n    command: sh\n    filter:\n      kinds: [message.received]\n")
	if out := validate(0); strings.Contains(out, `"severity": "error"`) {
		t.Errorf("granted plugin:\n%s", out)
	}
	for _, c := range []struct{ name, cfg, want string }{
		{"unknown kind", grant + "plugins:\n  - name: triage\n    command: sh\n    filter:\n      kinds: [message.nope]\n", "unknown event kind"},
		{"missing grant", "plugins:\n  - name: triage\n    command: sh\n", "no authorization.grants entry for plugin:triage"},
		{"command not found", grant + "plugins:\n  - name: triage\n    command: /nonexistent/heo-plugin\n", "command not found"},
		{"duplicate name", grant + "plugins:\n  - name: triage\n    command: sh\n  - name: triage\n    command: sh\n", "plugin names are unique"},
	} {
		s.write("cfg/heo/config.yaml", c.cfg)
		if out := validate(2); !strings.Contains(out, c.want) {
			t.Errorf("%s: want %q in\n%s", c.name, c.want, out)
		}
	}
}
