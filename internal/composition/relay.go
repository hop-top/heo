package composition

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// Host settings of relay.messages. A relay channel sends each reply to a
// gateway the operator runs, with a credential heo resolves for it: where
// that credential may be sent is the host's decision, so it is a setting
// of heo and never a field of a channel.
const (
	// KeyRelayCallbackHosts lists the hosts (host, or host:port, as they
	// appear in the URL) a relay.messages channel's callback and lookup
	// URLs may name. Empty: no channel can send, and a channel naming a
	// callback is refused.
	KeyRelayCallbackHosts = "adapters.relay.callback_hosts"
	// KeyRelayAllowHTTP permits http callbacks on the listed hosts, for a
	// gateway on this machine or a private network. The callback
	// credential then travels in clear text.
	KeyRelayAllowHTTP = "adapters.relay.allow_http"
	// KeyRelayTimeout bounds one callback or lookup request. A gateway
	// that answers only once its backend has sent the message (an
	// AppleScript send takes seconds) needs more than the default.
	KeyRelayTimeout = "adapters.relay.timeout"
)

// maxRelayTimeout is the longest request timeout accepted: the adapter
// waits twice that before a missing answer is looked up again.
const maxRelayTimeout = 10 * time.Minute

// RelayConfig is the relay.messages host settings.
type RelayConfig struct {
	// CallbackHosts are the allowed gateway hosts, lower-cased.
	CallbackHosts []string
	// AllowHTTP permits http callbacks.
	AllowHTTP bool
	// Timeout bounds one request to the gateway.
	Timeout time.Duration
}

func relayDefaults() map[string]any {
	return map[string]any{
		KeyRelayCallbackHosts: []string{},
		KeyRelayAllowHTTP:     false,
		KeyRelayTimeout:       relay.DefaultTimeout.String(),
	}
}

// ReadRelay reads and checks the relay.messages settings offline.
func ReadRelay(v *viper.Viper) (RelayConfig, []Problem) {
	var probs []Problem
	c := RelayConfig{AllowHTTP: v.GetBool(KeyRelayAllowHTTP)}
	hosts, err := hostList(v.Get(KeyRelayCallbackHosts))
	if err != nil {
		probs = append(probs, Problem{Key: KeyRelayCallbackHosts, Message: err.Error()})
	}
	for _, h := range hosts {
		if err := checkCallbackHost(h); err != nil {
			probs = append(probs, Problem{Key: KeyRelayCallbackHosts, Message: fmt.Sprintf("%q: %s", h, err)})
			continue
		}
		c.CallbackHosts = append(c.CallbackHosts, strings.ToLower(h))
	}
	c.Timeout = relay.DefaultTimeout
	raw := strings.TrimSpace(v.GetString(KeyRelayTimeout))
	d, err := time.ParseDuration(raw)
	switch {
	case raw == "":
	case err != nil || d <= 0 || d > maxRelayTimeout:
		probs = append(probs, Problem{Key: KeyRelayTimeout, Message: fmt.Sprintf(
			"%q: want a duration such as 15s, above zero and at most %s", v.GetString(KeyRelayTimeout), maxRelayTimeout)})
	default:
		c.Timeout = d
	}
	if c.AllowHTTP {
		for _, h := range c.CallbackHosts {
			host, _, err := net.SplitHostPort(h)
			if err != nil {
				host = strings.Trim(h, "[]")
			}
			if !isLoopback(host) {
				probs = append(probs, Problem{Key: KeyRelayAllowHTTP, Warning: true, Message: fmt.Sprintf(
					"true with %q listed: the callback credential travels to it in clear text", h)})
				break
			}
		}
	}
	return c, probs
}

// hostList reads a list of hosts: a YAML list, or a comma list from -c or
// the environment. Blank entries are dropped.
func hostList(raw any) ([]string, error) {
	var items []string
	switch r := raw.(type) {
	case nil:
	case string:
		items = strings.Split(r, ",")
	case []string:
		items = r
	case []any:
		for _, e := range r {
			s, ok := e.(string)
			if !ok {
				return nil, errors.New("want a list of host or host:port names")
			}
			items = append(items, s)
		}
	default:
		return nil, errors.New("want a list of host or host:port names")
	}
	var out []string
	for _, s := range items {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// checkCallbackHost refuses an entry that is not a bare host or host:port
// as a URL carries it: no scheme, path, credentials, query or wildcard,
// and a port, when named, a number.
func checkCallbackHost(h string) error {
	if strings.ContainsAny(h, " \t/\\?#@*%") || strings.Contains(h, "://") {
		return errors.New("want a host or host:port only, without a scheme, path, credentials or wildcard")
	}
	u, err := url.Parse("https://" + h)
	if err != nil || u.Host != h || u.Hostname() == "" {
		return errors.New("want a host or host:port")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("the port must be a number from 1 to 65535")
		}
	}
	return nil
}

// relayFactory is uma's relay.messages factory under the host's settings.
// A setting that does not check out stops the adapters from composing, as
// the widget's does: an allowlist is never half read.
func (o Options) relayFactory() (*relay.Factory, error) {
	cfg, probs := ReadRelay(o.viper())
	if err := problemsError(probs); err != nil {
		return nil, fmt.Errorf("relay adapter: %w", err)
	}
	return relay.NewFactory(relay.Options{CallbackHosts: cfg.CallbackHosts, AllowHTTP: cfg.AllowHTTP, Timeout: cfg.Timeout}), nil
}

// relayAccount is a relay.messages channel's account: its gateway, on its
// medium. The gateway is the host and path of the callback it replies to,
// the one thing a channel names that stands for it; a channel that only
// receives names none, and is keyed on its credential reference, whose
// webhook secret the gateway signs with. Two channels of one medium on one
// gateway would be handed one stream, so the claim is per gateway and
// medium, not per channel.
func relayAccount(ch model.Channel) (string, error) {
	var cfg relay.Config
	if err := decode(ch, relay.ConfigSchema, &cfg); err != nil {
		return "", err
	}
	if cfg.Medium == "" {
		return "", errors.New("configuration names no medium")
	}
	if cfg.Callback == nil || cfg.Callback.URL == "" {
		acct, err := credentialAccount(ch)
		if err != nil {
			return "", errors.New("configuration names no callback and the channel has no credential reference")
		}
		return cfg.Medium + "/" + acct, nil
	}
	u, err := url.Parse(cfg.Callback.URL)
	if err != nil || u.Host == "" {
		return "", errConfiguration
	}
	return cfg.Medium + "/" + strings.ToLower(u.Host) + strings.TrimSuffix(u.EscapedPath(), "/"), nil
}
