//go:build !heoe2e

package cmd

import (
	"net/http"

	"hop.top/heo/internal/storage/postgres"
)

// httpBase is the transport provider HTTP is sent over: nil, heo's own.
func httpBase() http.RoundTripper { return nil }

// storageRunner runs pg_dump and pg_restore: nil, as local processes.
func storageRunner() postgres.Runner { return nil }

// llmSeam routes the model calls' HTTP: nothing in a release build. Kit's
// LLM adapters use http.DefaultClient, which kit's root already guards
// for --offline.
func llmSeam() {}
