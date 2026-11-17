//go:build !heoe2e

package cmd

import "github.com/spf13/cobra"

// extraCommands is empty in a release build.
func (a *app) extraCommands() []*cobra.Command { return nil }

// observeBus records nothing in a release build.
func (a *app) observeBus() {}
