//go:build heoe2e

package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/bridge"
	"hop.top/heo/internal/control"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/runtime/bus"
	"hop.top/uma/adapter/chat/slack"
	"hop.top/uma/model"
)

func init() {
	// The fake provider that stands in for Slack's modals reports its
	// interactions as Slack does, under its own type: its trigger_id is
	// read as slack.chat's is, so the verbs run against it end to end.
	modalTrigger = func(in model.Interaction) (slack.Trigger, bool) {
		if in.Source == adapterstest.ViewsType {
			in.Source = slack.Type
		}
		return slack.TriggerOf(in)
	}
}

// busLogName is the file, in the state directory, the e2e build appends
// every bus event to: one JSON line per event and scope, written as the
// event is received, so a daemon killed outright has still logged what
// it published.
const busLogName = "e2e-bus.jsonl"

// busLine is one line of that file.
type busLine struct {
	PID     int             `json:"pid"`
	Scope   string          `json:"scope"`
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// observeBus subscribes one recorder of each scope to every topic and
// appends what each receives to the state directory's busLogName: the e2e tests
// see what hooks of each scope would, and a public recorder never
// receives what only the desk may. The release build has no recorder.
func (a *app) observeBus() {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		return
	}
	// The state directory is no longer created by an operation history
	// file, so the recorder makes it.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, busLogName)
	var mu sync.Mutex
	record := func(scope string) func(bus.Event) {
		return func(e bus.Event) {
			raw, _ := json.Marshal(e.Payload)
			line, _ := json.Marshal(busLine{PID: os.Getpid(), Scope: scope, Topic: string(e.Topic), Payload: raw})
			mu.Lock()
			defer mu.Unlock()
			f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the e2e test's own file
			if err != nil {
				return
			}
			_, _ = f.Write(append(line, '\n'))
			_ = f.Close()
		}
	}
	a.hub.Subscribe(bridge.ScopeDesk, "#", record("desk"))
	a.hub.Subscribe(bridge.ScopePublic, "#", record("public"))
}

// e2eDestroyResult is what the e2e destructive command reports.
type e2eDestroyResult struct {
	Actor   string `json:"actor"`
	Surface string `json:"surface"`
}

// e2eWhoamiResult is what the e2e read command reports: the caller and
// surface the daemon served it for.
type e2eWhoamiResult struct {
	Actor   string `json:"actor"`
	Tenant  string `json:"tenant"`
	Surface string `json:"surface"`
	PID     int    `json:"pid"`
}

// extraCommands adds, in an e2e build only, a daemon-backed destructive
// command that changes nothing, which the e2e tests drive the surfaces'
// destructive ceiling with, and a daemon-backed read served on every
// surface, which they compare the surfaces' callers with (`daemon
// ping` is served over the socket only).
// Release builds never carry them.
func (a *app) extraCommands() []*cobra.Command {
	grp := &cobra.Command{Use: "e2e", Short: "Commands only e2e builds carry", Args: cobra.NoArgs}
	destroy := &cobra.Command{
		Use:   "destroy",
		Short: "A daemon-backed destructive command that changes nothing",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(_ context.Context, c control.Caller) (e2eDestroyResult, error) {
				return e2eDestroyResult{Actor: string(c.Actor), Surface: string(c.Surface)}, nil
			})
			if err != nil {
				return err
			}
			return output.Dispatch(cmd, a.r.Viper, res)
		},
	}
	daemonCommand(destroy)
	cli.SetSideEffect(destroy, cli.SideEffectDestructive)
	cli.SetIdempotency(destroy, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(destroy, cli.OutputSchema{Type: &e2eDestroyResult{}, Version: "1.0"})
	whoami := &cobra.Command{
		Use:   "whoami",
		Short: "A daemon-backed read naming the caller the daemon served",
		Long:  "Report the actor, tenant and surface the daemon served this call for, and the daemon's pid.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(_ context.Context, c control.Caller) (e2eWhoamiResult, error) {
				return e2eWhoamiResult{Actor: string(c.Actor), Tenant: string(c.Tenant),
					Surface: string(c.Surface), PID: os.Getpid()}, nil
			})
			if err != nil {
				return err
			}
			return output.Dispatch(cmd, a.r.Viper, res)
		},
	}
	daemonCommand(whoami)
	cli.SetSideEffect(whoami, cli.SideEffectRead)
	cli.SetIdempotency(whoami, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(whoami, cli.OutputSchema{Type: &e2eWhoamiResult{}, Version: "1.0"})
	grp.AddCommand(destroy, whoami)
	return []*cobra.Command{grp}
}
