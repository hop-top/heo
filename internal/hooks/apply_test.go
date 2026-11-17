package hooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/heo/internal/hooks"
	"hop.top/uma/model"
)

// store is a dispatcher that dedupes the way uma does: an operation is
// keyed by (actor, key), the same key with the same content is a replay
// and with other content a conflict.
type store struct {
	mu      sync.Mutex
	known   map[string]bool
	applied map[string]string // actor|key -> content
	effects []string          // contents applied, in order
	actors  []model.ActorID
	calls   int
	failOn  map[string]error // verb -> error, until cleared
}

func newStore(verbs ...string) *store {
	s := &store{known: map[string]bool{}, applied: map[string]string{}, failOn: map[string]error{}}
	for _, v := range verbs {
		s.known[v] = true
	}
	return s
}

func (s *store) Check(c hooks.Call) error {
	if !s.known[c.Verb] {
		return fmt.Errorf("unknown verb %q", c.Verb)
	}
	return nil
}

func (s *store) Dispatch(_ context.Context, actor model.ActorID, c hooks.Call) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.actors = append(s.actors, actor)
	if err := s.failOn[c.Verb]; err != nil {
		return err
	}
	content, _ := json.Marshal([]any{c.Verb, c.Args, c.Flags})
	k := string(actor) + "|" + c.Key
	if prev, ok := s.applied[k]; ok {
		if prev != string(content) {
			return errors.New("key reused with other content")
		}
		return nil // replayed
	}
	s.applied[k] = string(content)
	s.effects = append(s.effects, string(content))
	return nil
}

func (s *store) effectCount() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.effects) }

type say struct {
	mu   sync.Mutex
	info []string
}

func (l *say) Info(msg any, kv ...any) {
	l.mu.Lock()
	l.info = append(l.info, fmt.Sprint(append([]any{msg}, kv...)...))
	l.mu.Unlock()
}
func (l *say) Error(any, ...any) {}

func delivery(plugin, ev string) hooks.Delivery {
	return hooks.Delivery{Plugin: plugin, Event: event(ev, "c1"), Attempt: 1}
}

func ops(o ...hooks.Operation) hooks.Result { return hooks.Result{Operations: o} }

func op(verb string, args ...string) hooks.Operation {
	return hooks.Operation{Verb: verb, Args: args}
}

func TestApplyRunsInOrderAsThePluginActor(t *testing.T) {
	s := newStore("label create", "note add", "conversation resolve")
	a := &hooks.OpApplier{Dispatch: s}
	res := ops(op("label create", "bug"), op("note add", "c1"), op("conversation resolve", "c1"))
	if err := a.Apply(t.Context(), delivery("triage", "e1"), res); err != nil {
		t.Fatal(err)
	}
	if s.effectCount() != 3 {
		t.Fatalf("applied %d, want 3", s.effectCount())
	}
	for i, got := range s.effects {
		if !strings.Contains(got, res.Operations[i].Verb) {
			t.Errorf("effect %d = %s, want verb %s: out of order", i, got, res.Operations[i].Verb)
		}
	}
	for _, actor := range s.actors {
		if actor != "plugin:triage" {
			t.Errorf("actor = %s, want plugin:triage", actor)
		}
	}
}

