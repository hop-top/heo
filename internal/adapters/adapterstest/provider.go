package adapterstest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"hop.top/heo/internal/adapters"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// ProviderType is the type key of the fake variant that sends over HTTP
// to a provider stand-in a test owns (Provider), registered beside Type
// in the e2e build so a cassette can record the exchange.
const ProviderType model.TypeKey = "test.heo.fake.provider"

// ProviderPurpose is the credential purpose the provider variant
// resolves its bearer token for.
const ProviderPurpose = "api"

// ProviderConfig is a provider-variant channel's configuration: a fake
// channel's, plus where its provider stand-in listens.
type ProviderConfig struct {
	Config
	// URL is the stand-in's base URL, an http(s) URL with no query.
	URL string `json:"provider_url"`
}

// Configuration returns c as a channel configuration extension.
func (c ProviderConfig) Configuration() model.Extension {
	data, _ := json.Marshal(c)
	return model.Extension{Name: string(ProviderType) + ".config", Version: 1, Data: data}
}

// SendPath is the path under the stand-in's URL a send is POSTed to.
const SendPath = "/v1/messages"

// SendBody is the JSON a provider-variant send POSTs: the route and the
// text, nothing the run chose (no operation or message ID), so the same
// send is the same request on every run.
type SendBody struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// Provider returns the fake adapter variant that sends over the host's
// HTTP client: one POST per send to the channel's provider URL, with the
// channel's credential as a bearer token, answered with {"id": "..."}.
// Everything else is the fake's: it receives over the webhook route and
// answers every capability. It is never in a release binary.
func Provider() adapters.Adapter {
	f := New()
	f.As(ProviderType)
	a := f.Adapter()
	a.Factory = &providerFactory{Factory: f}
	a.CredentialPurposes = []string{ProviderPurpose}
	return a
}

type providerFactory struct{ *Factory }

// Validate refuses a provider URL that is not an http(s) URL.
func (p *providerFactory) Validate(e model.Extension) []model.ValidationIssue {
	if issues := p.Factory.Validate(e); len(issues) > 0 {
		return issues
	}
	var c ProviderConfig
	if err := json.Unmarshal(e.Data, &c); err != nil {
		return []model.ValidationIssue{{Field: "provider_url", Code: "invalid", Message: err.Error()}}
	}
	if _, err := providerBase(c.URL); err != nil {
		return []model.ValidationIssue{{Field: "provider_url", Code: "invalid", Message: err.Error()}}
	}
	return nil
}

func providerBase(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, fmt.Errorf("want an http(s) URL with a host and no query, got %q", raw)
	}
	return u, nil
}

// Open opens the fake instance and gives it the host's HTTP client and
// credentials.
func (p *providerFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := p.open(ch)
	if err != nil {
		return nil, err
	}
	var c ProviderConfig
	if err := json.Unmarshal(ch.Configuration.Data, &c); err != nil {
		return nil, model.NewError(model.Permanent, "provider configuration does not parse")
	}
	base, err := providerBase(c.URL)
	if err != nil {
		return nil, model.NewError(model.Permanent, "provider_url: "+err.Error())
	}
	return &providerInstance{instance: inst, base: base, http: deps.HTTP, creds: deps.Credentials}, nil
}

type providerInstance struct {
	*instance
	base  *url.URL
	http  ports.HTTPClient
	creds ports.Credentials
}

// Send POSTs the text to the provider stand-in. A 2xx answer names the
// provider's message; a 4xx is the provider's refusal, a 5xx transient.
func (i *providerInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	cred, err := i.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: i.ch.Ref, Reference: i.ch.CredentialRef, Purpose: ProviderPurpose,
		Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return adapter.Outcome{}, err
	}
	secret, ok := cred.(ports.Secret)
	if !ok {
		return adapter.Outcome{}, model.NewError(model.Unsupported, "the provider takes a secret credential")
	}
	var text []string
	for _, p := range r.Content.Parts {
		if p.Kind == model.PartText {
			text = append(text, p.Text)
		}
	}
	body, err := json.Marshal(SendBody{To: r.Route.ID, Text: strings.Join(text, "\n")})
	if err != nil {
		return adapter.Outcome{}, err
	}
	target := *i.base
	target.Path = strings.TrimRight(target.Path, "/") + SendPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return adapter.Outcome{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(secret.Bytes))
	resp, err := i.http.Do(req)
	if err != nil {
		// The outcome is unknown: the request may have been served.
		return adapter.Outcome{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var answer struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &answer); err != nil || answer.ID == "" {
			return adapter.Outcome{}, model.NewError(model.Permanent, "the provider's answer names no message")
		}
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
			model.ProviderMessageRef(r.Route.Channel, answer.ID),
		}}, nil
	case resp.StatusCode >= 500:
		return adapter.Outcome{}, model.NewError(model.Transient, fmt.Sprintf("provider answered %d", resp.StatusCode))
	default:
		return adapter.Outcome{}, model.NewError(model.Permanent, fmt.Sprintf("provider refused the message: %d", resp.StatusCode))
	}
}
