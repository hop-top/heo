package cmd

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// Flags the contact and identity leaves share.
const (
	flagContact   = "contact"
	flagKind      = "kind"
	flagReason    = "reason"
	flagReference = "reference"
)

// contactView is a contact and the identities of it the caller may
// read.
type contactView struct {
	Contact    model.Contact    `json:"contact"    yaml:"contact"`
	Identities []model.Identity `json:"identities" yaml:"identities"`
}

// contactRow is a contact in a listing table.
type contactRow struct {
	ID         string `json:"id"          yaml:"id"          table:"ID,id"`
	Kind       string `json:"kind"        yaml:"kind"        table:"KIND"`
	Name       string `json:"name"        yaml:"name"        table:"NAME"`
	Revision   string `json:"revision"    yaml:"revision"    table:"REVISION"`
	MergedInto string `json:"merged_into" yaml:"merged_into" table:"MERGED INTO,id"`
}

func contactRowOf(c model.Contact) contactRow {
	return contactRow{ID: string(c.Ref.ID), Kind: c.Kind, Name: orDash(c.DisplayName), Revision: string(c.Revision), MergedInto: mergedInto(c)}
}

func (a *app) contactCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "contact",
		Short: "Read, create, edit and merge contacts: the people, bots and agents behind channel identities",
		Long: `Read, create, edit and merge contacts. A contact is who stands behind
one or more channel identities (` + "`heo identity`" + `): each identity points at
exactly one contact. Ingress gives every new sender a provisional contact
of its own; heo never matches identities to contacts by itself — an
identity joins another contact only when a desk actor links it
(` + "`identity link`" + `), and two contacts become one only when a desk actor
merges them (` + "`contact merge`" + `), after a preview.

Every command runs in the profile's daemon (none running exits 70).
Reading is open to the desk and viewer roles; creating, editing and
merging to the desk role. Any other caller is refused (exit 5).`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.contactListCmd(), a.contactGetCmd(), a.contactCreateCmd(), a.contactUpdateCmd(), a.contactMergeCmd())
	return grp
}

func (a *app) contactListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the contacts you may read",
		Long: `List the contacts you may read, a page at a time (--limit, --cursor
with the previous page's next). --kind (` + strings.Join(desk.ContactKinds(), ", ") + `;
repeat or comma-separate) narrows it. A contact left with no identities
after a link stays listed until retention removes it. Read-only.`,
		Example: `  heo contact list
  heo contact list --kind person --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			kinds, _ := cmd.Flags().GetStringSlice(flagKind)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Contact], error) {
				var out page[model.Contact]
				err := a.desk(ctx, c, model.ActionContactRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListContacts(ctx, kinds, pg)
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
			rows := make([]contactRow, 0, len(res.Items))
			for _, ct := range res.Items {
				rows = append(rows, contactRowOf(ct))
			}
			return renderPage(a, cmd, rows, res.Next, res.Complete)
		},
	}
	c.Flags().StringSlice(flagKind, nil, "Only contacts of these kinds: "+strings.Join(desk.ContactKinds(), ", "))
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[model.Contact]{}, "1.0")
	return c
}

func (a *app) contactGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <contact>",
		Short: "Show one contact and its identities",
		Long: `Show the contact <contact>, by record ID: its kind, name and revision,
and the identities of it you may read, each with its inbox and link
assurance. A contact that was merged into another reads as merged into
it (` + "`merged into`" + `): its identities are on that one. Read-only. No such
contact, or one you may not read, exits 3.`,
		Example: `  heo contact get ct_123
  heo contact get ct_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (contactView, error) {
				var out contactView
				err := a.desk(ctx, c, model.ActionContactRead, model.Audience{}, func(s desk.Service) (err error) {
					if out.Contact, err = s.FindContact(ctx, args[0]); err != nil {
						return err
					}
					out.Identities, err = s.ContactIdentities(ctx, out.Contact.Ref)
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
			ct := res.Contact
			if err := writeRows(cmd.OutOrStdout(), append(contactRows(ct), [2]string{"identities", fmt.Sprint(len(res.Identities))})); err != nil {
				return err
			}
			if len(res.Identities) == 0 {
				return nil
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return err
			}
			return output.Dispatch(cmd, a.r.Viper, identityRowsOf(res.Identities))
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &contactView{}, "1.0")
	return c
}
