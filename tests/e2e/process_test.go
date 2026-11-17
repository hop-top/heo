package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"hop.top/uma/model"
)

// socketLimit is the longest unix socket path the daemon binds.
const socketLimit = 103

// socketPathOf is a path under dir that is n bytes long.
func socketPathOf(dir string, n int) string {
	base := dir + "/"
	return base + strings.Repeat("a", n-len(base)-len(".sock")) + ".sock"
}

// TestServiceStopWithNoDaemonExitsZero: `service stop` with no daemon
// is not an error, however often it is repeated, in text and in JSON.
func TestServiceStopWithNoDaemonExitsZero(t *testing.T) {
	s := newSandbox(t)
	for i := range 2 {
		r := s.run(nil, "service", "stop")
		s.want(r, 0, "service", "stop")
		if !strings.Contains(r.stdout, "not_running") || r.stderr != "" {
			t.Errorf("stop %d: stdout %q, stderr %q; want not_running and nothing on stderr", i, r.stdout, r.stderr)
		}
	}
	r := s.run(nil, "service", "stop", "--format", "json")
	s.want(r, 0, "service", "stop", "--format", "json")
	var out struct{ Result string }
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Result != "not_running" {
		t.Errorf("json stop = %q, %v", r.stdout, err)
	}
	// Like daemon ping, a stop that finds nobody changes nothing on disk.
	if _, err := os.Stat(s.path("run", "heo")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stop with no daemon created the runtime dir: %v", err)
	}
}

// TestSocketPathBudget: a control socket path of 103 bytes binds; one
// byte more, given with --socket or derived from a long runtime
// directory, is refused as a usage error that names the limit, before
// anything is bound or created.
func TestSocketPathBudget(t *testing.T) {
	s := newSandbox(t)
	over := socketPathOf(s.path("sock"), socketLimit+1)
	r := s.run(nil, "service", "start", noRuntime, "--socket", over)
	s.want(r, 2, "service", "start", noRuntime, "--socket", over)
	for _, want := range []string{"USAGE", strconv.Itoa(socketLimit+1) + " bytes", "103-byte limit"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
		}
	}
	if _, err := os.Stat(s.path("sock")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused path created %s: %v", s.path("sock"), err)
	}

	// The default path is derived from the runtime directory.
	long := s.path("r" + strings.Repeat("x", 90))
	r = s.run([]string{"XDG_RUNTIME_DIR=" + long}, "service", "start", noRuntime)
	s.want(r, 2, "service", "start", noRuntime)
	if !strings.Contains(r.stderr, "103-byte limit") {
		t.Errorf("derived path: stderr lacks the limit:\n%s", r.stderr)
	}
	if _, err := os.Stat(long); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused derived path created %s: %v", long, err)
	}

	exact := socketPathOf(s.path("sock"), socketLimit)
	d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime, "--socket", exact)
	if got := d.addr("socket"); got != exact || len(got) != socketLimit {
		t.Errorf("socket at %q (%d bytes), want %q", got, len(got), exact)
	}
	if fi, err := os.Stat(exact); err != nil || fi.Mode().Type()&os.ModeSocket == 0 {
		t.Errorf("no socket at the limit: %v %v", fi, err)
	}
	if code := d.term(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, d.log())
	}
}

// TestDaemonSignals: SIGINT, SIGTERM and SIGHUP (a closed terminal;
// heo has no reload) each stop the daemon cleanly, exit 0 with the
// supervisor's clean-stop reason, and leave neither socket nor pidfile
// behind.
func TestDaemonSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			s := newSandbox(t)
			d := s.start(nil, nil, []string{"socket"}, "service", "start", noRuntime)
			sock, pidPath := d.addr("socket"), s.path("run", "heo", "default", "heo.pid")
			eventually(t, "the pidfile names the daemon", func() bool {
				b, err := os.ReadFile(pidPath) //nolint:gosec // G304: the test's own sandbox
				return err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(d.cmd.Process.Pid)
			})
			if err := d.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := d.wait(); code != 0 {
				t.Fatalf("exit %d after %s:\n%s", code, sig, d.log())
			}
			if !strings.Contains(d.log(), "reason=clean-stop") {
				t.Errorf("no clean stop reported:\n%s", d.log())
			}
			for _, left := range []string{sock, pidPath} {
				if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s left behind: %v", left, err)
				}
			}
		})
	}
}

