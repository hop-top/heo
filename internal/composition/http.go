package composition

import (
	"net"
	"net/http"
	"time"

	"hop.top/kit/go/core/netpolicy"
	"hop.top/uma/ports"
)

// NewHTTPClient returns the client adapters reach their providers
// through: base (a transport of heo's own when nil) under kit's network
// policy guard, so an offline run refuses provider traffic inside the
// transport. Adapters bound each call with their own timeouts.
//
// The default transport is built here rather than cloned from
// http.DefaultTransport, which kit's root replaces process wide with a
// guarded round tripper of its own.
func NewHTTPClient(base http.RoundTripper) ports.HTTPClient {
	if base == nil {
		base = newTransport()
	}
	return &http.Client{Transport: netpolicy.Guard(base)}
}

// newTransport has the standard library's default transport settings.
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
