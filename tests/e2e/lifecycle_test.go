package e2e

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// socketCall sends one command over a daemon's heo socket and returns
// the response envelope.
func socketCall(t *testing.T, sock string, path, args []string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if args == nil {
		args = []string{}
	}
	req, _ := json.Marshal(map[string]any{"path": path, "args": args, "flags": map[string]any{}})
	if _, err := conn.Write(append(req, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err, string(line))
	}
	return resp
}

// serviceStatus is `service status --format json`.
type serviceStatus struct {
	Services []struct {
		Service string `json:"service"`
		Ready   bool   `json:"ready"`
		Addr    string `json:"addr"`
		PID     int    `json:"pid"`
	} `json:"services"`
	Sections []struct {
		Title  string         `json:"title"`
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	} `json:"sections"`
}

func (s *sandbox) serviceStatus(pre ...string) serviceStatus {
	s.t.Helper()
	args := append(append([]string{}, pre...), "service", "status", "--format", "json")
	r := s.run(nil, args...)
	s.want(r, 0, args...)
	var st serviceStatus
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil {
		s.t.Fatal(err, r.stdout)
	}
	return st
}

// section returns the data of the named section.
func (st serviceStatus) section(t *testing.T, title string) (string, map[string]any) {
	t.Helper()
	for _, sec := range st.Sections {
		if sec.Title == title {
			return sec.Status, sec.Data
		}
	}
	t.Fatalf("service status has no %s section: %+v", title, st.Sections)
	return "", nil
}

// TestServiceGroupSurface: kit owns the `service` group; heo adds
// `service setup`, and `daemon ping` in its own `daemon` group, since
// the socket withholds kit's group; root `status` is kit's tool
// status, visible; `serve` is kit's hidden, deprecated alias; kit's
// `token` verbs are hidden (setup owns heo's token).
func TestServiceGroupSurface(t *testing.T) {
	s := newSandbox(t)
	help := s.run(nil, "--help")
	s.want(help, 0, "--help")
	for _, hidden := range []string{"    serve", "    token"} {
		if strings.Contains(help.stdout, hidden) {
			t.Errorf("root help lists %s:\n%s", strings.TrimSpace(hidden), help.stdout)
		}
	}
	for _, word := range []string{"service [command]", "daemon [command]", "status "} {
		if !strings.Contains(help.stdout, word) {
			t.Errorf("root help lacks %q:\n%s", word, help.stdout)
		}
	}
	sh := s.run(nil, "service", "--help")
	s.want(sh, 0, "service", "--help")
	for _, leaf := range []string{"start", "list", "stop", "restart", "status", "setup"} {
		if !strings.Contains(sh.stdout, "    "+leaf+" ") {
			t.Errorf("service help lacks %s:\n%s", leaf, sh.stdout)
		}
	}
	if strings.Contains(sh.stdout, "    ping ") {
		t.Errorf("service help lists ping:\n%s", sh.stdout)
	}
	dh := s.run(nil, "daemon", "--help")
	s.want(dh, 0, "daemon", "--help")
	if !strings.Contains(dh.stdout, "    ping ") {
		t.Errorf("daemon help lacks ping:\n%s", dh.stdout)
	}
	st := s.run(nil, "service", "start", "--help")
	s.want(st, 0, "service", "start", "--help")
	if !strings.Contains(st.stdout, "workers") || !strings.Contains(st.stdout, "receivers") {
		t.Errorf("start help lacks the runtime services:\n%s", st.stdout)
	}

	// Root status: the tool status with heo's profile section, no
	// daemon needed.
	r := s.run(nil, "status", "--format", "json")
	s.want(r, 0, "status")
	if !strings.Contains(r.stdout, `"heo-profile"`) || !strings.Contains(r.stdout, "heo://profile/default") {
		t.Errorf("root status lacks heo-profile:\n%s", r.stdout)
	}

	// No daemon: status is NOT_FOUND (exit 3), stop is a no-op
	// success, restart has nothing to restart (exit 3).
	r = s.run(nil, "service", "status", "--format", "json")
	s.want(r, 3, "service", "status")
	if !strings.Contains(r.stderr, `"code": "NOT_FOUND"`) {
		t.Errorf("service status stderr = %q", r.stderr)
	}
	r = s.run(nil, "service", "stop", "--format", "json")
	s.want(r, 0, "service", "stop")
	if !strings.Contains(r.stdout, `"not_running"`) {
		t.Errorf("service stop = %q", r.stdout)
	}
	s.want(s.run(nil, "service", "restart"), 3, "service", "restart")

	// serve: hidden, deprecated, and the same command.
	r = s.run(nil, "serve", "--list", "--format", "json")
	s.want(r, 0, "serve", "--list")
	if !strings.Contains(r.stdout, `"workers"`) {
		t.Errorf("serve --list = %q", r.stdout)
	}
	s.want(s.run(nil, "service", "bogus"), 2, "service", "bogus")
}

