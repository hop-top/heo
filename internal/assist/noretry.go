package assist

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Some provider SDKs retry on their own: a connection error, a 408,
// 409, 429 or any 5xx is sent again, twice, after the delay a
// Retry-After header names. heo retries nothing (a failed call is an
// error for the person or agent to act on), so a model call's HTTP goes
// through noRetry, which lets exactly one request leave per call and
// hands every later attempt of that call the first one's outcome.

// callState is what one model call has done on the wire.
type callState struct {
	mu   sync.Mutex
	sent bool
	err  error
	// wait is the delay the provider's 429 or 503 asked for, zero when
	// it asked for none or the header did not parse.
	wait time.Duration
}

var errNotRetried = errors.New("request not retried: heo sends one request per model call")

// callOf is the state of the model call ctx carries, nil when it does
// not carry one.
func callOf(ctx context.Context) *callState {
	s, _ := ctx.Value(callKey{}).(*callState)
	return s
}

// noRetry is the transport of model calls: it sends the first request
// of a call and refuses a second, whatever the SDK asked for. Requests
// that are not model calls pass through untouched.
type noRetry struct{ next http.RoundTripper }

func (n noRetry) RoundTrip(r *http.Request) (*http.Response, error) {
	s := callOf(r.Context())
	if s == nil {
		return n.base().RoundTrip(r)
	}
	s.mu.Lock()
	if s.sent {
		err := s.err
		s.mu.Unlock()
		if err == nil {
			err = errNotRetried
		}
		return nil, err
	}
	s.sent = true
	s.mu.Unlock()
	res, err := n.base().RoundTrip(r)
	if err != nil {
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		return nil, err
	}
	// The SDKs read this over the status code: no retry.
	res.Header.Set("X-Should-Retry", "false")
	if res.StatusCode == http.StatusTooManyRequests || res.StatusCode == http.StatusServiceUnavailable {
		if d := parseRetryAfter(res.Header.Get("Retry-After"), time.Now()); d > 0 {
			s.mu.Lock()
			s.wait = d
			s.mu.Unlock()
		}
	}
	return res, nil
}

// maxRetryAfter caps a delay a provider states: past it the number says
// little about when a retry would work, and the caller is better off
// deciding for itself.
const maxRetryAfter = time.Hour

// parseRetryAfter reads a Retry-After value (RFC 9110 section 10.2.3):
// delta-seconds, or an HTTP-date taken relative to now. An absent,
// malformed, negative or already elapsed value is zero, and one beyond
// maxRetryAfter is maxRetryAfter.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if v[0] >= '0' && v[0] <= '9' {
		for _, r := range v {
			if r < '0' || r > '9' {
				return 0
			}
		}
		secs, err := strconv.ParseUint(v, 10, 63)
		if err != nil || secs > uint64(maxRetryAfter/time.Second) {
			return maxRetryAfter // all digits: too large to hold is still too large
		}
		return time.Duration(secs) * time.Second
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	return min(max(t.Sub(now), 0), maxRetryAfter)
}

// retryAfterOf is the delay the model call ctx carries asked for.
func retryAfterOf(ctx context.Context) time.Duration {
	s := callOf(ctx)
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wait
}

func (n noRetry) base() http.RoundTripper {
	if n.next != nil {
		return n.next
	}
	return http.DefaultTransport
}

var noRetryOnce sync.Once

// installNoRetry puts noRetry in front of the transport kit's provider
// adapters send through (http.DefaultClient's, read at request time),
// once. Only requests of marked calls are affected.
func installNoRetry() {
	noRetryOnce.Do(func() {
		http.DefaultClient.Transport = noRetry{next: http.DefaultClient.Transport}
	})
}
