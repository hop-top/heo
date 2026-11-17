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

// Flags the contact write leaves add to the shared ones.
const (
	flagEmail        = "email"
	flagPhone        = "phone"
	flagIdentity     = "identity"
	flagIntoRevision = "into-revision"
)

// contactChangeOut is what a contact create or update reports: the
// contact as it is after the write, the operation and its key.
type contactChangeOut struct {
	Contact      model.Contact `json:"contact"         yaml:"contact"`
	Operation    operationView `json:"operation"       yaml:"operation"`
	OperationKey string        `json:"operation_key"   yaml:"operation_key"`
	Notes        []string      `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// contactMergeOut is what a contact merge reports: both contacts as they
// are after it, the link history records it appended (one per identity
// moved), the operation and its key.
type contactMergeOut struct {
	From         model.Contact        `json:"from"            yaml:"from"`
	Into         model.Contact        `json:"into"            yaml:"into"`
	Moved        []model.IdentityLink `json:"moved"           yaml:"moved"`
	Operation    operationView        `json:"operation"       yaml:"operation"`
	OperationKey string               `json:"operation_key"   yaml:"operation_key"`
	Notes        []string             `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// contactRows are a contact's rows: who it is, and where it was merged
// to when it was.
func contactRows(c model.Contact) [][2]string {
	rows := [][2]string{
		{"id", string(c.Ref.ID)},
		{"kind", c.Kind},
		{"name", orDash(c.DisplayName)},
	}
	for _, a := range c.Attributes {
		rows = append(rows, [2]string{string(a.Kind), a.Value})
	}
	rows = append(rows, [2]string{"revision", string(c.Revision)})
	if c.MergedInto != nil {
		rows = append(rows, [2]string{"merged into", string(c.MergedInto.ID)})
	}
	return rows
}

// attributeNames lists attributes as email and phone values.
func attributeNames(attrs []model.Attribute) string {
	vals := make([]string, 0, len(attrs))
	for _, a := range attrs {
		vals = append(vals, string(a.Kind)+" "+a.Value)
	}
	return strings.Join(vals, ", ")
}

func attributesOf(cmd *cobra.Command) desk.ContactAttributes {
	var a desk.ContactAttributes
	a.Email, _ = cmd.Flags().GetStringSlice(flagEmail)
	a.Phone, _ = cmd.Flags().GetStringSlice(flagPhone)
	a.EmailSet = cmd.Flags().Changed(flagEmail)
	a.PhoneSet = cmd.Flags().Changed(flagPhone)
	return a
}

func contactCreatePlan(p desk.ContactCreatePlan, key string) *cli.Plan {
	r := p.Request
	kind := r.Kind
	if kind == "" {
		kind = model.ContactKindPerson
	}
	args := map[string]any{"name": r.DisplayName, "kind": kind}
	if len(r.Attributes) > 0 {
		args["attributes"] = attributeNames(r.Attributes)
	}
	if key != "" {
		args["operation_key"] = key
	}
	effect := planEffect("create", "contact:"+r.DisplayName, false,
		"a contact no identity points at; an identity joins it only when a desk actor links it")
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect}, PrerequisitesChecked: []string{"authorization", "request"}}
}

