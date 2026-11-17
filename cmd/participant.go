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

// participantView is one participant of a conversation: the identity
// (one sender address in one inbox), its role there, and how sure uma
// is of the identity, where the caller may read identities.
type participantView struct {
	Identity  model.ID `json:"identity"            yaml:"identity"`
	Role      string   `json:"role"                yaml:"role"`
	Assurance string   `json:"assurance,omitempty" yaml:"assurance,omitempty"`
	Contact   model.ID `json:"contact,omitempty"   yaml:"contact,omitempty"`
}

// participantListOut is a conversation's participants.
type participantListOut struct {
	Conversation model.ID          `json:"conversation" yaml:"conversation"`
	Participants []participantView `json:"participants" yaml:"participants"`
}

// participantRow is a participant in a listing table.
type participantRow struct {
	Identity  string `json:"identity"  yaml:"identity"  table:"IDENTITY,id"`
	Role      string `json:"role"      yaml:"role"      table:"ROLE"`
	Assurance string `json:"assurance" yaml:"assurance" table:"ASSURANCE"`
	Contact   string `json:"contact"   yaml:"contact"   table:"CONTACT,id"`
}

func (a *app) participantCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "participant",
		Short: "List, add and remove the participants of a conversation",
		Long: `List who takes part in a conversation, and bring someone into it or take
them out at the provider.

Every command runs in the profile's daemon (none running exits 70).
Listing is a read of what uma recorded from the provider's traffic and
from the changes below, open to every role that reads the conversation.
Adding and removing change the conversation at its provider, so they
work only on channels whose adapter declares membership; they are the
desk role's. A change is queued: the command returns once uma accepted
it durably, and the participants list shows it when the provider
applied it.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.participantListCmd(), a.participantChangeCmd(false), a.participantChangeCmd(true))
	return grp
}

func (a *app) participantListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the participants of a conversation",
		Long: `List the participants of the conversation --conversation (record ID):
each identity with its role in the conversation (for instance contact),
and, where you may read the inbox's identities, the identity's assurance
level and the contact it points at. Without that right the identity and
role still show, and assurance and contact are empty. Read-only; no
capability is needed, since the list is whatever uma recorded: from
ingress, and from the changes the provider applied.`,
		Example: `  heo participant list --conversation conv_123
  heo participant list --conversation conv_123 --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			conv, _ := cmd.Flags().GetString(flagConversation)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (participantListOut, error) {
				out := participantListOut{Participants: []participantView{}}
				err := a.desk(ctx, c, model.ActionConversationRead, model.Audience{}, func(s desk.Service) error {
					cv, ps, err := s.Participants(ctx, conv)
					if err != nil {
						return err
					}
					out.Conversation = cv.Ref.ID
					for _, p := range ps {
						out.Participants = append(out.Participants, participantView{
							Identity: p.Identity.ID, Role: p.Role, Assurance: p.Assurance, Contact: p.Contact,
						})
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
			rows := make([]participantRow, 0, len(res.Participants))
			for _, p := range res.Participants {
				rows = append(rows, participantRow{
					Identity: string(p.Identity), Role: orDash(p.Role), Assurance: orDash(p.Assurance), Contact: orDash(string(p.Contact)),
				})
			}
			return output.Dispatch(cmd, a.r.Viper, rows)
		},
	}
	c.Flags().String(flagConversation, "", "Conversation to list the participants of (record ID) (required)")
	_ = c.MarkFlagRequired(flagConversation)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &participantListOut{}, "1.0")
	return c
}

// participantChangeOut is what adding or removing a participant
// reports: who, and the operation that sends the change at the
// provider — queued, never applied, when the command returns.
type participantChangeOut struct {
	Conversation model.ID       `json:"conversation"      yaml:"conversation"`
	Identity     model.ID       `json:"identity"          yaml:"identity"`
	Contact      model.ID       `json:"contact,omitempty" yaml:"contact,omitempty"`
	Remove       bool           `json:"remove"            yaml:"remove"`
	Revision     model.Revision `json:"revision"          yaml:"revision"`
	Operation    operationView  `json:"operation"         yaml:"operation"`
	OperationKey string         `json:"operation_key"     yaml:"operation_key"`
}

func participantChangeRows(o participantChangeOut) [][2]string {
	act := "add"
	if o.Remove {
		act = "remove"
	}
	rows := [][2]string{
		{"conversation", string(o.Conversation)},
		{"identity", string(o.Identity)},
	}
	if o.Contact != "" {
		rows = append(rows, [2]string{"contact", string(o.Contact)})
	}
	return append(rows,
		[2]string{"participant", act},
		[2]string{"revision", string(o.Revision) + " (as read; the provider's change bumps it)"},
	)
}

