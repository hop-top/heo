package e2e

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"hop.top/uma/model"
)

// receiversStopped matches the supervisor's report that the receivers, which
// stop just before the workers, are down: the drain has reached the workers.
var receiversStopped = regexp.MustCompile(`serve: stopped object=service .*service=receivers`)

// TestSIGHUPDrainsLikeSIGTERM: SIGHUP, which a closed terminal sends
// and heo answers with no reload, begins the same graceful drain as
// SIGTERM: a send in flight completes, the operation is accepted, the
// daemon exits 0 on its own and removes its socket and pidfile.
func TestSIGHUPDrainsLikeSIGTERM(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    drain_timeout: 30s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "2s"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		sock, pidPath := d.addr("socket"), site.s.path("run", "heo", "default", "heo.pid")
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })
		if err := d.cmd.Process.Signal(syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		if code := d.wait(); code != 0 {
			t.Fatalf("exit %d after SIGHUP, want 0 (129 is the default disposition):\n%s", code, d.log())
		}
		if steps := sendSteps(t, sendLog); count(steps, "done", op) != 1 || count(steps, "interrupted", op) != 0 {
			t.Errorf("send steps = %v, want the send finished by the drain", steps)
		}
		if !strings.Contains(d.log(), "runtime: workers drained") || !strings.Contains(d.log(), "reason=clean-stop") {
			t.Errorf("no clean drain reported:\n%s", d.log())
		}
		if got := operationState(t, site.db.operator("default"), op); got.State != model.ProviderAccepted {
			t.Errorf("operation = %s %+v, want provider_accepted", got.State, got.Failure)
		}
		for _, left := range []string{sock, pidPath} {
			if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s left behind: %v", left, err)
			}
		}
	})
}

// TestSecondSignalAbortsDrain: a second SIGTERM during the drain
// abandons it at once, as the manual says: the send in flight is
// interrupted (never sent again), its item stays leased, the daemon
// exits non-zero well within a second of the signal and says why, and
// the next run settles the abandoned item once, as outcome_unknown,
// without sending it again.
func TestSecondSignalAbortsDrain(t *testing.T) {
	withE2EBinary(t, func() {
		site := newCLISite(t, "    drain_timeout: 30s\n    lease: 5s\n")
		sendLog := site.s.path("sends.log")
		site.s.env[fakeSendDelayEnv] = "1m"
		site.s.env[fakeSendLogEnv] = sendLog
		op := site.db.queueFakeSend("default", "acct-out")
		d := site.start()
		eventually(t, "the send starts", func() bool { return count(sendSteps(t, sendLog), "start", op) == 1 })

		if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the drain reaches the workers", func() bool { return receiversStopped.MatchString(d.log()) })
		time.Sleep(300 * time.Millisecond)
		if d.isDone() {
			t.Fatalf("the daemon ended before the second signal:\n%s", d.log())
		}
		second := time.Now()
		if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-d.done:
		case <-time.After(10 * time.Second):
			t.Fatalf("daemon still draining 10s after the second signal:\n%s", d.log())
		}
		if took := time.Since(second); took > time.Second {
			t.Errorf("daemon exited %s after the second signal, want under 1s:\n%s", took, d.log())
		}
		if d.exit != 1 {
			t.Errorf("daemon exit %d, want 1 (an aborted drain is not a clean stop):\n%s", d.exit, d.log())
		}
		// The supervisor reports the abort once for each service it was
		// still stopping (the socket and the event bridge, which stop
		// after the workers); the run's final error line repeats them.
		for _, svc := range []string{"socket", "bridge"} {
			line := "service=" + svc + " reason=escalated error=\"drain aborted by second signal\""
			if n := strings.Count(d.log(), line); n != 1 {
				t.Errorf("'drain aborted by second signal' reported %d times for %s, want once:\n%s", n, svc, d.log())
			}
		}
		if n := strings.Count(d.log(), "reason=escalated error=\"drain aborted by second signal\""); n != 2 {
			t.Errorf("'drain aborted by second signal' reported for %d services, want 2:\n%s", n, d.log())
		}
		if steps := sendSteps(t, sendLog); count(steps, "interrupted", op) != 1 || count(steps, "done", op) != 0 {
			t.Fatalf("send steps after the abort = %v, want it interrupted", steps)
		}

		// The next run finds the item leased, settles it once the lease
		// expires, and does not send it again.
		delete(site.s.env, fakeSendDelayEnv)
		d = site.start()
		if waiting, inFlight, _ := site.queueDepth(); waiting != 0 || inFlight != 1 {
			t.Errorf("queue at restart: waiting %v, in flight %v; want the abandoned item still leased", waiting, inFlight)
		}
		eventually(t, "the abandoned item settles", func() bool {
			waiting, inFlight, dead := site.queueDepth()
			return waiting == 0 && inFlight == 0 && dead == 0
		})
		if got := operationState(t, site.db.operator("default"), op); got.State != model.Unknown {
			t.Errorf("operation = %s %+v, want outcome_unknown", got.State, got.Failure)
		}
		if steps := sendSteps(t, sendLog); count(steps, "start", op) != 1 || count(steps, "done", op) != 0 {
			t.Errorf("send steps across the abort = %v, want the one abandoned send only", steps)
		}
		site.stopService(d, 0)
	})
}
