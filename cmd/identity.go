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

// identityView is an identity and its link history.
type identityView struct {
	Identity model.Identity       `json:"identity" yaml:"identity"`
	History  []model.IdentityLink `json:"history"  yaml:"history"`
}

// identityChangeOut is what a link or unlink reports: the identity as it
// is after it, the link history record it appended (none when it
// changed nothing), the operation and its key.
type identityChangeOut struct {
	Identity     model.Identity      `json:"identity"        yaml:"identity"`
	Link         *model.IdentityLink `json:"link,omitempty"  yaml:"link,omitempty"`
	Operation    operationView       `json:"operation"       yaml:"operation"`
	OperationKey string              `json:"operation_key"   yaml:"operation_key"`
	Notes        []string            `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// identityRow is an identity in a listing table.
type identityRow struct {
	ID       string `json:"id"       yaml:"id"       table:"ID,id"`
	Inbox    string `json:"inbox"    yaml:"inbox"    table:"INBOX,id"`
	Contact  string `json:"contact"  yaml:"contact"  table:"CONTACT,id"`
	Kind     string `json:"kind"     yaml:"kind"     table:"KIND"`
	Source   string `json:"source"   yaml:"source"   table:"SOURCE"`
	Link     string `json:"link"     yaml:"link"     table:"LINK"`
	Revision string `json:"revision" yaml:"revision" table:"REVISION"`
}

func identityRowsOf(ids []model.Identity) []identityRow {
	rows := make([]identityRow, 0, len(ids))
	for _, i := range ids {
		rows = append(rows, identityRow{
			ID: string(i.Ref.ID), Inbox: string(i.Ref.Inbox.ID), Contact: string(i.Contact.ID), Kind: i.Kind,
			Source: identitySource(i), Link: string(i.LinkAssurance), Revision: string(i.Revision),
		})
	}
	return rows
}

// identitySource is the sender address an identity stands for.
func identitySource(i model.Identity) string {
	return orDash(strings.TrimSpace(i.Namespace + " " + i.SourceID))
}

func identityRows(i model.Identity) [][2]string {
	return [][2]string{
		{"id", string(i.Ref.ID)},
		{"inbox", string(i.Ref.Inbox.ID)},
		{"contact", string(i.Contact.ID)},
		{"kind", i.Kind},
		{"source", identitySource(i)},
		{"source assurance", orDash(i.Assurance)},
		{"link assurance", string(i.LinkAssurance)},
		{"revision", string(i.Revision)},
	}
}

// linkRow is a link history record in a table: what moved the
// identity, from where to where, who did and on what evidence.
type linkRow struct {
	Recorded  string `json:"recorded"  yaml:"recorded"  table:"RECORDED"`
	Class     string `json:"class"     yaml:"class"     table:"CLASS"`
	From      string `json:"from"      yaml:"from"      table:"FROM"`
	To        string `json:"to"        yaml:"to"        table:"TO"`
	Actor     string `json:"actor"     yaml:"actor"     table:"ACTOR"`
	Assurance string `json:"assurance" yaml:"assurance" table:"ASSURANCE"`
	Evidence  string `json:"evidence"  yaml:"evidence"  table:"EVIDENCE"`
	Reason    string `json:"reason"    yaml:"reason"    table:"REASON"`
}

func linkRowOf(l model.IdentityLink) linkRow {
	return linkRow{
		Recorded: stamp(l.RecordedAt), Class: string(l.Class), From: string(l.From.ID), To: string(l.To.ID),
		Actor: string(l.Actor), Assurance: string(l.Assurance), Evidence: evidenceName(l.Evidence), Reason: orDash(l.Reason),
	}
}

// evidenceName is how output names evidence: its kind and reference.
func evidenceName(e model.Evidence) string {
	return orDash(strings.TrimSpace(e.Verifier + " " + e.Reference))
}

func (a *app) identityCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "identity",
		Short: "Read channel identities and link them to contacts",
		Long: `Read channel identities and change the contact each belongs to. An
identity is one sender address in one inbox; it points at exactly one
contact, a provisional one of its own until someone links it.

heo never links identities by itself: no matching on names, addresses
or message text. ` + "`identity link`" + ` moves an identity to another contact on
your attestation, and ` + "`identity unlink`" + ` detaches it to a fresh contact,
for an audit reason. A link is association only: no conversation,
message, reply route or permission moves with it. Every change is
appended to the identity's link history, which ` + "`identity get`" + ` shows.

Every command runs in the profile's daemon (none running exits 70).
Reading is open to the desk and viewer roles; linking and unlinking are
the desk role's. Any other caller is refused (exit 5).`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.identityListCmd(), a.identityGetCmd(), a.identityLinkCmd(), a.identityUnlinkCmd())
	return grp
}

