package assist

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"hop.top/kit/go/ai/llm"
	// The local default provider registers itself with kit's registry.
	// Adding a provider is one more blank import here: nothing else in
	// heo names a provider.
	_ "hop.top/kit/go/ai/llm/ollama"
	// A server speaking the OpenAI chat API, declared as a named cloud
	// provider with llm.cloud.<name>.type: openai.
	_ "hop.top/kit/go/ai/llm/openai"
	"hop.top/kit/go/console/output"
)

// Plan is where a call would go, decided from the configuration alone:
// no file is read, no credential resolved and nothing sent. It is what
// --dry-run reports and what a call is checked against.
type Plan struct {
	// Provider is the kit scheme that serves the call.
	Provider string
	// Name is what llm.provider names: the scheme itself, or a cloud
	// provider declared with llm.cloud.<name>.type. It keys the opt-in
	// and the credential.
	Name  string
	Model string
	// Endpoint is the provider's base URL and Host its host:port.
	Endpoint string
	Host     string
	// Class is Local unless the endpoint is not on this machine or its
	// local network.
	Class          Class
	Timeout        time.Duration
	MaxPromptChars int
	// CredentialRef is the credential reference of a cloud provider,
	// empty for a local one and for a cloud provider with none set.
	CredentialRef string
}

// Providers lists the providers this build carries, in kit's registry.
func Providers() []string { return llm.Schemes() }

// planError is a setting the call cannot run under: the corrective
// error, and the key to change.
type planError struct {
	Err *output.Error
	Key string
}

func (p *planError) Error() string { return p.Err.Error() }
func (p *planError) Unwrap() error { return p.Err }

// prerequisite is a configuration the call cannot run under, with the
// key to change.
func prerequisite(key, msg, fix string) *planError {
	e := output.PrerequisiteError(msg)
	e.SuggestedFix = fix
	return &planError{Err: e, Key: key}
}

// Plan resolves the configuration to the one provider a call uses, or
// says what to change. It never falls back: a setting that does not
// resolve is an error, and a local provider never turns into a cloud
// one.
func (c Config) Plan() (Plan, error) {
	p, err := c.plan()
	if err != nil {
		return Plan{}, err
	}
	return p, nil
}

func (c Config) plan() (Plan, *planError) {
	name := c.Provider
	if name == "" {
		name = ProviderOllama
	}
	explicit := name != ProviderAuto
	scheme := name
	if !explicit {
		scheme = ProviderOllama
		name = ProviderOllama
	}
	// A provider the configuration declares by name is a cloud provider
	// whatever its address: it is a server heo does not run, and its
	// text and key go to it only with the opt-in and the consent.
	declared := false
	if explicit && c.Type != nil {
		if t := c.Type(name); t != "" {
			scheme, declared = t, true
		}
	}
	if !slices.Contains(llm.Schemes(), scheme) {
		if declared {
			key, _ := CloudTypeKeys(name)
			return Plan{}, prerequisite(key,
				fmt.Sprintf("%s %q: this build carries no such provider (it carries %s)", key, scheme, strings.Join(llm.Schemes(), ", ")),
				"set "+key+" to a provider this build carries, such as openai")
		}
		return Plan{}, prerequisite(KeyProvider,
			fmt.Sprintf("%s %q: this build carries no such provider (it carries %s)", KeyProvider, c.Provider, strings.Join(llm.Schemes(), ", ")),
			"set "+KeyProvider+" to "+ProviderOllama+", the local default")
	}
	if c.Model == "" {
		return Plan{}, prerequisite(KeyModel, KeyModel+" is empty", "set "+KeyModel+" to a model your provider serves, such as `heo config set "+KeyModel+" "+DefaultModel+"`")
	}
	if c.Timeout <= 0 {
		return Plan{}, prerequisite(KeyTimeout, KeyTimeout+" is not a positive duration", "set "+KeyTimeout+" to a duration such as 120s")
	}
	if c.MaxPromptChars < MinPromptChars {
		return Plan{}, prerequisite(KeyMaxPromptChars, fmt.Sprintf("%s %d: want at least %d", KeyMaxPromptChars, c.MaxPromptChars, MinPromptChars),
			fmt.Sprintf("set %s to a whole number of at least %d, such as %d", KeyMaxPromptChars, MinPromptChars, DefaultMaxPromptChars))
	}
	raw := c.Endpoint
	if declared && c.CloudEndpoint != nil {
		if e := c.CloudEndpoint(name); e != "" {
			raw = e
		}
	}
	if raw == "" {
		raw = localEndpoints[scheme]
	}
	if raw == "" {
		return Plan{}, prerequisite(KeyEndpoint, scheme+" has no default endpoint", "set "+KeyEndpoint)
	}
	endpoint, host, err := parseEndpoint(raw)
	if err != nil {
		return Plan{}, prerequisite(KeyEndpoint, KeyEndpoint+": "+err.Error(), "set "+KeyEndpoint+" to the provider's base URL, such as "+localEndpoints[ProviderOllama])
	}
	if err := checkURIPart(c.Model); err != nil {
		return Plan{}, prerequisite(KeyModel, KeyModel+": "+err.Error(), "set "+KeyModel+" to the model's name")
	}
	p := Plan{
		Provider: scheme, Name: name, Model: c.Model, Endpoint: endpoint, Host: host, Class: ClassOfHost(host),
		Timeout: c.Timeout, MaxPromptChars: c.MaxPromptChars,
	}
	if declared {
		p.Class = Cloud
	}
	if p.Class == Local {
		return p, nil
	}
	// A cloud provider: conversation text would leave for a host that
	// is not on this machine. "auto" never goes there, and neither does
	// a configuration that has not opted in to this provider by name.
	if !explicit {
		return Plan{}, prerequisite(KeyEndpoint,
			fmt.Sprintf("%s is %s, so %s=%s cannot use it: auto resolves local providers only", KeyEndpoint, host, KeyProvider, ProviderAuto),
			"set "+KeyProvider+" to "+scheme+" and opt in with "+enabledKey(scheme)+"=true, or set "+KeyEndpoint+" to a server on this machine")
	}
	if c.Enabled == nil || !c.Enabled(name) {
		return Plan{}, prerequisite(enabledKey(name),
			fmt.Sprintf("%s %s runs at %s, which is not on this machine: it is a cloud provider and conversation text would leave for it", KeyProvider, scheme, host),
			"opt in with `heo config set "+enabledKey(name)+" true`, or set "+KeyEndpoint+" to a server on this machine")
	}
	if c.Credential != nil {
		p.CredentialRef = c.Credential(name)
	}
	return p, nil
}

