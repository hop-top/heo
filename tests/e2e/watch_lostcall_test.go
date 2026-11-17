//go:build unix

package e2e

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedDaemon speaks the control socket's wire protocol for a task
// watch, deterministically: open and the first poll are answered, then
// the next polls are dropped as drop says (all of them when drops is
// negative), then a poll answers that the task is canceled and the close
// is answered. It is the daemon being stopped while it holds a step, with
// no daemon process, no store and no timing to race.
type scriptedDaemon struct {
	t     *testing.T
	drops int
	// drop ends a call it took without answering it.
	drop func(net.Conn)

	mu        sync.Mutex
	ops       map[string]int
	polls     int
	firstDrop time.Time
}

// dropClosed closes the connection with the request read.
func dropClosed(net.Conn) {}

// dropCancelled answers as the daemon does for a call it cancels as it
// stops: kit's socket reports the cancellation as INTERNAL.
func dropCancelled(conn net.Conn) {
	_, _ = conn.Write([]byte(`{"ok":false,"error":{"code":"INTERNAL","message":"cmdsurface: canceled: context canceled"}}` + "\n"))
}

func (s *sandbox) scripted(drops int, drop func(net.Conn)) *scriptedDaemon {
	s.t.Helper()
	d := &scriptedDaemon{t: s.t, drops: drops, drop: drop, ops: map[string]int{}}
	dir := s.path("run", "heo", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatal(err)
	}
	ln, err := net.Listen("unix", dir+"/heo.sock")
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				d.serve(conn)
			}()
		}
	}()
	return d
}

func (d *scriptedDaemon) serve(conn net.Conn) {
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Flags map[string]any `json:"flags"`
	}
	var step struct {
		Op string `json:"op"`
	}
	if json.Unmarshal(line, &req) != nil {
		return
	}
	if raw, _ := req.Flags["step"].(string); json.Unmarshal([]byte(raw), &step) != nil {
		return
	}
	out := `{"subscription":"sub_1","checkpoint":"c0","events":[],"inbox":"inbox_1","conversation":"conv_1","state":"submitted","terminal":false}`
	d.mu.Lock()
	d.ops[step.Op]++
	if step.Op == "poll" {
		d.polls++
		switch dropped := d.polls - 1; {
		case dropped >= 1 && (d.drops < 0 || dropped <= d.drops):
			if d.firstDrop.IsZero() {
				d.firstDrop = time.Now()
			}
			d.mu.Unlock()
			d.drop(conn)
			return
		case dropped > d.drops && d.drops >= 0:
			out = `{"subscription":"sub_1","events":[],"state":"canceled","terminal":true}`
		}
	}
	if step.Op == "close" {
		out = `{"subscription":"sub_1","events":[],"state":"canceled","terminal":true}`
	}
	d.mu.Unlock()
	_, _ = conn.Write([]byte(`{"ok":true,"result":{"exit_code":0,"data":` + out + `}}` + "\n"))
}

func (d *scriptedDaemon) count(op string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ops[op]
}

func (p *watchProc) finished(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(within):
		_ = p.cmd.Process.Kill()
		t.Fatalf("the watch was still running after %s:\n%s", within, p.errb.String())
	}
}

// TestTaskWatchRidesOutCallsTheDaemonDrops: a daemon that took a step
// and stopped answering it, by closing the connection or by cancelling
// the call as it stops, is a daemon that is unavailable like one that is
// not there: the attached watch says so once, tries again, and goes on to
// the task's end when the daemon answers, without opening again.
func TestTaskWatchRidesOutCallsTheDaemonDrops(t *testing.T) {
	for name, drop := range map[string]func(net.Conn){"closed": dropClosed, "cancelled": dropCancelled} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			d := s.scripted(3, drop)
			p := s.watchOf([]string{"task", "watch"}, "task_1", "--consumer", "c", "--reconnect-timeout", "30s", "--format", "json")
			p.finished(t, 30*time.Second)
			stderr := p.errb.String()
			if p.exit != 0 || !strings.Contains(stderr, "task task_1 reached canceled") {
				t.Fatalf("watch exit %d:\n%s", p.exit, stderr)
			}
			if n := strings.Count(stderr, waitingLine("30s")); n != 1 {
				t.Errorf("said it was waiting %d times for one outage, want once:\n%s", n, stderr)
			}
			if d.count("open") != 1 || d.count("poll") != 5 {
				t.Errorf("open %d, poll %d, want 1 and 5 (1 answered, 3 dropped, 1 final)", d.count("open"), d.count("poll"))
			}
		})
	}
}

// TestTaskWatchBoundCountsFromTheFirstDroppedCall: a daemon that drops
// every step from the first poll on is waited for the bound, counted from
// that first dropped call, then the watch fails PREREQUISITE (exit 70)
// naming the command that starts a daemon; with a bound of 0 it fails at
// once without saying it waits.
func TestTaskWatchBoundCountsFromTheFirstDroppedCall(t *testing.T) {
	for name, drop := range map[string]func(net.Conn){"closed": dropClosed, "cancelled": dropCancelled} {
		t.Run(name, func(t *testing.T) {
			s := newSandbox(t)
			d := s.scripted(-1, drop)
			p := s.watchOf([]string{"task", "watch"}, "task_1", "--consumer", "c", "--reconnect-timeout", "2s", "--format", "json")
			p.finished(t, 20*time.Second)
			gaveUp := time.Now()
			stderr := p.errb.String()
			if p.exit != 70 || !strings.Contains(stderr, "PREREQUISITE") || !strings.Contains(stderr, "heo service start") {
				t.Fatalf("watch past its bound: exit %d\n%s", p.exit, stderr)
			}
			if n := strings.Count(stderr, waitingLine("2s")); n != 1 {
				t.Errorf("said it was waiting %d times, want once:\n%s", n, stderr)
			}
			d.mu.Lock()
			from := d.firstDrop
			d.mu.Unlock()
			if took := gaveUp.Sub(from); took < 1900*time.Millisecond || took > 8*time.Second {
				t.Errorf("gave up %s after the first dropped call, want about its 2s bound", took)
			}

			p = s.watchOf([]string{"task", "watch"}, "task_1", "--consumer", "c0", "--reconnect-timeout", "0", "--format", "json")
			p.finished(t, 10*time.Second)
			if p.exit != 70 || !strings.Contains(p.errb.String(), "PREREQUISITE") || strings.Contains(p.errb.String(), "waiting") {
				t.Errorf("watch with no wait: exit %d\n%s", p.exit, p.errb.String())
			}
		})
	}
}
