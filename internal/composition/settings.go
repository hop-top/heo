package composition

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"time"

	"github.com/spf13/viper"

	"hop.top/uma/adapter/web/widget"
)

// Host settings the adapters read. They are heo runtime settings, apart
// from any channel's configuration, which lives in the data store.
const (
	// KeySMTPAddr is the address the smtp.mail MX receiver listens on,
	// such as 0.0.0.0:25. Empty, smtp.mail channels send only.
	KeySMTPAddr = "adapters.smtp.addr"
	// KeySMTPAllowRemote permits a non-loopback smtp.mail listener: an
	// MX that other hosts deliver to directly, not through a local
	// relay.
	KeySMTPAllowRemote = "adapters.smtp.allow_remote"
	// KeySMTPTLSCertFile is the PEM certificate chain (leaf first) the
	// MX listener presents on STARTTLS. Set with KeySMTPTLSKeyRef; both
	// empty, STARTTLS is not offered.
	KeySMTPTLSCertFile = "adapters.smtp.tls.cert_file"
	// KeySMTPTLSKeyRef is the credential reference of the listener's
	// private key in the profile's credential backend, resolved for
	// PurposeSMTPTLS: a secret envelope whose value is the PEM key.
	KeySMTPTLSKeyRef = "adapters.smtp.tls.key_ref"
	// KeySMTPAllowRemoteWithoutTLS accepts a non-loopback listener that
	// offers no STARTTLS, so other hosts deliver in plain text.
	KeySMTPAllowRemoteWithoutTLS = "adapters.smtp.allow_remote_without_tls"
	// KeyWidgetRetain is how many delivered events the widget keeps per
	// visitor in the data store. A stream resuming from a cursor behind
	// them gets a reset event and reloads its history.
	KeyWidgetRetain = "adapters.widget.retain"
)

// PurposeSMTPTLS is the purpose the listener key is resolved for.
const PurposeSMTPTLS = "smtp.tls"

// Defaults returns the adapters' built-in settings as flat keys.
func Defaults() map[string]any {
	d := map[string]any{
		KeySMTPAddr:                  "",
		KeySMTPAllowRemote:           false,
		KeySMTPTLSCertFile:           "",
		KeySMTPTLSKeyRef:             "",
		KeySMTPAllowRemoteWithoutTLS: false,
		KeyWidgetRetain:              widget.DefaultRetain,
	}
	maps.Copy(d, relayDefaults())
	maps.Copy(d, teamsDefaults())
	return d
}

// WidgetConfig is the widget's host settings.
type WidgetConfig struct {
	// Retain is the number of delivered events kept per visitor.
	Retain int
}

// ReadWidget reads and checks the widget settings offline.
func ReadWidget(v *viper.Viper) (WidgetConfig, []Problem) {
	c := WidgetConfig{Retain: v.GetInt(KeyWidgetRetain)}
	if c.Retain < 1 {
		return c, []Problem{{Key: KeyWidgetRetain, Message: fmt.Sprintf(
			"%v: want a whole number of events, at least 1", v.Get(KeyWidgetRetain))}}
	}
	return c, nil
}

// SMTPConfig is the smtp.mail receiver's host settings.
type SMTPConfig struct {
	Addr                  string
	AllowRemote           bool
	CertFile              string
	KeyRef                string
	AllowRemoteWithoutTLS bool
}

// TLS reports whether the listener has a certificate configured, and so
// offers STARTTLS.
func (c SMTPConfig) TLS() bool { return c.CertFile != "" || c.KeyRef != "" }

// Problem is one finding of a settings check. Warning marks a usable
// but weak setting.
type Problem struct {
	Key     string
	Message string
	Warning bool
}

// ReadSMTP reads and checks the smtp.mail receiver settings offline:
// nothing is read but the configuration.
func ReadSMTP(v *viper.Viper) (SMTPConfig, []Problem) {
	c := SMTPConfig{
		Addr:                  strings.TrimSpace(v.GetString(KeySMTPAddr)),
		AllowRemote:           v.GetBool(KeySMTPAllowRemote),
		CertFile:              strings.TrimSpace(v.GetString(KeySMTPTLSCertFile)),
		KeyRef:                strings.TrimSpace(v.GetString(KeySMTPTLSKeyRef)),
		AllowRemoteWithoutTLS: v.GetBool(KeySMTPAllowRemoteWithoutTLS),
	}
	var probs []Problem
	switch {
	case c.CertFile != "" && c.KeyRef == "":
		probs = append(probs, Problem{Key: KeySMTPTLSKeyRef, Message: fmt.Sprintf(
			"empty while %s is set: set both, or neither to offer no STARTTLS", KeySMTPTLSCertFile)})
	case c.CertFile == "" && c.KeyRef != "":
		probs = append(probs, Problem{Key: KeySMTPTLSCertFile, Message: fmt.Sprintf(
			"empty while %s is set: set both, or neither to offer no STARTTLS", KeySMTPTLSKeyRef)})
	}
	if c.Addr == "" {
		return c, probs
	}
	host, port, err := net.SplitHostPort(c.Addr)
	switch {
	case err != nil || port == "":
		probs = append(probs, Problem{Key: KeySMTPAddr, Message: fmt.Sprintf("%q: want host:port", c.Addr)})
	case isLoopback(host):
	case !c.AllowRemote:
		probs = append(probs, Problem{Key: KeySMTPAddr, Message: fmt.Sprintf(
			"%q is not a loopback address; bind loopback behind a relay, or set %s: true to accept mail from other hosts directly",
			c.Addr, KeySMTPAllowRemote)})
	case c.TLS():
	case !c.AllowRemoteWithoutTLS:
		probs = append(probs, Problem{Key: KeySMTPAllowRemote, Message: fmt.Sprintf(
			"true without STARTTLS: set %s and %s, or set %s: true to accept mail from other hosts in plain text",
			KeySMTPTLSCertFile, KeySMTPTLSKeyRef, KeySMTPAllowRemoteWithoutTLS)})
	default:
		probs = append(probs, Problem{Key: KeySMTPAllowRemoteWithoutTLS, Warning: true, Message: fmt.Sprintf(
			"true: %q offers no STARTTLS, so mail from other hosts arrives in plain text and senders that require TLS cannot deliver",
			c.Addr)})
	}
	return c, probs
}

// Check validates the adapters' host settings for `config validate`.
// With a listener certificate configured it also reads the certificate
// file and resolves the key through creds (see CheckSMTPTLS); nil creds
// checks the configuration alone.
func Check(ctx context.Context, v *viper.Viper, creds CredentialsFunc) []Problem {
	cfg, probs := ReadSMTP(v)
	_, wprobs := ReadWidget(v)
	probs = append(probs, wprobs...)
	_, rprobs := ReadRelay(v)
	probs = append(probs, rprobs...)
	_, tprobs := ReadTeams(v)
	probs = append(probs, tprobs...)
	if creds == nil || cfg.CertFile == "" || cfg.KeyRef == "" {
		return probs
	}
	return append(probs, CheckSMTPTLS(ctx, cfg, creds, time.Now())...)
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// problemsError joins blocking problems into one error, nil when every
// problem is a warning.
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
