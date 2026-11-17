package daemon

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/kit/go/console/serve"
)

// Service names, as `heo service start <service>` takes them.
const (
	NameWorkers   = "workers"
	NameReceivers = "receivers"
	NameWebhook   = "webhook"
	// NameBridge is the event bridge: uma's domain events republished
	// on the bus.
	NameBridge = "bridge"
	// NameApprovalChat announces held operations to the conversations
	// of held_operations.chat and takes decisions from them. It runs
	// nothing unless that is enabled.
	NameApprovalChat = "approval-chat"

	// NameHooks is the hook runner: one durable subscription per
	// configured plugin, each event handed to the plugin.
	NameHooks = "hooks"
)

// Configuration keys the runtime services read. Kit's own lifecycle
// keys (enabled, optional, ready_timeout, stop_timeout) sit beside them in the
// same services.<name> block.
const (
	// KeyWorkersConcurrency is how many workers claim and run queued
	// operations at once.
	KeyWorkersConcurrency = "services.workers.count"
	// KeyWorkersPoll is how long an idle worker waits before claiming
	// again.
	KeyWorkersPoll = "services.workers.poll"
	// KeyWorkersLease is how long a claim holds a work item; work a
	// crashed daemon held is claimable again once it expires.
	KeyWorkersLease = "services.workers.lease"
	// KeyWorkersDrainTimeout bounds the drain at shutdown: claimed work
	// still running then is abandoned to its lease and resumes on the
	// next start.
	KeyWorkersDrainTimeout = "services.workers.drain_timeout"
	// KeyWorkersHeldSweepEvery is how often a worker expires held
	// operations past their deadline: a duration, "0s" for uma's own
	// cadence, "off" for no sweep. It is also the window after which an
	// operation still held is marked carried over.
	KeyWorkersHeldSweepEvery = "services.workers.held_sweep_every"
	// KeyReceiversSettleTimeout bounds how long a channel retrying a
	// transient start failure holds the receivers service's readiness:
	// past it the channel settles waiting, the service reports ready and
	// degraded, and the channel keeps retrying.
	KeyReceiversSettleTimeout = "services.receivers.settle_timeout"
	// KeyReceiversStartSpread is the window over which the daemon's
	// start spreads the first connection of polling receivers (Gmail,
	// Graph), each at a stable per-channel point in it (uma's
	// ReceiveStartSpread), so a profile of many mailboxes does not hit
	// the provider in one second. "0s" starts them all at once. It never delays readiness.
	KeyReceiversStartSpread = "services.receivers.start_spread"
	// KeyWebhookAddr is the webhook listener's address.
	KeyWebhookAddr = "services.webhook.addr"
	// KeyWebhookAllowRemote permits a non-loopback webhook address.
	KeyWebhookAllowRemote = "services.webhook.allow_remote"
	// KeyWebhookPublicBaseURL is the URL providers reach the webhook
	// listener at, as published through a proxy or tunnel. It is the
	// provider-facing base only, never a UI URL.
	KeyWebhookPublicBaseURL = "services.webhook.public_base_url"
)

// Defaults for the runtime services' keys.
const (
	DefaultWorkersConcurrency  = 4
	DefaultWorkersPoll         = time.Second
	DefaultWorkersLease        = time.Minute
	DefaultWorkersDrainTimeout = 20 * time.Second
	// DefaultWorkersHeldSweepEvery is uma's own cadence (one minute).
	DefaultWorkersHeldSweepEvery = "0s"
	// HeldSweepOff turns the held-operation sweep off.
	HeldSweepOff                = "off"
	DefaultWebhookAddr          = "127.0.0.1:8083"
	maxWorkersConcurrency       = 256
	minWorkersLease             = 5 * time.Second
	webhookReadHeaderTimeout    = 5 * time.Second
	webhookReadTimeout          = 30 * time.Second
	webhookWriteTimeout         = 30 * time.Second
	webhookIdleTimeout          = 2 * time.Minute
	defaultOwnershipCheckPeriod = 10 * time.Second
)

// DefaultReceiversSettleTimeout leaves room under kit's default ready
// timeout for several transient retries.
const DefaultReceiversSettleTimeout = 10 * time.Second