// TestServiceControlPerProfile: each profile's daemon has a control
// socket of its own. status, stop and restart reach the selected
// profile's daemon only; a second daemon for one profile is refused
// before anything starts; restart keeps the pid and re-reads the
// configuration.
func TestServiceControlPerProfile(t *testing.T) {
	s := newSandbox(t)
	s.write("cfg/heo/profiles/work.yaml", "runtime:\n  tenant: work\n")
	def := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
	work := s.start(nil, nil, []string{"socket"}, "--profile", "work", "service", "start", noRuntime)
	for _, f := range []string{"control-default.sock", "control-work.sock"} {
		if fi, err := os.Stat(s.path("run", "heo", f)); err != nil || fi.Mode()&os.ModeSocket == 0 {
			t.Errorf("control socket %s: %v", f, err)
		}
	}

	pid := func(d *daemon) int { return d.cmd.Process.Pid }
	for _, c := range []struct {
		pre    []string
		d      *daemon
		tenant string
	}{{nil, def, "default"}, {[]string{"--profile", "work"}, work, "work"}} {
		st := s.serviceStatus(c.pre...)
		if len(st.Services) != 1 || st.Services[0].Service != "socket" || st.Services[0].PID != pid(c.d) {
			t.Errorf("%v: services = %+v, want socket of pid %d", c.pre, st.Services, pid(c.d))
		}
		status, data := st.section(t, "heo")
		if status != "ok" || data["tenant"] != c.tenant || data["pid"] != float64(pid(c.d)) {
			t.Errorf("%v: heo section = %s %v", c.pre, status, data)
		}
		for _, sec := range []string{"storage", "workers", "receivers", "webhook"} {
			if status, _ := st.section(t, sec); status != "empty" {
				t.Errorf("%v: %s = %s, want empty (not started)", c.pre, sec, status)
			}
		}
	}

	// One daemon per profile: CONFLICT, exit 4, before any service
	// starts or its socket is touched.
	args := []string{"service", "start", noRuntime, "--format", "json"}
	r := s.run(nil, args...)
	s.want(r, 4, args...)
	if !strings.Contains(r.stderr, `"code": "CONFLICT"`) || strings.Contains(r.stderr, "ready_reported") {
		t.Errorf("second start: %q", r.stderr)
	}
	if resp := socketCall(t, def.addr("socket"), []string{"config", "get"}, []string{"runtime.tenant"}); resp["ok"] != true {
		t.Errorf("the first daemon stopped answering: %v", resp)
	}

	// Restart work in place: same pid, configuration re-read.
	s.write("cfg/heo/profiles/work.yaml", "runtime:\n  tenant: work2\n")
	r = s.run(nil, "--profile", "work", "service", "restart", "--format", "json")
	s.want(r, 0, "service", "restart")
	st := s.serviceStatus("--profile", "work")
	if _, data := st.section(t, "heo"); data["tenant"] != "work2" || data["pid"] != float64(pid(work)) {
		t.Errorf("after restart: heo section = %v, want tenant work2, pid %d", data, pid(work))
	}
	if !strings.Contains(work.log(), "restarting") {
		t.Errorf("work did not restart:\n%s", work.log())
	}
	if _, data := s.serviceStatus().section(t, "heo"); data["tenant"] != "default" {
		t.Errorf("restarting work touched default: %v", data)
	}

	// Stop work: it drains and exits 0; default keeps running. stop
	// asks the daemon over its control socket, not through the
	// pidfile: it reaches the daemon with the pidfile gone.
	if err := os.Remove(s.path("run", "heo", "work", "heo.pid")); err != nil {
		t.Fatal(err)
	}
	r = s.run(nil, "--profile", "work", "service", "stop", "--format", "json")
	s.want(r, 0, "service", "stop")
	if !strings.Contains(r.stdout, `"stopped"`) {
		t.Errorf("stop work = %q", r.stdout)
	}
	if code := work.wait(); code != 0 {
		t.Fatalf("work exit %d:\n%s", code, work.log())
	}
	if !strings.Contains(work.log(), "reason=clean-stop") {
		t.Errorf("work did not drain:\n%s", work.log())
	}
	s.want(s.run(nil, "--profile", "work", "service", "status"), 3, "--profile", "work", "service", "status")
	if def.isDone() {
		t.Fatalf("stopping work stopped default:\n%s", def.log())
	}
	b, err := os.ReadFile(s.path("run", "heo", "default", "heo.pid"))
	if err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(pid(def)) {
		t.Errorf("default pidfile = %q (%v)", b, err)
	}
	r = s.run(nil, "service", "stop")
	s.want(r, 0, "service", "stop")
	if code := def.wait(); code != 0 {
		t.Fatalf("default exit %d:\n%s", code, def.log())
	}
}

