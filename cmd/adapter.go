package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/composition"
	"hop.top/kit/go/console/cli"
	"hop.top/kit/go/console/output"
	"hop.top/uma/model"
)

// adapterView is one adapter this build carries: what it declares, as
// its descriptor states it. Maturity is the adapter author's word on
// how far it is proven, apart from what it can do.
type adapterView struct {
	Type          model.TypeKey      `json:"type"           yaml:"type"`
	Maturity      string             `json:"maturity"       yaml:"maturity"`
	ContractMajor uint32             `json:"contract_major" yaml:"contract_major"`
	Capabilities  []model.Capability `json:"capabilities"   yaml:"capabilities"`
	// Verbs are the heo verbs the declared capabilities enable, such as
	// message edit for edit: what a channel on this adapter can be asked.
	Verbs []string `json:"verbs" yaml:"verbs"`
	// ConfigurationSchema names the channel configuration the adapter
	// validates: its schema name and version.
	ConfigurationSchema schemaView `json:"configuration_schema" yaml:"configuration_schema"`
	// CredentialPurposes are the purposes a channel's credential
	// reference resolves for.
	CredentialPurposes  []string `json:"credential_purposes"  yaml:"credential_purposes"`
	RequiredPermissions []string `json:"required_permissions" yaml:"required_permissions"`
	IdentityNamespaces  []string `json:"identity_namespaces"  yaml:"identity_namespaces"`
	Events              []string `json:"events"               yaml:"events"`
}

type schemaView struct {
	Name    string `json:"name"    yaml:"name"`
	Version uint32 `json:"version" yaml:"version"`
}

// adapterRow is an adapter in a listing table.
type adapterRow struct {
	Type         string `json:"type"         yaml:"type"         table:"TYPE"`
	Maturity     string `json:"maturity"     yaml:"maturity"     table:"MATURITY,priority=3"`
	Capabilities string `json:"capabilities" yaml:"capabilities" table:"CAPABILITIES,priority=4"`
	Verbs        string `json:"verbs"        yaml:"verbs"        table:"VERBS,priority=6"`
	Schema       string `json:"schema"       yaml:"schema"       table:"SCHEMA,priority=2"`
}

func viewOf(a adapters.Adapter) adapterView {
	d := a.Factory.Describe()
	nonNil := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return append([]string(nil), s...)
	}
	caps := append([]model.Capability{}, d.Capabilities...)
	return adapterView{
		Type: d.Type, Maturity: d.Maturity, ContractMajor: d.ContractMajor, Capabilities: caps, Verbs: adapters.VerbsOf(a),
		ConfigurationSchema: schemaView{Name: d.ConfigurationSchema.Name, Version: d.ConfigurationSchema.Version},
		CredentialPurposes:  nonNil(a.CredentialPurposes),
		RequiredPermissions: nonNil(d.RequiredPermissions),
		IdentityNamespaces:  nonNil(d.IdentityNamespaces),
		Events:              nonNil(d.Events),
	}
}

// installedAdapters is every adapter this binary runs, composed as the
// daemon composes them but over no data store: describing needs none.
func (a *app) installedAdapters() ([]adapterView, error) {
	r := adapters.NewRegistry()
	if err := composition.Register(r, composition.Options{Viper: a.viper}); err != nil {
		return nil, err
	}
	for _, ad := range extraAdapters() {
		if err := r.Register(ad); err != nil {
			return nil, err
		}
	}
	out := make([]adapterView, 0, r.Len())
	for _, t := range r.Types() {
		ad, _ := r.Lookup(t)
		out = append(out, viewOf(ad))
	}
	return out, nil
}

func (a *app) adapterCmd() *cobra.Command {
	grp := &cobra.Command{
		Use:   "adapter",
		Short: "Inspect the adapters this heo carries",
		Long: `Inspect the uma adapters this heo binary carries: the implementations
channels run. A channel is one configured connection; its adapter is
the code behind it.

Offline: read from the binary itself, with no daemon, data store or
network.`,
		Args: cobra.NoArgs,
	}
	grp.AddCommand(a.adapterListCmd(), a.adapterGetCmd())
	return grp
}

