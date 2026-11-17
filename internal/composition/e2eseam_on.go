//go:build heoe2e && !heoscale

package composition

import (
	"os"

	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
)

// Environment the e2e build reads, so a test can stand a local Gmail or
// Graph behind a mail channel. Release builds carry none of it.
const (
	// E2EGmailEnv is the Gmail API root the google.mail adapter uses.
	E2EGmailEnv = "HEO_E2E_GMAIL_ENDPOINT"
	// E2EGraphEnv is the Graph root the microsoft.mail adapter uses.
	E2EGraphEnv = "HEO_E2E_GRAPH_ENDPOINT"
)

func googleOptions() google.Options { return google.Options{Endpoint: os.Getenv(E2EGmailEnv)} }

func microsoftOptions() microsoft.Options {
	return microsoft.Options{Endpoint: os.Getenv(E2EGraphEnv)}
}