// TestServiceStartPolicyReachesServedCalls: --policy on `service start`
// is the policy served calls run under; the start itself is not gated
// by it.
func TestServiceStartPolicyReachesServedCalls(t *testing.T) {
	s := newSandbox(t)
	s.write("cfg/heo/policies/deny-writes.yaml", "name: deny-writes\nallow:\n  write: []\n  destructive: []\n")
	set := []string{"config", "set"}

	d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime, "--policy", "deny-writes")
	resp := socketCall(t, d.addr("socket"), set, []string{"runtime.tenant", "served"})
	if resp["ok"] != false || !strings.Contains(toJSON(resp), "policy: write not allowed") {
		t.Errorf("served write under deny-writes: %v", resp)
	}
	if resp := socketCall(t, d.addr("socket"), []string{"config", "get"}, []string{"runtime.tenant"}); resp["ok"] != true {
		t.Errorf("served read under deny-writes: %v", resp)
	}
	s.want(result{exit: d.term()}, 0)

	// The same write without --policy runs.
	d = s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
	if resp := socketCall(t, d.addr("socket"), set, []string{"runtime.tenant", "served"}); resp["ok"] != true {
		t.Errorf("served write without a policy: %v", resp)
	}
	s.want(result{exit: d.term()}, 0)
}

// TestServiceConfigOwnSourcesOnly: the daemon reads services.* from
// heo's own configuration only. A kit project file in the working
// directory, of any of kit's three names, changes nothing; the profile
// file beats the user file for the daemon as it does for `config get`.
func TestServiceConfigOwnSourcesOnly(t *testing.T) {
	for _, name := range []string{"kit.yaml", ".kit.yaml", ".kit/config.yaml"} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			stray := s.path("stray.sock")
			s.write("cwd/"+name, "services:\n  socket:\n    path: "+stray+"\n  api:\n    enabled: true\n")
			want := s.path("run", "heo", "default", "heo.sock")
			s.agrees(nil, "services.socket.path", want)
			s.agrees(nil, "services.api.enabled", "false")
			d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
			if got := d.addr("socket"); got != want {
				t.Errorf("socket at %s, want %s", got, want)
			}
			if d.isReady("api") {
				t.Error("a cwd kit file enabled api")
			}
			s.want(result{exit: d.term()}, 0)
		})
	}
	t.Run("profile-beats-user", func(t *testing.T) {
		s := newSandbox(t)
		user, prof := s.path("user.sock"), s.path("profile.sock")
		s.write("cfg/heo/config.yaml", "services:\n  socket:\n    path: "+user+"\n")
		s.write("cfg/heo/profiles/work.yaml", "services:\n  socket:\n    path: "+prof+"\n")
		pre := []string{"--profile", "work"}
		s.agrees(pre, "services.socket.path", prof)
		d := s.start(nil, nil, []string{"socket"}, append(pre, "service", "start", noRuntime)...)
		if got := d.addr("socket"); got != prof {
			t.Errorf("socket at %s, want the profile's %s", got, prof)
		}
		s.want(result{exit: d.term()}, 0)
	})
}

