package xrrx

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/testing/xrrtest"
	xrr "hop.top/xrr"
)

// Transport returns the http.RoundTripper provider HTTP goes through in
// this process: base (http.DefaultTransport when nil) behind uma's
// xrrtest transport, on the process's cassette directory.
//
// xrrtest keeps the headers allow-list, drops credential headers and
// parameters, scrubs credential fields and numbers repeated requests;
// this adds the scrub rules of ScrubText and ScrubRequestText and
// reports a replay miss to the miss log.
//
// The transport is built on the first request, so a process that never
// reaches a provider never touches its cassette directory.
func (e *Env) Transport(base http.RoundTripper) http.RoundTripper {
	return &lazyTransport{env: e, base: base}
}

type lazyTransport struct {
	env  *Env
	base http.RoundTripper

	once sync.Once
	rt   http.RoundTripper
}

func (l *lazyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	l.once.Do(func() {
		opts := []xrrtest.Option{
			xrrtest.WithMode(l.env.Mode),
			xrrtest.WithNormalize(normalizeRequest),
			xrrtest.WithScrub(scrubBody),
		}
		opts = append(opts, xrrtest.WithTransport(&headerScrub{base: l.env.forwarder(l.base)}))
		l.rt = xrrtest.NewTransport(&reporter{missLog: l.env.MissLog}, l.env.Dir, opts...)
	})
	return l.rt.RoundTrip(req)
}

// forwarder is the transport a request that must reach its provider goes
// over (record and passthrough only): base, except for api.github.com,
// api.twilio.com, slack.com, api.telegram.org and discord.com, which only
// a live recording reaches, each through its sandbox, hooks.slack.com
// and Discord's gateway, which no recording reaches (a response URL
// answers once, to the user who pressed the control; a WebSocket session
// is no HTTP exchange), and the placeholder Bot Connector service of a
// Teams recording, which only a live recording reaches, through its
// sandbox, and which no other host stands in for. login.microsoftonline.com,
// where the Teams daemon mints its Bot Connector token, is never reached:
// a live recording's sandbox answers the placeholder bot's token request
// itself and refuses every other.
func (e *Env) forwarder(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == slackHost || r.URL.Hostname() == slackHooksHost {
			if r.URL.Hostname() == slackHooksHost || e.Slack == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached only by a live recording of slack.com's Web API (%s=record and %s=1 with the sandbox named); not sent", r.URL.Hostname(), xrr.EnvMode, EnvLive)
			}
			return e.Slack.Transport(base).RoundTrip(r)
		}
		if h := r.URL.Hostname(); h == discordHost || strings.HasSuffix(h, discordGatewaySuffix) {
			if h != discordHost {
				return nil, fmt.Errorf("xrrx: %s is the Discord gateway, which no recording reaches (a WebSocket session is no HTTP exchange); not sent", h)
			}
			if e.Discord == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached only by a live recording (%s=record and %s=1 with the sandbox named); not sent", discordHost, xrr.EnvMode, EnvLive)
			}
			return e.Discord.Transport(base).RoundTrip(r)
		}
		if r.URL.Hostname() == TeamsEntraHost {
			if e.Teams == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached by no recording: a live recording of Microsoft Teams answers the placeholder bot's token request itself (%s=record and %s=1 with the sandbox named); not sent", TeamsEntraHost, xrr.EnvMode, EnvLive)
			}
			return e.Teams.TokenTransport().RoundTrip(r)
		}
		if r.URL.Hostname() == TeamsServiceHost {
			if e.Teams == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached only by a live recording (%s=record and %s=1 with the sandbox named); not sent", TeamsServiceHost, xrr.EnvMode, EnvLive)
			}
			return e.Teams.Transport(base).RoundTrip(r)
		}
		if r.URL.Hostname() == telegramHost {
			if e.Telegram == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached only by a live recording (%s=record and %s=1 with the sandbox named); not sent", telegramHost, xrr.EnvMode, EnvLive)
			}
			return e.Telegram.Transport(base).RoundTrip(r)
		}
		if r.URL.Hostname() == twilioHost {
			if e.Twilio == nil || e.Mode != xrr.ModeRecord {
				return nil, fmt.Errorf("xrrx: %s is reached only by a live recording (%s=record and %s=1 with the sandbox named); not sent", twilioHost, xrr.EnvMode, EnvLive)
			}
			return e.Twilio.Transport(base).RoundTrip(r)
		}
		if r.URL.Hostname() != githubHost {
			return base.RoundTrip(r)
		}
		if e.GitHub == nil || e.Mode != xrr.ModeRecord {
			return nil, fmt.Errorf("xrrx: %s is reached only by a live recording (%s=record and %s=1 with the sandbox named); not sent", githubHost, xrr.EnvMode, EnvLive)
		}
		return e.GitHub.Transport(base).RoundTrip(r)
	})
}

// headerScrub scrubs the response headers xrrtest records. Its hooks see
// bodies only, and an allow-listed header such as Location names the
// provider's host and port.
type headerScrub struct{ base http.RoundTripper }

func (h *headerScrub) RoundTrip(req *http.Request) (*http.Response, error) {
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	for k, vs := range resp.Header {
		for i, v := range vs {
			vs[i] = ScrubText(v)
		}
		resp.Header[k] = vs
	}
	return resp, nil
}

// normalizeRequest is the recorded form of a request: no OS-chosen
// host:port, temp path, timestamp or process id, so the same request
// fingerprints the same on every run.
func normalizeRequest(r *xrrtest.Request) {
	scrubURL(r.URL)
	r.Body = []byte(ScrubRequestText(string(r.Body)))
	switch r.URL.Hostname() {
	case twilioHost:
		r.Body = normalizeTwilioForm(r.Body)
	case slackHost:
		r.Body = normalizeSlackBody(r.Body)
	case telegramHost:
		r.Body = normalizeTelegramBody(r.Body)
	case discordHost:
		r.Body = normalizeDiscordBody(r.Body)
	case TeamsServiceHost:
		r.Body = normalizeTeamsBody(r.Body)
	case TeamsEntraHost:
		r.Body = normalizeTeamsTokenBody(r.Body)
	}
}

// scrubBody is the response (and request) body hook: what the OS chose
// is replaced; timestamps stay, as the adapter parses them.
func scrubBody(_ string, body []byte) []byte {
	return []byte(ScrubText(string(body)))
}

// reporter is the testing.TB xrrtest wants, in a process that is not a
// test: it carries the two calls xrrtest makes on a failure.
// Everything else it embeds is nil and is never called.
type reporter struct {
	testing.TB
	missLog string
}

func (r *reporter) Helper() {}

// Errorf is how xrrtest says a replay found no cassette. It is printed
// and appended to the miss log; the request itself fails.
func (r *reporter) Errorf(format string, args ...any) {
	reportMiss(r.missLog, fmt.Sprintf(format, args...))
}

// Fatalf is how xrrtest says the cassette directory cannot be used.
func (r *reporter) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

// reportMiss appends msg to the miss log, when there is one.
func reportMiss(path, msg string) {
	fmt.Fprintln(os.Stderr, msg)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the parent test names its own miss log
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintln(f, msg)
}
