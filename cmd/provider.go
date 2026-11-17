package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/desk"
	"hop.top/uma/model"
)

// annotationCapability is the heo annotation on a provider-dependent
// command: the capability its channel's adapter must declare. It is
// static, per command; a unit test reads it to assert that every such
// command says so, and discovery (spec, REST, MCP) carries it with the
// command.
const annotationCapability = "heo/capability"

// providerDependent marks c as a provider-dependent verb needing
// capability: its help opens with the line that says so, and the
// annotation names the capability. The verb is registered on every
// surface whatever the channels: a channel whose adapter does not
// declare the capability refuses it (exit 2), and `heo channel get`
// shows which do.
func providerDependent(c *cobra.Command, capability model.Capability) {
	c.Long = providerLine(capability) + "\n\n" + strings.TrimLeft(c.Long, "\n")
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[annotationCapability] = string(capability)
}

// providerLine is the first line of a provider-dependent command's help.
func providerLine(capability model.Capability) string {
	return fmt.Sprintf("Provider-dependent: needs %s on the conversation's channel; see heo channel get", capability)
}

// declarationArgs adds what a preview checked of the adapter to a
// plan's arguments: the adapter, and the `capability` line that names
// what was and was not checked. A submission has none.
func declarationArgs(args map[string]any, d *desk.Declaration) []string {
	checked := []string{"authorization"}
	if d == nil {
		return checked
	}
	args["adapter"] = d.Adapter
	args["capability"] = d.Line()
	return append(checked, "adapter capability (declared)")
}
