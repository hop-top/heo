package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
)

// cliSite is a sandbox on a fresh store, its credential backend holding
// a widget session key, configured and run through heo's own commands.
type cliSite struct {
	t    *testing.T
	db   *testDB
	s    *sandbox
	hook string
}

func newCLISite(t *testing.T, extra string) *cliSite {
	t.Helper()
	db := newDB(t).migrate()
	s := newSandbox(t)
	s.write("cfg/heo/config.yaml", fmt.Sprintf(
		"credentials:\n  backend: file\n  file:\n    dir: %q\nstorage:\n  postgres:\n    dsn: %q\n"+
			"services:\n  workers:\n    poll: 50ms\n%s",
		s.path("secrets"), db.dsn, extra,
	))
	key, err := json.Marshal(map[string]any{"v": 1, "kind": "secret", "value": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.write("secrets/uma/default/widget-key", string(key))
	return &cliSite{t: t, db: db, s: s, hook: freeAddr(t)}
}

// start runs the daemon with its webhook listener and the runtime
// services, less those disabled ("workers").
func (c *cliSite) start(disable ...string) *daemon {
	c.t.Helper()
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	want := []string{"socket", "receivers", "webhook"}
	if len(disable) > 0 {
		args = append(args, "--disable="+strings.Join(disable, ","))
	} else {
		want = append(want, "workers")
	}
	d := c.s.start(nil, nil, want, args...)
	c.t.Cleanup(func() {
		if c.t.Failed() {
			c.t.Logf("daemon log:\n%s", d.log())
		}
	})
	return d
}

// stop stops d, which must exit 0.
func (c *cliSite) stop(d *daemon) {
	c.t.Helper()
	if code := d.term(); code != 0 {
		c.t.Fatalf("SIGTERM exit %d:\n%s", code, d.log())
	}
}

// heo runs heo with args, requires exit, and returns the result.
func (c *cliSite) heo(exit int, args ...string) result {
	c.t.Helper()
	r := c.s.run(nil, args...)
	c.s.want(r, exit, args...)
	return r
}

// json runs heo with args and --format json, requires exit 0 and
// decodes stdout into v, a pointer.
func (c *cliSite) json(v any, args ...string) {
	c.t.Helper()
	r := c.heo(0, append(args, "--format", "json")...)
	// Decode into a fresh value: a map field reused across calls would
	// keep the previous answer's keys.
	rv := reflect.ValueOf(v).Elem()
	rv.Set(reflect.Zero(rv.Type()))
	if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
		c.t.Fatalf("heo %v: %v\n%s", args, err, r.stdout)
	}
}

type channelOutJSON struct {
	Channel   model.Channel
	Operation struct {
		ID       model.ID
		State    model.DeliveryState
		Replayed bool
	}
	OperationKey string `json:"operation_key"`
}

type inboxOutJSON struct {
	Inbox     model.Inbox
	Operation struct {
		State    model.DeliveryState
		Replayed bool
	}
	OperationKey string `json:"operation_key"`
}

type planJSON struct {
	Command string
	Args    map[string]json.RawMessage
	Effects []struct{ Kind, Target, Detail string }
}

type pageJSON[T any] struct {
	Items    []T
	Complete bool
}

func (c *cliSite) channels() []model.Channel {
	c.t.Helper()
	var p pageJSON[model.Channel]
	c.json(&p, "channel", "list")
	return p.Items
}

func (c *cliSite) inboxes() []model.Inbox {
	c.t.Helper()
	var p pageJSON[model.Inbox]
	c.json(&p, "inbox", "list")
	return p.Items
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return b.String()
}

// tableHas reports whether table holds want, however its columns are
// padded: a verb table is aligned to its longest name, which a new verb
// changes.
func tableHas(table, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(table), " "), strings.Join(strings.Fields(want), " "))
}

func wantStderr(t *testing.T, r result, code string, contains ...string) {
	t.Helper()
	if !strings.Contains(r.stderr, code) {
		t.Errorf("stderr lacks %s:\n%s", code, r.stderr)
	}
	for _, s := range contains {
		if !strings.Contains(r.stderr, s) {
			t.Errorf("stderr lacks %q:\n%s", s, r.stderr)
		}
	}
}

