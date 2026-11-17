package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/hooks"
	"hop.top/heo/internal/hooks/hookstest"
	"hop.top/uma/model"
)

// deadLog records the dead letters a host's hook runner was given.
type deadLog struct {
	mu   sync.Mutex
	list []hooks.DeadLetter
}

func (d *deadLog) Record(_ context.Context, l hooks.DeadLetter) error {
	d.mu.Lock()
	d.list = append(d.list, l)
	d.mu.Unlock()
	return nil
}

func (d *deadLog) all() []hooks.DeadLetter {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.list)
}

// applyLog records the results a host's hook runner applied.
type applyLog struct {
	mu  sync.Mutex
	got []hooks.Delivery
}

func (a *applyLog) Apply(_ context.Context, d hooks.Delivery, _ hooks.Result) error {
	a.mu.Lock()
	a.got = append(a.got, d)
	a.mu.Unlock()
	return nil
}

func (a *applyLog) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.got) }

// hooksRig is a bridge rig whose host also runs the hook service, with
// plugins on the fixture script.
type hooksRig struct {
	*bridgeRig
	cmd  string
	dead *deadLog
	app  *applyLog
}

func newHooksRig(t *testing.T, opts ...rigOption) *hooksRig {
	t.Helper()
	r := &hooksRig{bridgeRig: newBridgeRig(t, opts...), dead: &deadLog{}, app: &applyLog{}}
	r.cmd, _ = hookstest.Plugin(t)
	r.v.Set("services.hooks.enabled", true)
	r.host = daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
		Hooks:   daemon.HookOptions{Apply: r.app, Dead: r.dead},
	})
	return r
}

// plugins configures the plugins, each as the entry says, and grants
// each the roles named.
func (r *hooksRig) plugins(grants map[model.ActorID][]authz.Role, entries ...map[string]any) {
	var list []any
	for _, e := range entries {
		list = append(list, e)
	}
	r.v.Set("plugins", list)
	var g []any
	var gr []authz.Grant
	for actor, roles := range grants {
		var names []any
		for _, role := range roles {
			names = append(names, string(role))
		}
		g = append(g, map[string]any{"actor": string(actor), "roles": names})
		gr = append(gr, authz.Grant{Actor: actor, Roles: roles})
	}
	r.v.Set("authorization.grants", g)
	r.deps.Authorizer = authz.New(authz.Config{Owner: []authz.Role{authz.RoleDesk}, Grants: gr}, "acme", deskActor, daemon.ServiceActor)
}

