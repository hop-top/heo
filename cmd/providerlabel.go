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

// providerLabelRow is a provider mailbox in a listing: the machine form
// too, one object per mailbox in a stable order.
type providerLabelRow struct {
	Channel string `json:"channel" yaml:"channel" table:"CHANNEL"`
	ID      string `json:"id"      yaml:"id"      table:"ID,id"`
	Name    string `json:"name"    yaml:"name"    table:"NAME"`
	Kind    string `json:"kind"    yaml:"kind"    table:"KIND"`
	Role    string `json:"role"    yaml:"role"    table:"ROLE"`
}

// providerLabelCreateOut is what `provider-label create` reports: the
// create queued, the channel and name it asked, the operation and its key.
type providerLabelCreateOut struct {
	Channel      string        `json:"channel"          yaml:"channel"`
	Name         string        `json:"name"             yaml:"name"`
	Kind         string        `json:"kind,omitempty"   yaml:"kind,omitempty"`
	Effect       string        `json:"effect"           yaml:"effect"`
	Operation    operationView `json:"operation"        yaml:"operation"`
	OperationKey string        `json:"operation_key"    yaml:"operation_key"`
	Notes        []string      `json:"notes,omitempty"  yaml:"notes,omitempty"`
}

func providerLabelCreateRows(o providerLabelCreateOut) [][2]string {
	rows := [][2]string{
		{"channel", o.Channel},
		{"name", o.Name},
	}
	if o.Kind != "" {
		rows = append(rows, [2]string{"kind", o.Kind})
	}
	return append(append(rows, [2]string{"effect", o.Effect}), operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) providerLabelCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "provider-label",
		Short: "List and create the mailboxes a mail provider keeps",
		Long: `List and create the provider's own mailboxes of the mail channels you
reach through your inboxes: a Gmail label, an Exchange folder or
category, an IMAP mailbox. They are not heo's labels: ` + "`label`" + ` defines the
tenant's labels, uma's own, local to uma and matched to no provider's;
the ` + "`provider-`" + ` prefix keeps the two apart.

A mailbox has an ID (the provider's own: ` + "`Label_12`" + `, a folder ID, an IMAP
mailbox name), a name ("/" separating a hierarchy: ` + "`Clients/Acme`" + `), a kind
(label, folder or category) and, for the provider's special places, a role
(inbox, archive, trash, junk, sent, drafts).
` + "`conversation move`" + ` and ` + "`conversation tag`" + ` take the ID, or the name,
which they resolve through this list.

Every command runs in the profile's daemon (none running exits 70).
Only a channel whose adapter declares the extension (google.mail,
microsoft.mail, imap.mail) can list and create; ` + "`heo channel get`" + ` shows
the channel's adapter.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.providerLabelListCmd(), a.providerLabelCreateCmd())
	return grp
}

func (a *app) providerLabelListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the provider mailboxes of your mail channels",
		Long: `List the provider's mailboxes of the channels you reach through your
inboxes: channel (its key, else its ID), ID, name, kind and role, in a
stable order (channel, kind, name, ID), the same on every run. With --channel
(key or ID) only that channel's; without, every channel whose adapter
can list, and a channel that cannot is left out. Naming a channel that
cannot list, or that is not yours, is refused (exit 2, UNSUPPORTED, or
exit 3).

The list is read from the provider as you: the provider's own answer, so a
mailbox made in its own client shows at once (the adapter may keep a
provider's list for a few minutes). --format json prints the listing as
every list does, {"items": [...], "next": "", "complete": true}, an item
with the keys channel, id, name, kind and role, items [] when there are none;
a name-to-ID map is built from it by name within a channel. It is never
paged: the whole list is one page. Read-only; the provider is asked each time
and uma keeps nothing.`,
		Example: `  heo provider-label list
  heo provider-label list --channel support-mail --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			channel, _ := cmd.Flags().GetString(flagChannel)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[providerLabelRow], error) {
				out := page[providerLabelRow]{Items: []providerLabelRow{}, Complete: true}
				err := a.desk(ctx, c, model.ActionExtensionQuery, internalAudience, func(s desk.Service) error {
					list, err := s.ListProviderLabels(ctx, channel)
					for _, l := range list {
						out.Items = append(out.Items, providerLabelRow(l))
					}
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
			return renderPage(a, cmd, res.Items, res.Next, res.Complete)
		},
	}
	c.Flags().String(flagChannel, "", "Only this channel's mailboxes (key or ID)")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &page[providerLabelRow]{}, "1.0")
	return c
}

