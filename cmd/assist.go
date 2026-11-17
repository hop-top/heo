package cmd

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/assist"
	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/credentials"
	"hop.top/heo/internal/desk"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Flags of the LLM helpers.
const (
	flagSendToCloud = "send-to-cloud"
	flagInstruction = "instruction"
	flagTone        = "tone"
	flagWithNotes   = "include-notes"
)

// Bounds on the words a caller adds to a draft.
const (
	maxInstruction = 1000
	maxTone        = 60
)

// Audiences a helper reports: what reached the model.
const (
	audiencePublic = "public"
	audienceDesk   = "desk"
)

// assistMeta is what every helper reports about its call: where it went
// and what went into it.
type assistMeta struct {
	Provider         string       `json:"provider"           yaml:"provider"`
	Model            string       `json:"model"              yaml:"model"`
	Class            assist.Class `json:"class"              yaml:"class"`
	MessageCount     int          `json:"message_count"      yaml:"message_count"`
	MessagesInPrompt int          `json:"messages_in_prompt" yaml:"messages_in_prompt"`
	Truncated        bool         `json:"truncated"          yaml:"truncated"`
	Audience         string       `json:"audience"           yaml:"audience"`
	MachineGenerated bool         `json:"machine_generated"  yaml:"machine_generated"`
}

// summaryOut is a conversation summary.
type summaryOut struct {
	Summary    string `json:"summary" yaml:"summary"`
	assistMeta `yaml:",inline"`
}

// triageOut is a triage: suggestions, applied to nothing.
type triageOut struct {
	assist.Triage `yaml:",inline"`
	assistMeta    `yaml:",inline"`
}

// draftOut is a draft reply, sent nowhere.
type draftOut struct {
	Draft      string `json:"draft" yaml:"draft"`
	assistMeta `yaml:",inline"`
}

// assistRequest is what one helper reads and how it prompts.
type assistRequest struct {
	conversation string
	// action is the right the helper needs (see withClient).
	action string
	// notes asks for the internal notes the caller may read.
	notes bool
	// labels asks for the tenant's label names.
	labels bool
	prompt func(entries []assist.Entry, labels []string, maxChars int) assist.Prompt
}

// prepared is a prompt ready to send, and what is known of it without
// sending it.
type prepared struct {
	plan   assist.Plan
	prompt assist.Prompt
	labels []string
	meta   assistMeta
}

// refuseOffline refuses a call that would leave this machine when the
// run is --offline, as `channel check --remote` does, before the daemon
// is asked. A configuration that does not resolve is left for the call
// to report.
func (a *app) refuseOffline(cmd *cobra.Command) error {
	if !cli.IsOffline(cmd.Context()) {
		return nil
	}
	if plan, err := assist.ConfigFrom(a.r.Viper).Plan(); err == nil && plan.Class == assist.Cloud {
		e := output.UsageError(fmt.Sprintf("%s at %s is a cloud provider: the call needs the network, and --offline is set", plan.Provider, plan.Host))
		e.SuggestedFix = "rerun without --offline, or point " + assist.KeyEndpoint + " at a server on this machine"
		return e
	}
	return nil
}

// assistPrepare resolves the provider from the configuration and reads
// the conversation as the caller, then builds the prompt. It sends
// nothing and writes nothing; a --dry-run stops here.
func (a *app) assistPrepare(ctx context.Context, c control.Caller, r assistRequest) (prepared, error) {
	var p prepared
	err := a.desk(ctx, c, r.action, model.Audience{}, func(s desk.Service) error {
		plan, err := assist.ConfigFrom(a.r.Viper).Plan()
		if err != nil {
			return err
		}
		p.plan = plan
		th, err := s.Thread(ctx, r.conversation, r.notes)
		if err != nil {
			return err
		}
		if len(th.Messages) == 0 {
			return output.NotFoundError(fmt.Sprintf("conversation %s holds no messages you may read", r.conversation))
		}
		if r.labels {
			ls, err := s.AllLabels(ctx)
			if err != nil {
				return err
			}
			for _, l := range ls {
				p.labels = append(p.labels, l.Name)
			}
		}
		p.prompt = r.prompt(entriesOf(th.Messages), p.labels, plan.MaxPromptChars)
		p.meta = assistMeta{
			Provider: plan.Provider, Model: plan.Model, Class: plan.Class,
			MessageCount: th.Total, MessagesInPrompt: p.prompt.Included,
			Truncated: th.Cut || p.prompt.Truncated(), Audience: audiencePublic, MachineGenerated: true,
		}
		if th.Notes {
			p.meta.Audience = audienceDesk
		}
		return nil
	})
	return p, err
}