// plugin is a config entry running the fixture in mode, logging to log.
func (r *hooksRig) plugin(name, log, mode string, extra map[string]any) map[string]any {
	e := map[string]any{"name": name, "command": r.cmd, "args": []any{log, mode}, "timeout": "20s",
		"retry": map[string]any{"max_attempts": 2, "backoff": map[string]any{"initial": "5ms", "max": "10ms"}}}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func (r *hooksRig) status(name string) (hooks.Status, bool) {
	for _, st := range r.host.Status().Hooks {
		if st.Plugin == name {
			return st, true
		}
	}
	return hooks.Status{}, false
}

func logPath(t *testing.T) string {
	t.Helper()
	_, log := hookstest.Plugin(t)
	return log
}

func TestHooksOffByDefault(t *testing.T) {
	if daemon.Defaults()["services.hooks.enabled"] != false {
		t.Errorf("services.hooks.enabled defaults to %v, want false", daemon.Defaults()["services.hooks.enabled"])
	}
	r := newBridgeRig(t)
	s := r.run(5*time.Second, daemon.NameBridge)
	s.ready(daemon.NameBridge)
	if st := r.host.Status(); len(st.Hooks) != 0 || service(st, daemon.NameHooks).State != daemon.StateNotStarted {
		t.Errorf("hooks ran without being started: %+v", st.Hooks)
	}
}

// TestHooksDeliverAsThePluginActor: each plugin reads the log as its own
// actor, so what it receives is what it is granted: a plugin with the
// desk role also gets the internal note, one without it does not. Each
// has a subscription of its own, named from its filter, with its own
// cursor.
func TestHooksDeliverAsThePluginActor(t *testing.T) {
	r := newHooksRig(t)
	pub, desk := logPath(t), logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{
		"plugin:pub":  {authz.RoleObserver},
		"plugin:desk": {authz.RoleObserver, authz.RoleDesk},
	},
		r.plugin("pub", pub, hookstest.ModeOK, map[string]any{"filter": map[string]any{"kinds": []any{model.EventMessageCreated}}}),
		r.plugin("desk", desk, hookstest.ModeOK, map[string]any{"filter": map[string]any{"kinds": []any{model.EventMessageCreated}}}))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)

	conv := r.start("a", "hello")
	r.note(conv, "a", "internal")
	eventually(t, "the desk plugin handled the reply and the note", func() bool { return len(hookstest.Seen(desk)) == 2 })
	eventually(t, "the public plugin handled the reply", func() bool { return len(hookstest.Seen(pub)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if len(hookstest.Seen(pub)) != 1 {
		t.Errorf("the plugin without desk saw %v: it must not see the note", hookstest.Seen(pub))
	}
	if r.app.count() != 3 {
		t.Errorf("applied %d results, want 3", r.app.count())
	}

	st, ok := r.status("desk")
	if !ok {
		t.Fatalf("no status for desk: %+v", r.host.Status().Hooks)
	}
	other, _ := r.status("pub")
	if st.Consumer == other.Consumer || st.Delivered != 2 || st.Cursor == "" || st.State != hooks.StateReady || st.LastError != nil {
		t.Errorf("desk status %+v, pub status %+v", st, other)
	}
	if sc, ok := r.hookScope("desk"); !ok || sc.State != daemon.ScopeReady {
		t.Errorf("scope %+v %v", sc, ok)
	}
}

// TestHooksCursorSurvivesRestart: a stopped service resumes at its
// checkpoint: an event handled before the stop is not handled again, one
// committed while it was down is.
func TestHooksCursorSurvivesRestart(t *testing.T) {
	r := newHooksRig(t)
	log := logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:p": {authz.RoleObserver}},
		r.plugin("p", log, hookstest.ModeOK, nil))
	// Another service holds the store open while the plugins are down.
	hold := r.run(5*time.Second, daemon.NameBridge)
	hold.ready(daemon.NameBridge)
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "one")
	eventually(t, "first handled", func() bool { return len(hookstest.Seen(log)) >= 1 })
	eventually(t, "cursor moved", func() bool { st, _ := r.status("p"); return st.Cursor != "" && st.InFlight == 0 })
	s.stop()
	handled := len(hookstest.Seen(log))

	r.start("b", "two")
	s2 := r.run(5*time.Second, daemon.NameHooks)
	s2.ready(daemon.NameHooks)
	eventually(t, "the event committed while down is handled", func() bool { return len(hookstest.Seen(log)) > handled })
	time.Sleep(100 * time.Millisecond)
	seen := hookstest.Seen(log)
	for i, id := range seen {
		if slices.Index(seen, id) != i {
			t.Errorf("event %s handled twice: %v", id, seen)
		}
	}
}

