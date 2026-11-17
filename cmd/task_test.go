package cmd

import (
	"strings"
	"testing"

	"hop.top/uma/model"
)

// TestCancelNotesAreHonest: what a cancel says it did is what uma
// stored. Committed and read back canceled is a cancellation; an
// operation that is not committed is not one yet; a committed operation
// whose task does not read canceled is said so, not claimed.
func TestCancelNotesAreHonest(t *testing.T) {
	committed := operationView{ID: "op1", State: model.Committed}
	for _, tc := range []struct {
		name string
		out  cancelOut
		want []string
		not  []string
	}{
		{"canceled", cancelOut{Task: "t1", State: model.TaskCanceled, Operation: committed},
			[]string{"committed: the task is canceled", "nothing is pushed to the remote agent"}, []string{"answers the remote", "replayed"}},
		{"answered", cancelOut{Task: "t1", State: model.TaskCanceled, Answered: true, Operation: committed},
			[]string{"answers the remote agent's cancel request"}, nil},
		{"replayed", cancelOut{Task: "t1", State: model.TaskCanceled, Operation: operationView{ID: "op1", State: model.Committed, Replayed: true}},
			[]string{"replayed"}, nil},
		{"queued", cancelOut{Task: "t1", State: model.TaskWorking, Operation: operationView{ID: "op1", State: model.Queued, Durable: true}},
			[]string{"queued", "only once the operation commits"}, []string{"committed: the task is canceled"}},
		{"not canceled", cancelOut{Task: "t1", State: model.TaskWorking, Operation: committed},
			[]string{"reads working, not canceled"}, []string{"committed: the task is canceled"}},
		{"not re-read", cancelOut{Task: "t1", Operation: committed},
			[]string{"reads -, not canceled"}, []string{"committed: the task is canceled"}},
	} {
		got := strings.Join(cancelNotes(tc.out), "\n")
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: notes lack %q:\n%s", tc.name, w, got)
			}
		}
		for _, n := range tc.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: notes claim %q:\n%s", tc.name, n, got)
			}
		}
	}
}

// TestTaskRowsSayWhoAskedAndWhatIsFinal: a text view says a cancel
// request is the remote agent's ask, and that a final state is final.
func TestTaskRowsSayWhoAskedAndWhatIsFinal(t *testing.T) {
	asked := model.Task{Ref: model.TaskRef{ID: "t1"}, State: model.TaskWorking, CancelRequested: true}
	if got := cancelRequestedText(asked); !strings.Contains(got, "the remote agent asked") {
		t.Errorf("cancel requested = %q", got)
	}
	if got := cancelRequestedText(model.Task{}); got != "no" {
		t.Errorf("no request = %q", got)
	}
	if got := taskStateText(model.TaskCanceled); !strings.Contains(got, "terminal") {
		t.Errorf("canceled = %q", got)
	}
	if got := taskStateText(model.TaskWorking); strings.Contains(got, "terminal") {
		t.Errorf("working = %q", got)
	}
	row := taskRowOf(asked)
	if row.ID != "t1" || row.State != model.TaskWorking || row.Cancel != "yes" {
		t.Errorf("row = %+v", row)
	}
}

// TestTaskVerbsAreNotProviderDependent: no adapter capability gates a
// task verb, so none carries the provider-dependent marker.
func TestTaskVerbsAreNotProviderDependent(t *testing.T) {
	a := newApp()
	n := 0
	for _, c := range heoCommands(a) {
		if !strings.HasPrefix(c.CommandPath(), "heo task ") {
			continue
		}
		n++
		if _, ok := c.Annotations[annotationCapability]; ok {
			t.Errorf("%s is marked provider-dependent", c.CommandPath())
		}
		if strings.Contains(c.Long, "Provider-dependent:") {
			t.Errorf("%s help says it is provider-dependent", c.CommandPath())
		}
	}
	if n != 4 {
		t.Fatalf("found %d task verbs, want list, get, watch and cancel", n)
	}
}
