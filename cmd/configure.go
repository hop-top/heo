package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/authz"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/daemon"
	"hop.top/heo/internal/errmap"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/core/id"
	"hop.top/uma"
	"hop.top/uma/model"
)

// Flags the channel and inbox leaves share.
const (
	flagLimit    = "limit"
	flagCursor   = "cursor"
	flagRevision = "revision"
	// flagOperationKey is kit's --idempotency-key, registered on every
	// conditional write; its value is the uma operation ID.
	flagOperationKey = "idempotency-key"
)

// defaultLimit is a list's page size when --limit is not set.
const defaultLimit = 50

// operationKeyPrefix prefixes the operation keys heo mints.
const operationKeyPrefix = "op"

// configure runs fn in the daemon with the caller's view of the
// profile's channels and inboxes, once heo's policy permits the caller
// action at all (see withClient). Each write fn submits is applied to
// the running receivers and webhook routes once uma answers it.
func (a *app) configure(ctx context.Context, c control.Caller, action string, fn func(channels.Service) error) error {
	return a.withClient(ctx, c, action, model.Audience{}, func(rt daemon.Runtime, client uma.Client, _ *authz.Policy) error {
		return fn(channels.Service{
			Engine: rt.Engine, Client: client, Tenant: c.Tenant, Adapters: rt.Adapters,
			Committed: a.runtime.Reconcile,
		})
	})
}

// withClient runs fn in the daemon with the open runtime, the caller's
// uma client and heo's policy, once the policy permits the caller
// action, on content under audience, at all: a caller without the role
// is refused UNAUTHORIZED (exit 5) on every surface, before any read.
// uma then authorizes each call fn makes for the caller.
func (a *app) withClient(ctx context.Context, c control.Caller, action string, audience model.Audience,
	fn func(daemon.Runtime, uma.Client, *authz.Policy) error,
) error {
	pol, err := a.authorization()
	if err != nil {
		return errmap.Of(err)
	}
	if err := permitted(pol, c, action, audience); err != nil {
		return err
	}
	err = a.runtime.Use(ctx, func(rt daemon.Runtime) error {
		client, err := rt.Engine.For(c.Principal())
		if err != nil {
			return err
		}
		return fn(rt, client, pol)
	})
	if errors.Is(err, daemon.ErrNotRunning) {
		e := output.PrerequisiteError("the daemon's runtime is not running: no runtime service holds the data store open")
		e.SuggestedFix = "start the daemon with its runtime services: `" + a.heoCommand("service start") + "`"
		return e
	}
	if err != nil {
		return errmap.Of(err)
	}
	return nil
}

// permitted refuses, as UNAUTHORIZED (exit 5), a caller heo's policy
// does not permit action on content under audience.
func permitted(pol *authz.Policy, c control.Caller, action string, audience model.Audience) error {
	ok, reason := pol.PermitsUnder(c.Principal(), action, audience)
	if ok {
		return nil
	}
	what := action
	if v := audience.Visibility; v != "" && v != model.Public {
		what += " on " + string(v) + " content"
	}
	e := output.UnauthorizedError(fmt.Sprintf("%s may not %s (%s)", c.Actor, what, reason))
	e.SuggestedFix = "ask the installation owner for a role that grants it (authorization.grants)"
	return e
}

// addPageFlags adds --limit and --cursor to a list leaf.
func addPageFlags(c *cobra.Command) {
	c.Flags().Int(flagLimit, defaultLimit, "Page size (1 to 100)")
	c.Flags().String(flagCursor, "", "Cursor from a previous page's next, to continue a listing")
}

func pageOf(cmd *cobra.Command) (model.PageRequest, error) {
	limit, _ := cmd.Flags().GetInt(flagLimit)
	cursor, _ := cmd.Flags().GetString(flagCursor)
	if limit < 1 || limit > 100 {
		return model.PageRequest{}, output.UsageError(fmt.Sprintf("--%s %d: want 1 to 100", flagLimit, limit))
	}
	return model.PageRequest{Limit: limit, After: model.Cursor(cursor)}, nil
}

// page is one page of a listing.
type page[T any] struct {
	Items    []T          `json:"items"    yaml:"items"`
	Next     model.Cursor `json:"next"     yaml:"next"`
	Complete bool         `json:"complete" yaml:"complete"`
}

func pageFrom[T any](p model.Page[T]) page[T] {
	items := p.Items
	if items == nil {
		items = []T{}
	}
	return page[T]{Items: items, Next: p.Next, Complete: p.Complete}
}

// addRevisionFlag adds the required --revision a replacing write names.
func addRevisionFlag(c *cobra.Command, kind string) {
	c.Flags().String(flagRevision, "", "Revision of the "+kind+" being replaced, from `"+kind+" get` (required)")
	_ = c.MarkFlagRequired(flagRevision)
}

func revisionOf(cmd *cobra.Command) model.Revision {
	v, _ := cmd.Flags().GetString(flagRevision)
	return model.Revision(v)
}