// Purpose is the credential purpose of a cloud provider's key: its own
// entry in the profile's credential backend, with the reference's shared
// entry as the fallback.
func Purpose(name string) string { return "llm." + name }

func enabledKey(name string) string {
	k, _ := CloudKeys(name)
	return k
}

// checkURIPart refuses a value that would not survive being put in a
// provider URI whole.
func checkURIPart(v string) error {
	if strings.ContainsAny(v, "&?# \t\r\n") {
		return fmt.Errorf("%q holds a character a provider URI cannot carry (& ? # or a space)", v)
	}
	return nil
}

// uri is the kit provider URI of the plan. The model is the URI's
// model and the endpoint its base_url parameter; the key, when there is
// one, its api_key. Kit masks the key in every error it raises.
func (p Plan) uri(key string) (string, error) {
	u := p.Provider + "://" + p.Model + "?base_url=" + p.Endpoint
	if key != "" {
		if err := checkURIPart(key); err != nil {
			return "", fmt.Errorf("the credential holds a character a provider URI cannot carry")
		}
		u += "&api_key=" + key
	}
	parsed, err := llm.ParseURI(u)
	if err != nil || parsed.Model != p.Model || parsed.Host != "" {
		return "", fmt.Errorf("%s %q is not a model name a provider URI can carry", KeyModel, p.Model)
	}
	return u, nil
}

// KeyFor is the credential a call to p carries: the given one for a
// cloud provider, none for a local one, whatever was resolved.
func KeyFor(p Plan, key string) string {
	if p.Class == Local {
		return ""
	}
	return key
}

// Open builds the provider for the plan through kit's registry. key is
// the cloud provider's credential, empty for none; a local provider is
// never given one.
func (p Plan) Open(key string) (llm.Completer, func(), error) {
	comp, closeFn, err := p.open(key)
	if err != nil {
		return nil, nil, err
	}
	return comp, closeFn, nil
}

func (p Plan) open(key string) (llm.Completer, func(), *planError) {
	installNoRetry()
	u, err := p.uri(KeyFor(p, key))
	if err != nil {
		return nil, nil, prerequisite(KeyModel, err.Error(), "check "+KeyModel+" and the credential")
	}
	prov, err := llm.Resolve(u)
	if err != nil {
		return nil, nil, prerequisite(KeyProvider, "provider "+p.Provider+": "+err.Error(), "check "+KeyProvider+", "+KeyModel+" and "+KeyEndpoint)
	}
	comp, ok := prov.(llm.Completer)
	if !ok {
		_ = prov.Close()
		return nil, nil, prerequisite(KeyProvider, "provider "+p.Provider+" cannot complete text", "set "+KeyProvider+" to "+ProviderOllama)
	}
	return comp, func() { _ = prov.Close() }, nil
}

// callKey marks a context as a model call.
type callKey struct{}

// WithCall marks ctx as carrying a model call. The marker changes
// nothing in a release build; the e2e build routes the HTTP of marked
// calls, and only those, through a cassette (see cmd/xrr_e2e.go).
func WithCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, callKey{}, &callState{})
}

// IsCall reports whether ctx is marked as a model call.
func IsCall(ctx context.Context) bool { return callOf(ctx) != nil }