// entriesOf is the prompt's view of messages: who wrote each and what
// it says, nothing else.
func entriesOf(ms []model.Message) []assist.Entry {
	out := make([]assist.Entry, 0, len(ms))
	for _, m := range ms {
		from := assist.Inbound
		switch {
		case desk.IsNote(m):
			from = assist.Note
		case m.Origin != nil:
			from = assist.Reply
		}
		text := strings.TrimSpace(desk.Text(m.Content))
		if text == "" {
			text = "[no text: an attachment or other content]"
		}
		out = append(out, assist.Entry{From: from, Text: text})
	}
	return out
}

// assistPlanKeys are the plan arguments a preview prints as rows, in this
// order.
var assistPlanKeys = []string{
	"provider", "model", "endpoint", "class", "audience", "message_count", "messages_in_prompt", "truncated",
	"prompt_chars", "max_prompt_chars", "timeout",
}

// assistPlan is the preview of a call: where it would go, what it would
// carry, and that it sends nothing.
func assistPlan(p prepared) *cli.Plan {
	m := p.meta
	args := map[string]any{
		"provider": m.Provider, "model": m.Model, "endpoint": p.plan.Host, "class": string(m.Class),
		"message_count": m.MessageCount, "messages_in_prompt": m.MessagesInPrompt, "truncated": m.Truncated,
		"audience": m.Audience, "prompt_chars": p.prompt.Chars, "max_prompt_chars": p.plan.MaxPromptChars,
		"timeout": p.plan.Timeout.String(),
	}
	detail := fmt.Sprintf("would send about %d characters of conversation text to %s provider %s at %s; a preview sends nothing",
		p.prompt.Chars, m.Class, m.Provider, p.plan.Host)
	plan := &cli.Plan{
		Args:                 args,
		Effects:              []cli.Effect{planEffect("send", "llm:"+m.Provider+"@"+p.plan.Host, false, detail)},
		PrerequisitesChecked: []string{"authorization", "conversation", "provider"},
	}
	if m.Class == assist.Cloud {
		plan.Warnings = append(plan.Warnings, "cloud provider: conversation text would leave this machine; the call needs --"+flagSendToCloud)
	}
	return plan
}

// assistCaller opens the plan's provider for a real call. A cloud
// provider is called only when this call says so (--send-to-cloud) and
// its credential, when the configuration names one, resolves; a local
// provider is given no credential at all.
func (a *app) assistCaller(ctx context.Context, cmd *cobra.Command, plan assist.Plan) (assist.Caller, func(), error) {
	key := ""
	if plan.Class == assist.Cloud {
		if ok, _ := cmd.Flags().GetBool(flagSendToCloud); !ok {
			e := output.ConsentRefusedError(fmt.Sprintf("%s at %s is a cloud provider: this would send the conversation's text off this machine", plan.Provider, plan.Host))
			e.SuggestedFix = "run it again with --" + flagSendToCloud + " to allow this call, or point " + assist.KeyEndpoint + " at a server on this machine"
			return assist.Caller{}, nil, e
		}
		if plan.CredentialRef != "" {
			var err error
			if key, err = a.assistKey(ctx, plan); err != nil {
				return assist.Caller{}, nil, err
			}
		}
	}
	comp, closeFn, err := plan.Open(key)
	if err != nil {
		return assist.Caller{}, nil, err
	}
	return assist.Caller{Plan: plan, LLM: comp}, closeFn, nil
}

