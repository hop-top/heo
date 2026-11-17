// Package xrrx is the seam between heo's built binary and hop.top/xrr
// cassettes, for process-level tests.
//
// A test starts the e2e build (tag heoe2e) with XRR_MODE and
// XRR_CASSETTE_DIR set; the build wraps the calls heo makes out of
// process in this package's recorders: provider HTTP, through uma's
// xrrtest transport, and the pg_dump/pg_restore runner, through xrr's
// exec adapter. Replay is the default and never reaches the network or
// starts the tool; record needs an explicit XRR_MODE=record and is
// refused when CI is set.
//
// The package is plain Go with no build tag so its own tests run under
// `go test ./...`, but only files tagged heoe2e import it. A release
// binary does not link it, and so does not link xrr: the graph test in
// this package fails the build when it does.
//
// What reaches a cassette is decided here, not at each call site:
// credentials, loopback host:port pairs, temp paths and, in requests,
// timestamps and process ids never do. See ScrubText and Audit.
package xrrx