func (a *app) contactCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Create a contact no identity points at",
		Long: `Create a contact named --name, of --kind (` + strings.Join(desk.ContactKinds(), ", ") + `; person unless
given), with optional --email and --phone attributes. The contact holds
no identity: heo never matches identities to contacts, and an identity
joins it only when a desk actor links it (` + "`identity link`" + `).

Attributes are lookup hints, never proof of anything and never used to
match: an email is kept with its domain lowercased, a phone number in
E.164 form (+ and digits; spaces, dots, dashes and brackets are
stripped), and neither is ever verified by this command. Repeat a flag or
comma-separate for several.

Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed); the same
key with other arguments is a conflict (exit 4); without a key, heo mints
one and prints it. --dry-run prints the contact and writes nothing. Desk
role.`,
		Example: `  heo contact create --name "Ada Lovelace" --email ada@example.com
  heo contact create --name "Support bot" --kind bot --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString(flagName)
			kind, _ := cmd.Flags().GetString(flagKind)
			attrs := attributesOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[contactChangeOut], error) {
				var w written[contactChangeOut]
				err := a.desk(ctx, c, model.ActionContactWrite, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanContactCreate(ctx, kind, name, attrs, opID(key))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = contactCreatePlan(p, key)
						return nil
					}
					made, err := s.CreateContact(ctx, p)
					if err != nil {
						return err
					}
					w.Out = contactChangeOut{Contact: made.Contact, Operation: operationOf(made.Acceptance), OperationKey: key}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, contactChangeRows, nil)
		},
	}
	c.Flags().String(flagName, "", "The contact's display name (required)")
	_ = c.MarkFlagRequired(flagName)
	c.Flags().String(flagKind, "", "Kind of contact: "+strings.Join(desk.ContactKinds(), ", ")+" (default person)")
	c.Flags().StringSlice(flagEmail, nil, "Email address attribute; repeat or comma-separate")
	c.Flags().StringSlice(flagPhone, nil, "Phone number attribute, E.164 (+ and digits); repeat or comma-separate")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &contactChangeOut{}, Version: "1.0"})
	return c
}

func contactChangeRows(o contactChangeOut) [][2]string {
	return append(contactRows(o.Contact), operationRows(o.Operation, o.OperationKey)...)
}

func contactUpdatePlan(p desk.ContactUpdatePlan, key string) *cli.Plan {
	r := p.Request
	args := map[string]any{
		"contact": r.Contact.ID, "name": r.DisplayName, "expected_revision": r.ExpectedRevision, "current": p.Current,
	}
	if len(r.Attributes) > 0 {
		args["attributes"] = attributeNames(r.Attributes)
	}
	if len(p.Changes) > 0 {
		args["change"] = strings.Join(p.Changes, ", ")
	}
	if key != "" {
		args["operation_key"] = key
	}
	detail := "sets " + strings.Join(p.Changes, " and ") + "; the kind and the identities stay"
	if len(p.Changes) == 0 {
		detail = "none: the contact already has these values"
	}
	return &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "contact:"+string(r.Contact.ID), true, detail)},
		PrerequisitesChecked: []string{"authorization", "revision", "request"}, Warnings: p.Warnings}
}

func (a *app) contactUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "update <contact>",
		Short: "Change a contact's name or attributes",
		Long: `Change the contact <contact> (record ID): --name sets its display
name; --email and --phone each replace the contact's attributes of that
kind with the values given (repeat a flag or comma-separate for several;
an empty value, --email=, clears the kind). What you leave out stays as
it is. A contact's kind and identities never change here.

--revision names the contact's revision, as ` + "`contact get`" + ` shows it: if the
contact changed since, it is a conflict (exit 4) and nothing is written.
Committed as a uma operation under --idempotency-key: rerunning with the
same key and arguments returns the same operation (replayed), even once
the revision has moved on; without a key, heo mints one and prints it.
Setting what is already set commits nothing and leaves the revision as it
is. --dry-run prints the change and writes nothing. Desk role.`,
		Example: `  heo contact update ct_123 --name "Ada King" --revision rev_123
  heo contact update ct_123 --email ada@example.com --phone=+12025550100 --revision rev_123 --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			var change desk.ContactChange
			if cmd.Flags().Changed(flagName) {
				n, _ := cmd.Flags().GetString(flagName)
				change.Name = &n
			}
			change.Attributes = attributesOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[contactChangeOut], error) {
				var w written[contactChangeOut]
				err := a.desk(ctx, c, model.ActionContactWrite, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanContactUpdate(ctx, args[0], rev, change, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = contactUpdatePlan(p, key)
						return nil
					}
					done, err := s.UpdateContact(ctx, p)
					if err != nil {
						return err
					}
					w.Out = contactChangeOut{Contact: done.Contact, Operation: operationOf(done.Acceptance), OperationKey: key, Notes: p.Warnings}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, contactChangeRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagName, "", "New display name")
	c.Flags().StringSlice(flagEmail, nil, "Replace the contact's email attributes; empty clears them")
	c.Flags().StringSlice(flagPhone, nil, "Replace the contact's phone attributes, E.164; empty clears them")
	addRevisionFlag(c, "contact")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWrite)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &contactChangeOut{}, Version: "1.0"})
	return c
}