// TestHooksDeadLetterAndStatus: a plugin that keeps failing is recorded
// as a dead letter once its attempts are spent, the stream moves on, and
// the status says what failed.
func TestHooksDeadLetterAndStatus(t *testing.T) {
	r := newHooksRig(t)
	bad, good := logPath(t), logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:bad": {authz.RoleObserver}, "plugin:good": {authz.RoleObserver}},
		r.plugin("bad", bad, hookstest.ModeExit1, nil), r.plugin("good", good, hookstest.ModeOK, nil))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "hi")
	eventually(t, "the failing plugin's event dead-lettered", func() bool { return len(r.dead.all()) == 1 })
	dl := r.dead.all()[0]
	if dl.Plugin != "bad" || dl.Attempts != 2 || dl.Permanent || dl.Event.Kind == "" {
		t.Errorf("dead letter %+v", dl)
	}
	eventually(t, "cursor moved past it", func() bool {
		st, _ := r.status("bad")
		return st.DeadLettered >= 1 && st.InFlight == 0 && st.Cursor != ""
	})
	st, _ := r.status("bad")
	if st.LastError == nil || st.LastError.Event == "" || st.LastError.Message != "exit 1" {
		t.Errorf("status %+v", st)
	}
	if g, _ := r.status("good"); g.DeadLettered != 0 || g.LastError != nil || g.Delivered == 0 {
		t.Errorf("the healthy plugin was touched: %+v", g)
	}
}

// TestHooksDeadLettersAreDurableAndReplayable: with a store, a failing
// plugin's event is listed as a dead letter with its class, health
// events go out on the bus, and replay and discard run through the
// ledger the host exposes.
func TestHooksDeadLettersAreDurableAndReplayable(t *testing.T) {
	r := newHooksRig(t)
	r.host = daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
		Hooks:   daemon.HookOptions{Apply: r.app, DeadDir: t.TempDir()},
	})
	health := listen(t, r.hub, bridge.ScopeDesk)
	bad := logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:bad": {authz.RoleObserver}}, r.plugin("bad", bad, hookstest.ModeExit3, nil))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "hi")
	var led *hooks.Ledger
	eventually(t, "a dead letter listed", func() bool {
		led = r.host.Ledger()
		if led == nil {
			return false
		}
		l, _ := led.List(context.Background(), "bad", hooks.StateDead)
		return len(l) == 1
	})
	l, _ := led.List(context.Background(), "bad", hooks.StateDead)
	if l[0].Attempts != 2 || l[0].Class != hooks.ClassCrash || l[0].Event == "" {
		t.Errorf("record %+v", l[0])
	}
	eventually(t, "health events on the bus", func() bool {
		return health.count("heo.runtime.hooks.dead_lettered") == 1 && health.count("heo.runtime.hooks.retry") == 1
	})
	if _, err := led.Discard(context.Background(), "bad", l[0].Event); err != nil {
		t.Fatal(err)
	}
	if d, _ := led.List(context.Background(), "bad", hooks.StateDead); len(d) != 0 {
		t.Errorf("still dead: %+v", d)
	}
}

// TestHooksSlowPluginDoesNotSlowTheRest: a plugin that never answers
// holds nobody: another plugin and the bridge carry on with every event,
// and the daemon stops on time.
func TestHooksSlowPluginDoesNotSlowTheRest(t *testing.T) {
	r := newHooksRig(t)
	slow, fast := logPath(t), logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:slow": {authz.RoleObserver}, "plugin:fast": {authz.RoleObserver}},
		r.plugin("slow", slow, hookstest.ModeSlow, nil), r.plugin("fast", fast, hookstest.ModeOK, nil))
	busLog := listen(t, r.hub, bridge.ScopeDesk)
	s := r.run(5*time.Second, daemon.NameBridge, daemon.NameWorkers, daemon.NameHooks)
	s.ready(daemon.NameBridge, daemon.NameWorkers, daemon.NameHooks)

	r.start("a", "one")
	eventually(t, "the slow plugin has the event", func() bool { return len(hookstest.Seen(slow)) == 1 })
	r.start("b", "two")
	r.start("c", "three")
	eventually(t, "the fast plugin handled every event while the slow one hung", func() bool {
		st, _ := r.status("fast")
		return st.Delivered >= 3
	})
	eventually(t, "the bridge published them too", func() bool { return busLog.count("uma.public.message.created") >= 3 })
	if st, _ := r.status("slow"); st.Delivered != 0 || st.InFlight == 0 {
		t.Errorf("slow status %+v", st)
	}
	start := time.Now()
	if res := s.stop(); res.Err != nil {
		t.Errorf("stop: %v", res.Err)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("stop took %s with a plugin hung", time.Since(start))
	}
}

