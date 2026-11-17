package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/control"
	"hop.top/heo/internal/desk"
	"hop.top/heo/internal/errmap"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// argsReadLimit bounds the args document a command reads.
const argsReadLimit = 1 << 20

// readArgsInput reads --args for a command typed at a terminal: "-" is
// standard input, a value that is a JSON object is the document itself,
// anything else a file. At most argsReadLimit bytes are read.
func readArgsInput(cmd *cobra.Command, name string) ([]byte, error) {
	var r io.Reader
	switch trimmed := strings.TrimSpace(name); {
	case trimmed == "":
		return nil, output.UsageError("--" + flagArgs + ": name the file holding the edited arguments, or - for standard input")
	case strings.HasPrefix(trimmed, "{"):
		return []byte(trimmed), nil
	case trimmed == "-":
		r = cmd.InOrStdin()
	default:
		f, err := os.Open(name) //nolint:gosec // G304: the caller named the file
		if err != nil {
			var pe *os.PathError
			if errors.As(err, &pe) {
				err = pe.Err
			}
			return nil, output.UsageError(fmt.Sprintf("--%s %s: %v", flagArgs, name, err))
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, argsReadLimit+1))
	if err != nil {
		return nil, output.UsageError(fmt.Sprintf("--%s: %v", flagArgs, err))
	}
	if len(b) > argsReadLimit {
		return nil, output.UsageError(fmt.Sprintf("--%s: larger than %d KiB", flagArgs, argsReadLimit>>10))
	}
	return b, nil
}

// editArgs is what an --args document says: the edited intent, and the
// operation and revision it was read at when it is `held show` output.
type editArgs struct {
	Intent    json.RawMessage
	Operation string
	Revision  string
}

// parseEditArgs reads the document of --args. It is either the output of
// `held show --format json` with its intent edited (a JSON object with an
// "intent" member: that member is the new intent, and id and revision say
// what it was read from) or the intent itself.
func parseEditArgs(raw string) (editArgs, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	var doc map[string]json.RawMessage
	if err := dec.Decode(&doc); err != nil || doc == nil {
		return editArgs{}, output.UsageError("--" + flagArgs + ": want a JSON object: the held operation as `held show --format json` prints it, with intent edited")
	}
	if dec.More() {
		return editArgs{}, output.UsageError("--" + flagArgs + ": more than one JSON document")
	}
	in, shown := doc["intent"]
	if !shown {
		return editArgs{Intent: json.RawMessage(raw)}, nil
	}
	out := editArgs{Intent: in}
	if t := bytes.TrimSpace(in); len(t) == 0 || t[0] != '{' {
		return editArgs{}, output.UsageError("--" + flagArgs + ": intent: want a JSON object")
	}
	for k, dst := range map[string]*string{"id": &out.Operation, "revision": &out.Revision} {
		if v, ok := doc[k]; ok {
			if err := json.Unmarshal(v, dst); err != nil {
				return editArgs{}, output.UsageError("--" + flagArgs + ": " + k + ": want a string")
			}
		}
	}
	return out, nil
}

