//go:build !heoe2e

package cmd

import "hop.top/heo/internal/adapters"

// extraAdapters is empty in a release build.
func extraAdapters() []adapters.Adapter { return nil }
