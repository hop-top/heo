package assist

import (
	"context"
	"strings"

	"hop.top/kit/go/console/output"
)

// emptyAnswer is a model that answered with nothing.
func emptyAnswer(c Caller) error {
	e := output.TransientError(c.Plan.Model + " at " + c.Plan.Host + " answered with no text")
	e.Code = "LLM_OUTPUT_INVALID"
	e.SuggestedFix = "run the command again, or set " + KeyModel + " to another model; nothing was changed"
	return e
}

// Text asks the model for prose and returns it trimmed: a summary or a
// draft. A single code fence round the whole answer is removed. The text
// is only ever shown; nothing reads it as an instruction.
func Text(ctx context.Context, c Caller, p Prompt) (string, error) {
	answer, err := c.Complete(ctx, p.Messages)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(answer)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") && len(s) > 6 {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(s, "```"), "```"))
		if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.ContainsAny(s[:i], " \t") {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	if s == "" {
		return "", emptyAnswer(c)
	}
	return s, nil
}