func (a *app) heldEditCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "edit [<operation>]",
		Short: "Edit a held operation's arguments and approve it as edited",
		Long: `Replace the arguments of the held operation <operation> and approve it as
edited: uma queues it with your arguments in place of the proposal's, and
the daemon's workers apply them. The edit counts against the proposer's
trust as a correction, on the operation's own trust key unless
--corrected-keys names the keys the edit changed.

--args is the edited arguments as JSON: a file, - for standard input, or
the JSON itself. Seed it from ` + "`held show --format json`" + `: save that output,
edit its intent and pass it. A document with an intent member also names
the operation (id) and the revision it was read at, which --revision
overrides; a document without one is the intent itself. uma refuses
arguments that change what the operation targets, its audience or its
base, or that are not the intent's shape (exit 2). Name the operation by
<operation> or, where a positional argument cannot be passed (MCP), by
--operation; not both.

An edit after the target changed, a new inbound message on the
conversation say, is refused as stale (exit 4): read the operation again
with ` + "`held show`" + ` and edit what you now see; an edit has no --force.
--note records why. --dry-run shows what the edit would do and writes
nothing. --idempotency-key tags the call; an edit is idempotent whatever
it is, so a rerun returns the stored result.

A plugin actor may not decide: it is refused, exit 5.`,
		Example: `  heo operation held edit op_123 --args edit.json --note "fixed the greeting"
  heo operation held edit op_123 --args edit.json --dry-run
  heo operation held edit --operation op_123 --args "$(cat edit.json)"`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ids, err := heldIDs(cmd, args)
			if err != nil {
				return err
			}
			raw, _ := cmd.Flags().GetString(flagArgs)
			route, _ := control.RouteOf(cmd.Context(), a.control.servesControl())
			if route == control.Forward { // the daemon cannot see this process's files
				b, err := readArgsInput(cmd, raw)
				if err != nil {
					return err
				}
				raw = string(b)
				if err := cmd.Flags().Set(flagArgs, raw); err != nil {
					return err
				}
			}
			doc, err := parseEditArgs(raw)
			if err != nil {
				return err
			}
			if doc.Operation != "" && doc.Operation != ids[0] {
				return output.UsageError(fmt.Sprintf("--%s names operation %s, not %s: seed it from `held show %s --format json`", flagArgs, doc.Operation, ids[0], ids[0]))
			}
			key, err := operationKey(cmd)
			if err != nil {
				return err
			}
			rev := revisionOf(cmd)
			if rev == "" {
				rev = model.Revision(doc.Revision)
			}
			note, _ := cmd.Flags().GetString(flagDecisionNote)
			corrected, _ := cmd.Flags().GetStringSlice(flagCorrectedKeys)
			res, err := daemonRun(a, cmd, args, func(ctx context.Context, c control.Caller) (written[heldDecideOut], error) {
				var w written[heldDecideOut]
				err := a.desk(ctx, c, model.ActionOperationEdit, model.Audience{}, func(s desk.Service) error {
					if cli.IsDryRun(cmd) {
						if c.Actor.IsPlugin() {
							return model.NewError(model.Unauthorized, "a plugin actor may not decide a held operation")
						}
						h, err := s.GetHeld(ctx, ids[0])
						if err != nil {
							return err
						}
						if h.Status != string(model.Held) {
							return model.NewError(model.Conflict, fmt.Sprintf("operation %s is %s, not held", ids[0], h.Status))
						}
						p := &cli.Plan{Args: map[string]any{"operations": []string{ids[0]}, "decision": "edit", "note": note,
							"expected_revision": string(rev), "operation_key": key},
							PrerequisitesChecked: []string{"operation", "held", "authorization", "arguments are JSON"}}
						if len(corrected) > 0 {
							p.Args["corrected_keys"] = corrected
						}
						p.Effects = append(p.Effects, planEffect("update", "operation:"+ids[0], false, fmt.Sprintf(
							"edit %s on %s proposed by %s: replaces its %d-byte intent with %d bytes and queues it as edited; counts as a correction",
							h.Kind, h.Target, h.Proposer, len(h.Intent), len(doc.Intent))))
						w.Plan = p
						return nil
					}
					dr, err := s.EditHeld(ctx, desk.EditRequest{ID: ids[0], Args: doc.Intent, CorrectedKeys: corrected,
						Revision: rev, Note: note, Correlation: key})
					r := heldResult{Operation: ids[0], OK: err == nil}
					out := heldDecideOut{Decision: "edit", Results: []heldResult{}, OperationKey: key}
					if err != nil {
						e := errmap.Of(err)
						r.Error, r.ErrorCode = e.Message, e.Code
						out.Failed++
					} else {
						r.Status, r.Decision, r.Replayed, r.Correction, r.Revision = dr.Status, dr.Decision, dr.Replayed, dr.Correction, dr.Revision
					}
					out.Results = append(out.Results, r)
					w.Out = out
					return nil
				})
				return w, err
			})
			if err != nil {
				return err
			}
			if err := renderDeskWrite(a, cmd, res, heldDecideRows, nil); err != nil || res.Plan != nil {
				return err
			}
			return heldFailure(res.Out)
		},
	}
	c.Flags().String(flagOperation, "", "The held operation, in place of <operation> (for MCP, which cannot pass it)")
	c.Flags().String(flagArgs, "", "The edited arguments as JSON: `held show --format json` with its intent edited; a file, - for standard input, or the JSON itself (required)")
	_ = c.MarkFlagRequired(flagArgs)
	c.Flags().StringSlice(flagCorrectedKeys, nil, "The trust keys the edit corrects (default: the operation's own); repeatable")
	c.Flags().String(flagRevision, "", "Revision of the held operation you read, from `held show` (default: the one in --args, else the current)")
	c.Flags().String(flagDecisionNote, "", "Why: recorded with the decision")
	daemonCommand(c)
	cli.SetSideEffect(c, cli.SideEffectWriteShared)
	cli.SetIdempotency(c, cli.IdempotencyConditional)
	setOutputSchema(c, &heldDecideOut{}, "1.0")
	return c
}
