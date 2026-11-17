package cmd

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

func (a *app) attentionCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "attention",
		Short: "Read what waits on you: unread conversations and held operations in one feed",
		Long: `Read what waits on you in one place: the conversations unread for you
that are open or pending and not snoozed, and the operations plugin
actors proposed that wait for a decision. Every command runs in the
profile's daemon (none running exits 70) and is served on every surface.`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	grp.AddCommand(a.attentionListCmd())
	return grp
}

// attentionRow is a row of the feed in a table.
type attentionRow struct {
	Kind   string `json:"kind"   yaml:"kind"   table:"KIND"`
	At     string `json:"at"     yaml:"at"     table:"CHANGED"`
	Inbox  string `json:"inbox"  yaml:"inbox"  table:"INBOX"`
	ID     string `json:"id"     yaml:"id"     table:"ID,id"`
	Status string `json:"status" yaml:"status" table:"STATUS"`
	Held   string `json:"held"   yaml:"held"   table:"HELD"`
}

func attentionRowOf(it desk.AttentionItem) attentionRow {
	r := attentionRow{Kind: it.Kind, At: stamp(it.At), Inbox: orDash(it.Inbox)}
	ids := make([]string, len(it.Held))
	for i, h := range it.Held {
		ids[i] = h.ID
	}
	r.Held = orDash(strings.Join(ids, ","))
	if it.Conversation != nil {
		r.ID, r.Status = string(it.Conversation.Ref.ID), it.Conversation.Status
		return r
	}
	r.ID, r.Status = it.Held[0].ID, it.Held[0].Status
	return r
}

func (a *app) attentionListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List what waits on you: unread conversations and held operations, newest change first",
		Long: `List what waits on you in one feed, newest change first, a page at a
time (--limit, --cursor with the previous page's next; one cursor pages
both kinds). A row is one of:

  conversation  a conversation unread for you, open or pending and not
                snoozed, with the operations held on it (held), such as a
                proposed message edit or a proposed reply;
  operation     a held operation alone: its target is no conversation (a
                label create), or a conversation that is not waiting
                (read, snoozed, resolved).

Each row's time (at) is its newest change: for a conversation, the newest
of its last activity (see ` + "`conversation list`" + `) and the last change of each
operation held on it; for an operation, its own last change. --inbox (key or
ID) keeps the rows of that inbox; a held operation whose target has no inbox
(a label) is not in an inbox's feed. --updated-since keeps the rows
that changed after a time (RFC 3339 or a date).

The unread, open or pending and not snoozed conditions are the conversation
filters of ` + "`conversation list`" + ` at their strictest; the conversation
rows are those it lists with ` + "`--unread --status open,pending`" + `. The
held operations are those ` + "`operation held list`" + ` lists. Both are uma's
reads, authorized as the caller may read them; reading the feed needs the
rights to read workflow state and operations, which desk actors hold.

Cost: uma keeps no activity time to order by, so heo reads every waiting
conversation and its held operations, and every held operation, before it
cuts the first page; the cost grows with them, not with --limit. A cursor
names the last row of its page and is valid with the same filters. Read-only.`,
		Example: `  heo attention list
  heo attention list --inbox support --limit 20
  heo attention list --updated-since 2026-10-01T09:00:00Z --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.AttentionFilter
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			if f.UpdatedSince, err = timeFlag(cmd, flagUpdatedSince); err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[desk.AttentionItem], error) {
				var out page[desk.AttentionItem]
				// The feed reads conversations' workflow state, then operations:
				// each action is checked for the caller.
				pol, err := a.authorization()
				if err != nil {
					return out, errmap.Of(err)
				}
				if err := permitted(pol, c, model.ActionOperationRead, model.Audience{}); err != nil {
					return out, err
				}
				err = a.desk(ctx, c, model.ActionConversationReadWorkflow, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListAttention(ctx, f, pg)
					out = pageFrom(p)
					return err
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]attentionRow, len(res.Items))
			for i, it := range res.Items {
				rows[i] = attentionRowOf(it)
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Only rows of this inbox (key or ID)")
	c.Flags().String(flagUpdatedSince, "", "Only rows that changed after this time (RFC 3339 or a date)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[desk.AttentionItem]{}, "1.0")
	return c
}