// agrees checks that `config get key` reports want.
func (s *sandbox) agrees(pre []string, key, want string) {
	s.t.Helper()
	r := s.get(nil, pre, key)
	s.want(r, 0, "config", "get", key)
	if got := strings.TrimSpace(r.stdout); got != want {
		s.t.Errorf("config get %s = %q, want %q", key, got, want)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// isDone reports whether the daemon has exited, after a short grace.
func (d *daemon) isDone() bool {
	select {
	case <-d.done:
		return true
	case <-time.After(200 * time.Millisecond):
		return false
	}
}

// TestServiceStatusRuntimeSections: with the runtime up, `service
// status` reports the daemon's data store and each runtime service, as
// the daemon sees them, below kit's per-service rows.
func TestServiceStatusRuntimeSections(t *testing.T) {
	site := newCLISite(t, "")
	d := site.start()
	defer site.stop(d)
	st := site.s.serviceStatus()
	names := map[string]bool{}
	for _, row := range st.Services {
		names[row.Service] = row.Ready
	}
	for _, svc := range []string{"socket", "workers", "receivers", "webhook"} {
		if !names[svc] {
			t.Errorf("service %s not ready in %+v", svc, st.Services)
		}
	}
	status, data := st.section(t, "storage")
	if status != "ok" || data["state"] != "ready" || data["report"] == nil {
		t.Errorf("storage section = %s %v", status, data)
	}
	for _, svc := range []string{"workers", "receivers", "webhook"} {
		status, data := st.section(t, svc)
		if status != "ok" || (data["state"] != "ready" && data["state"] != "degraded") {
			t.Errorf("%s section = %s %v", svc, status, data)
		}
	}
	if _, data := st.section(t, "webhook"); data["address"] != site.hook {
		t.Errorf("webhook address = %v, want %s", data["address"], site.hook)
	}
	// Who owns the store: this daemon, by pid, since it opened it.
	pid := float64(d.cmd.Process.Pid)
	status, data = st.section(t, "store-owner")
	if status != "ok" || data["owner"] != "heo/default" || data["pid"] != pid || data["since"] == nil {
		t.Errorf("store-owner section = %s %v, want heo/default pid %v", status, data, pid)
	}
	// The operation queue, empty on a fresh store.
	status, data = st.section(t, "queue")
	if status != "ok" || data["kind"] != "operation.execute" || data["waiting"] != 0.0 ||
		data["in_flight"] != 0.0 || data["dead_lettered"] != 0.0 {
		t.Errorf("queue section = %s %v", status, data)
	}
	// The versions the daemon runs and its store's schema.
	status, data = st.section(t, "versions")
	if status != "ok" || data["compat"] != "compatible" || data["schema"] == nil || data["schema"] != data["library_schema"] ||
		!strings.HasPrefix(data["kit"].(string), "hop.top/kit@") || !strings.HasPrefix(data["uma"].(string), "hop.top/uma@") ||
		!strings.HasPrefix(data["store"].(string), "hop.top/uma/store/postgres@") || data["heo"] == "" {
		t.Errorf("versions section = %s %v", status, data)
	}
	// No section carries the store's connection string.
	raw := site.s.run(nil, "service", "status", "--format", "json")
	site.s.want(raw, 0, "service", "status")
	for _, secret := range []string{site.db.dsn, site.db.name} {
		if strings.Contains(raw.stdout, secret) {
			t.Errorf("service status leaks %q:\n%s", secret, raw.stdout)
		}
	}
	// The table view prints kit's rows, then one row per section.
	r := site.s.run(nil, "service", "status")
	site.s.want(r, 0, "service", "status")
	for _, want := range []string{"SECTION", "storage", "receivers", "state=ready", "store-owner", "owner=heo/default",
		"queue", "in_flight=0", "versions", "compat=compatible"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("table lacks %q:\n%s", want, r.stdout)
		}
	}
}
