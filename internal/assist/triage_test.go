package assist_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"hop.top/heo/internal/assist"
	"hop.top/kit/go/ai/llm"
	"hop.top/kit/go/console/output"
)

var tenantLabels = []string{"Billing", "refund", "VIP"}

const goodTriage = `{"category":"billing","priority":"high","sentiment":"negative","labels":["billing","Refund","made-up","vip","billing"],"next_action":"Check the invoice and reply with a corrected one."}`

func TestParseTriageKeepsTheTenantsLabelsAndDropsTheRest(t *testing.T) {
	got, err := assist.ParseTriage(goodTriage, tenantLabels)
	if err != nil {
		t.Fatal(err)
	}
	if got.Category != "billing" || got.Priority != "high" || got.Sentiment != "negative" || got.NextAction == "" {
		t.Fatalf("triage = %+v", got)
	}
	// Names come back in the tenant's spelling, once each; the invented
	// one is reported, not kept.
	if strings.Join(got.Labels, ",") != "Billing,refund,VIP" || strings.Join(got.DroppedLabels, ",") != "made-up" {
		t.Fatalf("labels %v dropped %v", got.Labels, got.DroppedLabels)
	}
}

func TestParseTriageAcceptsOneObjectBareOrFenced(t *testing.T) {
	for _, in := range []string{goodTriage, "  \n" + goodTriage + "\n", "```json\n" + goodTriage + "\n```", "```\n" + goodTriage + "\n```"} {
		if _, err := assist.ParseTriage(in, tenantLabels); err != nil {
			t.Errorf("%.30q: %v", in, err)
		}
	}
}

func TestParseTriageRefusesWhatIsNotTheSchema(t *testing.T) {
	for name, in := range map[string]string{
		"empty":            "",
		"prose":            "Sure! Here is the triage: " + goodTriage,
		"two objects":      goodTriage + goodTriage,
		"array":            "[" + goodTriage + "]",
		"not json":         `{"category": billing}`,
		"missing category": `{"priority":"low","sentiment":"neutral","labels":[],"next_action":"x"}`,
		"missing labels":   `{"category":"a","priority":"low","sentiment":"neutral","next_action":"x"}`,
		"bad priority":     `{"category":"a","priority":"critical","sentiment":"neutral","labels":[],"next_action":"x"}`,
		"bad sentiment":    `{"category":"a","priority":"low","sentiment":"angry","labels":[],"next_action":"x"}`,
		"labels not list":  `{"category":"a","priority":"low","sentiment":"neutral","labels":"billing","next_action":"x"}`,
		"empty action":     `{"category":"a","priority":"low","sentiment":"neutral","labels":[],"next_action":"  "}`,
		"long category":    `{"category":"` + strings.Repeat("a", 41) + `","priority":"low","sentiment":"neutral","labels":[],"next_action":"x"}`,
		"long action":      `{"category":"a","priority":"low","sentiment":"neutral","labels":[],"next_action":"` + strings.Repeat("a", 301) + `"}`,
	} {
		_, err := assist.ParseTriage(in, tenantLabels)
		if !errors.Is(err, assist.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// Extra fields are ignored, not obeyed.
	extra := `{"category":"a","priority":"LOW","sentiment":"Neutral","labels":[],"next_action":"x","run":"rm -rf /"}`
	if got, err := assist.ParseTriage(extra, tenantLabels); err != nil || got.Priority != "low" || got.Sentiment != "neutral" {
		t.Errorf("extra field: %+v, %v", got, err)
	}
}

// script is a model that answers from a list, one answer per call, and
// remembers what it was asked. It stands for the provider at kit's
// llm.Completer interface; what a real model answers is recorded
// separately, in the cassettes of tests/e2e.
type script struct {
	answers []string
	asked   [][]llm.Message
	err     error
}

func (s *script) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	s.asked = append(s.asked, req.Messages)
	if s.err != nil {
		return llm.Response{}, s.err
	}
	i := len(s.asked) - 1
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	return llm.Response{Content: s.answers[i]}, nil
}

func localCaller(m llm.Completer) assist.Caller {
	return assist.Caller{Plan: assist.Plan{Provider: "ollama", Model: "m", Host: "127.0.0.1:11434", Class: assist.Local, Timeout: 5e9}, LLM: m}
}

func TestRunTriageRepairsOnceThenSucceeds(t *testing.T) {
	m := &script{answers: []string{"I think this is about billing.", goodTriage}}
	p := assist.TriagePrompt(thread, tenantLabels, 12000)
	got, err := assist.RunTriage(t.Context(), localCaller(m), p, tenantLabels)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.asked) != 2 || got.Category != "billing" {
		t.Fatalf("asked %d times, got %+v", len(m.asked), got)
	}
	retry := m.asked[1]
	if len(retry) != 4 || retry[2].Role != "assistant" || retry[2].Content != "I think this is about billing." ||
		retry[3].Role != "user" || !strings.Contains(retry[3].Content, "was not accepted") {
		t.Fatalf("the repair prompt = %+v", retry)
	}
	if retry[0].Content != p.Messages[0].Content || retry[1].Content != p.Messages[1].Content {
		t.Error("the repair must keep the original prompt")
	}
}

func TestRunTriageFailsAfterTheSecondInvalidAnswer(t *testing.T) {
	m := &script{answers: []string{"nope", `{"category":"a"}`}}
	_, err := assist.RunTriage(t.Context(), localCaller(m), assist.TriagePrompt(thread, nil, 12000), nil)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "LLM_OUTPUT_INVALID" || oe.ExitCode != output.ExitTransient || oe.SuggestedFix == "" {
		t.Fatalf("err = %v", err)
	}
	if len(m.asked) != 2 {
		t.Fatalf("asked %d times, want exactly one repair", len(m.asked))
	}
}

func TestRunTriageAsksOnceWhenTheFirstAnswerIsValid(t *testing.T) {
	m := &script{answers: []string{goodTriage}}
	if _, err := assist.RunTriage(t.Context(), localCaller(m), assist.TriagePrompt(thread, tenantLabels, 12000), tenantLabels); err != nil || len(m.asked) != 1 {
		t.Fatalf("asked %d times: %v", len(m.asked), err)
	}
}

func TestTextTrimsAndRefusesNothing(t *testing.T) {
	p := assist.SummaryPrompt(thread, 12000)
	for in, want := range map[string]string{"  A summary.\n": "A summary.", "```\nfenced\n```": "fenced", "```text\nfenced\n```": "fenced"} {
		got, err := assist.Text(t.Context(), localCaller(&script{answers: []string{in}}), p)
		if err != nil || got != want {
			t.Errorf("Text(%q) = %q, %v, want %q", in, got, err, want)
		}
	}
	_, err := assist.Text(t.Context(), localCaller(&script{answers: []string{" \n"}}), p)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "LLM_OUTPUT_INVALID" {
		t.Errorf("an empty answer = %v", err)
	}
}
