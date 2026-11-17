package composition

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
)

// The issue adapters beside github.issues: gitlab.issues, jira.issues
// and linear.issues. Each names the provider account a channel connects
// to, which heo's one-receiver-per-account check keys on, and reports
// send, edit and delete unavailable until the channel's replies key is
// on (reason disabled), as github.issues does. They ship no extension
// effect, so no heo verb beyond the capability-gated ones (message edit
// and delete, `issue transition` where the adapter declares the acts,
// history fetch) is enabled on them.

// hostOf is the lower-cased host of a configured API or site URL, or
// errConfiguration when it names none.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	return strings.ToLower(u.Host), nil
}

// gitlabAccount is a gitlab.issues channel's account: the GitLab user its
// credential acts as (account_id: the token owner, or a project or group
// access token's bot user), on its API host. A channel naming no account
// ID (webhooks only) names it only through its credential, as
// githubAccount.
func gitlabAccount(ch model.Channel) (string, error) {
	var cfg gitlab.Config
	if err := decode(ch, gitlab.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	api := cfg.APIURL
	if api == "" {
		api = gitlab.DefaultAPIURL
	}
	host, err := hostOf(api)
	if err != nil {
		return "", err
	}
	if cfg.AccountID > 0 {
		return host + "/account:" + strconv.FormatInt(cfg.AccountID, 10), nil
	}
	acct, err := credentialAccount(ch)
	if err != nil {
		return "", err
	}
	return host + "/" + acct, nil
}

// jiraAccount is a jira.issues channel's account: the Atlassian account
// the credential acts as (account_id) on its Jira Cloud site; without
// one, its credential. The site is required by the adapter's own
// configuration, so a channel without one does not name an account.
func jiraAccount(ch model.Channel) (string, error) {
	var cfg jira.Config
	if err := decode(ch, jira.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.SiteURL == "" {
		return "", errors.New("configuration names no site_url")
	}
	host, err := hostOf(cfg.SiteURL)
	if err != nil {
		return "", err
	}
	if cfg.AccountID != "" {
		// Atlassian account IDs are opaque: kept as given.
		return host + "/account:" + cfg.AccountID, nil
	}
	acct, err := credentialAccount(ch)
	if err != nil {
		return "", err
	}
	return host + "/" + acct, nil
}

// linearAccount is a linear.issues channel's account: the Linear user the
// credential acts as (account_id, a UUID), on its API host; without one,
// its credential.
func linearAccount(ch model.Channel) (string, error) {
	var cfg linear.Config
	if err := decode(ch, linear.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	api := cfg.APIURL
	if api == "" {
		api = linear.DefaultAPIURL
	}
	host, err := hostOf(api)
	if err != nil {
		return "", err
	}
	if cfg.AccountID != "" {
		return host + "/account:" + strings.ToLower(cfg.AccountID), nil
	}
	acct, err := credentialAccount(ch)
	if err != nil {
		return "", err
	}
	return host + "/" + acct, nil
}

// repliesOff names the capabilities an issue channel declares and reports
// unavailable until its replies are on: send, edit and delete, and for
// jira.issues reconcile too. A configuration that does not parse is left
// to the channel check.
func repliesOff(replies bool, extra ...model.Capability) map[model.Capability]string {
	if replies {
		return nil
	}
	const why = "replies is off in the channel configuration"
	off := map[model.Capability]string{model.CapabilitySend: why, model.CapabilityEdit: why, model.CapabilityDelete: why}
	for _, c := range extra {
		off[c] = why
	}
	return off
}

func gitlabCapabilityOff(ch model.Channel) map[model.Capability]string {
	var cfg gitlab.Config
	if err := decode(ch, gitlab.ConfigSchema, &cfg); err != nil {
		return nil
	}
	return repliesOff(cfg.Replies)
}

func jiraCapabilityOff(ch model.Channel) map[model.Capability]string {
	var cfg jira.Config
	if err := decode(ch, jira.ConfigSchema, &cfg); err != nil {
		return nil
	}
	return repliesOff(cfg.Replies, model.CapabilityReconcile)
}

func linearCapabilityOff(ch model.Channel) map[model.Capability]string {
	var cfg linear.Config
	if err := decode(ch, linear.ConfigSchema, &cfg); err != nil {
		return nil
	}
	return repliesOff(cfg.Replies)
}