func (a *app) adapterListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "List the installed adapters, their maturity and capabilities",
		Long: `List every adapter this binary carries: its type key (what
` + "`channel create --type`" + ` takes), its maturity, the capabilities it
declares and the configuration schema its channels use.

Verbs are the heo commands the declared capabilities enable (` + "`message edit`" + `
for edit, say); an adapter lacking one refuses that verb, exit 2.

Capabilities are declarations. What a configured channel can do right
now — with its credentials, against its provider — is what
` + "`channel check --remote`" + ` reports. Maturity is separate: how far the
adapter is proven, not what it can do.

Offline: no daemon, data store or network.`,
		Example: `  heo adapter list
  heo adapter list --format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := a.installedAdapters()
			if err != nil {
				return err
			}
			if a.structured() {
				return output.Dispatch(cmd, a.r.Viper, list)
			}
			rows := make([]adapterRow, 0, len(list))
			for _, v := range list {
				rows = append(rows, adapterRow{
					Type: string(v.Type), Maturity: orDash(v.Maturity),
					Capabilities: joinCaps(v.Capabilities), Verbs: orDash(strings.Join(v.Verbs, ", ")),
					Schema: fmt.Sprintf("%s/v%d", v.ConfigurationSchema.Name, v.ConfigurationSchema.Version),
				})
			}
			return output.Dispatch(cmd, a.r.Viper, rows)
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &[]adapterView{}, Version: "1.0"})
	return c
}

func (a *app) adapterGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <type>",
		Short: "Show one adapter's declarations",
		Long: `Show what the adapter <type> declares: maturity, contract version,
capabilities and the heo verbs they enable, its channels' configuration
schema, the credential purposes a channel's credential reference resolves for, the provider
permissions it needs, its identity namespaces and the events it emits.

An adapter this binary does not carry exits 3. Offline: no daemon,
data store or network.`,
		Example: `  heo adapter get smtp.mail
  heo adapter get native.webwidget --format json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := a.installedAdapters()
			if err != nil {
				return err
			}
			for _, v := range list {
				if string(v.Type) != args[0] {
					continue
				}
				if a.structured() {
					return output.Dispatch(cmd, a.r.Viper, v)
				}
				return writeRows(cmd.OutOrStdout(), [][2]string{
					{"type", string(v.Type)},
					{"maturity", orDash(v.Maturity)},
					{"contract", fmt.Sprintf("v%d", v.ContractMajor)},
					{"capabilities", joinCaps(v.Capabilities)},
					{"verbs", orDash(strings.Join(v.Verbs, ", "))},
					{"configuration", fmt.Sprintf("%s/v%d", v.ConfigurationSchema.Name, v.ConfigurationSchema.Version)},
					{"credential purposes", orDash(strings.Join(v.CredentialPurposes, ", "))},
					{"permissions", orDash(strings.Join(v.RequiredPermissions, ", "))},
					{"identities", orDash(strings.Join(v.IdentityNamespaces, ", "))},
				})
			}
			e := output.NotFoundError(fmt.Sprintf("no adapter %q in this build", args[0]))
			e.SuggestedFix = "list the installed adapters with `" + a.heoCommand("adapter list") + "`"
			return e
		},
	}
	cli.SetSideEffect(c, cli.SideEffectRead)
	cli.SetIdempotency(c, cli.IdempotencyYes)
	_ = cli.SetOutputSchema(c, cli.OutputSchema{Type: &adapterView{}, Version: "1.0"})
	return c
}

func joinCaps(caps []model.Capability) string {
	s := make([]string, len(caps))
	for i, c := range caps {
		s[i] = string(c)
	}
	return orDash(strings.Join(s, ", "))
}
