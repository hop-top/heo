package assist

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"hop.top/kit/go/ai/llm"
	"hop.top/kit/go/console/output"
)

// Triage is a model's suggestions for a conversation. They are
// suggestions only: nothing here is applied to the conversation.
type Triage struct {
	// Category is a short topic.
	Category string `json:"category"    yaml:"category"`
	// Priority is low, normal, high or urgent.
	Priority string `json:"priority"    yaml:"priority"`
	// Sentiment is the customer's: negative, neutral or positive.
	Sentiment string `json:"sentiment"   yaml:"sentiment"`
	// Labels are suggested label names, each one the tenant defines.
	Labels []string `json:"labels"      yaml:"labels"`
	// DroppedLabels are the names the model suggested that the tenant
	// does not define; they are reported, never created or applied.
	DroppedLabels []string `json:"dropped_labels" yaml:"dropped_labels"`
	// NextAction is one sentence on what the team should do next.
	NextAction string `json:"next_action" yaml:"next_action"`
}

// The values a triage may hold.
var (
	Priorities = []string{"low", "normal", "high", "urgent"}
	Sentiments = []string{"negative", "neutral", "positive"}
)

const (
	maxCategory   = 40
	maxNextAction = 300
	maxSuggested  = 20
)

// ErrInvalid is a model answer that is not a valid triage.
var ErrInvalid = errors.New("not a valid triage")

// rawTriage is the answer as the model gives it.
type rawTriage struct {
	Category   *string   `json:"category"`
	Priority   *string   `json:"priority"`
	Sentiment  *string   `json:"sentiment"`
	Labels     *[]string `json:"labels"`
	NextAction *string   `json:"next_action"`
}

// ParseTriage checks a model's answer against the schema and returns
// it, with every label it names that the tenant defines (labels, the
// definitions' names) in the tenant's own spelling, and those it does
// not in DroppedLabels. The answer must be one JSON object, bare or in
// a single code fence; anything else, a missing field or a value out of
// range is ErrInvalid, with what is wrong. Nothing in it is acted on.
func ParseTriage(answer string, labels []string) (Triage, error) {
	obj, err := jsonObject(answer)
	if err != nil {
		return Triage{}, err
	}
	var raw rawTriage
	if err := json.Unmarshal(obj, &raw); err != nil {
		return Triage{}, fmt.Errorf("%w: %s", ErrInvalid, jsonProblem(err))
	}
	var t Triage
	switch {
	case raw.Category == nil:
		return t, invalid("category is missing")
	case raw.Priority == nil:
		return t, invalid("priority is missing")
	case raw.Sentiment == nil:
		return t, invalid("sentiment is missing")
	case raw.Labels == nil:
		return t, invalid("labels is missing (use [] for none)")
	case raw.NextAction == nil:
		return t, invalid("next_action is missing")
	}
	t.Category = strings.TrimSpace(*raw.Category)
	t.Priority = strings.ToLower(strings.TrimSpace(*raw.Priority))
	t.Sentiment = strings.ToLower(strings.TrimSpace(*raw.Sentiment))
	t.NextAction = strings.TrimSpace(*raw.NextAction)
	switch {
	case t.Category == "" || utf8.RuneCountInString(t.Category) > maxCategory || hasControl(t.Category):
		return t, invalid(fmt.Sprintf("category must be a short topic of at most %d characters", maxCategory))
	case !slices.Contains(Priorities, t.Priority):
		return t, invalid(fmt.Sprintf("priority %q: want one of %s", t.Priority, strings.Join(Priorities, ", ")))
	case !slices.Contains(Sentiments, t.Sentiment):
		return t, invalid(fmt.Sprintf("sentiment %q: want one of %s", t.Sentiment, strings.Join(Sentiments, ", ")))
	case t.NextAction == "" || utf8.RuneCountInString(t.NextAction) > maxNextAction:
		return t, invalid(fmt.Sprintf("next_action must be one sentence of at most %d characters", maxNextAction))
	case len(*raw.Labels) > maxSuggested:
		return t, invalid(fmt.Sprintf("labels holds %d names: want at most %d", len(*raw.Labels), maxSuggested))
	}
	t.Labels, t.DroppedLabels = matchLabels(*raw.Labels, labels)
	return t, nil
}

func invalid(what string) error { return fmt.Errorf("%w: %s", ErrInvalid, what) }

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// jsonProblem says what is wrong with a JSON document without quoting
// it back.
func jsonProblem(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("%s must be a %s", ute.Field, ute.Type)
	}
	return "it is not valid JSON"
}

// jsonObject extracts the one JSON object an answer must be: bare, or
// inside a single code fence.
func jsonObject(answer string) ([]byte, error) {
	s := strings.TrimSpace(answer)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimPrefix(s, "json")
		s = strings.TrimSpace(s)
		s = strings.TrimSpace(strings.TrimSuffix(s, "```"))
	}
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, invalid("the answer must be one JSON object and nothing else")
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	var probe map[string]json.RawMessage
	if err := dec.Decode(&probe); err != nil {
		return nil, invalid("it is not valid JSON")
	}
	if dec.More() {
		return nil, invalid("the answer must be one JSON object and nothing else")
	}
	return []byte(s), nil
}

// matchLabels splits suggested names into those the tenant defines, in
// the tenant's spelling, and those it does not, each once. Names match
// under simple case folding, as uma matches label names.
func matchLabels(suggested, defined []string) (kept, dropped []string) {
	byFold := make(map[string]string, len(defined))
	for _, d := range defined {
		byFold[strings.ToLower(strings.TrimSpace(d))] = d
	}
	kept, dropped = []string{}, []string{}
	for _, s := range suggested {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if name, ok := byFold[strings.ToLower(s)]; ok {
			if !slices.Contains(kept, name) {
				kept = append(kept, name)
			}
			continue
		}
		if !slices.Contains(dropped, s) {
			dropped = append(dropped, s)
		}
	}
	return kept, dropped
}

// repair asks the model to answer again, once, naming what was wrong.
const repair = `Your answer was not accepted: %s. Answer again with one JSON object and nothing else, with the fields category, priority, sentiment, labels and next_action.`

// RunTriage asks the model for a triage and checks the answer. An answer
// that is not valid is sent back once with what is wrong; a second
// failure is an error, never a guess.
func RunTriage(ctx context.Context, c Caller, p Prompt, labels []string) (Triage, error) {
	answer, err := c.Complete(ctx, p.Messages)
	if err != nil {
		return Triage{}, err
	}
	t, perr := ParseTriage(answer, labels)
	if perr == nil {
		return t, nil
	}
	retry := append(slices.Clone(p.Messages),
		llm.Message{Role: "assistant", Content: answer},
		llm.Message{Role: "user", Content: fmt.Sprintf(repair, strings.TrimPrefix(perr.Error(), ErrInvalid.Error()+": "))})
	answer, err = c.Complete(ctx, retry)
	if err != nil {
		return Triage{}, err
	}
	if t, perr = ParseTriage(answer, labels); perr != nil {
		e := output.TransientError(fmt.Sprintf("%s at %s did not answer with a valid triage, even when asked again: %s", c.Plan.Model, c.Plan.Host, strings.TrimPrefix(perr.Error(), ErrInvalid.Error()+": ")))
		e.Code = "LLM_OUTPUT_INVALID"
		e.SuggestedFix = "run the command again, or set " + KeyModel + " to a model that follows instructions better; nothing was changed"
		return Triage{}, e
	}
	return t, nil
}