func (r *hooksRig) hookScope(name string) (daemon.ScopeStatus, bool) {
	for _, sc := range service(r.host.Status(), daemon.NameHooks).Scopes {
		if sc.Scope == daemon.HooksScopePrefix+name {
			return sc, true
		}
	}
	return daemon.ScopeStatus{}, false
}

// TestHooksFilters: a plugin's inboxes and channels, keys or IDs, narrow
// what it receives, and a filter that names nothing leaves that plugin
// waiting while the others run.
func TestHooksFilters(t *testing.T) {
	r := newHooksRig(t)
	byInbox, byChannel, other, missing := logPath(t), logPath(t), logPath(t), logPath(t)
	grants := map[model.ActorID][]authz.Role{}
	for _, n := range []string{"inbox", "channel", "other", "missing"} {
		grants[model.ActorID("plugin:"+n)] = []authz.Role{authz.RoleObserver}
	}
	r.plugins(grants,
		r.plugin("inbox", byInbox, hookstest.ModeOK, map[string]any{"filter": map[string]any{"inboxes": []any{string(r.inbox.ID)}}}),
		r.plugin("channel", byChannel, hookstest.ModeOK, map[string]any{"filter": map[string]any{"channels": []any{"out"}}}),
		r.plugin("other", other, hookstest.ModeOK, map[string]any{"filter": map[string]any{"channels": []any{"elsewhere"}}}),
		r.plugin("missing", missing, hookstest.ModeOK, map[string]any{"filter": map[string]any{"inboxes": []any{string(r.inbox.ID), "no-such-inbox"}}}))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	eventually(t, "the plugin naming an unknown inbox is waiting", func() bool {
		sc, ok := r.hookScope("missing")
		return ok && sc.State == daemon.ScopeWaiting
	})
	r.start("a", "hello")
	defer func() {
		for _, h := range r.host.Status().Hooks {
			t.Logf("%s %s %v", h.Plugin, h.State, h.LastError)
		}
	}()
	eventually(t, "inbox filter delivers", func() bool { return len(hookstest.Seen(byInbox)) >= 1 })
	eventually(t, "channel filter delivers", func() bool { return len(hookstest.Seen(byChannel)) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if got := hookstest.Seen(other); len(got) != 0 {
		t.Errorf("the plugin filtering on another channel received %v", got)
	}
	if got := hookstest.Seen(missing); len(got) != 0 {
		t.Errorf("the plugin that cannot open received %v", got)
	}
	if sc, _ := r.hookScope("inbox"); sc.State != daemon.ScopeReady {
		t.Errorf("scope %+v", sc)
	}
}

// TestHooksPluginLaunch: a plugin gets its own token, read from the
// credential source, the surface address and a working directory of its
// own under the plugins directory; another plugin's token is not in its
// environment.
func TestHooksPluginLaunch(t *testing.T) {
	r := newHooksRig(t)
	dir := t.TempDir()
	r.deps.PluginsDir, r.deps.PluginURL = dir, "http://127.0.0.1:8765"
	r.deps.PluginToken = func(_ context.Context, name string) (string, bool, error) {
		return name + ".secret-of-" + name, true, nil
	}
	a, b := logPath(t), logPath(t)
	kinds := map[string]any{"filter": map[string]any{"kinds": []any{model.EventMessageCreated}}}
	r.plugins(map[model.ActorID][]authz.Role{"plugin:a": {authz.RoleObserver}, "plugin:b": {authz.RoleObserver}},
		r.plugin("a", a, hookstest.ModeEnv, kinds), r.plugin("b", b, hookstest.ModeEnv, kinds))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "hello")
	for _, log := range []string{a, b} {
		eventually(t, "the plugin ran", func() bool {
			b, _ := os.ReadFile(filepath.Clean(log + ".pwd")) // written after the environment
			return len(b) > 0
		})
	}
	for name, log := range map[string]string{"a": a, "b": b} {
		raw, err := os.ReadFile(filepath.Clean(log + ".env"))
		if err != nil {
			t.Fatal(err)
		}
		env := string(raw)
		for _, want := range []string{"HEO_PLUGIN_NAME=" + name, "HEO_PLUGIN_TOKEN=" + name + ".secret-of-" + name, "HEO_URL=http://127.0.0.1:8765"} {
			if !strings.Contains(env, want+"\n") {
				t.Errorf("%s: environment lacks %s:\n%s", name, want, env)
			}
		}
		if other := map[string]string{"a": "b", "b": "a"}[name]; strings.Contains(env, "secret-of-"+other) {
			t.Errorf("%s sees %s's token", name, other)
		}
		pwd, _ := os.ReadFile(filepath.Clean(log + ".pwd"))
		want, _ := filepath.EvalSymlinks(filepath.Join(dir, name))
		if strings.TrimSpace(string(pwd)) != want {
			t.Errorf("%s ran in %q, want %q", name, pwd, want)
		}
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s directory: %v %v", name, fi, err)
		}
	}
}

