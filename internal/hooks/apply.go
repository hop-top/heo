package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"hop.top/heo/internal/credentials"
	"hop.top/uma/model"
)

// Limits of the result schema (docs/hook-result.schema.json).
const (
	// MaxKeyLen is the longest idempotency_key a plugin may name.
	MaxKeyLen = 200
	// maxOperationID is uma's bound on an operation ID, which is what
	// the key becomes.
	maxOperationID = 255
)

// VerbPattern is what the schema allows as a verb: two or more words.
const VerbPattern = `^[a-z]+( [a-z-]+)+$`

var verbPattern = regexp.MustCompile(VerbPattern)

// ReservedFlags are the flags a plugin does not set: the key comes from
// the operation, and a plugin neither previews (an operation that
// silently did nothing) nor confirms for anyone.
var ReservedFlags = []string{"idempotency-key", "dry-run", "confirm"}

// Validate checks r against the result schema: the version, each verb's
// form, each flag's type and the keys' bounds. It does not know which
// verbs exist; the Dispatcher does.
func (r Result) Validate() error {
	if r.HeoVersion != 0 && r.HeoVersion != EnvelopeVersion {
		return fmt.Errorf("result version %d: this heo reads %d", r.HeoVersion, EnvelopeVersion)
	}
	for i, op := range r.Operations {
		if err := op.validate(); err != nil {
			return fmt.Errorf("operations[%d]: %w", i, err)
		}
	}
	return nil
}

func (op Operation) validate() error {
	switch {
	case op.Verb == "":
		return errors.New("no verb")
	case !verbPattern.MatchString(op.Verb):
		return fmt.Errorf("verb %q: want a heo verb in lower case such as \"message reply\"", op.Verb)
	case len(op.IdempotencyKey) > MaxKeyLen:
		return fmt.Errorf("idempotency_key is longer than %d bytes", MaxKeyLen)
	}
	if op.IdempotencyKey != "" {
		if err := model.OperationID(op.IdempotencyKey).Validate(); err != nil {
			return fmt.Errorf("idempotency_key: %w", err)
		}
	}
	for name, v := range op.Flags {
		for _, r := range ReservedFlags {
			if name == r {
				return fmt.Errorf("flag %q is not the plugin's to set", name)
			}
		}
		switch v.(type) {
		case string, bool, json.Number, int, int64, float64:
		default:
			return fmt.Errorf("flag %q: want a string, number or boolean", name)
		}
	}
	return nil
}

// Call is one operation as the Dispatcher takes it: the same verb,
// positional arguments and flags a request to the daemon's command
// surface names, and the key the operation is committed under.
type Call struct {
	// Verb is the command path, e.g. "message reply".
	Verb  string
	Args  []string
	Flags map[string]any
	// Key is the idempotency key.
	Key string
}

// Dispatcher runs a command as an actor through the code path the CLI
// verb takes, so the actor's authorization decides. heo's is the command
// tree's bridge (cmd).
type Dispatcher interface {
	// Check reports why a plugin may not run c: no such verb, one that
	// never runs in the daemon (terminal only, self-hosting), one that
	// cannot be replayed, or a flag the verb lacks. It runs nothing.
	Check(c Call) error
	// Dispatch runs c as actor. A refusal that retrying will not change
	// (authorization, usage) is wrapped by Permanent.
	Dispatch(ctx context.Context, actor model.ActorID, c Call) error
}

// OpError is the failure of one operation of a result: the index and
// verb of the operation that stopped the run, and how many before it
// stay applied. A retry re-sends the event, and the applied operations
// replay as no-ops under their keys.
type OpError struct {
	Index   int
	Verb    string
	Applied int
	Err     error
}

func (e *OpError) Error() string { return fmt.Sprintf("operations[%d] %s: %v", e.Index, e.Verb, e.Err) }
func (e *OpError) Unwrap() error { return e.Err }

// OpApplier applies a result operation by operation through a
// Dispatcher, as the plugin's actor.
type OpApplier struct {
	Dispatch Dispatcher
	// Logger receives the record of a dry run; nil keeps none.
	Logger Logger
}

// Apply implements Applier. A result that fails the schema or names a
// verb the Dispatcher refuses is rejected whole: nothing is applied. A
// valid one is applied in order and stops at the first operation that
// fails, an *OpError; earlier ones stay applied.
func (a *OpApplier) Apply(ctx context.Context, d Delivery, r Result) error {
	if err := r.Validate(); err != nil {
		return &Failure{Class: ClassMalformed, Reason: fmt.Sprintf("result rejected: %v", err)}
	}
	calls := make([]Call, len(r.Operations))
	for i, op := range r.Operations {
		calls[i] = Call{Verb: op.Verb, Args: op.Args, Flags: op.Flags, Key: OperationKey(d.Plugin, d.Event.ID, op)}
		if err := a.Dispatch.Check(calls[i]); err != nil {
			return &Failure{Class: ClassMalformed, Reason: fmt.Sprintf("result rejected: operations[%d] %s: %v", i, op.Verb, err)}
		}
	}
	if d.DryRun {
		a.record(d, calls)
		return nil
	}
	actor := credentials.PluginActor(d.Plugin)
	for i, c := range calls {
		if err := a.Dispatch.Dispatch(ctx, actor, c); err != nil {
			return &OpError{Index: i, Verb: c.Verb, Applied: i, Err: err}
		}
	}
	return nil
}