func (a *app) identityListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the identities you may read",
		Long: `List the identities you may read, in every inbox you may read, a page
at a time (--limit, --cursor with the previous page's next), each with
its inbox, contact, sender address and link assurance. --contact (record
ID) and --inbox (key or ID) narrow it. Read-only.`,
		Example: `  heo identity list
  heo identity list --contact ct_123
  heo identity list --inbox web --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			pg, err := pageOf(cmd)
			if err != nil {
				return err
			}
			var f desk.IdentityFilter
			f.Contact, _ = cmd.Flags().GetString(flagContact)
			f.Inbox, _ = cmd.Flags().GetString(flagInbox)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (page[model.Identity], error) {
				var out page[model.Identity]
				err := a.desk(ctx, c, model.ActionIdentityRead, model.Audience{}, func(s desk.Service) error {
					p, err := s.ListIdentities(ctx, f, pg)
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
			return renderPage(a, cmd, identityRowsOf(res.Items), res.Next, res.Complete)
		},
	}
	c.Flags().String(flagContact, "", "Only identities of this contact (record ID)")
	c.Flags().String(flagInbox, "", "Only identities in this inbox (key or ID)")
	addPageFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &page[model.Identity]{}, Version: "1.0"})
	return c
}

func (a *app) identityGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <identity>",
		Short: "Show one identity and its link history",
		Long: `Show the identity <identity>, by record ID: its inbox, contact, sender
address, source and link assurance and revision (what ` + "`--revision`" + `
takes), then its link history, oldest first: every link, relink and
unlink, from which contact to which, by whom, on what evidence and for
what reason. Read-only. No such identity, or one you may not read,
exits 3.`,
		Example: `  heo identity get id_123
  heo identity get id_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (identityView, error) {
				var out identityView
				err := a.desk(ctx, c, model.ActionIdentityRead, model.Audience{}, func(s desk.Service) (err error) {
					if out.Identity, err = s.FindIdentity(ctx, args[0]); err != nil {
						return err
					}
					out.History, err = s.History(ctx, out.Identity.Ref)
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
			if err := writeRows(cmd.OutOrStdout(), identityRows(res.Identity)); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if len(res.History) == 0 {
				_, err := fmt.Fprintln(w, "\nlink history: none (the identity is on the contact it was created with)")
				return err
			}
			if _, err := fmt.Fprintln(w, "\nlink history:"); err != nil {
				return err
			}
			rows := make([]linkRow, 0, len(res.History))
			for _, l := range res.History {
				rows = append(rows, linkRowOf(l))
			}
			return output.Dispatch(cmd, a.r.Viper, rows)
		},
	}
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &identityView{}, Version: "1.0"})
	return c
}

// changeRows are a link or unlink's output rows: the identity, the
// record appended and the operation.
func changeRows(o identityChangeOut) [][2]string {
	rows := identityRows(o.Identity)
	if l := o.Link; l != nil {
		rows = append(rows,
			[2]string{"class", string(l.Class)},
			[2]string{"from", string(l.From.ID)},
			[2]string{"to", string(l.To.ID)},
			[2]string{"actor", string(l.Actor)},
			[2]string{"evidence", evidenceName(l.Evidence)},
		)
		if l.Reason != "" {
			rows = append(rows, [2]string{"reason", l.Reason})
		}
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func changeOut(ch desk.Changed, key string, notes []string) identityChangeOut {
	return identityChangeOut{Identity: ch.Identity, Link: ch.Record, Operation: operationOf(ch.Acceptance), OperationKey: key, Notes: notes}
}

// linkPlan renders a link plan for --dry-run.
func linkPlan(p desk.LinkPlan, key string) *cli.Plan {
	id := p.Identity
	args := map[string]any{
		"identity": id.Ref.ID, "inbox": id.Ref.Inbox.ID, "from": id.Contact.ID, "contact": p.Target.Ref.ID,
		"class": orDash(string(p.Class)), "evidence": model.EvidenceKindAttestation, "assurance": model.LinkAssuranceAsserted,
		"expected_revision": p.Request.ExpectedRevision, "current": id,
	}
	if r := p.Request.Evidence.Reference; r != "" {
		args["reference"] = r
	}
	if key != "" {
		args["operation_key"] = key
	}
	detail := fmt.Sprintf("identity %s moves from contact %s to contact %s (%s) on your attestation, assurance asserted; "+
		"association only: no conversation, message or reply route moves; appended to its link history",
		id.Ref.ID, id.Contact.ID, p.Target.Ref.ID, p.Class)
	if p.Class == "" {
		detail = fmt.Sprintf("none: identity %s is already on contact %s", id.Ref.ID, p.Target.Ref.ID)
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "identity:"+string(id.Ref.ID), true, detail)},
		PrerequisitesChecked: []string{"authorization", "revision", "contact", "kind", "request"}, Warnings: p.Warnings}
}

