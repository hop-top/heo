package e2e

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// TestWatchWithNoDaemonExits70: a watch started while no daemon answers
// fails like every other daemon command: PREREQUISITE, exit 70, the
// command that starts one, at once. It does not wait for a daemon to
// appear, retry silently or print a result.
func TestWatchWithNoDaemonExits70(t *testing.T) {
	s := newSandbox(t)
	for _, args := range [][]string{
		{"event", "watch"},
		{"task", "watch", "task_1"},
		{"operation", "watch", "op_1"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var r result
			within(t, 2*time.Second, "heo "+strings.Join(args, " "), func() { r = s.run(nil, args...) })
			s.want(r, 70, args...)
			for _, want := range []string{"PREREQUISITE", "no heo daemon is running", "heo service start"} {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
				}
			}
			if r.stdout != "" || strings.Contains(r.stderr, "waiting") {
				t.Errorf("stdout %q, stderr %q: a watch with no daemon printed a result or waited", r.stdout, r.stderr)
			}
		})
	}
}

// ridden is one kind of watch, run against a daemon that goes away and
// comes back.
type ridden struct {
	name string
	// watch starts the watch with the reconnect flags, as consumer n.
	watch func(n string, flags ...string) *watchProc
	// attached waits until the watch holds its subscription.
	attached func(p *watchProc)
	// stop and start take the daemon down and bring it back; the last
	// start has what the watch needs to move on (workers running).
	stop  func()
	start func(final bool)
	// resume makes progress after the final start and checks the watch
	// went on with it, then ends the watch.
	resume func(p *watchProc)
}

// TestWatchReconnectsWithinItsBound drives each watch against a daemon
// that stops: a watch started with --reconnect-timeout 0 gives up at
// once, one with a bound waits that long saying so once on stderr and
// then fails with exit 70, and one whose daemon returns within the
// bound carries on where it was, without a repeat or a gap.
func TestWatchReconnectsWithinItsBound(t *testing.T) {
	withE2EBinary(t, func() {
		t.Run("event", func(t *testing.T) { rideEvent(t) })
		t.Run("operation", func(t *testing.T) { rideOperation(t) })
		t.Run("task", func(t *testing.T) { rideTask(t) })
	})
}

func rideEvent(t *testing.T) {
	site := newCLISite(t, "")
	site.db.queueFakeSend("default", "acct-out")
	d := site.start("workers")
	h := ridden{
		name: "event",
		watch: func(n string, flags ...string) *watchProc {
			return site.s.watch(append([]string{"--consumer", n, "--from", "beginning", "--format", "json"}, flags...)...)
		},
		stop: func() { site.stop(d) },
		start: func(final bool) {
			if final {
				d = site.start()
			} else {
				d = site.start("workers")
			}
		},
	}
	h.attached = func(p *watchProc) {
		t.Helper()
		// The events of the queued send are committed before the watch
		// starts, and it replays them from the beginning: wait for them
		// by what they are, not for a quiet spell a loaded start can
		// outlast.
		seen := map[string]bool{}
		p.until(func(ev model.Event) bool {
			seen[ev.Kind] = true
			return seen["message.created"] && seen["conversation.changed"]
		})
	}
	h.resume = func(p *watchProc) {
		t.Helper()
		// The workers send what was queued: events the watch has not
		// seen, none twice.
		got := p.quiet(4 * time.Second)
		if len(got) == 0 {
			t.Fatalf("no event after the daemon came back:\n%s", p.errb.String())
		}
		seen := map[string]bool{}
		for _, l := range got {
			if seen[l] {
				t.Errorf("event repeated after the reconnect: %s", l)
			}
			seen[l] = true
		}
		code, stderr := p.interrupt()
		if code != 0 || !strings.Contains(stderr, "stopped: consumer ") {
			t.Fatalf("interrupted watch: exit %d\n%s", code, stderr)
		}
	}
	h.exercise(t)
}

func rideOperation(t *testing.T) {
	site := newCLISite(t, "")
	op := site.db.queueFakeSend("default", "acct-out")
	d := site.start("workers")
	h := ridden{
		name: "operation",
		watch: func(_ string, flags ...string) *watchProc {
			return site.s.watchOf([]string{"operation", "watch"}, append([]string{string(op.ID), "--format", "json"}, flags...)...)
		},
		attached: func(p *watchProc) {
			t.Helper()
			select {
			case line := <-p.lines:
				if !strings.Contains(line, string(op.ID)) {
					t.Fatalf("first line %q", line)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("the watch read no state:\n%s", p.errb.String())
			}
		},
		stop: func() { site.stop(d) },
		start: func(final bool) {
			if final {
				d = site.start()
			} else {
				d = site.start("workers")
			}
		},
	}
	h.resume = func(p *watchProc) {
		t.Helper()
		// The workers send the queued operation: the watch ends by the
		// state it ends in.
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("the watch did not end after the daemon came back:\n%s", p.errb.String())
		}
		lines := p.quiet(100 * time.Millisecond)
		if p.exit != 0 || len(lines) == 0 || !strings.Contains(lines[len(lines)-1], string(model.ProviderAccepted)) {
			t.Fatalf("watch exit %d, lines %v\n%s", p.exit, lines, p.errb.String())
		}
	}
	h.exercise(t)
}