// DefaultReceiversStartSpread is the mail adapters' default poll
// interval: a first poll lands within one interval of the start.
const DefaultReceiversStartSpread = 5 * time.Minute

// Defaults returns the runtime services' built-in values as flat keys.
// Workers, receivers and the event bridge are enabled: an empty install
// runs them with nothing to do. The bridge costs one durable
// subscription and nothing else until a bus subscriber exists. The
// webhook listener stays off until enabled: it opens a TCP port, and
// providers can only reach it once public_base_url is published. The
// hook runner stays off too: it starts the plugins the configuration
// names, which is for the operator to ask.
func Defaults() map[string]any {
	return map[string]any{
		"services." + NameWorkers + ".enabled":   true,
		"services." + NameReceivers + ".enabled": true,
		"services." + NameWebhook + ".enabled":   false,
		"services." + NameBridge + ".enabled":    true,
		// Enabled, and registered only when held_operations.chat.enabled is
		// set (cmd gates it).
		"services." + NameApprovalChat + ".enabled": true,
		"services." + NameHooks + ".enabled":        false,
		KeyWorkersConcurrency:                       DefaultWorkersConcurrency,
		KeyWorkersPoll:                              DefaultWorkersPoll.String(),
		KeyWorkersLease:                             DefaultWorkersLease.String(),
		KeyWorkersDrainTimeout:                      DefaultWorkersDrainTimeout.String(),
		KeyWorkersHeldSweepEvery:                    DefaultWorkersHeldSweepEvery,
		KeyReceiversSettleTimeout:                   DefaultReceiversSettleTimeout.String(),
		KeyReceiversStartSpread:                     DefaultReceiversStartSpread.String(),
		KeyWebhookAddr:                              DefaultWebhookAddr,
		KeyWebhookAllowRemote:                       false,
		KeyWebhookPublicBaseURL:                     "",
	}
}

// WorkersConfig configures the workers service.
type WorkersConfig struct {
	Concurrency  int
	Poll         time.Duration
	Lease        time.Duration
	DrainTimeout time.Duration
	// HeldSweepEvery is the cadence of the held-operation expiry sweep:
	// zero is uma's default, negative turns the sweep off.
	HeldSweepEvery time.Duration
}

// ReceiversConfig configures the receivers service.
type ReceiversConfig struct {
	SettleTimeout time.Duration
	// StartSpread is the window of the spread first start; zero
	// starts every receiver at once.
	StartSpread time.Duration
}

// WebhookConfig configures the webhook service.
type WebhookConfig struct {
	Addr          string
	AllowRemote   bool
	PublicBaseURL string
}

// Problem is one finding of a settings check. Warning marks a usable
// but weak setting.
type Problem struct {
	Key     string
	Message string
	Warning bool
}

// ReadWorkers reads and checks the workers settings.
func ReadWorkers(v *viper.Viper) (WorkersConfig, []Problem) {
	var probs []Problem
	c := WorkersConfig{Concurrency: v.GetInt(KeyWorkersConcurrency)}
	if c.Concurrency < 1 || c.Concurrency > maxWorkersConcurrency {
		probs = append(probs, Problem{Key: KeyWorkersConcurrency,
			Message: fmt.Sprintf("%d: must be between 1 and %d", c.Concurrency, maxWorkersConcurrency)})
	}
	c.Poll, probs = duration(v, KeyWorkersPoll, time.Millisecond, probs)
	c.Lease, probs = duration(v, KeyWorkersLease, minWorkersLease, probs)
	c.DrainTimeout, probs = duration(v, KeyWorkersDrainTimeout, 0, probs)
	c.HeldSweepEvery, probs = heldSweep(v, probs)
	return c, probs
}

// heldSweep reads KeyWorkersHeldSweepEvery: a duration, or "off".
func heldSweep(v *viper.Viper, probs []Problem) (time.Duration, []Problem) {
	raw := strings.TrimSpace(v.GetString(KeyWorkersHeldSweepEvery))
	if raw == "" {
		return 0, probs
	}
	if raw == HeldSweepOff {
		return -1, probs
	}
	return duration(v, KeyWorkersHeldSweepEvery, 0, probs)
}