func (a *OpApplier) record(d Delivery, calls []Call) {
	if a.Logger == nil {
		return
	}
	a.Logger.Info("hooks: dry run, nothing applied", "plugin", d.Plugin, "event", string(d.Event.ID),
		"attempt", d.Attempt, "operations", len(calls))
	for i, c := range calls {
		args, _ := json.Marshal(c.Args)
		flags, _ := json.Marshal(c.Flags)
		a.Logger.Info("hooks: dry run, would apply", "plugin", d.Plugin, "event", string(d.Event.ID),
			"index", i, "verb", c.Verb, "args", string(args), "flags", string(flags), "key", c.Key)
	}
}

// OperationKey is the idempotency key of op for the event ev of plugin:
// the plugin's own idempotency_key, or one derived from the plugin, the
// event ID and a hash of the operation's content (verb, args and flags
// as canonical JSON), so a redelivery derives the same key and a retry
// that returns other operations does not collide with the first. A key
// too long for uma is hashed whole.
func OperationKey(plugin string, ev model.ID, op Operation) string {
	if op.IdempotencyKey != "" {
		return op.IdempotencyKey
	}
	args, flags := op.Args, op.Flags
	if args == nil {
		args = []string{}
	}
	if flags == nil {
		flags = map[string]any{}
	}
	// encoding/json writes map keys sorted: canonical for flags.
	canon, _ := json.Marshal(struct {
		Verb  string         `json:"verb"`
		Args  []string       `json:"args"`
		Flags map[string]any `json:"flags"`
	}{strings.Join(strings.Fields(op.Verb), " "), args, flags})
	sum := sha256.Sum256(canon)
	h := hex.EncodeToString(sum[:12])
	if k := "hook:" + plugin + ":" + string(ev) + ":" + h; len(k) <= maxOperationID && model.OperationID(k).Validate() == nil {
		return k
	}
	whole := sha256.Sum256([]byte(plugin + "\x00" + string(ev) + "\x00" + string(canon)))
	return "hook:" + hex.EncodeToString(whole[:])
}

// PlannedOp is one operation of a result as it would be applied: the
// call, the key it would run under and whether Check would let it run.
type PlannedOp struct {
	Index   int            `json:"index" yaml:"index"`
	Verb    string         `json:"verb" yaml:"verb"`
	Args    []string       `json:"args,omitempty" yaml:"args,omitempty"`
	Flags   map[string]any `json:"flags,omitempty" yaml:"flags,omitempty"`
	Key     string         `json:"idempotency_key" yaml:"idempotency_key"`
	Allowed bool           `json:"allowed" yaml:"allowed"`
	// Refusal is why Check would refuse the operation.
	Refusal string `json:"refusal,omitempty" yaml:"refusal,omitempty"`
}

// Planner shows what applying a result would do, running nothing. An
// Applier that is one lets a test run report the operations and their
// checks.
type Planner interface {
	// Plan checks r as Apply would. The returned reject is why Apply
	// would reject the whole result, empty when it would apply it.
	Plan(d Delivery, r Result) (ops []PlannedOp, reject string)
}

// Plan implements Planner: the schema and each verb's Check, as Apply
// runs them, and nothing dispatched.
func (a *OpApplier) Plan(d Delivery, r Result) ([]PlannedOp, string) {
	ops := make([]PlannedOp, len(r.Operations))
	var reject string
	if err := r.Validate(); err != nil {
		reject = fmt.Sprintf("result rejected: %v", err)
	}
	for i, op := range r.Operations {
		c := Call{Verb: op.Verb, Args: op.Args, Flags: op.Flags, Key: OperationKey(d.Plugin, d.Event.ID, op)}
		ops[i] = PlannedOp{Index: i, Verb: op.Verb, Args: op.Args, Flags: op.Flags, Key: c.Key, Allowed: true}
		if err := a.Dispatch.Check(c); err != nil {
			ops[i].Allowed, ops[i].Refusal = false, err.Error()
			if reject == "" {
				reject = fmt.Sprintf("result rejected: operations[%d] %s: %v", i, op.Verb, err)
			}
		}
	}
	return ops, reject
}
