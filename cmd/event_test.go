package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/cmdsurface"
	"hop.top/uma/model"
)

// TestWatchAcksOnlyWhatItWrote: the watch loop writes a batch's lines
// before it acknowledges the batch, retries a transient refusal rather
// than ending, acknowledges a batch written as it is interrupted, and
// stops with exit 0 and the checkpoint on stderr.
func TestWatchAcksOnlyWhatItWrote(t *testing.T) {
	a := newApp()
	a.r.Viper.Set("format", output.JSON)
	var out, errb bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	ev := model.Event{ID: "evt_1", Kind: model.EventMessageCreated, Audience: model.Audience{Visibility: model.Public}}
	var log []string
	polls := 0
	step := func(ctx context.Context, st watchStep) (watchStepOut, error) {
		log = append(log, st.Op)
		switch st.Op {
		case stepOpen:
			return watchStepOut{Subscription: "sub_1", Checkpoint: "c0"}, nil
		case stepPoll:
			polls++
			switch polls {
			case 1:
				return watchStepOut{}, output.TransientError("rate limited")
			case 2:
				if out.Len() != 0 {
					t.Fatal("lines written before the batch arrived")
				}
				// Interrupted while the batch is in hand: it is still
				// written and acknowledged.
				cancel()
				return watchStepOut{Subscription: "sub_1", Events: []model.Event{ev}, Token: "tok"}, nil
			}
			t.Fatal("polled after the interrupt")
		case stepAck:
			if st.Token != "tok" || !strings.Contains(out.String(), `"id":"evt_1"`) {
				t.Fatalf("ack %+v before its line was written (stdout %q)", st, out.String())
			}
			if ctx.Err() != nil {
				t.Fatal("ack ran on the interrupted context")
			}
			return watchStepOut{Subscription: "sub_1", Checkpoint: "c1"}, nil
		}
		return watchStepOut{}, nil
	}
	if err := a.watch(ctx, cmd, step, desk.WatchStart{Consumer: "c"}, 0); err != nil {
		t.Fatalf("watch = %v, want a clean stop", err)
	}
	if got := strings.Join(log, ","); got != "open,poll,poll,ack" {
		t.Fatalf("steps = %s", got)
	}
	var line model.Event
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &line); err != nil || line.ID != "evt_1" {
		t.Fatalf("stdout = %q (%v), want one NDJSON line", out.String(), err)
	}
	if !strings.Contains(errb.String(), "consumer c at checkpoint c1") {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// TestStreamingCommandsWithheldFromServedSurfaces: a command that runs
// until its caller stops following, or for as long as a provider takes
// to answer page after page, would hold the daemon's one served call,
// so each is kept off REST, SSE, gRPC and MCP and left on the control
// socket. event watch, task watch and history fetch are the only such
// commands; the others carry no withholding.
func TestStreamingCommandsWithheldFromServedSurfaces(t *testing.T) {
	a := newApp()
	streaming := map[string]bool{"heo event watch": true, "heo task watch": true, "heo history fetch": true}
	found := 0
	for _, c := range heoCommands(a) {
		got, withheld := c.Annotations[cmdsurface.AnnotationWithholdFrom]
		path := c.CommandPath()
		if !streaming[path] {
			if withheld {
				t.Errorf("%s: withheld from %q, but it does not stream", path, got)
			}
			continue
		}
		found++
		surfaces, err := cmdsurface.ParseWithholdFrom(got)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		want := []cmdsurface.Surface{cmdsurface.SurfaceREST, cmdsurface.SurfaceSSE, cmdsurface.SurfaceRPC, cmdsurface.SurfaceMCP}
		if len(surfaces) != len(want) {
			t.Fatalf("%s withheld from %v, want %v", path, surfaces, want)
		}
		for _, s := range want {
			if !slices.Contains(surfaces, s) {
				t.Errorf("%s: not withheld from %s", path, s)
			}
		}
		for _, s := range []cmdsurface.Surface{cmdsurface.SurfaceSocket, cmdsurface.SurfaceCLI} {
			if slices.Contains(surfaces, s) {
				t.Errorf("%s: withheld from %s, which carries the terminal's watch", path, s)
			}
		}
	}
	if found != len(streaming) {
		t.Fatalf("found %d of %d streaming commands", found, len(streaming))
	}
}

// TestEventListStart: a list starts at the beginning unless it is told
// otherwise, at the tail on request or at a cursor; a cursor and --from
// together, and a start that is neither, are usage errors.
func TestEventListStart(t *testing.T) {
	a := newApp()
	parse := func(args ...string) (desk.ListStart, error) {
		c := a.eventListCmd()
		if err := c.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		return listStartOf(c)
	}
	for _, tc := range []struct {
		args []string
		want desk.ListStart
	}{
		{nil, desk.ListStart{From: desk.StartBeginning}},
		{[]string{"--from", "tail"}, desk.ListStart{From: desk.StartTail}},
		{[]string{"--cursor", "pg.42"}, desk.ListStart{Cursor: "pg.42"}},
	} {
		if got, err := parse(tc.args...); err != nil || got != tc.want {
			t.Errorf("%v = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{{"--cursor", "pg.42", "--from", "tail"}, {"--cursor", "pg.42", "--from", "beginning"}, {"--from", "middle"}} {
		if _, err := parse(args...); err == nil || !strings.Contains(err.Error(), "--") {
			t.Errorf("%v = %v, want a usage error", args, err)
		}
	}
}

// TestEventListIsServed: event list is a plain request and reply, so no
// surface withholds it; it only reads.
func TestEventListIsServed(t *testing.T) {
	a := newApp()
	c := a.eventListCmd()
	if _, withheld := c.Annotations[cmdsurface.AnnotationWithholdFrom]; withheld {
		t.Fatal("event list is withheld from a served surface")
	}
	for _, flag := range []string{"inbox", "kind", "from", "cursor", "limit"} {
		if c.Flags().Lookup(flag) == nil {
			t.Errorf("event list has no --%s", flag)
		}
	}
	for _, flag := range []string{"conversation", "since", "consumer"} {
		if c.Flags().Lookup(flag) != nil {
			t.Errorf("event list has --%s, which uma's event list does not honour", flag)
		}
	}
}

// TestExpiredCursorExplainsTheWayBack: a watch and a list explain a
// purged position alike, and each names how to start over.
func TestExpiredCursorExplainsTheWayBack(t *testing.T) {
	a := newApp()
	purged := model.NewError(model.CursorExpired, "behind retention")
	for name, err := range map[string]error{
		"watch": a.watchFailure(purged, desk.WatchStart{}),
		"list":  a.listFailure(purged),
	} {
		var oe *output.Error
		if !errors.As(err, &oe) || !strings.Contains(oe.Message, "purged by event retention") ||
			!strings.Contains(oe.SuggestedFix, "--from beginning") || !strings.Contains(oe.SuggestedFix, "reconcile") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got := a.listFailure(model.NewError(model.NotFound, "x")); !model.IsCode(got, model.NotFound) {
		t.Errorf("another failure = %v, want it unchanged", got)
	}
}
