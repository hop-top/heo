// Package assist is heo's LLM helpers: a summary of a conversation, a
// triage of it, and a draft reply, over kit's ai/llm package.
//
// Three rules shape it.
//
// Local first, swappable. Every model is a provider chosen by
// configuration (llm.provider), and the default is a model served on
// this machine. A provider whose endpoint is not on this machine, or
// any provider but the local default named by "auto", is a cloud
// provider: it runs only when the configuration opts in to it by name
// (llm.cloud.<name>.enabled) and llm.provider names it. "auto" never
// resolves to one, no credential is read for a local provider, and a
// provider that fails is an error, never a reason to try another.
//
// Messages are data. Conversation text is wrapped in delimiters the
// prompt tells the model to treat as untrusted, and what the model
// answers is only ever shown: it is never parsed into a command, a tool
// call or an argument that runs anything (triage's answer is checked
// against a schema and reported as suggestions).
//
// Nothing is written. These helpers read a conversation as the calling
// actor and print what the model said; a label, a status or a reply
// changes only when a person or agent runs the command that does it.
package assist

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Configuration keys.
const (
	// KeyProvider selects the provider: "ollama" (the default), "auto",
	// or the name of a cloud provider the configuration opts in to.
	KeyProvider = "llm.provider"
	// KeyModel is the model the provider runs.
	KeyModel = "llm.model"
	// KeyEndpoint is the provider's base URL. Empty is the provider's
	// local default.
	KeyEndpoint = "llm.endpoint"
	// KeyTimeout bounds one model call.
	KeyTimeout = "llm.timeout"
	// KeyMaxPromptChars bounds the prompt, system text included.
	KeyMaxPromptChars = "llm.max_prompt_chars"

	// cloudPrefix prefixes the per-provider section:
	// llm.cloud.<name>.enabled, .credential_ref, .type and .endpoint.
	cloudPrefix = "llm.cloud."
)

// Defaults of the configuration.
const (
	// ProviderOllama is the local default provider.
	ProviderOllama = "ollama"
	// ProviderAuto picks the local default and nothing else.
	ProviderAuto = "auto"
	// DefaultModel is a placeholder: set llm.model to a model your
	// server has pulled.
	DefaultModel = "llama3.2"
	// DefaultTimeout is generous because a cold local model takes a
	// while to load.
	DefaultTimeout = 120 * time.Second
	// DefaultMaxPromptChars fits a 4096-token context with room for the
	// answer; raise it for a model with a longer one.
	DefaultMaxPromptChars = 12000
	// MinPromptChars is the smallest prompt budget: below it the fixed
	// instructions leave no room for the conversation.
	MinPromptChars = 3000
)

// localEndpoints are the default base URLs of the providers that run on
// this machine.
var localEndpoints = map[string]string{
	ProviderOllama: "http://127.0.0.1:11434",
}

// Defaults returns the built-in settings as flat keys.
func Defaults() map[string]any {
	return map[string]any{
		KeyProvider:       ProviderOllama,
		KeyModel:          DefaultModel,
		KeyEndpoint:       "",
		KeyTimeout:        "120s",
		KeyMaxPromptChars: DefaultMaxPromptChars,
	}
}

// CloudTypeKeys returns the keys that declare the cloud provider name
// as a server of a kind: .type is the kit provider scheme it speaks
// (openai for a server with the OpenAI chat API) and .endpoint its base
// URL, which llm.endpoint stands in for when empty.
func CloudTypeKeys(name string) (typ, endpoint string) {
	return cloudPrefix + name + ".type", cloudPrefix + name + ".endpoint"
}

// CloudKeys returns the opt-in keys of the cloud provider name.
func CloudKeys(name string) (enabled, credentialRef string) {
	return cloudPrefix + name + ".enabled", cloudPrefix + name + ".credential_ref"
}

// Config is the LLM settings, read.
type Config struct {
	Provider       string
	Model          string
	Endpoint       string
	Timeout        time.Duration
	MaxPromptChars int

	// Enabled reports whether the configuration opts in to the cloud
	// provider name. Credential is that provider's credential reference.
	Enabled    func(name string) bool
	Credential func(name string) string
	// Type is the scheme the named cloud provider speaks, empty when
	// the configuration does not declare one; CloudEndpoint its base
	// URL, empty when it has none of its own.
	Type          func(name string) string
	CloudEndpoint func(name string) string
}

// ConfigFrom reads the settings from v. A duration or number that does
// not parse is kept as zero for Plan to refuse with the key's name.
func ConfigFrom(v *viper.Viper) Config {
	c := Config{
		Provider:       strings.ToLower(strings.TrimSpace(v.GetString(KeyProvider))),
		Model:          strings.TrimSpace(v.GetString(KeyModel)),
		Endpoint:       strings.TrimSpace(v.GetString(KeyEndpoint)),
		MaxPromptChars: v.GetInt(KeyMaxPromptChars),
		Enabled: func(name string) bool {
			k, _ := CloudKeys(name)
			return v.GetBool(k)
		},
		Credential: func(name string) string {
			_, k := CloudKeys(name)
			return strings.TrimSpace(v.GetString(k))
		},
		Type: func(name string) string {
			k, _ := CloudTypeKeys(name)
			return strings.ToLower(strings.TrimSpace(v.GetString(k)))
		},
		CloudEndpoint: func(name string) string {
			_, k := CloudTypeKeys(name)
			return strings.TrimSpace(v.GetString(k))
		},
	}
	c.Timeout, _ = time.ParseDuration(strings.TrimSpace(v.GetString(KeyTimeout)))
	return c
}

// Class says where a provider runs.
type Class string

// The classes.
const (
	// Local is a provider on this machine or its local network: a
	// loopback, private or link-local address.
	Local Class = "local"
	// Cloud is every other: conversation text leaves for a host the
	// configuration does not vouch for.
	Cloud Class = "cloud"
)

// ClassOfHost classifies a host or host:port. Only an IP address in a
// loopback, private or link-local range, or localhost, is Local; a
// name that is not localhost is Cloud, because a name cannot be known
// to stay on the local network. Use the address of a server on your
// network.
func ClassOfHost(hostport string) Class {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return Local
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return Local
	}
	return Cloud
}

// parseEndpoint checks a base URL and returns it without a trailing
// slash, with its host.
func parseEndpoint(raw string) (endpoint, host string, err error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return "", "", fmt.Errorf("%q is not a base URL such as http://127.0.0.1:11434", raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", "", fmt.Errorf("%q: the scheme must be http or https", raw)
	case u.User != nil:
		return "", "", fmt.Errorf("the URL must not carry credentials: a cloud provider's key is a credential reference (llm.cloud.<name>.credential_ref)")
	case u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "&? \t\r\n#"):
		return "", "", fmt.Errorf("%q: a base URL has no query, fragment or spaces", raw)
	}
	return strings.TrimRight(raw, "/"), u.Host, nil
}