// assistKey resolves a cloud provider's credential in the profile's
// credential backend, for the provider's own purpose with the shared
// entry as the fallback.
func (a *app) assistKey(ctx context.Context, plan assist.Plan) (string, error) {
	_, refKey := assist.CloudKeys(plan.Name)
	creds, err := a.openCredentials(ctx)
	if err != nil {
		e := output.PrerequisiteError("credential backend: " + err.Error())
		e.SuggestedFix = "check the credentials.* settings with `heo config validate`"
		return "", e
	}
	cred, err := creds.Resolve(ctx, credentials.Request{
		Reference: plan.CredentialRef, Purpose: assist.Purpose(plan.Name), Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		e := output.PrerequisiteError(fmt.Sprintf("credential %q (purpose %s) does not resolve: %s", plan.CredentialRef, assist.Purpose(plan.Name), err))
		e.SuggestedFix = "store the provider's key under that reference in the profile's credential backend, or fix " + refKey
		return "", e
	}
	s, ok := cred.(ports.Secret)
	if !ok {
		e := output.PrerequisiteError(fmt.Sprintf("credential %q is not a secret envelope holding the provider's key", plan.CredentialRef))
		e.SuggestedFix = "store the key as a secret envelope under that reference"
		return "", e
	}
	key := string(s.Bytes)
	clear(s.Bytes)
	return strings.TrimSpace(key), nil
}

// helperHelp is the paragraph every helper's help shares.
const helperHelp = `The command reads the conversation as you (internal notes only when you
may read them: desk), sends the text to the configured model, and prints
what it answers. The answer is machine-generated, and it is only ever
shown: it is never run, and nothing in heo changes. The provider is
llm.provider: a model on this machine by default; a cloud provider only
when the configuration opts in to it by name and each call passes
--send-to-cloud. If the provider is down the command fails and says how
to fix it; it never tries another. --dry-run reads the conversation and
shows the provider, its host class (local or cloud), the message count
and the prompt size, and sends nothing. The conversation's text is
untrusted data to the model, delimited as such. Daemon command.`

func (a *app) conversationSummarizeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "summarize <conversation>",
		Short: "Summarize a conversation with the configured model",
		Long: `Summarize the conversation <conversation> (record ID) in a few
sentences for a teammate who has not read it. Desk and viewer roles: a
viewer's summary holds public messages only.

The prompt is bounded by llm.max_prompt_chars. When the conversation does
not fit, the newest messages are kept and the oldest dropped (the newest
is cut to fit if it alone is too long); the answer reports truncated and
how many messages went in.

` + helperHelp,
		Example: `  heo conversation summarize conv_123
  heo conversation summarize conv_123 --dry-run
  heo conversation summarize conv_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.refuseOffline(cmd); err != nil {
				return err
			}
			req := assistRequest{conversation: args[0], action: authz.ActionAssistRead, notes: true,
				prompt: func(e []assist.Entry, _ []string, n int) assist.Prompt { return assist.SummaryPrompt(e, n) }}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[summaryOut], error) {
				var w written[summaryOut]
				p, err := a.assistPrepare(ctx, c, req)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					w.Plan = assistPlan(p)
					return w, nil
				}
				caller, closeFn, err := a.assistCaller(ctx, cmd, p.plan)
				if err != nil {
					return w, err
				}
				defer closeFn()
				text, err := assist.Text(ctx, caller, p.prompt)
				w.Out = summaryOut{Summary: text, assistMeta: p.meta}
				return w, err
			})
			if err != nil {
				return err
			}
			return renderAssist(a, cmd, res, func(o summaryOut) (string, [][2]string, string) {
				return o.Summary, metaRows(o.assistMeta), ""
			})
		},
	}
	addAssistFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &summaryOut{}, "1.0")
	return c
}

func (a *app) conversationTriageCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "triage <conversation>",
		Short: "Suggest a category, priority, sentiment, labels and next action",
		Long: `Suggest how to triage the conversation <conversation> (record ID): a
category, a priority (` + strings.Join(assist.Priorities, ", ") + `), the customer's sentiment
(` + strings.Join(assist.Sentiments, ", ") + `), labels, and one sentence on the next action. Desk
and viewer roles.

These are suggestions only: nothing is applied, no label is added, no
status changes. Suggested labels are chosen from the tenant's own
(` + "`label list`" + `); a name the model makes up is dropped and listed as
dropped_labels, never created. ` + "`conversation label`" + ` applies the ones you
accept. The model's answer must fit a fixed JSON schema; if it does not,
it is asked once more, and a second failure is an error, never a guess.

` + helperHelp,
		Example: `  heo conversation triage conv_123
  heo conversation triage conv_123 --dry-run
  heo conversation triage conv_123 --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.refuseOffline(cmd); err != nil {
				return err
			}
			req := assistRequest{conversation: args[0], action: authz.ActionAssistRead, notes: true, labels: true,
				prompt: assist.TriagePrompt}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[triageOut], error) {
				var w written[triageOut]
				p, err := a.assistPrepare(ctx, c, req)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					w.Plan = assistPlan(p)
					return w, nil
				}
				caller, closeFn, err := a.assistCaller(ctx, cmd, p.plan)
				if err != nil {
					return w, err
				}
				defer closeFn()
				t, err := assist.RunTriage(ctx, caller, p.prompt, p.labels)
				w.Out = triageOut{Triage: t, assistMeta: p.meta}
				return w, err
			})
			if err != nil {
				return err
			}
			return renderAssist(a, cmd, res, func(o triageOut) (string, [][2]string, string) {
				rows := [][2]string{
					{"category", o.Category}, {"priority", o.Priority}, {"sentiment", o.Sentiment},
					{"labels", orDash(strings.Join(o.Labels, ", "))},
				}
				if len(o.DroppedLabels) > 0 {
					rows = append(rows, [2]string{"dropped labels", strings.Join(o.DroppedLabels, ", ") + " (not labels of this tenant)"})
				}
				rows = append(rows, [2]string{"next action", o.NextAction})
				return "", append(rows, metaRows(o.assistMeta)...), "suggestions only: nothing was applied; `heo conversation label` applies the ones you accept"
			})
		},
	}
	addAssistFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &triageOut{}, "1.0")
	return c
}

