package assist

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"hop.top/kit/go/ai/llm"
	llmerrors "hop.top/kit/go/ai/llm/errors"
	"hop.top/kit/go/console/output"
)

// temperature keeps answers as repeatable as a model allows.
var temperature = 0.2

// Result is what a model said.
type Result struct {
	Text string
}

// Caller makes the model calls of one command: the plan's provider,
// one call at a time, each bounded by the plan's timeout. It has no
// second provider to turn to when the first fails.
type Caller struct {
	Plan Plan
	LLM  llm.Completer
}

// Complete sends msgs to the provider and returns its answer. Every
// failure is a corrective error (see mapError).
func (c Caller) Complete(ctx context.Context, msgs []llm.Message) (string, error) {
	ctx, cancel := context.WithTimeout(WithCall(ctx), c.Plan.Timeout)
	defer cancel()
	resp, err := c.LLM.Complete(ctx, llm.Request{Messages: msgs, Model: c.Plan.Model, Temperature: &temperature})
	if err != nil {
		return "", mapError(c.Plan, ctx, err)
	}
	return resp.Content, nil
}

// mapError renders a provider failure as the corrective error a caller
// can act on, naming the key to change. A missing server, a missing
// model and a refused credential are prerequisites (exit 70, 3 and 5);
// a timeout, a rate limit and a server fault may pass on a retry (exit
// 6). Nothing here retries or tries another provider.
func mapError(p Plan, ctx context.Context, err error) error {
	var (
		model   *llmerrors.ErrModel
		auth    *llmerrors.ErrAuth
		rate    *llmerrors.ErrRateLimit
		status  *llmerrors.HTTPStatusError
		urlFail *url.Error
	)
	where := fmt.Sprintf("%s at %s", p.Provider, p.Host)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		e := output.TransientError(fmt.Sprintf("%s did not answer within %s", where, p.Timeout.Round(time.Second)))
		e.SuggestedFix = "raise " + KeyTimeout + " (a cold model takes a while to load), use a smaller model, or run the command again"
		return e
	case errors.Is(err, context.Canceled):
		return err
	case errors.As(err, &model):
		e := output.NotFoundError(fmt.Sprintf("model %q is not available on %s", p.Model, where))
		e.SuggestedFix = "pull it on that server (`ollama pull " + p.Model + "`), or set " + KeyModel + " to a model it serves"
		return e
	case errors.As(err, &auth):
		e := output.UnauthorizedError(where + " refused the credential")
		_, cred := CloudKeys(p.Name)
		e.SuggestedFix = "store a valid key and name it in " + cred
		return e
	case errors.As(err, &rate):
		e := output.TransientError(where + " is rate limiting this client")
		e.SuggestedFix = "run the command again later"
		withDelay(e, max(retryAfterOf(ctx), min(rate.RetryAfter, maxRetryAfter)))
		return e
	case errors.As(err, &status):
		e := output.TransientError(fmt.Sprintf("%s answered with a server error (%d)", where, status.StatusCode))
		e.SuggestedFix = "run the command again; if it persists, check the server's own log"
		withDelay(e, retryAfterOf(ctx))
		return e
	case errors.As(err, &urlFail) || strings.Contains(err.Error(), "connection refused"):
		return unreachable(p)
	}
	e := output.GenericError("provider " + where + ": " + clip(llm.RedactURI(err.Error()), 200))
	e.SuggestedFix = "check " + KeyProvider + ", " + KeyModel + " and " + KeyEndpoint + " with `heo config get llm`"
	return e
}

// withDelay carries the wait a provider asked for: the envelope's cause
// (a key=value pair, as the other mapped errors carry theirs) for a
// machine, the fix for a person. Nothing waits or retries on it. No
// delay leaves e as it is.
func withDelay(e *output.Error, d time.Duration) {
	if d <= 0 {
		return
	}
	w := ((d + time.Second - 1) / time.Second * time.Second).String()
	e.Cause = "retry_after=" + w
	e.SuggestedFix = "retry after " + w + " (the provider asked for it); heo does not retry on its own"
}

// unreachable is the provider not answering at all. heo does not try
// another one: the fix is to start this one or point the configuration
// at one that runs.
func unreachable(p Plan) *output.Error {
	e := output.PrerequisiteError(fmt.Sprintf("%s at %s is not reachable, and heo does not fall back to another provider", p.Provider, p.Host))
	e.SuggestedFix = "start the " + p.Provider + " server, or set " + KeyEndpoint + " to a server that runs"
	if p.Class == Cloud {
		e.SuggestedFix = "check the network and " + KeyEndpoint
	}
	return e
}

// clip shortens s to n runes: a provider's error may quote the request.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
