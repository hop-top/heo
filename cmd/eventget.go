package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// eventDetailRows is an event as `event get` shows it in text.
func eventDetailRows(ev model.Event) [][2]string {
	return [][2]string{
		{"id", string(ev.ID)},
		{"kind", ev.Kind},
		{"recorded", stamp(ev.RecordedAt)},
		{"occurred", stamp(ev.OccurredAt)},
		{"resource", string(ev.Resource.Kind) + ":" + string(ev.Resource.ID)},
		{"inbox", orDash(string(ev.Inbox))},
		{"visibility", orDash(string(ev.Audience.Visibility))},
		{"actor", orDash(string(ev.Actor))},
		{"correlation", orDash(string(ev.Correlation))},
		{"payload", fmt.Sprintf("%s v%d", orDash(ev.Payload.Name), ev.Payload.Version)},
		{"revision", string(ev.Revision)},
	}
}

func (a *app) eventGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <event>",
		Short: "Show one committed event by its ID",
		Long: `Show the event <event>, the ID a list or a watch printed: its kind, the
resource it concerns, the inbox, its visibility, who and when, and the
schema of its payload. An event is one reference, never a message body;
` + "`message list`" + ` and ` + "`conversation get`" + ` show content. Read-only:
it opens no subscription and moves no checkpoint.

The event must be one you may read, and every way it can be out of
reach looks the same: an event that does not exist, one of another
tenant, one past event retention and one whose content is not public
(an internal note's) to a caller who is not a desk actor all exit 3
(NOT_FOUND) with the same message, so the answer never says whether
the event is there. Reading roles: desk, viewer, external.`,
		Example: `  heo event get evt_123
  heo event get evt_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (model.Event, error) {
				var ev model.Event
				err := a.desk(ctx, c, model.ActionEventList, model.Audience{}, func(s desk.Service) (err error) {
					ev, err = s.GetEvent(ctx, args[0])
					return err
				})
				return ev, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return writeRows(cmd.OutOrStdout(), eventDetailRows(res))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &model.Event{}, Version: "1.0"})
	return c
}