func (a *app) messageDraftCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "draft <conversation>",
		Short: "Draft a reply to a conversation; it is never sent",
		Long: `Draft the next reply in the conversation <conversation> (record ID),
from its public messages, as text. --instruction adds what the draft
should say and --tone how. Desk role (the right to reply).

A draft is never sent and creates no operation: it is text for you to
read and edit. A reply is public, so internal notes stay out of the
prompt unless --include-notes asks for them as private context. To send
the draft, preview it with ` + "`message reply --dry-run`" + ` and then send it:
--format text prints the draft alone on stdout (the status lines go to
stderr), so it fits ` + "`--text \"$(heo message draft <conversation> --format text)\"`" + `.

` + helperHelp,
		Example: `  heo message draft conv_123
  heo message draft conv_123 --instruction "offer a refund within 5 days" --tone friendly
  heo message draft conv_123 --dry-run
  heo message draft conv_123 --format text`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			instruction, _ := cmd.Flags().GetString(flagInstruction)
			tone, _ := cmd.Flags().GetString(flagTone)
			withNotes, _ := cmd.Flags().GetBool(flagWithNotes)
			if err := a.refuseOffline(cmd); err != nil {
				return err
			}
			if utf8.RuneCountInString(instruction) > maxInstruction {
				return output.UsageError(fmt.Sprintf("--%s: at most %d characters", flagInstruction, maxInstruction))
			}
			if utf8.RuneCountInString(tone) > maxTone {
				return output.UsageError(fmt.Sprintf("--%s: at most %d characters", flagTone, maxTone))
			}
			req := assistRequest{conversation: args[0], action: model.ActionMessageReply, notes: withNotes,
				prompt: func(e []assist.Entry, _ []string, n int) assist.Prompt {
					return assist.DraftPrompt(e, instruction, tone, n)
				}}
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[draftOut], error) {
				var w written[draftOut]
				p, err := a.assistPrepare(ctx, c, req)
				if err != nil {
					return w, err
				}
				if cli.IsDryRun(cmd) {
					w.Plan = assistPlan(p)
					return w, nil
				}
				caller, closeFn, err := a.assistCaller(ctx, cmd, p.plan)
				if err != nil {
					return w, err
				}
				defer closeFn()
				text, err := assist.Text(ctx, caller, p.prompt)
				w.Out = draftOut{Draft: text, assistMeta: p.meta}
				return w, err
			})
			if err != nil {
				return err
			}
			return renderAssist(a, cmd, res, func(o draftOut) (string, [][2]string, string) {
				return o.Draft, metaRows(o.assistMeta), "a draft only: nothing was sent; preview it with `heo message reply " + args[0] + " --text <draft> --dry-run`"
			})
		},
	}
	c.Flags().String(flagInstruction, "", "What the draft should say or do (at most 1000 characters)")
	c.Flags().String(flagTone, "", "The tone to write in, such as friendly or formal (at most 60 characters)")
	c.Flags().Bool(flagWithNotes, false, "Give the model the internal notes you may read, as private context")
	addAssistFlags(c)
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	setOutputSchema(c, &draftOut{}, "1.0")
	return c
}

