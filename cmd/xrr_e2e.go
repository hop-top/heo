//go:build heoe2e

package cmd

import (
	"net/http"
	"sync"

	"hop.top/heo/internal/assist"
	"hop.top/heo/internal/storage/postgres"
	"hop.top/heo/internal/xrrx"
)

// httpBase is the transport provider HTTP is sent over. With XRR_MODE
// set, in an e2e build only, it records or replays through xrr; a
// release build never links xrr (see internal/xrrx).
func httpBase() http.RoundTripper {
	e := xrrx.MustFromEnv()
	if e == nil {
		return nil
	}
	return e.Transport(nil)
}

// storageRunner runs pg_dump and pg_restore: with XRR_MODE set, in an
// e2e build only, through xrr's exec adapter.
func storageRunner() postgres.Runner {
	e := xrrx.MustFromEnv()
	if e == nil {
		return nil
	}
	return e.Runner(postgres.ExecRunner{})
}

var llmSeamOnce sync.Once

// llmSeam routes the HTTP of model calls through the cassette. Kit's
// LLM adapters take no HTTP client and use http.DefaultClient, so with
// XRR_MODE set, in an e2e build only, that client's transport sends a
// call marked by assist.WithCall to xrr and everything else the way it
// went before. A release build never links xrr.
func llmSeam() {
	llmSeamOnce.Do(func() {
		e := xrrx.MustFromEnv()
		if e == nil {
			return
		}
		http.DefaultClient.Transport = &callRouter{recorded: e.Transport(nil)}
	})
}

// callRouter sends model calls to the cassette and any other request to
// http.DefaultTransport, looked up per request so the guard kit installs
// on it applies.
type callRouter struct{ recorded http.RoundTripper }

func (c *callRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	if assist.IsCall(r.Context()) {
		return c.recorded.RoundTrip(r)
	}
	return http.DefaultTransport.RoundTrip(r)
}
