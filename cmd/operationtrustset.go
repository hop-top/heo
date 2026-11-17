package cmd

import (
	"context"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/trustcfg"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// flagTrustKey names the trust key where MCP cannot pass a positional
// argument; flagCount and flagReset are the change.
const (
	flagTrustKey = "key"
	flagCount    = "count"
	flagReset    = "reset"
)

// trustSetRow is a manual trust change in a table.
type trustSetRow struct {
	Key     string `json:"key"     yaml:"key"     table:"KEY"`
	Outcome string `json:"outcome" yaml:"outcome" table:"OUTCOME"`
	Before  string `json:"before"  yaml:"before"  table:"BEFORE"`
	After   string `json:"after"   yaml:"after"   table:"AFTER"`
	Level   string `json:"level"   yaml:"level"   table:"LEVEL"`
}

func trustSetRowOf(r desk.TrustSetResult) trustSetRow {
	return trustSetRow{Key: r.Key, Outcome: r.Outcome, Before: countOf(r.Before), After: countOf(r.After), Level: r.After.LevelLabel}
}

func countOf(r desk.TrustRow) string {
	t := "-"
	if r.Threshold != nil {
		t = itoa(int64(*r.Threshold))
	}
	return itoa(r.Clean) + " of " + t
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (a *app) trustSetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set [<key>]",
		Short: "Promote or demote a trust key by hand: set its clean count, or reset it",
		Long: `Set the clean count of one trust key to --count N, or send it back to
zero with --reset, as a person: the desk promotes a class that has earned
it and demotes one that went wrong. <key> is the key trust list shows (or
--key, where a positional argument cannot be passed, over MCP); --inbox
names the mailbox whose channel counts it. The key must already be
counted: by a decision or an import.

The change is neither an approval nor a correction: uma's approval and
correction totals and the last correction stay as they are (a count above
the stored approvals raises that total to it), and held history shows
nothing. It is audited as a decision is: the result, the daemon's log and
the surfaces' audit sinks record the actor, the key, the count before and
after and --note.

A class under held_operations.trust.never_auto takes a count below its
threshold only (ask each, or in batches); at or above it is refused.
unsubscribe takes none. A plugin actor is refused (exit 5). Desk or
operator role. --dry-run reports the change and writes nothing.`,
		Example: `  heo operation trust set plugin:triage/uid:1001/support/state:archive --inbox support --count 3
  heo operation trust set plugin:triage/uid:1001/support/state:trash --inbox support --reset --note "wrong twice"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, _ := cmd.Flags().GetString(flagKey)
			switch {
			case key == "" && len(args) == 0:
				return output.UsageError("name the key: <key>, or --" + flagKey + " <key>")
			case key != "" && len(args) > 0:
				return output.UsageError("<key> and --" + flagKey + " both name the key: pass one")
			case len(args) > 0:
				key = args[0]
			}
			inbox, _ := cmd.Flags().GetString(flagInbox)
			if inbox == "" {
				return output.UsageError("--" + flagInbox + ": name the inbox whose channel counts the key")
			}
			req := desk.TrustSetRequest{Key: key, DryRun: cli.IsDryRun(cmd)}
			req.Reset, _ = cmd.Flags().GetBool(flagReset)
			if cmd.Flags().Changed(flagCount) {
				n, _ := cmd.Flags().GetInt64(flagCount)
				req.Count = &n
			}
			if (req.Count != nil) == req.Reset {
				return output.UsageError("name one change: --" + flagCount + " <n> or --" + flagReset)
			}
			req.Note, _ = cmd.Flags().GetString(flagDecisionNote)
			cfg, err := trustcfg.Load(a.r.Viper)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (desk.TrustSetResult, error) {
				var out desk.TrustSetResult
				err := a.desk(ctx, c, model.ActionOperationApprove, model.Audience{}, func(s desk.Service) error {
					in, err := s.FindInbox(ctx, inbox)
					if err != nil {
						return err
					}
					return a.runtime.Use(ctx, func(rt daemon.Runtime) error {
						svc, err := rt.Engine.For(model.Principal{Tenant: c.Tenant, Actor: daemon.ServiceActor})
						if err != nil {
							return err
						}
						out, err = s.SetTrust(ctx, svc.Operations(), cfg, in.Channel, req, time.Now())
						if err == nil && !out.DryRun && out.Outcome != desk.TrustSetUnchanged {
							hookLogger{a}.Info("trust set", "actor", out.Actor, "key", out.Key, "outcome", out.Outcome,
								"before", out.Before.Clean, "after", out.After.Clean, "note", out.Note)
						}
						return err
					})
				})
				return out, err
			})
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, res)
			}
			return output.Dispatch(cmd, a.r.Viper, []trustSetRow{trustSetRowOf(res)})
		},
	}
	c.Flags().String(flagKey, "", "The trust key, in place of <key> (for MCP, which cannot pass it)")
	c.Flags().String(flagInbox, "", "Inbox (key or ID) whose channel counts the key (required)")
	c.Flags().Int64(flagCount, 0, "Set the clean count to this")
	c.Flags().Bool(flagReset, false, "Send the clean count back to zero")
	c.Flags().String(flagDecisionNote, "", "Why: recorded with the change")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &desk.TrustSetResult{}, Version: "1.0"})
	return c
}