// TestPortContentionRefusesStartAndLeaksNothing: a daemon whose
// webhook address is taken is refused with the reason and exit 1, and
// has left no control socket, pidfile or half-started service; the
// holder of the port is undisturbed, and the same start works once the
// port is free.
func TestPortContentionRefusesStartAndLeaksNothing(t *testing.T) {
	c := newCLISite(t, "")
	hold, err := net.Listen("tcp", c.hook)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Close() }()
	args := []string{"-c", "services.webhook.addr=" + c.hook, "service", "start", "--enable", "webhook"}
	r := c.s.run(nil, args...)
	c.s.want(r, 1, args...)
	if !strings.Contains(r.stderr, "address already in use") || !strings.Contains(r.stderr, c.hook) {
		t.Errorf("stderr does not say the port is taken:\n%s", r.stderr)
	}
	pid := c.s.path("run", "heo", "default", "heo.pid")
	if _, err := os.Stat(pid); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused daemon left a pidfile: %v", err)
	}
	if ping := c.s.run(nil, "daemon", "ping"); ping.exit != 70 {
		t.Errorf("a daemon answers after a refused start: exit %d\n%s", ping.exit, ping.stderr)
	}
	conn, err := net.DialTimeout("tcp", c.hook, time.Second)
	if err != nil {
		t.Fatalf("the holder lost its port: %v", err)
	}
	_ = conn.Close()

	_ = hold.Close()
	d := c.start()
	if got := d.addr("webhook"); got != c.hook {
		t.Errorf("webhook at %q, want %q", got, c.hook)
	}
	c.stop(d)
}

// TestRepeatedStopDuringDrain: `service stop` asked again while a
// delivery drains does not escalate. A stop that gives up waiting
// (--wait) exits 6 and leaves the daemon draining; later stops, however
// many, wait for the same drain. The in-flight send completes rather
// than being interrupted, the daemon exits 0 on its own (a kill would
// read -1), and a stop after it is gone is the not-running exit 0.
func TestRepeatedStopDuringDrain(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    drain_timeout: 30s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "4s"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })

		first := site.asyncRun("service", "stop")
		time.Sleep(300 * time.Millisecond)
		// A stop that will not wait out the drain says so and stops nothing.
		impatient := site.heo(6, "service", "stop", "--wait", "200ms")
		if d.isDone() {
			t.Fatalf("an impatient stop ended the daemon:\n%s", d.log())
		}
		t.Logf("impatient stop: %s", strings.TrimSpace(impatient.stderr))
		second := site.asyncRun("service", "stop")

		for name, wait := range map[string]func() result{"first": first, "second": second} {
			if r := wait(); r.exit != 0 || !strings.Contains(r.stdout, "stopped") {
				t.Errorf("%s stop: exit %d, stdout %q, stderr %q; want exit 0 stopped", name, r.exit, r.stdout, r.stderr)
			}
		}
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("daemon still running:\n%s", d.log())
		}
		if d.exit != 0 {
			t.Fatalf("daemon exit %d, want 0 (a signal kill reads -1):\n%s", d.exit, d.log())
		}
		steps := sendSteps(t, sendLog)
		if count(steps, "done", op) != 1 || count(steps, "interrupted", op) != 0 {
			t.Errorf("send steps = %v; repeated stops must let the drain finish", steps)
		}
		if strings.Contains(d.log(), "aborted") || strings.Contains(d.log(), "did not finish") {
			t.Errorf("drain abandoned:\n%s", d.log())
		}
		if got := operationState(t, site.db.operator("default"), op); got.State != model.ProviderAccepted {
			t.Errorf("operation = %s %+v, want provider_accepted", got.State, got.Failure)
		}
		site.heo(0, "service", "stop") // nobody left: not running, exit 0
	})
}

// TestCrashRecoversStaleStateAndQueuedWork: a daemon killed outright
// (SIGKILL, no drain) leaves its socket and pidfile; the next start
// takes both over, and the delivery the dead daemon had in flight is
// settled once its lease runs out, as outcome_unknown, without being
// sent again: it may have reached the provider.
func TestCrashRecoversStaleStateAndQueuedWork(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    lease: 5s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "1m"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })
		sock, pidPath := d.addr("socket"), site.s.path("run", "heo", "default", "heo.pid")
		first := d.cmd.Process.Pid

		if err := d.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		d.wait()
		if _, err := os.Stat(sock); err != nil {
			t.Fatalf("a killed daemon removed its socket (%v): the test does not exercise a stale one", err)
		}
		if b, err := os.ReadFile(pidPath); err != nil || strings.TrimSpace(string(b)) != strconv.Itoa(first) { //nolint:gosec // G304: the test's own sandbox
			t.Fatalf("pidfile = %q, %v; want the dead daemon's %d", b, err, first)
		}

		delete(site.s.env, fakeSendDelayEnv)
		d = site.start()
		eventually(t, "the pidfile names the new daemon", func() bool {
			b, err := os.ReadFile(pidPath) //nolint:gosec // G304: the test's own sandbox
			return err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(d.cmd.Process.Pid)
		})
		eventually(t, "the abandoned item settles", func() bool {
			waiting, inFlight, dead := site.queueDepth()
			return waiting == 0 && inFlight == 0 && dead == 0
		})
		if got := operationState(t, site.db.operator("default"), op); got.State != model.Unknown {
			t.Errorf("operation = %s %+v, want outcome_unknown", got.State, got.Failure)
		}
		if steps := sendSteps(t, sendLog); count(steps, "start", op) != 1 || count(steps, "done", op) != 0 {
			t.Errorf("send steps across the crash = %v, want the one abandoned send only", steps)
		}
		site.stopService(d, 0)
	})
}

