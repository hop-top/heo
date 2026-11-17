package cmd

import (
	"context"
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

// trustRow is a trust key in a listing table.
type trustRow struct {
	Proposer   string `json:"proposer"     yaml:"proposer"     table:"PROPOSER"`
	OnBehalfOf string `json:"on_behalf_of" yaml:"on_behalf_of" table:"ON BEHALF OF"`
	Channel    string `json:"channel"      yaml:"channel"      table:"CHANNEL"`
	Verb       string `json:"verb"         yaml:"verb"         table:"VERB"`
	Class      string `json:"class"        yaml:"class"        table:"CLASS"`
	Clean      int64  `json:"clean"        yaml:"clean"        table:"CLEAN"`
	Corrected  string `json:"last_correction" yaml:"last_correction" table:"LAST CORRECTION"`
	Automatic  string `json:"automatic"    yaml:"automatic"    table:"AUTOMATIC"`
}

func trustRowOf(r desk.TrustRow) trustRow {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	last := "-"
	if r.LastCorrectionAt != nil {
		last = stamp(*r.LastCorrectionAt)
	}
	return trustRow{Proposer: dash(r.Proposer), OnBehalfOf: dash(r.OnBehalfOf), Channel: dash(r.Channel), Verb: dash(r.Verb),
		Class: dash(r.Class), Clean: r.Clean, Corrected: last, Automatic: yesNo(r.Automatic)}
}

func (a *app) operationTrustCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "trust",
		Short: "Review the trust earned by the operations plugin actors proposed",
		Long: `uma counts the consecutive clean approvals under each trust key
(proposer, on-behalf-of actor, mailbox, verb and class); heo's trust
ladder decides from those counts which keys run automatically. ` + "`trust list`" + `
shows the counts and the decision; ` + "`trust import`" + ` seeds them from an
approval history earned elsewhere; ` + "`trust set`" + ` moves a count by hand. Every command runs in the profile's
daemon (none running exits 70).`,
		Args: cobra.NoArgs,
	}
	cli.SetHierarchical(grp)
	grp.AddCommand(a.trustListCmd(), a.trustImportCmd(), a.trustSetCmd())
	return grp
}

func (a *app) trustListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the trust keys of a mailbox: counts, threshold, level and whether each is automatic",
		Long: `List the trust keys uma counts for the channel of --inbox, in key
order, a page at a time (--limit, --cursor with the previous page's next).
Each shows the parts of its key (proposer, on-behalf-of actor, mailbox,
verb, class), its clean count, its last correction and whether the trust
ladder, under the current configuration (thresholds, seeded classes,
approval-always mailboxes, unattended, supervised week), runs it
automatically; --format json also gives the counts' totals, the
threshold, the level (ask_each, ask_in_batches, auto or never_auto, derived
by the daemon) and, for a held key, why. --label name=value keeps the keys with that label. Read-only.`,
		Example: `  heo operation trust list --inbox support
  heo operation trust list --inbox support --label verb=state --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			inbox, _ := cmd.Flags().GetString(flagInbox)
			if inbox == "" {
				return output.UsageError("--" + flagInbox + ": name the inbox whose channel's trust to list")
			}
			labels, _ := cmd.Flags().GetStringToString(flagLabel)
			cfg, err := trustcfg.Load(a.r.Viper)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[desk.TrustRow], error) {
				out := page[desk.TrustRow]{Items: []desk.TrustRow{}}
				err := a.desk(ctx, c, model.ActionOperationRead, model.Audience{}, func(s desk.Service) error {
					in, err := s.FindInbox(ctx, inbox)
					if err != nil {
						return err
					}
					return a.runtime.Use(ctx, func(rt daemon.Runtime) error {
						svc, err := rt.Engine.For(model.Principal{Tenant: c.Tenant, Actor: daemon.ServiceActor})
						if err != nil {
							return err
						}
						p, err := desk.ListTrust(ctx, svc.Operations(), cfg, in.Channel, labels, pg, time.Now())
						if err != nil {
							return err
						}
						out.Items, out.Next, out.Complete = p.Items, p.Next, p.Complete
						return nil
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
			rows := make([]trustRow, len(res.Items))
			for i, r := range res.Items {
				rows[i] = trustRowOf(r)
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagInbox, "", "Inbox (key or ID) whose channel's trust keys to list")
	c.Flags().StringToString(flagLabel, nil, "Only keys with this label, name=value (repeatable)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[desk.TrustRow]{}, "1.0")
	return c
}