func TestKeyDefaultsToPluginEventAndContent(t *testing.T) {
	flagged := hooks.Operation{Verb: "note add", Args: []string{"c1"}, Flags: map[string]any{"text": "hi", "placement": "local"}}
	reordered := hooks.Operation{Verb: "note add", Args: []string{"c1"}, Flags: map[string]any{"placement": "local", "text": "hi"}}
	other := hooks.Operation{Verb: "note add", Args: []string{"c1"}, Flags: map[string]any{"text": "bye", "placement": "local"}}
	k := func(plugin, ev string, o hooks.Operation) string {
		key := hooks.OperationKey(plugin, model.ID(ev), o)
		if err := model.OperationID(key).Validate(); err != nil {
			t.Fatalf("key %q is not a valid operation ID: %v", key, err)
		}
		return key
	}
	base := k("triage", "e1", flagged)
	if k("triage", "e1", reordered) != base {
		t.Error("flag order changed the key: content is canonical JSON")
	}
	for name, got := range map[string]string{
		"other content": k("triage", "e1", other), "other event": k("triage", "e2", flagged), "other plugin": k("other", "e1", flagged),
	} {
		if got == base {
			t.Errorf("%s: key unchanged", name)
		}
	}
	if !strings.Contains(base, "triage") || !strings.Contains(base, "e1") {
		t.Errorf("key %q names neither the plugin nor the event", base)
	}
	own := flagged
	own.IdempotencyKey = "mine"
	if k("triage", "e1", own) != "mine" {
		t.Error("a plugin's own key is not used as given")
	}
	long := strings.Repeat("e", 300)
	if len(k("triage", long, flagged)) > 255 {
		t.Error("derived key exceeds uma's 255 bytes")
	}
}

func TestApplyKeysAreStableAcrossRedeliveries(t *testing.T) {
	s := newStore("note add")
	a := &hooks.OpApplier{Dispatch: s}
	res := ops(op("note add", "c1"))
	for range 3 {
		if err := a.Apply(t.Context(), delivery("triage", "e1"), res); err != nil {
			t.Fatal(err)
		}
	}
	if s.calls != 3 || s.effectCount() != 1 {
		t.Errorf("%d calls, %d effects, want 3 and 1: a redelivery must replay, not apply twice", s.calls, s.effectCount())
	}
}

func TestInvalidResultIsRejectedWhole(t *testing.T) {
	for name, r := range map[string]hooks.Result{
		"no verb":          ops(op("note add", "c1"), op("")),
		"verb not a verb":  ops(op("note add", "c1"), op("Note Add")),
		"single word verb": ops(op("note add", "c1"), op("note")),
		"flag not scalar":  ops(op("note add", "c1"), hooks.Operation{Verb: "note add", Flags: map[string]any{"text": []any{"x"}}}),
		"flag nil":         ops(op("note add", "c1"), hooks.Operation{Verb: "note add", Flags: map[string]any{"text": nil}}),
		"reserved key":     ops(op("note add", "c1"), hooks.Operation{Verb: "note add", Flags: map[string]any{"idempotency-key": "k"}}),
		"reserved dry-run": ops(op("note add", "c1"), hooks.Operation{Verb: "note add", Flags: map[string]any{"dry-run": true}}),
		"key too long":     ops(op("note add", "c1"), hooks.Operation{Verb: "note add", IdempotencyKey: strings.Repeat("k", 201)}),
		"version":          {HeoVersion: 2, Operations: []hooks.Operation{op("note add", "c1")}},
	} {
		s := newStore("note add")
		err := (&hooks.OpApplier{Dispatch: s}).Apply(t.Context(), delivery("triage", "e1"), r)
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if s.calls != 0 {
			t.Errorf("%s: %d operations applied, want none: a rejected result applies nothing", name, s.calls)
		}
		var oe *hooks.OpError
		if errors.As(err, &oe) {
			t.Errorf("%s: rejection reported as an operation failure: %v", name, err)
		}
	}
}

func TestUnknownVerbRejectsTheWholeResult(t *testing.T) {
	s := newStore("note add")
	err := (&hooks.OpApplier{Dispatch: s}).Apply(t.Context(), delivery("triage", "e1"),
		ops(op("note add", "c1"), op("plugin token mint", "x")))
	if err == nil || !strings.Contains(err.Error(), "operations[1]") {
		t.Fatalf("err = %v, want a rejection naming operations[1]", err)
	}
	if s.calls != 0 {
		t.Errorf("%d operations applied: the valid first one must wait for the whole result to check out", s.calls)
	}
}