// operationKey returns the operation key a write submits under: the
// caller's --idempotency-key, or one minted here and set on the flag, so
// a command sent to the daemon carries it. A preview needs none.
func operationKey(cmd *cobra.Command) (string, error) {
	f := cmd.Flags().Lookup(flagOperationKey)
	if f == nil {
		return "", errors.New("command has no --" + flagOperationKey)
	}
	if k := f.Value.String(); k != "" {
		if err := model.OperationID(k).Validate(); err != nil {
			return "", output.UsageError("--" + flagOperationKey + ": " + err.Error())
		}
		return k, nil
	}
	if cli.IsDryRun(cmd) {
		return "", nil
	}
	k, err := id.New(operationKeyPrefix)
	if err != nil {
		return "", err
	}
	if err := cmd.Flags().Set(flagOperationKey, k); err != nil {
		return "", err
	}
	return k, nil
}

// planMode is how a write's plan holds the conflicts uma enforces: all
// final under --dry-run, uma's to decide when submitting, so a retry
// under the same --idempotency-key replays.
func planMode(cmd *cobra.Command) channels.Mode {
	if cli.IsDryRun(cmd) {
		return channels.Preview
	}
	return channels.Submit
}

// previewOperation stands in for the operation key while a preview
// validates a request: a preview submits nothing.
const previewOperation model.OperationID = "preview"

func opID(key string) model.OperationID {
	if key == "" {
		return previewOperation
	}
	return model.OperationID(key)
}

// refuseSecret refuses a flag value that looks like credential
// material: channels carry credential references, never credentials.
func refuseSecret(cmd *cobra.Command, names ...string) error {
	for _, n := range names {
		f := cmd.Flags().Lookup(n)
		if f == nil || !f.Changed {
			continue
		}
		if settings.LooksLikeSecret(f.Value.String()) {
			e := output.UsageError(fmt.Sprintf("--%s looks like a credential: heo takes credential references, never credentials", n))
			e.SuggestedFix = "store the credential in the profile's credential backend and pass its reference with --credential-ref"
			return e
		}
	}
	return nil
}

// operationView is the operation a write committed.
type operationView struct {
	ID       model.ID            `json:"id"       yaml:"id"`
	State    model.DeliveryState `json:"state"    yaml:"state"`
	Durable  bool                `json:"durable"  yaml:"durable"`
	Replayed bool                `json:"replayed" yaml:"replayed"`
	// AlreadyHeld: the proposal was not stored because an operation of
	// the same kind on the same target is already held; ID is that one.
	AlreadyHeld bool `json:"already_held,omitempty" yaml:"already_held,omitempty"`
}

func operationOf(acc model.Acceptance) operationView {
	return operationView{ID: acc.Operation.Ref.ID, State: acc.Operation.State, Durable: acc.Durable, Replayed: acc.Replayed,
		AlreadyHeld: acc.AlreadyHeld}
}

// written is a write leaf's output: under --dry-run the plan, otherwise
// the record as stored after the write and the operation that wrote it.
// Its JSON is the plan itself under --dry-run, so a preview reads the
// same on every surface and a forwarded one renders here unchanged.
type written[R any] struct {
	Plan *cli.Plan
	Out  R
}

func (w written[R]) MarshalJSON() ([]byte, error) {
	if w.Plan != nil {
		return json.Marshal(w.Plan)
	}
	return json.Marshal(w.Out)
}

func (w *written[R]) UnmarshalJSON(b []byte) error {
	var probe struct {
		Effects json.RawMessage `json:"effects"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	if probe.Effects != nil {
		w.Plan = &cli.Plan{}
		return json.Unmarshal(b, w.Plan)
	}
	return json.Unmarshal(b, &w.Out)
}

// render prints a write's output: the plan, the structured output, or
// the record's fields and the operation as rows, with notes on stderr.
func (w written[R]) render(a *app, cmd *cobra.Command, rows func(R) [][2]string, notes []string) error {
	if w.Plan != nil {
		return cli.RenderPlan(cmd, *w.Plan)
	}
	if a.structured() {
		return output.Dispatch(cmd, a.r.Viper, w.Out)
	}
	if err := writeRows(cmd.OutOrStdout(), rows(w.Out)); err != nil {
		return err
	}
	for _, n := range notes {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "note: "+n); err != nil {
			return err
		}
	}
	return nil
}

// planEffect describes one configuration write for a plan.
func planEffect(kind, target string, reversible bool, detail string) cli.Effect {
	return cli.Effect{Kind: kind, Target: target, Reversible: reversible, Detail: detail}
}

func writeRows(w io.Writer, rows [][2]string) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", r[0], r[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func operationRows(op operationView, key string) [][2]string {
	return [][2]string{
		{"operation", fmt.Sprintf("%s (%s%s%s)", op.ID, op.State, map[bool]string{true: ", replayed"}[op.Replayed],
			map[bool]string{true: ", already held"}[op.AlreadyHeld])},
		{"operation key", key},
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