// TestHooksPluginWithoutTokenIsNotStarted: with a credential source
// wired, a plugin that has no token minted is refused with the fix
// named; it reads nothing, and the plugin that has one runs.
func TestHooksPluginWithoutTokenIsNotStarted(t *testing.T) {
	r := newHooksRig(t)
	r.deps.PluginToken = func(_ context.Context, name string) (string, bool, error) {
		return "ok.x", name == "ok", nil
	}
	ok, none := logPath(t), logPath(t)
	kinds := map[string]any{"filter": map[string]any{"kinds": []any{model.EventMessageCreated}}}
	r.plugins(map[model.ActorID][]authz.Role{"plugin:ok": {authz.RoleObserver}, "plugin:none": {authz.RoleObserver}},
		r.plugin("ok", ok, hookstest.ModeOK, kinds), r.plugin("none", none, hookstest.ModeOK, kinds))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "hello")
	eventually(t, "the plugin with a token ran", func() bool { return len(hookstest.Seen(ok)) == 1 })
	sc, found := r.hookScope("none")
	if !found || sc.State != daemon.ScopeFailed || !strings.Contains(sc.Reason, "heo plugin token mint none") {
		t.Errorf("scope %+v %v", sc, found)
	}
	if _, running := r.status("none"); running || len(hookstest.Seen(none)) != 0 {
		t.Error("a plugin with no token was started")
	}
}