func TestAuthorizationRefusalStopsTheRun(t *testing.T) {
	s := newStore("label create", "note add", "conversation resolve")
	s.failOn["note add"] = hooks.Permanent(errors.New("UNAUTHORIZED: plugin:triage holds no role for message.note"))
	err := (&hooks.OpApplier{Dispatch: s}).Apply(t.Context(), delivery("triage", "e1"),
		ops(op("label create", "bug"), op("note add", "c1"), op("conversation resolve", "c1")))
	if !hooks.IsPermanent(err) {
		t.Errorf("err = %v, want a permanent failure: a refusal is not retried", err)
	}
	var oe *hooks.OpError
	if !errors.As(err, &oe) || oe.Index != 1 || oe.Verb != "note add" {
		t.Fatalf("err = %v, want an OpError at index 1", err)
	}
	if s.effectCount() != 1 || s.calls != 2 {
		t.Errorf("%d effects over %d calls, want 1 over 2: earlier stays applied, later never runs", s.effectCount(), s.calls)
	}
}

func TestMidListFailureThenRetrySkipsWhatIsApplied(t *testing.T) {
	s := newStore("label create", "note add", "conversation resolve")
	s.failOn["note add"] = errors.New("uma unavailable")
	a := &hooks.OpApplier{Dispatch: s}
	res := ops(op("label create", "bug"), op("note add", "c1"), op("conversation resolve", "c1"))

	err := a.Apply(t.Context(), delivery("triage", "e1"), res)
	var oe *hooks.OpError
	if !errors.As(err, &oe) || oe.Index != 1 {
		t.Fatalf("err = %v, want an OpError at index 1", err)
	}
	if hooks.IsPermanent(err) {
		t.Error("an unavailable store is retried, not dead-lettered")
	}
	if s.effectCount() != 1 {
		t.Fatalf("%d applied before the retry, want 1", s.effectCount())
	}

	delete(s.failOn, "note add")
	d := delivery("triage", "e1")
	d.Attempt = 2
	if err := a.Apply(t.Context(), d, res); err != nil {
		t.Fatal(err)
	}
	if s.effectCount() != 3 {
		t.Errorf("%d effects after the retry, want 3: the label must not be applied twice", s.effectCount())
	}
	if n := countOf(s.effects, "label create"); n != 1 {
		t.Errorf("label create applied %d times, want 1", n)
	}
}

func countOf(effects []string, verb string) int {
	n := 0
	for _, e := range effects {
		if strings.Contains(e, verb) {
			n++
		}
	}
	return n
}

func TestDryRunRecordsAndAppliesNothing(t *testing.T) {
	s := newStore("label create", "note add")
	l := &say{}
	a := &hooks.OpApplier{Dispatch: s, Logger: l}
	d := delivery("triage", "e1")
	d.DryRun = true
	if err := a.Apply(t.Context(), d, ops(op("label create", "bug"), op("note add", "c1"))); err != nil {
		t.Fatal(err)
	}
	if s.calls != 0 {
		t.Errorf("%d operations dispatched in a dry run", s.calls)
	}
	joined := strings.Join(l.info, "\n")
	for _, want := range []string{"label create", "note add", "triage", "e1", "hook"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dry run recorded %q, want it to name %q", joined, want)
		}
	}
	// A dry run still refuses what a real run would refuse.
	d2 := delivery("triage", "e2")
	d2.DryRun = true
	if err := a.Apply(t.Context(), d2, ops(op("nope nope"))); err == nil {
		t.Error("a dry run accepted an unknown verb")
	}
}