// TestAdapterCommandsOffline: the installed adapters, their maturity
// apart from their capabilities, read from the binary with no daemon.
func TestAdapterCommandsOffline(t *testing.T) {
	s := newSandbox(t)
	r := s.run(nil, "adapter", "list", "--format", "json")
	s.want(r, 0, "adapter", "list")
	var list []struct {
		Type                string
		Maturity            string
		Capabilities        []model.Capability
		ConfigurationSchema struct{ Name string } `json:"configuration_schema"`
		CredentialPurposes  []string              `json:"credential_purposes"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil {
		t.Fatalf("%v\n%s", err, r.stdout)
	}
	byType := map[string]int{}
	for i, a := range list {
		byType[a.Type] = i
	}
	i, ok := byType[string(widget.Type)]
	if !ok || len(list) < 8 {
		t.Fatalf("adapters = %+v", list)
	}
	if w := list[i]; w.Maturity != "experimental" || !slices.Contains(w.Capabilities, model.CapabilityReceiveHTTP) ||
		w.ConfigurationSchema.Name != widget.ConfigSchema || !slices.Equal(w.CredentialPurposes, []string{widget.CredentialPurpose}) {
		t.Errorf("widget = %+v", w)
	}
	r = s.run(nil, "adapter", "get", string(model.TypeSmtpMail))
	s.want(r, 0, "adapter", "get")
	for _, want := range []string{"maturity", "experimental", "credential purposes", "smtp.relay"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("adapter get lacks %q:\n%s", want, r.stdout)
		}
	}
	r = s.run(nil, "adapter", "get", "nope.adapter")
	s.want(r, 3, "adapter", "get", "nope.adapter")
}

// TestChannelAndInboxCommands drives the release binary on Postgres:
// channel and inbox writes go through the daemon, at a revision, under
// an operation key; a preview writes nothing; a stale revision and a
// bound destination conflict (exit 4); checks run locally and against
// the adapter; config validate checks the stored channels through the
// daemon, and says when it cannot.
func TestChannelAndInboxCommands(t *testing.T) {
	c := newCLISite(t, "")
	r := c.heo(70, "channel", "list")
	wantStderr(t, r, "PREREQUISITE", "service start")
	var v struct {
		Valid    bool
		Channels struct {
			Checked bool
			Count   int
			Skipped string
		}
	}
	c.json(&v, "config", "validate")
	if !v.Valid || v.Channels.Checked || !strings.Contains(v.Channels.Skipped, "the daemon owns: no heo daemon is running") {
		t.Fatalf("validate without a daemon = %+v", v)
	}

	d := c.start()
	create := []string{
		"channel", "create", "--type", string(widget.Type), "--key", "web",
		"--configuration", `{"sites":["site-1"]}`, "--credential-ref", "widget-key",
	}
	var plan planJSON
	c.json(&plan, append(create, "--dry-run")...)
	var previewed model.Channel
	if err := json.Unmarshal(plan.Args["channel"], &previewed); err != nil {
		t.Fatal(err)
	}
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "create" || plan.Effects[0].Target != "channel:web" ||
		previewed.Configuration.Name != widget.ConfigSchema || previewed.CredentialRef != "widget-key" {
		t.Fatalf("preview = %+v: %+v", plan, previewed)
	}
	if got := c.channels(); len(got) != 0 {
		t.Fatalf("--dry-run wrote %+v", got)
	}

	var made channelOutJSON
	c.json(&made, create...)
	if made.Channel.Key != "web" || made.Channel.Revision == "" || made.Operation.State != model.Committed ||
		!strings.HasPrefix(made.OperationKey, "op_") {
		t.Fatalf("create = %+v", made)
	}

	// A retry under the same key replays; another key conflicts.
	again := []string{
		"channel", "create", "--type", string(widget.Type), "--key", "web2",
		"--configuration", `{"sites":["site-2"]}`, "--credential-ref", "widget-key",
	}
	var first, retried channelOutJSON
	c.json(&first, append(again, "--idempotency-key", "k1")...)
	c.json(&retried, append(again, "--idempotency-key", "k1")...)
	if first.Operation.Replayed || !retried.Operation.Replayed || retried.Channel.Ref != first.Channel.Ref ||
		retried.OperationKey != "k1" {
		t.Fatalf("create %+v, then retried %+v", first, retried)
	}
	r = c.heo(4, append(again, "--idempotency-key", "k2")...)
	wantStderr(t, r, "CONFLICT", "channel.key")
	if got := c.channels(); len(got) != 2 {
		t.Fatalf("channels = %+v, want web and web2", got)
	}

	// A credential is refused as one, never stored.
	r = c.heo(2, "channel", "update", "web", "--revision", string(made.Channel.Revision),
		"--credential-ref", "ghp_1234567890abcdefghijABCDEFGHIJ123456")
	if strings.Contains(r.stderr, "ghp_1234567890") {
		t.Errorf("refusal echoes the credential:\n%s", r.stderr)
	}

	// Update at a stale revision: conflict, nothing written.
	r = c.heo(4, "channel", "update", "web", "--revision", "stale", "--configuration", `{"sites":["site-1","site-3"]}`)
	wantStderr(t, r, "CONFLICT", "channel web is at revision "+string(made.Channel.Revision))
	c.json(&plan, "channel", "update", "web", "--revision", string(made.Channel.Revision),
		"--configuration", `{"sites":["site-1","site-3"]}`, "--dry-run")
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || plan.Args["current"] == nil {
		t.Fatalf("update preview = %+v", plan)
	}
	var got model.Channel
	c.json(&got, "channel", "get", "web")
	if got.Revision != made.Channel.Revision || compactJSON(t, got.Configuration.Data) != `{"sites":["site-1"]}` {
		t.Fatalf("after stale update and preview: %+v", got)
	}
	var updated channelOutJSON
	c.json(&updated, "channel", "update", "web", "--revision", string(got.Revision), "--configuration", `{"sites":["site-1","site-3"]}`)
	if updated.Channel.Revision == got.Revision || compactJSON(t, updated.Channel.Configuration.Data) != `{"sites":["site-1","site-3"]}` {
		t.Fatalf("update = %+v", updated)
	}

	// Inboxes: one destination, one inbox.
	inboxArgs := []string{
		"inbox", "create", "--channel", "web", "--key", "webin", "--name", "Website", "--destination", "site-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1,
	}
	c.json(&plan, append(inboxArgs, "--dry-run")...)
	if len(plan.Effects) != 1 || plan.Effects[0].Target != "inbox:webin" || len(c.inboxes()) != 0 {
		t.Fatalf("inbox preview = %+v, or it wrote", plan)
	}
	var in inboxOutJSON
	c.json(&in, inboxArgs...)
	if in.Inbox.Key != "webin" || in.Inbox.Channel != made.Channel.Ref || in.Inbox.Disabled || in.Operation.State != model.Committed {
		t.Fatalf("inbox create = %+v", in)
	}
	r = c.heo(4, "inbox", "create", "--channel", "web", "--key", "dup", "--destination", "site-1", "--mode", model.ModePerRoom)
	wantStderr(t, r, "CONFLICT", `destination "site-1" already bound to inbox webin`)
	if got := c.inboxes(); len(got) != 1 {
		t.Fatalf("inboxes = %+v", got)
	}
	c.json(&plan, "inbox", "disable", "webin", "--revision", string(in.Inbox.Revision), "--dry-run")
	var still model.Inbox
	c.json(&still, "inbox", "get", "webin")
	if len(plan.Effects) != 1 || plan.Effects[0].Kind != "update" || still.Disabled || still.Revision != in.Inbox.Revision {
		t.Fatalf("disable preview = %+v, then %+v", plan, still)
	}

	// Checks: local, then the authenticated probe; neither writes.
	var checks struct {
		OK     bool
		Remote bool
		Checks []struct {
			Key      string
			OK       bool
			Findings []struct{ Severity, Scope, Code string }
			Remote   *struct {
				Capabilities []struct {
					Name      model.Capability
					Available bool
					Required  bool
				}
				Credentials []struct{ Purpose, Status, Key string }
			}
		}
	}
	c.json(&checks, "channel", "check", "web")
	if !checks.OK || len(checks.Checks) != 1 || checks.Checks[0].Remote != nil {
		t.Fatalf("local check = %+v", checks)
	}
	c.json(&checks, "channel", "check", "web", "--remote")
	rc := checks.Checks[0]
	if !checks.OK || rc.Remote == nil || len(rc.Remote.Credentials) != 1 ||
		rc.Remote.Credentials[0].Purpose != widget.CredentialPurpose || rc.Remote.Credentials[0].Status != "ok" {
		t.Fatalf("remote check = %+v", checks)
	}
	required := map[model.Capability]bool{}
	for _, cp := range rc.Remote.Capabilities {
		if cp.Required && cp.Available {
			required[cp.Name] = true
		}
	}
	if !required[model.CapabilityReceiveHTTP] || !required[model.CapabilitySend] {
		t.Errorf("remote capabilities = %+v", rc.Remote.Capabilities)
	}
	r = c.heo(2, "channel", "check", "web", "--remote", "--offline")
	wantStderr(t, r, "USAGE", "--offline")
	c.json(&checks, "channel", "check")
	if len(checks.Checks) != 2 {
		t.Fatalf("check all = %+v", checks)
	}

	c.json(&v, "config", "validate")
	if !v.Valid || !v.Channels.Checked || v.Channels.Count != 2 {
		t.Fatalf("validate with the daemon = %+v", v)
	}
	c.stop(d)
}

// TestInboxDisableRejectsInboundNotSends: a disabled inbox refuses what
// visitors send it, while a reply queued to its conversation still
// delivers once workers run; enabled again, it accepts inbound.
func TestInboxDisableRejectsInboundNotSends(t *testing.T) {
	c := newCLISite(t, "")
	d := c.start()
	var ch channelOutJSON
	c.json(&ch, "channel", "create", "--type", string(widget.Type), "--key", "web",
		"--configuration", `{"sites":["site-1"]}`, "--credential-ref", "widget-key")
	var in inboxOutJSON
	c.json(&in, "inbox", "create", "--channel", "web", "--key", "webin", "--destination", "site-1",
		"--mode", model.ModePerContact, "--lock-to-single=true", "--lifecycle-rules", model.RuleSetContactLifecycleV1)
	route := "http://" + c.hook + "/webhooks/" + string(ch.Channel.Ref.ID)
	site := &widgetSite{t: t, db: c.db, s: c.s, hook: c.hook, route: route}
	token, conv := site.visit()
	c.stop(d)

	// No workers: the reply stays queued while the inbox is disabled.
	d = c.start("workers")
	op := c.db.operator("default")
	queued := reply(t, op, conv, "queued-before-disable")
	var off inboxOutJSON
	c.json(&off, "inbox", "disable", "webin", "--revision", string(in.Inbox.Revision))
	if !off.Inbox.Disabled {
		t.Fatalf("disable = %+v", off)
	}
	r := c.heo(4, "inbox", "enable", "webin", "--revision", string(in.Inbox.Revision))
	wantStderr(t, r, "CONFLICT", "inbox webin is at revision")
	code, body := postJSON(t, route+"/messages", `{"id":"m-2","text":"while disabled"}`, "Authorization", "Bearer "+token)
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("visitor message to a disabled inbox = %d %s, want 422", code, body)
	}
	if st := operationNow(t, op, queued).State; st != model.Queued {
		t.Fatalf("reply = %s before workers run, want queued", st)
	}
	c.stop(d)

	d = c.start()
	events := widgetEvents(t, route, token, "")
	if ev := next(t, events); ev.name != widget.EventMessage || !strings.Contains(ev.data, "queued-before-disable") {
		t.Fatalf("visitor's event = %+v, want the reply queued before the inbox was disabled", ev)
	}
	if got := operationState(t, op, queued); got.State != model.ProviderAccepted && got.State != model.Delivered {
		t.Errorf("queued reply = %s %+v", got.State, got.Failure)
	}
	var on inboxOutJSON
	c.json(&on, "inbox", "enable", "webin", "--revision", string(off.Inbox.Revision))
	if on.Inbox.Disabled {
		t.Fatalf("enable = %+v", on)
	}
	if code, body := postJSON(t, route+"/messages", `{"id":"m-3","text":"enabled again"}`, "Authorization", "Bearer "+token); code != http.StatusAccepted {
		t.Fatalf("visitor message once enabled = %d %s", code, body)
	}
	c.stop(d)
}

// operationNow reads op's state once.
func operationNow(t *testing.T, c uma.Client, op model.OperationRef) model.Operation {
	t.Helper()
	got, err := c.Operations().Get(t.Context(), model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "e2e"}, Ref: op})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestChannelCommandsNeedTheirRole: a caller the policy does not make an
// operator reads inboxes but no channel, configures nothing and probes
// nothing (UNAUTHORIZED, exit 5, the policy's reason); a caller with no
// role reaches nothing.
func TestChannelCommandsNeedTheirRole(t *testing.T) {
	c := newCLISite(t, "authorization:\n  owner: [viewer]\n")
	ids := c.db.configure("default", model.Channel{
		Key: "web", Type: widget.Type, Configuration: widget.Configuration("site-1"), CredentialRef: "widget-key",
	})
	d := c.start()
	if got := c.inboxes(); len(got) != 0 {
		t.Fatalf("viewer inbox list = %+v", got)
	}
	for _, args := range [][]string{
		{"channel", "check", string(ids["web"]), "--remote"},
		{"channel", "list"},
		{"channel", "get", "web"},
		{"channel", "create", "--type", string(widget.Type), "--key", "web2", "--configuration", `{"sites":["site-2"]}`},
		{"channel", "create", "--type", string(widget.Type), "--key", "web2", "--configuration", `{"sites":["site-2"]}`, "--dry-run"},
		{"inbox", "create", "--channel", "web", "--destination", "site-1", "--mode", model.ModePerRoom},
	} {
		r := c.heo(5, args...)
		wantStderr(t, r, "UNAUTHORIZED", "heo.authz.not_granted")
		if r.stdout != "" {
			t.Errorf("heo %v printed %s", args, r.stdout)
		}
	}
	list, err := c.db.operator("default").Configuration().Channels(t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "e2e"}, Page: model.PageRequest{Limit: 10},
	})
	if err != nil || len(list.Items) != 1 {
		t.Fatalf("channels = %+v, %v; want only the one configured", list.Items, err)
	}
	c.stop(d)

	c.s.write("cfg/heo/profiles/default.yaml", "authorization:\n  owner: []\n")
	d = c.start()
	wantDenied(t, c.s.run(nil, "inbox", "list"), "heo.authz.no_role")
	c.stop(d)
}

// TestChannelCheckRemoteProbe: the e2e build's fake adapter probes its
// provider's permissions; a capability the provider denies, that the
// channel's inbox needs, fails the remote check (exit 2) after the
// report, which shows the capability unavailable and required.
func TestChannelCheckRemoteProbe(t *testing.T) {
	withE2EBinary(t, func() {
		c := newCLISite(t, "")
		d := c.start()
		c.heo(0, "channel", "create", "--type", "test.heo.fake", "--key", "fake",
			"--configuration", `{"account":"bot-1","denied":["send"]}`)
		c.heo(0, "inbox", "create", "--channel", "fake", "--key", "room", "--destination", "room-1", "--mode", model.ModePerRoom)
		c.heo(0, "channel", "check", "fake")
		r := c.heo(2, "channel", "check", "fake", "--remote", "--format", "json")
		wantStderr(t, r, "VALIDATION")
		var res struct {
			OK     bool
			Checks []struct {
				Findings []struct{ Severity, Scope, Code string }
				Remote   struct {
					Capabilities []struct {
						Name                string
						Available, Required bool
						Reason              string
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || len(res.Checks) != 1 {
			t.Fatalf("%v\n%s", err, r.stdout)
		}
		send := false
		for _, cp := range res.Checks[0].Remote.Capabilities {
			if cp.Name == string(model.CapabilitySend) {
				send = !cp.Available && cp.Required && cp.Reason == string(model.Unauthorized)
			}
		}
		if res.OK || !send {
			t.Fatalf("remote check = %+v", res)
		}
		r = c.heo(2, "channel", "check", "fake", "--remote")
		for _, want := range []string{"FAILED", "capability:send", "required"} {
			if !strings.Contains(r.stdout, want) {
				t.Errorf("report lacks %q:\n%s", want, r.stdout)
			}
		}
		c.stop(d)
	})
}