// participantChangeNote is what a queued change says of delivery: the
// participants change when the provider applied it, not before.
func participantChangeNote(op operationView) []string {
	notes := queuedNote(op)
	if len(notes) == 0 {
		return nil
	}
	return append(notes, fmt.Sprintf("the participants change only once the provider applies it: `heo delivery get %s` shows the operation", op.ID))
}

// participantChangeCmd builds `participant add` and `participant remove`.
func (a *app) participantChangeCmd(remove bool) *cobra.Command {
	verb, what, example := "add", "Add a participant to a conversation, at its provider", "heo participant add --conversation conv_123 --revision rev_9 ident_42"
	long := `Bring the identity or contact <who> (record ID) into the conversation
--conversation (record ID) on the platform it lives on. An identity is
one sender address in the conversation's inbox; a contact must have
exactly one identity there (otherwise name the identity). Adding a
participant that already is one is a conflict (exit 4).`
	if remove {
		verb, what, example = "remove", "Remove a participant from a conversation, at its provider", "heo participant remove --conversation conv_123 --revision rev_9 ident_42"
		long = `Take the identity or contact <who> (record ID) out of the conversation
--conversation (record ID) on the platform it lives on: they stop taking
part there, and a person removed can lose sight of the thread. Removing
someone who is no participant, or the conversation's owner, is a
conflict (exit 4). A later message from the person adds them back to
uma's list, as for any sender; removing them at the provider again, or
bringing them back there, is a new command.

This reaches a third party and the provider, so it is withheld from the
REST, gRPC and MCP surfaces until the operator names them; the control
socket allows it, and the terminal asks for --confirm=yes.`
	}
	c := &cobra.Command{
		Use:   verb + " <who>",
		Short: what,
		Long: long + `

--revision names the conversation's revision, as ` + "`conversation get`" + `
shows it, and is always sent: if the conversation changed since, it is
a conflict (exit 4) naming the current revision, and nothing is written.

The change is queued: the command returns once uma accepted it
durably, with its operation, never applied. The daemon's workers send
it, and the participants list shows the change only when the provider
applied it. Only a channel whose adapter declares membership can do
this; any other is refused, exit 2 (UNSUPPORTED), before anything is
written, and a declared one the channel's configuration switched off
fails the operation.

--dry-run prints the platform effect, says which adapter declares
membership and that live availability (the provider account's rights,
the channel's configuration) is not checked, and writes nothing.
Committed under --idempotency-key: rerunning with the same key and
revision returns the same operation (replayed); without one, heo mints
a key and prints it. Desk role.`,
		Example: "  " + example + " --dry-run\n  " + example + " --idempotency-key member-42",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			conv, _ := cmd.Flags().GetString(flagConversation)
			rev := revisionOf(cmd)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[participantChangeOut], error) {
				var w written[participantChangeOut]
				err := a.desk(ctx, c, model.ActionConversationMembership, internalAudience, func(s desk.Service) error {
					p, err := s.PlanParticipant(ctx, conv, args[0], remove, rev, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					cv := p.Conversation
					if cli.IsDryRun(cmd) {
						args := map[string]any{
							"conversation": cv.Ref.ID, "inbox": cv.Ref.Inbox.ID, "identity": p.Identity.Ref.ID,
							"contact": p.Identity.Contact.ID, "audience": model.Internal, "expected_revision": rev,
						}
						if key != "" {
							args["operation_key"] = key
						}
						checked := declarationArgs(args, p.Declaration)
						w.Plan = &cli.Plan{Args: args, Effects: []cli.Effect{planEffect("update", "conversation:"+string(cv.Ref.ID), true, fmt.Sprintf(
							"%s identity %s %s the participants of conversation %s at its provider; queued for the daemon's workers, applied only when the provider confirms",
							verb, p.Identity.Ref.ID, map[bool]string{false: "to", true: "from"}[remove], cv.Ref.ID))},
							PrerequisitesChecked: append(checked, "conversation", "identity", "revision", "request")}
						return nil
					}
					submit := s.AddParticipant
					if remove {
						submit = s.RemoveParticipant
					}
					acc, err := submit(ctx, p)
					if err != nil {
						return err
					}
					w.Out = participantChangeOut{
						Conversation: cv.Ref.ID, Identity: p.Identity.Ref.ID, Contact: p.Identity.Contact.ID, Remove: remove,
						Revision: cv.Revision, Operation: operationOf(acc), OperationKey: key,
					}
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, participantChangeRows, participantChangeNote(res.Out.Operation))
		},
	}
	c.Flags().String(flagConversation, "", "Conversation to change the participants of (record ID) (required)")
	_ = c.MarkFlagRequired(flagConversation)
	addRevisionFlag(c, "conversation")
	providerDependent(c, model.CapabilityMembership)
	daemonCommand(c)
	if remove {
		cli.SetSideEffect(c, cli.SideEffectDestructiveShared)
	} else {
		cli.SetSideEffect(c, cli.SideEffectWriteShared)
	}
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &participantChangeOut{}, "1.0")
	return c
}