// providerLabelCreatePlan renders a create plan for --dry-run: the
// external change at the provider, which exists once the operation
// settled.
func providerLabelCreatePlan(p desk.ProviderLabelPlan, key string) *cli.Plan {
	name := p.Channel.Key
	if name == "" {
		name = string(p.Channel.Ref.ID)
	}
	args := map[string]any{
		"channel": name, "name": p.Name,
		"delivery": "queued, not applied: the mailbox exists at the provider once the operation settled; its ID is then among the operation's provider references",
	}
	if p.Kind != "" {
		args["kind"] = p.Kind
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := effectArgs(args, p.Declaration)
	detail := fmt.Sprintf("external: a mailbox %q in the account of channel %s, created as %s; the provider shows it in its own clients",
		p.Name, name, p.Request.Action.Name)
	if p.Existing != nil {
		args["current"] = p.Existing
		detail += fmt.Sprintf("; the provider holds %s %q already (%s): the create is idempotent by name and changes nothing",
			p.Existing.Kind, p.Existing.Name, p.Existing.ID)
		checked = append(checked, "mailbox list (read now)")
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("create", "provider_mailbox:"+name+"/"+p.Name, true, detail)},
		PrerequisitesChecked: append(checked, "channel", "request")}
}

func (a *app) providerLabelCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a mailbox at a mail provider",
		Long: `Create the mailbox <name> in the mail account of a channel: a Gmail label,
an Exchange folder (--kind category for a category) or an IMAP mailbox. "/"
in the name is the hierarchy (` + "`Clients/Acme`" + `), whatever the provider
spells it with. --channel (key or ID) names the channel; without it the one
channel you reach that can create is used, and several are refused (exit 2)
with their names.

Idempotent by name: the provider compares names as it does itself (Gmail
and Exchange without regard to case), and a mailbox of that name is the
result, never a second one. --dry-run reads the provider's list and says
when the name exists already. It is an external write: queued as a uma
operation, the command returns once uma accepted it durably, never
applied. The daemon's workers send it; ` + "`delivery get <operation>`" + ` follows
it, and once it settled its provider references hold the mailbox ID
(` + "`provider-label list`" + ` shows it too). A plugin actor's create is held for
a decision while held_operations is on (` + "`operation held`" + `).

Only a channel whose adapter declares extension_execute and the
create capability of its type (google.mail.mailbox_create,
microsoft.mail.mailbox_create, imap.mail.mailbox_create) can do this:
--dry-run refuses any other, exit 2 (UNSUPPORTED), as does the real
run, before anything is written; --dry-run also asks the adapter,
offline, to check the name (VALIDATION, exit 2). Committed under
--idempotency-key: rerunning with the same key and arguments returns the
same operation (replayed); without one, heo mints a key and prints it.
Desk role.`,
		Example: `  heo provider-label create Clients/Acme --channel support-mail --dry-run
  heo provider-label create Clients/Acme --channel support-mail
  heo provider-label create Waiting --kind category --channel outlook --idempotency-key mk-waiting`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			channel, _ := cmd.Flags().GetString(flagChannel)
			kind, _ := cmd.Flags().GetString(flagKind)
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[providerLabelCreateOut], error) {
				var w written[providerLabelCreateOut]
				err := a.deskHeld(ctx, c, model.ActionExtensionExecute, internalAudience, func(s desk.Service) error {
					p, err := s.PlanProviderLabelCreate(ctx, args[0], kind, channel, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = providerLabelCreatePlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionExtensionExecute, Verb: "provider-label", Class: "create",
						Channel: string(p.Channel.Ref.ID), Target: p.Channel.Ref.Ref(), Preview: providerLabelCreatePlan(p, key)})
					if err != nil {
						return err
					}
					acc, err := s.CreateProviderLabel(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					name := p.Channel.Key
					if name == "" {
						name = string(p.Channel.Ref.ID)
					}
					w.Out = providerLabelCreateOut{Channel: name, Name: p.Name, Kind: p.Kind, Effect: p.Request.Action.Name,
						Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"not yet created at the provider: `heo delivery get %s` follows it, and `heo provider-label list --channel %s` shows the mailbox once it exists",
						acc.Operation.Ref.ID, shellWord(name)))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, providerLabelCreateRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagChannel, "", "Channel whose account gets the mailbox (key or ID); the one that can create when omitted")
	c.Flags().String(flagKind, "", "Mailbox kind: label, folder or category; the adapter's default when omitted")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &providerLabelCreateOut{}, "1.0")
	return c
}
