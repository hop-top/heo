package cmd

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// formatJSONL is --format's value for one JSON record per line.
const formatJSONL = "jsonl"

// heldHistoryRow is one audit record in a table.
type heldHistoryRow struct {
	At        string `json:"at"        yaml:"at"        table:"AT"`
	Operation string `json:"operation" yaml:"operation" table:"OPERATION"`
	Event     string `json:"event"     yaml:"event"     table:"EVENT"`
	Who       string `json:"who"       yaml:"who"       table:"WHO"`
	Detail    string `json:"detail"    yaml:"detail"    table:"DETAIL"`
}

func heldHistoryRowOf(r desk.HeldAuditRecord) heldHistoryRow {
	row := heldHistoryRow{Operation: r.Operation, Event: r.Event, Who: orDash(r.Subject), At: "-"}
	if r.At != nil {
		row.At = stamp(*r.At)
	}
	var d []string
	switch r.Event {
	case desk.AuditProposed:
		d = append(d, string(r.Capability)+" on "+r.Target)
		if r.Reason != "" {
			d = append(d, r.Reason)
		}
	case desk.AuditEdited:
		d = append(d, "before "+compact(r.Before), "after "+compact(r.After))
	case desk.AuditApproved, desk.AuditRejected:
		if len(r.Applied) > 0 {
			d = append(d, "applied "+compact(r.Applied))
		}
		if r.Note != "" {
			d = append(d, "note: "+r.Note)
		}
	case desk.AuditExpired:
		d = append(d, "no decision before the deadline")
	}
	row.Detail = strings.Join(d, "; ")
	return row
}

// compact is a JSON value on one line, cut for a table cell.
func compact(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "-"
	}
	s := string(raw)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// writeHeldJSONL writes one record per line: the audit trail as a log
// a consumer appends to and tails.
func writeHeldJSONL(w io.Writer, recs []desk.HeldAuditRecord) error {
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) operationHeldHistoryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "history [<operation>]",
		Short: "Show the audit trail of held operations: who decided, when, what changed",
		Long: `Show the audit trail of a held operation, or of every held operation
you may read when none is named: when it was proposed and by whom, with
the preview the decider was shown; each edit with its author, the time
and the arguments before and after; and the decision (approved,
rejected) with who made it, when, the note and, for an edit, the
arguments applied, or the expiry that closed it with no decision.

The trail is read from uma's held operation and its events, never kept
by heo: whatever uma shows you of the operation, and nothing else. An
operation you may not read, or one that was never held, is not found
(exit 3), and the listing leaves it out. Records are oldest first
within an operation. Each record carries the consent audit vocabulary:
capability (the operation kind), decision (allow or deny) and, for an
expiry, reason expired.

Name one operation as <operation> or, where a positional argument cannot be
passed (MCP), as --operation; not both.

` + "`--format jsonl`" + ` writes one record per line, for a log or a pipe; json and
yaml write the list. Read-only.`,
		Example: `  heo operation held history
  heo operation held history op_123
  heo operation held history --state expired --since 2026-10-01
  heo operation held history --format jsonl`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f desk.OperationFilter
			var err error
			named, _ := cmd.Flags().GetString(flagOperation)
			if named != "" && len(args) > 0 {
				return output.UsageError("<operation> and --" + flagOperation + " both name the operation: pass one")
			}
			one := named
			if len(args) == 1 {
				one = args[0]
			}
			f.Kinds, _ = cmd.Flags().GetStringSlice(flagKind)
			f.States, _ = cmd.Flags().GetStringSlice(flagState)
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			if f.Since, err = timeFlag(cmd, flagSince); err != nil {
				return err
			}
			if f.Until, err = timeFlag(cmd, flagUntil); err != nil {
				return err
			}
			if one != "" && (len(f.Kinds) > 0 || len(f.States) > 0 || f.Inbox != "" || f.Since != nil || f.Until != nil) {
				return output.UsageError("<operation> names one operation: --" + flagKind + ", --" + flagState + ", --" +
					flagInbox + ", --" + flagSince + " and --" + flagUntil + " narrow the listing")
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) ([]desk.HeldAuditRecord, error) {
				var out []desk.HeldAuditRecord
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) (err error) {
					if one != "" {
						out, err = s.HeldHistory(ctx, one)
					} else {
						out, err = s.ListHeldHistory(ctx, f)
					}
					return err
				})
				if out == nil {
					out = []desk.HeldAuditRecord{}
				}
				return out, err
			})
			if err != nil {
				return err
			}
			switch {
			case a.r.Viper.GetString("format") == formatJSONL:
				return writeHeldJSONL(cmd.OutOrStdout(), res)
			case a.structured():
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			rows := make([]heldHistoryRow, len(res))
			for i, r := range res {
				rows[i] = heldHistoryRowOf(r)
			}
			return output.Dispatch(cmd, a.r.Viper, rows)
		},
	}
	c.Flags().String(flagOperation, "", "One held operation, in place of <operation> (for MCP, which cannot pass it)")
	c.Flags().StringSlice(flagKind, nil, "Only held operations of these kinds, such as message.edit")
	c.Flags().StringSlice(flagState, nil, "Only held operations in these states, such as held, expired or queued")
	c.Flags().String(flagInbox, "", "Only held operations on this inbox (key or ID)")
	c.Flags().String(flagSince, "", "Only operations last changed at or after this time (RFC 3339 or a date)")
	c.Flags().String(flagUntil, "", "Only operations last changed before this time (RFC 3339 or a date)")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &[]desk.HeldAuditRecord{}, Version: "1.0"})
	return c
}
