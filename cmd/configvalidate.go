package cmd

import (
	"errors"

	"github.com/spf13/cobra"

	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/control"
	"hop.top/heo/internal/settings"
	"hop.top/kit/go/console/output"
	"hop.top/kit/go/transport/socket"
	"hop.top/uma/model"
)

// channelsValidation is what `config validate` did about the channels
// and inboxes in the data store.
type channelsValidation struct {
	// Checked is true when the daemon checked them.
	Checked bool `json:"checked" yaml:"checked"`
	// Count is how many channels were checked.
	Count int `json:"count" yaml:"count"`
	// Skipped says why they were not checked.
	Skipped string `json:"skipped,omitempty" yaml:"skipped,omitempty"`
}

// detailStore is the detail of an issue found in the data store.
const detailStore = "data store, checked by the daemon"

// validateChannels checks every channel and inbox the caller may read,
// as `channel check` does locally: in the daemon when this process is
// it, through the control socket otherwise. The data store is the
// daemon's alone, so with no daemon to ask nothing is opened and the
// check is reported skipped, with why.
func (a *app) validateChannels(cmd *cobra.Command) ([]settings.Issue, channelsValidation) {
	ctx := cmd.Context()
	var checks []channels.ChannelCheck
	var err error
	route, meta := control.RouteOf(ctx, a.control.servesControl())
	switch route {
	case control.Local:
		var caller control.Caller
		caller, err = control.CallerOf(meta, model.TenantID(a.r.Viper.GetString(settings.KeyTenant)))
		if err == nil {
			checks, err = a.checkChannels(ctx, caller, "", false)
		}
	case control.Forward:
		var res control.Result
		res, err = a.controlClient().Invoke(ctx, socket.Request{
			Path: []string{"channel", "check"}, Args: []string{}, Flags: map[string]any{"format": output.JSON},
		})
		if err == nil {
			var cc channelChecks
			cc, err = control.DecodeReport[channelChecks](res, codeCheckFailed)
			checks = cc.Checks
		}
	default:
		err = errors.New("this process is not the profile's daemon and does not relay to it")
	}
	if err != nil {
		return nil, channelsValidation{Skipped: skipReason(err)}
	}
	var issues []settings.Issue
	for _, c := range checks {
		name := "channel:" + channels.Name(c.Key, c.ID)
		for _, f := range c.Findings {
			key := name
			if f.Scope != "channel" {
				key += " " + f.Scope
			}
			issues = append(issues, settings.Issue{
				Severity: settings.Severity(f.Severity), Key: key, Message: f.Message + " (" + f.Code + ")",
				Origin: settings.OriginStore, Detail: detailStore,
			})
		}
	}
	return issues, channelsValidation{Checked: true, Count: len(checks)}
}

// skipReason says why the daemon did not check the channels: its own
// refusal, with the way forward, prefixed by why only it can check them.
func skipReason(err error) string {
	var oe *output.Error
	if !errors.As(err, &oe) {
		return err.Error()
	}
	msg := oe.Message
	if oe.SuggestedFix != "" {
		msg += "; " + oe.SuggestedFix
	}
	switch oe.Code {
	case output.CodePrerequisite:
		return "channels and inboxes live in the data store the daemon owns: " + msg
	case output.CodeUnauthorized:
		return "the daemon refused to show you the channels: " + msg
	}
	return msg
}