// addAssistFlags adds the flag every helper takes: the per-call consent
// to a cloud provider.
func addAssistFlags(c *cobra.Command) {
	c.Flags().Bool(flagSendToCloud, false, "Allow this call to send the conversation's text to a cloud provider (refused without it)")
}

// metaRows is the call's facts as rows.
func metaRows(m assistMeta) [][2]string {
	msgs := fmt.Sprintf("%d", m.MessageCount)
	if m.MessagesInPrompt < m.MessageCount || m.Truncated {
		msgs = fmt.Sprintf("%d (%d in the prompt: the oldest were left out)", m.MessageCount, m.MessagesInPrompt)
	}
	return [][2]string{
		{"provider", fmt.Sprintf("%s (%s) %s", m.Provider, m.Class, m.Model)},
		{"messages", msgs},
		{"audience", m.Audience},
	}
}

// renderAssist prints a helper's answer: the plan of a preview, the
// structured output, or text. In text a body (the summary or the draft)
// comes first, then the rows; --format text on a draft prints the body
// alone on stdout. The machine-generated note and the helper's own go on
// stderr.
func renderAssist[R any](a *app, cmd *cobra.Command, w written[R], parts func(R) (string, [][2]string, string)) error {
	if w.Plan != nil {
		if err := cli.RenderPlan(cmd, *w.Plan); err != nil || a.structured() {
			return err
		}
		// As a desk write's preview does: the facts as rows, which the
		// effects table may leave out on a narrow terminal.
		var rows [][2]string
		for _, k := range assistPlanKeys {
			if v, ok := w.Plan.Args[k]; ok {
				rows = append(rows, [2]string{strings.ReplaceAll(k, "_", " "), fmt.Sprint(v)})
			}
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
			return err
		}
		return writeRows(cmd.OutOrStdout(), rows)
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, w.Out)
	}
	body, rows, note := parts(w.Out)
	out, errw := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if a.r.Viper.GetString("format") == output.Text && body != "" {
		if _, err := fmt.Fprintln(out, body); err != nil {
			return err
		}
	} else {
		if body != "" {
			if _, err := fmt.Fprintln(out, body); err != nil {
				return err
			}
			if _, err := fmt.Fprintln(out); err != nil {
				return err
			}
		}
		if err := writeRows(out, rows); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(errw, "note: machine-generated by the configured model; check it before relying on it"); err != nil {
		return err
	}
	if note != "" {
		_, err := fmt.Fprintln(errw, "note: "+note)
		return err
	}
	return nil
}
