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

// flagMethod is `conversation unsubscribe`'s method flag.
const flagMethod = "method"

// conversationUnsubscribeOut is what `conversation unsubscribe` reports:
// the conversation as uma holds it (untouched), the provider message
// whose headers the adapter reads, the method it may use, the effect
// run, the operation and its key.
type conversationUnsubscribeOut struct {
	Conversation model.Conversation `json:"conversation"    yaml:"conversation"`
	Message      string             `json:"message"         yaml:"message"`
	Method       string             `json:"method"          yaml:"method"`
	Effect       string             `json:"effect"          yaml:"effect"`
	Operation    operationView      `json:"operation"       yaml:"operation"`
	OperationKey string             `json:"operation_key"   yaml:"operation_key"`
	Notes        []string           `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// unsubscribeGuarantee is what the preview, the help and the docs say the
// adapter does and refuses: the whole safety argument in one place, the
// adapter's own documented rules, which heo does not re-implement.
const unsubscribeGuarantee = "the adapter reads the message's List-Unsubscribe headers again from the provider and takes no URL or address from this command; " +
	"one-click (RFC 8058) only for a single https URI (default port, no credentials, a public host) with a Post header and a DKIM signature of the sender's organization covering both, " +
	"sent as one POST of List-Unsubscribe=One-Click through its own client, which refuses a host that resolves to a private, loopback, link-local or reserved address and follows no redirect; " +
	"mailto only to one address in the sender's own organization; every other URI (http, ftp, tel) is never followed and no GET is made"

// conversationUnsubscribePlan renders an unsubscribe plan for --dry-run:
// what would be asked of the provider, and that the request to the
// sender cannot be undone. It sends and writes nothing.
func conversationUnsubscribePlan(p desk.UnsubscribePlan, key string) *cli.Plan {
	cur := p.Conversation
	args := map[string]any{
		"conversation": cur.Ref.ID, "inbox": cur.Ref.Inbox.ID, "destination": cur.ReplyRoute,
		"visibility": p.Request.Audience.Visibility, "message": p.Message, "method": p.Method,
		"change":   p.Request.Action.Name + " " + string(p.Request.Action.Data),
		"current":  cur,
		"policy":   unsubscribeGuarantee,
		"delivery": "queued, not applied: the method is decided from the message's headers when the daemon's workers run it, and a refusal there is final; nothing changes in uma",
	}
	if key != "" {
		args["operation_key"] = key
	}
	checked := effectArgs(args, p.Declaration)
	effect := planEffect("send", "mail:message "+p.Message, false, fmt.Sprintf(
		"external and irreversible: ask the sender of provider message %s of conversation %s to stop mailing this mailbox (%s, method %s); "+
			"a request leaves for a third party and cannot be recalled; "+unsubscribeGuarantee,
		p.Message, cur.Ref.ID, p.Request.Action.Name, p.Method))
	return &cli.Plan{Args: args, Effects: []cli.Effect{effect},
		PrerequisitesChecked: append(checked, "conversation", "message", "request")}
}

func conversationUnsubscribeRows(o conversationUnsubscribeOut) [][2]string {
	return append([][2]string{
		{"conversation", string(o.Conversation.Ref.ID)},
		{"message", orDash(o.Message)},
		{"method", o.Method},
		{"effect", o.Effect},
	}, operationRows(o.Operation, o.OperationKey)...)
}

func (a *app) conversationUnsubscribeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "unsubscribe <conversation>",
		Short: "Unsubscribe the mailbox from the sender of a mail conversation's message",
		Long: `Ask the sender of the message the mail conversation <conversation>
(record ID) replies to, the latest the peer wrote, to stop mailing this
mailbox, by what the message's own List-Unsubscribe headers allow:

  auto (default)   one-click when the message qualifies, else mailto
  one_click        RFC 8058 one-click only
  mailto           a mail to the sender's own organization only

heo sends no URL and no address: the adapter reads the message again
from the provider when the operation runs and decides from its headers.
One-click is one POST of List-Unsubscribe=One-Click to a single https
URI (default port, no credentials, a public host), and only when the
message carries a List-Unsubscribe-Post header and a valid DKIM
signature of the sender's organization covering both fields. The
request goes through the adapter's own client, which refuses a host that
resolves to a private, loopback, link-local (cloud metadata) or reserved
address, follows no redirect and keeps no cookie. Mailto goes only to
one address in the sender's organization, with no cc or bcc. Every other
URI (http, ftp, tel) is never followed and no GET is ever made. A message
that qualifies for neither is refused by the adapter, a final answer
shown on the operation (` + "`delivery get`" + `).

This is external and irreversible: a request leaves for a third party
and cannot be recalled, and the sender may keep mailing for a while. It
is also the consent RFC 8058 asks of the person: it is a plugin actor's
write that heo holds for a decision while held_operations is on (see
` + "`operation held`" + `), and approving it is the yes. --dry-run previews
exactly this and sends nothing.

The command returns once uma accepted it durably: queued, never applied.
The daemon's workers run it, and ` + "`delivery get <operation>`" + ` follows it. Only
a channel whose adapter declares extension_execute and ships its
unsubscribe effect can do this (google.mail, microsoft.mail); --dry-run
refuses any other, exit 2 (UNSUPPORTED), as does the real run, before
anything is written. --dry-run also asks the adapter, offline, to check
the method (VALIDATION, exit 2). A conversation the peer has written no
message in has nothing to unsubscribe from (CONFLICT, exit 4). Committed
under --idempotency-key: rerunning with the same key and arguments
returns the same operation (replayed) and asks the sender once; without
one, heo mints a key and prints it. Desk role.`,
		Example: `  heo conversation unsubscribe conv_123 --dry-run
  heo conversation unsubscribe conv_123 --idempotency-key unsub-123
  heo conversation unsubscribe conv_123 --method mailto`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			method, _ := cmd.Flags().GetString(flagMethod)
			if err := desk.CheckUnsubscribeMethod(method); err != nil {
				return output.UsageError("--" + strings.TrimPrefix(err.Error(), "validation: "))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[conversationUnsubscribeOut], error) {
				var w written[conversationUnsubscribeOut]
				err := a.deskHeld(ctx, c, model.ActionExtensionExecute, internalAudience, func(s desk.Service) error {
					p, err := s.PlanUnsubscribe(ctx, args[0], method, opID(key), planMode(cmd))
					if err != nil {
						return err
					}
					if cli.IsDryRun(cmd) {
						w.Plan = conversationUnsubscribePlan(p, key)
						return nil
					}
					h, err := s.Hold(ctx, &p.Request.Command, desk.HoldInput{
						Action: model.ActionExtensionExecute, Verb: "unsubscribe",
						Channel: string(p.Conversation.Ref.Inbox.ID), Target: p.Conversation.Ref.Ref(), Preview: conversationUnsubscribePlan(p, key)})
					if err != nil {
						return err
					}
					conv, acc, err := s.Unsubscribe(ctx, p)
					if acc, err = h.Settle(ctx, acc, err); err != nil {
						return err
					}
					w.Out = conversationUnsubscribeOut{Conversation: conv, Message: p.Message, Method: p.Method,
						Effect: p.Request.Action.Name, Operation: operationOf(acc), OperationKey: key}
					w.Out.Notes = append(queuedNote(w.Out.Operation), fmt.Sprintf(
						"not yet asked of the sender: `heo delivery get %s` shows whether the adapter's rules allowed it and what it did", acc.Operation.Ref.ID))
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			return renderDeskWrite(a, cmd, res, conversationUnsubscribeRows, res.Out.Notes)
		},
	}
	c.Flags().String(flagMethod, "", "Method the adapter may use: auto (default), one_click or mailto")
	providerDependent(c, model.CapabilityExtensionExecute)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &conversationUnsubscribeOut{}, "1.0")
	return c
}