func rideTask(t *testing.T) {
	s := newTaskSite(t, "")
	task := s.open("e1", "ctx-1")
	id := string(task.Ref.ID)
	h := ridden{
		name: "task",
		watch: func(n string, flags ...string) *watchProc {
			return s.s.watchOf([]string{"task", "watch"}, append([]string{id, "--consumer", n, "--format", "json"}, flags...)...)
		},
		attached: func(p *watchProc) {
			t.Helper()
			eventually(t, "the watch to open", func() bool { return strings.Contains(p.errb.String(), "watching task "+id) })
		},
		stop:  func() { s.stop(s.d) },
		start: func(bool) { s.d = s.start() },
	}
	h.resume = func(p *watchProc) {
		t.Helper()
		// A task event after the reconnect, then the task's end.
		s.host(task, "ride-w", model.TaskUpdate{State: model.TaskWorking})
		p.until(func(ev model.Event) bool { return ev.Kind == model.EventTaskUpdated })
		var cur model.Task
		s.json(&cur, "task", "get", id)
		var c cancelJSON
		s.json(&c, "task", "cancel", id, "--revision", string(cur.Revision))
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("the watch did not end at the final state:\n%s", p.errb.String())
		}
		if p.exit != 0 || !strings.Contains(p.errb.String(), "task "+id+" reached canceled") {
			t.Fatalf("watch exit %d\n%s", p.exit, p.errb.String())
		}
	}
	h.exercise(t)
}

// waitingLine is what a waiting watch says on stderr, once.
func waitingLine(bound string) string { return "daemon unavailable, waiting up to " + bound }

// exercise runs the three behaviours in turn, ending with the daemon
// back and the watch moving on.
func (h ridden) exercise(t *testing.T) {
	// A bound that runs out: exit 70 after about that long.
	p := h.watch("bound", "--reconnect-timeout", "2s")
	h.attached(p)
	// The daemon stops answering somewhere inside stop, which returns
	// once the process is gone: the bound runs from then, never before
	// stop began and never after it returned.
	stopping := time.Now()
	h.stop()
	stopped := time.Now()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s watch still waiting 15s after the daemon stopped (bound 2s):\n%s", h.name, p.errb.String())
	}
	took, tookSinceStop := time.Since(stopping), time.Since(stopped)
	stderr := p.errb.String()
	if p.exit != 70 || !strings.Contains(stderr, "PREREQUISITE") || !strings.Contains(stderr, "heo service start") {
		t.Errorf("%s watch past its bound: exit %d\n%s", h.name, p.exit, stderr)
	}
	if n := strings.Count(stderr, waitingLine("2s")); n != 1 {
		t.Errorf("%s watch said it was waiting %d times, want once:\n%s", h.name, n, stderr)
	}
	if took < 1900*time.Millisecond || tookSinceStop > 8*time.Second {
		t.Errorf("%s watch gave up %s after the daemon began to stop (%s after it stopped), want about its 2s bound", h.name, took, tookSinceStop)
	}

	// A bound of zero: no waiting at all.
	h.start(false)
	p = h.watch("zero", "--reconnect-timeout", "0")
	h.attached(p)
	h.stop()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s watch with --reconnect-timeout 0 still running after the daemon stopped:\n%s", h.name, p.errb.String())
	}
	if p.exit != 70 || !strings.Contains(p.errb.String(), "PREREQUISITE") || strings.Contains(p.errb.String(), "waiting") {
		t.Errorf("%s watch with no wait: exit %d\n%s", h.name, p.exit, p.errb.String())
	}

	// A daemon that comes back within the bound.
	h.start(false)
	p = h.watch("ride", "--reconnect-timeout", "30s")
	h.attached(p)
	h.stop()
	eventually(t, h.name+" watch to say it is waiting", func() bool { return strings.Contains(p.errb.String(), waitingLine("30s")) })
	time.Sleep(1500 * time.Millisecond) // several retries
	select {
	case <-p.done:
		t.Fatalf("%s watch gave up with 28s of its bound left (exit %d):\n%s", h.name, p.exit, p.errb.String())
	default:
	}
	h.start(true)
	h.resume(p)
	if n := strings.Count(p.errb.String(), waitingLine("30s")); n != 1 {
		t.Errorf("%s watch said it was waiting %d times for one outage, want once:\n%s", h.name, n, p.errb.String())
	}
}