func TestParseResultReadsTheSchemaStrictly(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":     `{"operations":[],"extra":1}`,
		"unknown op field":  `{"operations":[{"verb":"note add","target":"c1"}]}`,
		"bad verb":          `{"operations":[{"verb":"Note"}]}`,
		"flag object":       `{"operations":[{"verb":"note add","flags":{"text":{}}}]}`,
		"args not strings":  `{"operations":[{"verb":"note add","args":[1]}]}`,
		"operations absent": `{"message":"hi"}`,
	} {
		if _, err := hooks.ParseResult([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	r, err := hooks.ParseResult([]byte(`{"operations":[{"verb":"issue update","args":["i1"],"flags":{"milestone":12345678901234567890,"force":true}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(r.Operations[0].Flags["milestone"]); got != "12345678901234567890" {
		t.Errorf("number flag = %s, want it kept exactly", got)
	}
	if r, err := hooks.ParseResult(nil); err != nil || len(r.Operations) != 0 {
		t.Errorf("empty output = %+v, %v, want no operations", r, err)
	}
}

// TestResultSchemaMatchesTheDispatcherShape: the published schema names
// the fields the applier reads, bounds them as it does and reserves the
// flags it reserves.
func TestResultSchemaMatchesTheDispatcherShape(t *testing.T) {
	raw, err := os.ReadFile("../../docs/hook-result.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var sch struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       struct {
			Operation struct {
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"operation"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatal(err)
	}
	tags := func(v any) []string {
		var out []string
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			out = append(out, strings.Split(rt.Field(i).Tag.Get("json"), ",")[0])
		}
		slices.Sort(out)
		return out
	}
	keys := func(m map[string]json.RawMessage) []string {
		out := slices.Collect(maps.Keys(m))
		slices.Sort(out)
		return out
	}
	if got, want := keys(sch.Properties), tags(hooks.Result{}); !slices.Equal(got, want) {
		t.Errorf("result properties = %v, want the fields of Result %v", got, want)
	}
	if got, want := keys(sch.Defs.Operation.Properties), tags(hooks.Operation{}); !slices.Equal(got, want) {
		t.Errorf("operation properties = %v, want the fields of Operation %v", got, want)
	}
	var verb struct {
		Pattern string `json:"pattern"`
	}
	var key struct {
		MaxLength int `json:"maxLength"`
	}
	var flags struct {
		Names struct {
			Not struct {
				Enum []string `json:"enum"`
			} `json:"not"`
		} `json:"propertyNames"`
	}
	_ = json.Unmarshal(sch.Defs.Operation.Properties["verb"], &verb)
	_ = json.Unmarshal(sch.Defs.Operation.Properties["idempotency_key"], &key)
	_ = json.Unmarshal(sch.Defs.Operation.Properties["flags"], &flags)
	if verb.Pattern != hooks.VerbPattern {
		t.Errorf("schema verb pattern %q, applier %q", verb.Pattern, hooks.VerbPattern)
	}
	if key.MaxLength != hooks.MaxKeyLen {
		t.Errorf("schema key maxLength %d, applier %d", key.MaxLength, hooks.MaxKeyLen)
	}
	got := slices.Clone(flags.Names.Not.Enum)
	want := slices.Clone(hooks.ReservedFlags)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("schema reserves flags %v, applier %v", got, want)
	}
	if !slices.Equal(sch.Defs.Operation.Required, []string{"verb"}) {
		t.Errorf("operation requires %v, want only verb", sch.Defs.Operation.Required)
	}
}

// TestRetryThatReturnsOtherOperationsDoesNotCollide: the default key is
// the operation's content, not its position, so a retry whose result
// differs from the first attempt's applies what is new and replays what
// is not.
func TestRetryThatReturnsOtherOperationsDoesNotCollide(t *testing.T) {
	s := newStore("label create", "note add")
	s.failOn["note add"] = errors.New("uma unavailable")
	a := &hooks.OpApplier{Dispatch: s}
	if err := a.Apply(t.Context(), delivery("triage", "e1"), ops(op("label create", "bug"), op("note add", "c1"))); err == nil {
		t.Fatal("want the note to fail")
	}
	delete(s.failOn, "note add")
	d := delivery("triage", "e1")
	d.Attempt = 2
	if err := a.Apply(t.Context(), d, ops(op("label create", "urgent"), op("label create", "bug"), op("note add", "c1"))); err != nil {
		t.Fatalf("retry with other operations: %v", err)
	}
	if got := countOf(s.effects, "label create"); got != 2 {
		t.Errorf("%d labels created, want bug once and urgent once", got)
	}
}