// mergePlan renders a contact merge plan for --dry-run: uma's preview of
// what moves, and the revisions to merge at.
func mergePlan(p desk.MergePlan, key string) *cli.Plan {
	r, pv := p.Request, p.Preview
	from, into := string(r.From.ID), string(r.Into.ID)
	args := map[string]any{
		"from": from, "into": into, "reason": r.Reason,
		"expected_revision": r.ExpectedRevision, "expected_into_revision": r.ExpectedIntoRevision,
	}
	if key != "" {
		args["operation_key"] = key
	}
	if p.Merged {
		effect := planEffect("update", "contact:"+from, false, fmt.Sprintf("none: contact %s is already merged into %s", from, into))
		return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
			PrerequisitesChecked: []string{"authorization", "revision", "reason", "request"}, Warnings: p.Warnings}
	}
	ids, convs := make([]string, 0, len(pv.Identities)), make([]string, 0, len(pv.Conversations))
	effects := []cli.Effect{
		planEffect("update", "contact:"+from, false, fmt.Sprintf(
			"becomes a redirect to contact %s and reads as merged; its revision changes; there is no unmerge", into)),
		planEffect("update", "contact:"+into, false, fmt.Sprintf(
			"takes the %d identities of contact %s; its name and attributes stay; its revision changes", len(pv.Identities), from)),
	}
	for _, id := range pv.Identities {
		ids = append(ids, string(id.ID))
		effects = append(effects, planEffect("update", "identity:"+string(id.ID), true, fmt.Sprintf(
			"moves from contact %s to contact %s; link history class merge, assurance asserted, reason %s", from, into, r.Reason)))
	}
	for _, cv := range pv.Conversations {
		convs = append(convs, string(cv.ID))
		effects = append(effects, planEffect("update", "conversation:"+string(cv.ID), true, fmt.Sprintf(
			"its owner moves to contact %s; the owner and the reply route stay", into)))
	}
	args["identities"] = strings.Join(ids, ", ")
	args["conversations"] = strings.Join(convs, ", ")
	args["more_conversations"] = pv.MoreConversations
	warns := p.Warnings
	if pv.MoreConversations {
		warns = append(warns, "more conversations move than are listed: the list is partial")
	}
	return &cli.Plan{Args: args, Effects: effects,
		PrerequisitesChecked: []string{"authorization", "revision", "reason", "preview", "request"}, Warnings: warns}
}

func mergeRows(o contactMergeOut) [][2]string {
	moved := make([]string, 0, len(o.Moved))
	for _, l := range o.Moved {
		moved = append(moved, string(l.Identity.ID))
	}
	rows := [][2]string{
		{"from", string(o.From.Ref.ID)},
		{"into", string(o.Into.Ref.ID)},
		{"merged into", mergedInto(o.From)},
		{"from revision", string(o.From.Revision)},
		{"into revision", string(o.Into.Revision)},
		{"identities moved", fmt.Sprint(len(o.Moved))},
		{"identities", orDash(strings.Join(moved, ", "))},
	}
	return append(rows, operationRows(o.Operation, o.OperationKey)...)
}

func mergedInto(c model.Contact) string {
	if c.MergedInto == nil {
		return "-"
	}
	return string(c.MergedInto.ID)
}

