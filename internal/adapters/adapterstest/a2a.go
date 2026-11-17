package adapterstest

import (
	"net/http"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
)

// A2ATokenPrefix starts the bearer token the e2e build's A2A endpoint
// accepts: "Bearer e2e-<agent>" is the remote agent <agent>. It is a
// test stand-in for a host's authentication, never in a release binary.
const A2ATokenPrefix = "e2e-"

// A2A returns uma's own a2a.messages adapter for the e2e build, so the
// built daemon can be driven by a remote agent over JSON-RPC: it opens
// tasks, asks to cancel them and reads them back. Authentication is the
// bearer token A2ATokenPrefix + agent; a channel's provider account is
// the channel itself, since the endpoint is mounted per channel.
func A2A() adapters.Adapter {
	f, err := a2a.New(a2a.Options{Authenticate: func(r *http.Request, _ model.ChannelRef) (a2a.Principal, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "+A2ATokenPrefix)
		if !ok || tok == "" {
			return a2a.Principal{}, model.NewError(model.Unauthorized, "bearer token required")
		}
		return a2a.Principal{ID: tok}, nil
	}})
	if err != nil {
		panic(err)
	}
	return adapters.Adapter{
		Factory: f,
		Account: func(ch model.Channel) (string, error) { return string(ch.Ref.ID), nil },
	}
}