// TestStdoutStderrContract: stdout carries the result and only the
// result; stderr carries diagnostics and only diagnostics. A success
// leaves stderr empty, a failure leaves stdout empty, and with
// --format json a failure of a command that parsed has one error
// envelope on stderr, naming the code the exit status stands for. The daemon logs on stderr and writes
// nothing to stdout.
func TestStdoutStderrContract(t *testing.T) {
	s := newSandbox(t)
	type envelope struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	}
	for _, c := range []struct {
		name string
		args []string
		exit int
		code string
		// parse: the command line did not parse, so no output format
		// was in force yet and the diagnostic is plain text.
		parse bool
	}{
		{"unknown flag", []string{"config", "list", "--no-such-flag"}, 2, "USAGE", true},
		{"unknown subcommand", []string{"service", "nosuch"}, 2, "USAGE", true},
		{"unknown config key", []string{"config", "get", "no.such.key"}, 3, "NOT_FOUND", false},
		{"no daemon", []string{"daemon", "ping"}, 70, "PREREQUISITE", false},
	} {
		t.Run("failure/"+c.name, func(t *testing.T) {
			r := s.run(nil, c.args...)
			s.want(r, c.exit, c.args...)
			if r.stdout != "" {
				t.Errorf("a failure wrote to stdout:\n%s", r.stdout)
			}
			if !strings.Contains(r.stderr, c.code) {
				t.Errorf("stderr lacks %s:\n%s", c.code, r.stderr)
			}
			jr := s.run(nil, append(append([]string{}, c.args...), "--format", "json")...)
			if jr.stdout != "" || jr.exit != c.exit {
				t.Errorf("--format json: exit %d, stdout %q", jr.exit, jr.stdout)
			}
			if c.parse {
				return
			}
			var env envelope
			if err := json.Unmarshal([]byte(jr.stderr), &env); err != nil {
				t.Fatalf("stderr is not one JSON envelope: %v\n%s", err, jr.stderr)
			}
			if env.Code != c.code || env.ExitCode != c.exit || env.Message == "" {
				t.Errorf("envelope = %+v, want code %s, exit %d and a message", env, c.code, c.exit)
			}
		})
	}
	for _, args := range [][]string{
		{"config", "list"},
		{"config", "list", "--format", "json"},
		{"config", "get", "services.socket.enabled"},
		{"service", "stop"},
		{"service", "list", "--format", "json"},
		{"--help"},
		{"daemon", "ping", "--help"},
	} {
		t.Run("success/"+strings.Join(args, "_"), func(t *testing.T) {
			r := s.run(nil, args...)
			s.want(r, 0, args...)
			if strings.TrimSpace(r.stdout) == "" {
				t.Error("a success wrote no result to stdout")
			}
			if r.stderr != "" {
				t.Errorf("a success wrote to stderr:\n%s", r.stderr)
			}
			if slices.Contains(args, "json") {
				var v any
				if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
					t.Errorf("--format json stdout is not JSON: %v\n%s", err, r.stdout)
				}
			}
		})
	}

	t.Run("daemon", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), runTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, heoBin, "service", "start", noRuntime) //nolint:gosec // runs the binary TestMain built
		cmd.Dir = s.path("cwd")
		for k, v := range s.env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		var out, errb lockedBuffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the daemon is ready", func() bool { return strings.Contains(errb.String(), "ready_reported object=supervisor") })
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("daemon: %v\n%s", err, errb.String())
		}
		if out.String() != "" {
			t.Errorf("the daemon wrote to stdout:\n%s", out.String())
		}
		if !strings.Contains(errb.String(), "reason=clean-stop") {
			t.Errorf("the daemon's log is not on stderr:\n%s", errb.String())
		}
	})
}