func (a *app) contactMergeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "merge <from> <into>",
		Short: "Merge one contact into another, once you have reviewed the preview",
		Long: `Merge the contact <from> into the contact <into> (record IDs), of the
same kind: every identity on <from> moves to <into>, and <from> stays as
a redirect that reads as merged into <into>. Each identity's link history
gains a record of class merge, with you as the actor, link assurance
asserted and --reason; a conversation keeps its owner and its reply
route, and <from>'s name and attributes are not copied. There is no
unmerge: an identity can be moved off <into> again only with
` + "`identity unlink`" + ` or ` + "`identity link`" + `, and <from> stays merged.

heo never merges by itself: no matching on names, addresses or message
text, and nothing here runs unless you ask. Start with --dry-run: it
prints uma's own preview, the identities and conversations that move, and
the two revisions to merge at, and writes nothing. Then run the merge
with those revisions: --revision is <from>'s, --into-revision <into>'s.
If either contact changed since, or the identities on <from> are no
longer the ones you reviewed (name them with --identity to hold heo to
that), it is a conflict (exit 4) and nothing moves: preview again.

--reason is required: an audit reason code, one lowercase word or
snake_case name such as same_person, recorded on every link record and
kept after redaction, so it is never personal data.

Merging asks uma for the contact.merge action, and for each identity it
moves, for the same relink permission ` + "`identity link`" + ` needs; an identity whose
link assurance is above asserted needs the operator role's link override
besides. A single refusal stops the whole merge (exit 5).

Destructive: it asks for confirmation on a terminal; without one it is
refused (exit 5) unless --confirm=yes is passed. REST, gRPC and MCP do
not serve it until the operator names the surface; the control socket
does. Committed as a uma operation under --idempotency-key: rerunning
with the same key and arguments returns the same operation (replayed)
and moves nothing twice; without a key, heo mints one and prints it.
Desk role.`,
		Example: `  heo contact merge ct_456 ct_123 --reason same_person --dry-run
  heo contact merge ct_456 ct_123 --reason same_person --revision rev_456 --into-revision rev_123`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dry := cli.IsDryRun(cmd)
			for _, f := range []string{flagRevision, flagIntoRevision} {
				if v, _ := cmd.Flags().GetString(f); v == "" && !dry {
					return output.UsageError("--" + f + " is required unless --dry-run: the merge names the revisions it was reviewed at; `heo contact merge " +
						args[0] + " " + args[1] + " --reason <code> --dry-run` shows them")
				}
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			in := desk.MergeInput{From: args[0], Into: args[1], Revision: revisionOf(cmd)}
			irev, _ := cmd.Flags().GetString(flagIntoRevision)
			in.IntoRevision = model.Revision(irev)
			in.Reason, _ = cmd.Flags().GetString(flagReason)
			in.Identities, _ = cmd.Flags().GetStringSlice(flagIdentity)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[contactMergeOut], error) {
				var w written[contactMergeOut]
				err := a.desk(ctx, c, model.ActionContactMerge, model.Audience{}, func(s desk.Service) error {
					p, err := s.PlanMerge(ctx, in, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if dry {
						w.Plan = mergePlan(p, key)
						return nil
					}
					done, err := s.MergeContacts(ctx, p)
					if err != nil {
						return err
					}
					w.Out = contactMergeOut{From: done.From, Into: done.Into, Moved: done.Links, Operation: operationOf(done.Acceptance),
						OperationKey: key, Notes: p.Warnings}
					if w.Out.Moved == nil {
						w.Out.Moved = []model.IdentityLink{}
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, mergeRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagReason, "", "Audit reason code, such as same_person; recorded, never personal data (required)")
	_ = c.MarkFlagRequired(flagReason)
	c.Flags().String(flagRevision, "", "Revision of the contact merged (<from>), from the --dry-run preview or `contact get` (required unless --dry-run)")
	c.Flags().String(flagIntoRevision, "", "Revision of the contact merged into (<into>) (required unless --dry-run)")
	c.Flags().StringSlice(flagIdentity, nil, "Identity you reviewed (record ID); repeat or comma-separate. With any, the merge refuses to move another set")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectDestructiveShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &contactMergeOut{}, Version: "1.0"})
	return c
}