// ReadReceivers reads and checks the receivers settings. The settle
// timeout must end before the service's ready timeout, so a channel
// retrying a transient start degrades the service rather than failing
// its start.
func ReadReceivers(v *viper.Viper) (ReceiversConfig, []Problem) {
	var c ReceiversConfig
	var probs []Problem
	c.SettleTimeout, probs = duration(v, KeyReceiversSettleTimeout, 0, probs)
	c.StartSpread, probs = duration(v, KeyReceiversStartSpread, 0, probs)
	if len(probs) > 0 {
		return c, probs
	}
	ready := serve.DefaultReadyTimeout
	if d := v.GetDuration("services." + NameReceivers + ".ready_timeout"); d > 0 {
		ready = d
	}
	if c.SettleTimeout >= ready {
		probs = append(probs, Problem{Key: KeyReceiversSettleTimeout, Message: fmt.Sprintf(
			"%s: must be below services.%s.ready_timeout (%s)", c.SettleTimeout, NameReceivers, ready)})
	}
	return c, probs
}

// ReadWebhook reads and checks the webhook settings.
func ReadWebhook(v *viper.Viper) (WebhookConfig, []Problem) {
	var probs []Problem
	c := WebhookConfig{
		Addr:          strings.TrimSpace(v.GetString(KeyWebhookAddr)),
		AllowRemote:   v.GetBool(KeyWebhookAllowRemote),
		PublicBaseURL: strings.TrimSpace(v.GetString(KeyWebhookPublicBaseURL)),
	}
	host, _, err := net.SplitHostPort(c.Addr)
	switch {
	case err != nil:
		probs = append(probs, Problem{Key: KeyWebhookAddr, Message: fmt.Sprintf("%q: want host:port", c.Addr)})
	case !isLoopback(host) && !c.AllowRemote:
		probs = append(probs, Problem{Key: KeyWebhookAddr, Message: fmt.Sprintf(
			"%q is not a loopback address; bind loopback behind a proxy, or set %s: true to accept provider traffic directly",
			c.Addr, KeyWebhookAllowRemote)})
	}
	if c.PublicBaseURL != "" {
		if msg := checkBaseURL(c.PublicBaseURL); msg != "" {
			probs = append(probs, Problem{Key: KeyWebhookPublicBaseURL, Message: msg})
		}
	}
	return c, probs
}

// Check validates every runtime service setting offline, for
// `config validate`. A webhook listener enabled with no public base
// URL is a warning: it serves, but no provider can be pointed at it.
func Check(v *viper.Viper) []Problem {
	_, probs := ReadWorkers(v)
	_, rp := ReadReceivers(v)
	probs = append(probs, rp...)
	_, wp := ReadWebhook(v)
	probs = append(probs, wp...)
	if v.GetBool("services."+NameWebhook+".enabled") && strings.TrimSpace(v.GetString(KeyWebhookPublicBaseURL)) == "" {
		probs = append(probs, Problem{Key: KeyWebhookPublicBaseURL, Warning: true,
			Message: "empty while the webhook listener is enabled: set the URL providers reach it at"})
	}
	return probs
}

// duration reads key as a Go duration of at least min.
func duration(v *viper.Viper, key string, min time.Duration, probs []Problem) (time.Duration, []Problem) {
	raw := strings.TrimSpace(v.GetString(key))
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		return 0, append(probs, Problem{Key: key, Message: fmt.Sprintf("%q: want a duration such as 30s", raw)})
	case d < min || d < 0:
		return 0, append(probs, Problem{Key: key, Message: fmt.Sprintf("%s: must be at least %s", d, min)})
	}
	return d, probs
}

// checkBaseURL reports what is wrong with a public base URL, or "".
func checkBaseURL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return fmt.Sprintf("%q: want an absolute URL such as https://hooks.example.com/heo", raw)
	case u.Scheme != "https" && u.Scheme != "http":
		return fmt.Sprintf("%q: want an http or https URL", raw)
	case u.User != nil:
		return "carries user information; providers authenticate by signature, never by URL"
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Sprintf("%q: must not carry a query or fragment", raw)
	}
	return ""
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// problemsError joins blocking problems into one validation error, nil
// when every problem is a warning.
func problemsError(probs []Problem) error {
	var msgs []string
	for _, p := range probs {
		if !p.Warning {
			msgs = append(msgs, p.Key+": "+p.Message)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