// TestHooksLedgerTestsAndReplaysFromAPosition: on a real store, a replay
// from the beginning delivers the log to the plugin again as the
// plugin's actor, one from the plugin's own cursor finds nothing after
// it, and a test run answers on a stored event and applies nothing.
func TestHooksLedgerTestsAndReplaysFromAPosition(t *testing.T) {
	r := newHooksRig(t)
	r.host = daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
		Hooks:   daemon.HookOptions{Apply: r.app, DeadDir: t.TempDir()},
	})
	log := logPath(t)
	r.plugins(map[model.ActorID][]authz.Role{"plugin:p": {authz.RoleObserver}},
		r.plugin("p", log, hookstest.ModeOK, map[string]any{"filter": map[string]any{"kinds": []any{model.EventMessageCreated}}}))
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "hello")
	eventually(t, "delivered and acknowledged", func() bool {
		st, _ := r.status("p")
		return r.app.count() == 1 && st.Cursor != "" && st.InFlight == 0
	})
	led := r.host.Ledger()
	ctx := context.Background()

	// From its own cursor there is nothing left to deliver.
	st, _ := r.status("p")
	rep, err := led.ReplayFrom(ctx, "p", string(st.Cursor), 10, false)
	if err != nil || rep.Delivered != 0 || r.app.count() != 1 {
		t.Fatalf("from the cursor: %+v %v (applied %d)", rep, err, r.app.count())
	}

	// A dry run from the beginning lists the event and runs nothing.
	dry, err := led.ReplayFrom(ctx, "p", hooks.FromBeginning, 10, true)
	if err != nil || len(dry.Events) == 0 || dry.Events[0].Outcome != hooks.ReplayWould || r.app.count() != 1 {
		t.Fatalf("dry run: %+v %v (applied %d)", dry, err, r.app.count())
	}

	// A test run answers and applies nothing.
	id := dry.Events[0].Event
	tr, err := led.Test(ctx, "p", id)
	if err != nil || !tr.Admitted || !tr.Ok || tr.Applied || len(tr.Operations) != 1 || r.app.count() != 1 {
		t.Fatalf("test: %+v %v (applied %d)", tr, err, r.app.count())
	}

	// From the beginning it is delivered again, as the plugin.
	rep, err = led.ReplayFrom(ctx, "p", hooks.FromBeginning, 10, false)
	if err != nil || rep.Delivered != len(dry.Events) || rep.Failed != 0 || r.app.count() != 1+rep.Delivered {
		t.Fatalf("from the beginning: %+v %v (applied %d)", rep, err, r.app.count())
	}
	if _, err := led.ReplayFrom(ctx, "nope", hooks.FromBeginning, 10, false); err == nil {
		t.Error("replayed a plugin that is not running")
	}
}

// TestHooksFilterChangeHandsOverWithoutLoss: a plugin whose filter changes
// while it is down opens its new consumer at the old one's checkpoint, so
// the events committed in between are delivered; the old consumer is
// closed and the record of it cleared.
func TestHooksFilterChangeHandsOverWithoutLoss(t *testing.T) {
	r := newHooksRig(t)
	dir := t.TempDir()
	r.host = daemon.New(daemon.Options{
		Viper:   func() *viper.Viper { return r.v },
		Resolve: func(context.Context) (daemon.Deps, error) { return r.deps, nil },
		Hub:     r.hub,
		Hooks:   daemon.HookOptions{Apply: r.app, Dead: r.dead, DeadDir: dir},
	})
	log := logPath(t)
	grants := map[model.ActorID][]authz.Role{"plugin:p": {authz.RoleObserver}}
	r.plugins(grants, r.plugin("p", log, hookstest.ModeOK, map[string]any{"filter": map[string]any{"kinds": []any{"message.created"}}}))
	hold := r.run(5*time.Second, daemon.NameBridge)
	hold.ready(daemon.NameBridge)
	s := r.run(5*time.Second, daemon.NameHooks)
	s.ready(daemon.NameHooks)
	r.start("a", "one")
	eventually(t, "first handled", func() bool { return len(hookstest.Seen(log)) >= 1 })
	eventually(t, "cursor moved", func() bool { st, _ := r.status("p"); return st.Cursor != "" && st.InFlight == 0 })
	s.stop()
	per := len(hookstest.Seen(log))

	r.start("b", "two")
	r.start("c", "three")
	r.plugins(grants, r.plugin("p", log, hookstest.ModeOK, map[string]any{"filter": map[string]any{"kinds": []any{"message.created", "conversation.changed"}}}))
	s2 := r.run(5*time.Second, daemon.NameHooks)
	s2.ready(daemon.NameHooks)
	eventually(t, "the events committed across the change are delivered", func() bool {
		seen := map[string]bool{}
		for _, id := range hookstest.Seen(log) {
			seen[id] = true
		}
		return len(seen) >= 3*per
	})
	eventually(t, "the old consumer is closed and forgotten", func() bool {
		l, err := hooks.NewFileStore(dir).LoadLineage(context.Background(), "p")
		return err == nil && len(l.Retiring) == 0 && len(l.Active) == 1
	})
}