func (a *app) identityLinkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "link <identity>",
		Short: "Link an identity to a contact, on your attestation",
		Long: `Link the identity <identity> (record ID) to the contact --contact
(record ID), of the identity's own kind: from now on the identity belongs
to that contact. heo links only when you ask, on your attestation that
the identity is the contact's (link assurance asserted); it never
matches identities by itself.

A link is association only: the identity's conversations, messages and
reply routes stay where they are, and no one gains access by it. The
contact the identity leaves is kept, so a link can be undone by linking
back, or by ` + "`identity unlink`" + `. uma records the move in the identity's link
history, with you as the actor (` + "`identity get`" + `).

--revision names the identity's revision, as ` + "`identity get`" + ` shows it: if
the identity changed since, it is a conflict (exit 4) and nothing is
written. --reference names an opaque proof ID for the attestation, such
as a ticket number, never personal data; one reference backs one link
only. Committed as a uma operation under --idempotency-key: rerunning
with the same key and arguments returns the same operation (replayed);
without one, heo mints a key and prints it. --dry-run prints the change
and writes nothing. Desk role.`,
		Example: `  heo identity link id_123 --contact ct_456 --revision rev_123
  heo identity link id_123 --contact ct_456 --revision rev_123 --reference TICKET-42 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			contact, _ := cmd.Flags().GetString(flagContact)
			ref, _ := cmd.Flags().GetString(flagReference)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[identityChangeOut], error) {
				var w written[identityChangeOut]
				err := a.desk(ctx, c, model.ActionIdentityLink, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanLink(ctx, args[0], contact, rev, ref, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = linkPlan(p, key)
						return nil
					}
					ch, err := s.Link(ctx, p)
					if err != nil {
						return err
					}
					w.Out = changeOut(ch, key, p.Warnings)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, changeRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagContact, "", "Contact to link the identity to (record ID; required)")
	_ = c.MarkFlagRequired(flagContact)
	c.Flags().String(flagReference, "", "Opaque proof ID backing the attestation, such as a ticket number; single use")
	addRevisionFlag(c, "identity")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &identityChangeOut{}, Version: "1.0"})
	return c
}

// unlinkPlan renders an unlink plan for --dry-run.
func unlinkPlan(p desk.UnlinkPlan, key string) *cli.Plan {
	id := p.Identity
	args := map[string]any{
		"identity": id.Ref.ID, "inbox": id.Ref.Inbox.ID, "from": id.Contact.ID, "class": model.LinkClassUnlink,
		"reason": p.Request.Reason, "assurance": model.LinkAssuranceNone,
		"expected_revision": p.Request.ExpectedRevision, "current": id,
	}
	if r := p.Request.Evidence.Reference; r != "" {
		args["evidence"] = model.EvidenceKindAttestation
		args["reference"] = r
	}
	if key != "" {
		args["operation_key"] = key
	}
	detail := fmt.Sprintf("identity %s leaves contact %s for a fresh %s contact uma creates, reason %s; "+
		"its messages keep their author; appended to its link history", id.Ref.ID, id.Contact.ID, id.Kind, p.Request.Reason)
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "identity:"+string(id.Ref.ID), true, detail)},
		PrerequisitesChecked: []string{"authorization", "revision", "reason", "request"}}
}

func (a *app) identityUnlinkCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unlink <identity>",
		Short: "Detach an identity from its contact, for a reason",
		Long: `Detach the identity <identity> (record ID) from its contact: uma
creates a fresh contact of the identity's kind and moves the identity
there, link assurance none. Messages the identity wrote keep it as
their author, and the contact it leaves is kept.

--reason is required: an audit reason code, one lowercase word or
snake_case name such as wrong_person, recorded on the link history and
kept after redaction — never personal data. --reference optionally backs
the unlink with your attestation under an opaque proof ID.

--revision names the identity's revision, as ` + "`identity get`" + ` shows it: if
the identity changed since, it is a conflict (exit 4) and nothing is
written. Committed as a uma operation under --idempotency-key: rerunning
with the same key and arguments returns the same operation (replayed);
without one, heo mints a key and prints it. --dry-run prints the change
and writes nothing. Desk role.`,
		Example: `  heo identity unlink id_123 --reason wrong_person --revision rev_123
  heo identity unlink id_123 --reason shared_device --revision rev_123 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			reason, _ := cmd.Flags().GetString(flagReason)
			ref, _ := cmd.Flags().GetString(flagReference)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[identityChangeOut], error) {
				var w written[identityChangeOut]
				err := a.desk(ctx, c, model.ActionIdentityUnlink, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanUnlink(ctx, args[0], reason, rev, ref, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = unlinkPlan(p, key)
						return nil
					}
					ch, err := s.Unlink(ctx, p)
					if err != nil {
						return err
					}
					w.Out = changeOut(ch, key, nil)
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, changeRows, nil)
		},
	}
	c.Flags().String(flagReason, "", "Audit reason code, such as wrong_person; recorded, never personal data (required)")
	_ = c.MarkFlagRequired(flagReason)
	c.Flags().String(flagReference, "", "Opaque proof ID backing the unlink with your attestation; single use")
	addRevisionFlag(c, "identity")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &identityChangeOut{}, Version: "1.0"})
	return c
}
