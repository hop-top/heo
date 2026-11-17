//go:build !heoscale && !heoe2e

package composition

import (
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/microsoft"
)

// googleOptions and microsoftOptions are the provider options of a
// release build: the providers' own endpoints.
func googleOptions() google.Options { return google.Options{} }

func microsoftOptions() microsoft.Options { return microsoft.Options{} }
