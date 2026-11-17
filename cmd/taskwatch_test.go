package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

type taskWatchRun struct {
	a    *app
	cmd  *cobra.Command
	out  *bytes.Buffer
	errb *bytes.Buffer
}

func newTaskWatchRun(t *testing.T) taskWatchRun {
	t.Helper()
	a := newApp()
	a.r.Viper.Set("format", output.JSON)
	r := taskWatchRun{a: a, cmd: &cobra.Command{}, out: &bytes.Buffer{}, errb: &bytes.Buffer{}}
	r.cmd.SetOut(r.out)
	r.cmd.SetErr(r.errb)
	return r
}

var taskWatchStart = desk.WatchStart{Consumer: "heo-task-t1", From: desk.StartTail}

func taskEvent(id string) model.Event {
	return model.Event{ID: model.ID(id), Kind: model.EventTaskUpdated, Audience: model.Audience{Visibility: model.Internal}}
}

// TestTaskWatchEndsOnceTheTaskIsFinalAndDrained: the loop writes a
// batch's lines before it acknowledges the batch, keeps polling while a
// final task still has events to deliver, ends with exit 0 only when a
// poll scans nothing more, and then closes the subscription.
func TestTaskWatchEndsOnceTheTaskIsFinalAndDrained(t *testing.T) {
	r := newTaskWatchRun(t)
	var log []string
	polls := 0
	step := func(_ context.Context, st taskStep) (taskStepOut, error) {
		log = append(log, st.Op)
		switch st.Op {
		case stepOpen:
			return taskStepOut{Subscription: "sub_1", Checkpoint: "c0", Inbox: "in1", Conversation: "cv1", State: model.TaskSubmitted}, nil
		case stepPoll:
			if st.Inbox != "in1" || st.Conversation != "cv1" || st.Subscription != "sub_1" {
				t.Fatalf("poll lacks the task's location: %+v", st)
			}
			polls++
			switch polls {
			case 1:
				return taskStepOut{Events: []model.Event{taskEvent("e1")}, Token: "tok1", State: model.TaskWorking}, nil
			case 2:
				// The task is final, yet its last events are still in
				// the batches that follow: the watch goes on.
				return taskStepOut{Events: []model.Event{taskEvent("e2")}, Token: "tok2", State: model.TaskCompleted, Terminal: true}, nil
			}
			return taskStepOut{State: model.TaskCompleted, Terminal: true}, nil
		case stepAck:
			want := map[string]string{"tok1": `"id":"e1"`, "tok2": `"id":"e2"`}[st.Token]
			if want == "" || !strings.Contains(r.out.String(), want) {
				t.Fatalf("ack %+v before its line was written (stdout %q)", st, r.out.String())
			}
			return taskStepOut{Checkpoint: model.Cursor("c-" + st.Token)}, nil
		case stepClose:
			if st.Subscription != "sub_1" {
				t.Fatalf("close = %+v", st)
			}
			return taskStepOut{}, nil
		}
		t.Fatalf("unexpected step %q", st.Op)
		return taskStepOut{}, nil
	}
	if err := r.a.followTask(t.Context(), r.cmd, "t1", taskWatchStart, step, 0, 0); err != nil {
		t.Fatalf("watch = %v, want a clean end", err)
	}
	if got := strings.Join(log, ","); got != "open,poll,ack,poll,ack,poll,close" {
		t.Fatalf("steps = %s", got)
	}
	lines := strings.Split(strings.TrimSpace(r.out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q, want two NDJSON lines", r.out.String())
	}
	for i, l := range lines {
		var ev model.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil || string(ev.ID) != []string{"e1", "e2"}[i] {
			t.Fatalf("line %d = %q (%v)", i, l, err)
		}
	}
	if !strings.Contains(r.errb.String(), "task t1 reached completed") {
		t.Fatalf("stderr = %q", r.errb.String())
	}
}

// TestTaskWatchOfAFinalTaskEndsOnceDrained: a task already final has
// nothing more to wait for: the watch opens its subscription, delivers
// what the consumer has not seen, and ends at once, closing it.
func TestTaskWatchOfAFinalTaskEndsOnceDrained(t *testing.T) {
	r := newTaskWatchRun(t)
	var log []string
	step := func(_ context.Context, st taskStep) (taskStepOut, error) {
		log = append(log, st.Op)
		return taskStepOut{Subscription: "sub_1", State: model.TaskCanceled, Terminal: true}, nil
	}
	if err := r.a.followTask(t.Context(), r.cmd, "t1", taskWatchStart, step, 0, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Join(log, ",") != "open,poll,close" || r.out.Len() != 0 || !strings.Contains(r.errb.String(), "task t1 reached canceled") {
		t.Fatalf("steps %v, stdout %q, stderr %q", log, r.out.String(), r.errb.String())
	}
}

// TestTaskWatchTimeoutIsTransient: a task still open when --timeout
// passes is exit 6 (transient), naming the state and how to resume.
func TestTaskWatchTimeoutIsTransient(t *testing.T) {
	r := newTaskWatchRun(t)
	step := func(_ context.Context, st taskStep) (taskStepOut, error) {
		if st.Op == stepOpen {
			return taskStepOut{Subscription: "sub_1", State: model.TaskWorking}, nil
		}
		return taskStepOut{State: model.TaskWorking}, nil
	}
	start := time.Now()
	err := r.a.followTask(t.Context(), r.cmd, "t1", taskWatchStart, step, 50*time.Millisecond, 0)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Transience != output.TransienceTransient || !strings.Contains(oe.Message, "still working") ||
		!strings.Contains(oe.SuggestedFix, "--consumer heo-task-t1") {
		t.Fatalf("err = %v (%+v), want a transient timeout", err, oe)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

// TestTaskWatchInterruptAcksWhatItWrote: an interrupt that lands while a
// batch is in hand still writes and acknowledges it, then stops with
// exit 0 and the checkpoint on stderr; it does not close the
// subscription, which can be resumed.
func TestTaskWatchInterruptAcksWhatItWrote(t *testing.T) {
	r := newTaskWatchRun(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var log []string
	step := func(ctx context.Context, st taskStep) (taskStepOut, error) {
		log = append(log, st.Op)
		switch st.Op {
		case stepOpen:
			return taskStepOut{Subscription: "sub_1", Checkpoint: "c0", State: model.TaskWorking}, nil
		case stepPoll:
			cancel()
			return taskStepOut{Events: []model.Event{taskEvent("e1")}, Token: "tok", State: model.TaskWorking}, nil
		case stepAck:
			if ctx.Err() != nil {
				t.Fatal("ack ran on the interrupted context")
			}
			return taskStepOut{Checkpoint: "c1"}, nil
		}
		t.Fatalf("unexpected step %q", st.Op)
		return taskStepOut{}, nil
	}
	if err := r.a.followTask(ctx, r.cmd, "t1", taskWatchStart, step, 0, 0); err != nil {
		t.Fatalf("watch = %v, want a clean stop", err)
	}
	if got := strings.Join(log, ","); got != "open,poll,ack" {
		t.Fatalf("steps = %s", got)
	}
	if !strings.Contains(r.errb.String(), "consumer heo-task-t1 at checkpoint c1") || !strings.Contains(r.out.String(), `"id":"e1"`) {
		t.Fatalf("stdout %q, stderr %q", r.out.String(), r.errb.String())
	}
}

// TestTaskEventRowsSayWhatChanged: a text row names the state move of a
// task.updated, and the status message a message.created carries.
func TestTaskEventRowsSayWhatChanged(t *testing.T) {
	payload := func(p model.TaskUpdatedPayload) model.Event {
		b, _ := json.Marshal(p)
		return model.Event{ID: "e", Kind: model.EventTaskUpdated, Resource: model.Ref{Kind: model.KindTask, ID: "t1"}, Payload: model.Extension{Data: b}}
	}
	for _, tc := range []struct {
		ev   model.Event
		want string
	}{
		{payload(model.TaskUpdatedPayload{State: &model.StatusChange{From: "working", To: "canceled"}}), "state working -> canceled"},
		{payload(model.TaskUpdatedPayload{State: &model.StatusChange{To: "submitted"}}), "state (new) -> submitted"},
		{payload(model.TaskUpdatedPayload{Changes: []string{"artifacts", "status_message"}}), "artifacts,status_message"},
		{model.Event{Kind: model.EventMessageCreated}, "status message"},
	} {
		if got := taskEventChange(tc.ev); got != tc.want {
			t.Errorf("%s change = %q, want %q", tc.ev.Kind, got, tc.want)
		}
	}
	if row := taskEventRow(payload(model.TaskUpdatedPayload{State: &model.StatusChange{From: "working", To: "canceled"}})); !strings.Contains(row, "task:t1") {
		t.Errorf("row = %q", row)
	}
}

// TestTaskWatchStartDefaults: the consumer defaults to the task's own,
// and a start that is neither tail nor beginning is a usage error.
func TestTaskWatchStartDefaults(t *testing.T) {
	a := newApp()
	c := a.taskWatchCmd()
	if err := c.ParseFlags(nil); err != nil {
		t.Fatal(err)
	}
	w, err := taskWatchStartOf(c, "t9")
	if err != nil || w.Consumer != "heo-task-t9" || w.From != desk.StartTail {
		t.Fatalf("start = %+v, %v", w, err)
	}
	if err := c.ParseFlags([]string{"--consumer", "mine", "--from", "beginning"}); err != nil {
		t.Fatal(err)
	}
	if w, err = taskWatchStartOf(c, "t9"); err != nil || w.Consumer != "mine" || w.From != desk.StartBeginning {
		t.Fatalf("start = %+v, %v", w, err)
	}
	if err := c.ParseFlags([]string{"--from", "middle"}); err != nil {
		t.Fatal(err)
	}
	if _, err := taskWatchStartOf(c, "t9"); err == nil {
		t.Fatal("--from middle accepted")
	}
}
