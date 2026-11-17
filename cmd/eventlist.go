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

// eventRowOut is an event in a listing table: the watch's row.
type eventRowOut struct {
	Recorded   string `json:"recorded"   yaml:"recorded"   table:"RECORDED"`
	Kind       string `json:"kind"       yaml:"kind"       table:"KIND"`
	Resource   string `json:"resource"   yaml:"resource"   table:"RESOURCE"`
	Inbox      string `json:"inbox"      yaml:"inbox"      table:"INBOX"`
	Visibility string `json:"visibility" yaml:"visibility" table:"VISIBILITY"`
	ID         string `json:"id"         yaml:"id"         table:"ID,id"`
}

func eventRowOf(ev model.Event) eventRowOut {
	return eventRowOut{
		Recorded: stamp(ev.RecordedAt), Kind: ev.Kind, Resource: string(ev.Resource.Kind) + ":" + string(ev.Resource.ID),
		Inbox: orDash(string(ev.Inbox)), Visibility: orDash(string(ev.Audience.Visibility)), ID: string(ev.ID),
	}
}

func (a *app) eventListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List committed events a page at a time, without a subscription",
		Long: `List the events uma committed that you may read, oldest first, a page
at a time, starting at --from beginning (default: the oldest retained
event) or tail, or at --cursor, a position an earlier page's next or a
watch's checkpoint printed. Each event is one reference — its kind, the
resource it concerns, the inbox, its visibility, its ID — never a
message body; ` + "`message list`" + ` and ` + "`conversation get`" + ` show content.
--inbox (key or ID) and --kind (repeat or comma-separate) narrow it.
Read-only: unlike ` + "`event watch`" + ` it opens no subscription and moves no
checkpoint, so it can be repeated and run by many callers at once.

--limit bounds the events uma scans for the page, not the events shown:
an event you may not read counts against it and is dropped without a
trace, so a page can be short or empty and still not be the last. Always
continue at the page's next (--cursor) until complete is true; the
next also continues a ` + "`watch`" + `, or is a checkpoint to start one at.

Content that is not public (internal notes, restricted delivery
updates) is listed to desk actors alone, checked for every event.

heo never skips history silently. A position behind event retention is
refused (CURSOR_EXPIRED, exit 4): the events after it were purged, so
start over with --from beginning and reconcile from current state.
Reading roles: desk, viewer, external.`,
		Example: `  heo event list
  heo event list --inbox web --kind message.created --limit 20
  heo event list --from tail --format json
  heo event list --cursor pg.42 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			start, err := listStartOf(cmd)
			if err != nil {
				return err
			}
			var f desk.EventFilter
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			f.Kinds, _ = cmd.Flags().GetStringSlice(flagKind)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Event], error) {
				var out page[model.Event]
				err := a.desk(ctx, c, model.ActionEventList, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListEvents(ctx, f, start, pg.Limit)
					out = pageFrom(p)
					return a.listFailure(err)
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]eventRowOut, 0, len(res.Items))
			for _, ev := range res.Items {
				rows = append(rows, eventRowOf(ev))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Only events of this inbox (key or ID)")
	c.Flags().StringSlice(flagKind, nil, "Only events of these kinds, such as message.created")
	c.Flags().String(flagFrom, desk.StartBeginning, "Where a list without --cursor starts: beginning (oldest retained) or tail (the head)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &page[model.Event]{}, Version: "1.0"})
	return c
}

// listStartOf reads --from and --cursor.
func listStartOf(cmd *cobra.Command) (desk.ListStart, error) {
	var st desk.ListStart
	cursor, _ := cmd.Flags().GetString(flagCursor)
	st.From, _ = cmd.Flags().GetString(flagFrom)
	if st.From != desk.StartTail && st.From != desk.StartBeginning {
		return st, output.UsageError(fmt.Sprintf("--%s %q: want %s or %s", flagFrom, st.From, desk.StartBeginning, desk.StartTail))
	}
	if cursor != "" && cmd.Flags().Changed(flagFrom) {
		return st, output.UsageError(fmt.Sprintf("--%s and --%s both say where to start: pass one", flagCursor, flagFrom))
	}
	if cursor != "" {
		st.From, st.Cursor = "", model.Cursor(cursor)
	}
	return st, nil
}

// listFailure explains a list that cannot go on: a position behind event
// retention, as a watch explains it.
func (a *app) listFailure(err error) error {
	if model.IsCode(err, model.CursorExpired) {
		return a.expiredCursor(err, "start the list over: `"+a.heoCommand("event list --from beginning")+"` (oldest retained) or --from tail")
	}
	return err
}
