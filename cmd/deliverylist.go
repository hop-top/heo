package cmd

import (
	"context"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// flagState filters a delivery list by state.
const flagState = "state"

// deliveryRow is a delivery in a listing: the operation uma holds for an
// outbound write the caller submitted, with what the write was.
type deliveryRow struct {
	Operation    string `json:"operation"    yaml:"operation"    table:"OPERATION"`
	State        string `json:"state"        yaml:"state"        table:"STATE"`
	Kind         string `json:"kind"         yaml:"kind"         table:"KIND"`
	Conversation string `json:"conversation" yaml:"conversation" table:"CONVERSATION"`
	Updated      string `json:"updated"      yaml:"updated"      table:"UPDATED"`
	Key          string `json:"key"          yaml:"key"          table:"KEY"`
	Inbox        string `json:"inbox"        yaml:"inbox"`
	Verdict      string `json:"verdict"      yaml:"verdict"`
	Revision     string `json:"revision"     yaml:"revision"`
	// NextAttemptAt is when uma next works on the delivery (an
	// outcome_unknown one is reconciled then; a retry_pending one is
	// sent again), when it has scheduled that.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty" yaml:"next_attempt_at,omitempty"`
}

func deliveryRowOf(op model.Operation) deliveryRow {
	o := operationDetail(op)
	var t model.Ref
	if op.Key != nil {
		t = op.Key.Target
	}
	var inbox, conv model.ID
	switch t.Kind {
	case model.KindInbox:
		inbox = t.ID
	case model.KindConversation:
		inbox, conv = t.Inbox, t.ID
	default:
		inbox = t.Inbox
	}
	if m := op.ResultMessage; m != nil {
		inbox, conv = m.Conversation.Inbox.ID, m.Conversation.ID
	}
	return deliveryRow{
		Operation: string(op.Ref.ID), State: string(op.State), Kind: o.Kind, Conversation: orDash(string(conv)),
		Updated: stamp(op.UpdatedAt), Key: o.Key, Inbox: string(inbox), Verdict: string(desk.VerdictOf(op.State)),
		Revision: string(op.Revision), NextAttemptAt: op.NextAttemptAt,
	}
}

func (a *app) deliveryListCmd() *cobra.Command {
	states := make([]string, len(desk.DeliveryStates))
	for i, s := range desk.DeliveryStates {
		states[i] = string(s)
	}
	c := &cobra.Command{
		Use:   "list",
		Short: "List the deliveries of the writes you submitted, newest change first",
		Long: `List the outbound writes you submitted — replies, notes, conversation
starts, issue actions — with the state of each: the operation uma holds
for the key you submitted it under. Newest change first, so a delivery
that settled moves to the top, a page at a time (--limit, --cursor with
the previous page's next). A page can be short and still have more:
follow next until it is complete.

--state keeps deliveries in these states (` + strings.Join(states, ", ") + `;
repeat or comma-separate): ` + "`--state outcome_unknown`" + ` is what to reconcile,
` + "`--state dead_lettered`" + ` what ` + "`delivery retry`" + ` may replay, ` + "`--state failed`" + `
what to send again under a new key. --inbox (key or ID) and
--conversation (ID) keep one inbox's or conversation's (name the inbox
too when your role cannot read conversations, as the operator's cannot),
and --since and --until bound the time of the delivery's last change,
[since, until).

The list is uma's operation ledger (see ` + "`operation list`" + `) restricted to the
outbound kinds: replies, notes, conversation starts and issue actions.
You see the writes you sent only, whatever else your role may read by ID;
one past uma's operation retention is not listed. Read-only.
Desk and operator roles.`,
		Example: `  heo delivery list --state outcome_unknown
  heo delivery list --inbox support --state failed,dead_lettered
  heo delivery list --conversation conv_123 --since 2026-10-01 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.OperationFilter
			f.States, _ = cmd.Flags().GetStringSlice(flagState)
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			f.Conversation, _ = cmd.Flags().GetString(flagConversation)
			if f.Since, err = timeFlag(cmd, flagSince); err != nil {
				return err
			}
			if f.Until, err = timeFlag(cmd, flagUntil); err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[deliveryRow], error) {
				out := page[deliveryRow]{Items: []deliveryRow{}}
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
					f.Actor = c.Actor
					p, err := s.ListDeliveries(ctx, f, pg)
					if err != nil {
						return err
					}
					out.Next, out.Complete = p.Next, p.Complete
					for _, op := range p.Items {
						out.Items = append(out.Items, deliveryRowOf(op))
					}
					return nil
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return renderPage(a, cmd, res.Items, res.Next, res.Complete)
		},
	}
	c.Flags().StringSlice(flagState, nil, "Only deliveries in these states: "+strings.Join(states, ", "))
	c.Flags().String(flagInbox, "", "Only deliveries through this inbox (key or ID)")
	c.Flags().String(flagConversation, "", "Only deliveries in this conversation (ID)")
	c.Flags().String(flagSince, "", "Only deliveries last changed at or after this time (RFC 3339 or a date)")
	c.Flags().String(flagUntil, "", "Only deliveries last changed before this time (RFC 3339 or a date)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[deliveryRow]{}, "1.0")
	return c
}
