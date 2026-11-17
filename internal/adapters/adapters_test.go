package adapters_test

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func TestRegisterRules(t *testing.T) {
	r := adapters.NewRegistry()
	if err := r.Register(adapters.Adapter{}); err == nil {
		t.Error("nil factory registered")
	}
	f := adapterstest.New()
	if err := r.Register(adapters.Adapter{Factory: f}); err == nil || !strings.Contains(err.Error(), "names no provider account") {
		t.Errorf("receiving adapter without Account: %v", err)
	}
	if err := r.Register(f.Adapter()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(f.Adapter()); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("duplicate type: %v", err)
	}
	if r.Len() != 1 || len(r.Factories()) != 1 {
		t.Errorf("registry holds %d", r.Len())
	}
}

func TestAccountOfQualifiesByType(t *testing.T) {
	a := adapterstest.New().Adapter()
	ch := model.Channel{Type: adapterstest.Type, Configuration: adapterstest.Config{Account: "bot-1"}.Configuration()}
	got, err := a.AccountOf(ch)
	if err != nil || got != string(adapterstest.Type)+"\x00bot-1" {
		t.Errorf("AccountOf = %q, %v", got, err)
	}
	ch.Configuration = adapterstest.Config{}.Configuration()
	if _, err := a.AccountOf(ch); err == nil {
		t.Error("empty account accepted")
	}
}

func TestRoutesNeedReceiveHTTP(t *testing.T) {
	r := adapters.NewRegistry()
	sendOnly := sendOnlyFactory{adapterstest.New()}
	routes := func(model.Channel) map[string]http.Handler { return nil }
	if err := r.Register(adapters.Adapter{Factory: sendOnly, Routes: routes}); err == nil || !strings.Contains(err.Error(), "does not declare receive_http") {
		t.Errorf("routes on an adapter without receive_http: %v", err)
	}
	f := adapterstest.New().Adapter()
	f.Routes = routes
	if err := r.Register(f); err != nil {
		t.Fatal(err)
	}
	if got := r.Types(); len(got) != 1 || got[0] != adapterstest.Type {
		t.Errorf("Types = %v", got)
	}
}

// sendOnlyFactory is the fake adapter declaring send alone.
type sendOnlyFactory struct{ *adapterstest.Factory }

func (f sendOnlyFactory) Describe() adapter.Descriptor {
	d := f.Factory.Describe()
	d.Type = "test.heo.sendonly"
	d.Capabilities = []model.Capability{model.CapabilitySend}
	return d
}

// TestCredentialReceivingRules: a purpose tied to a way of receiving is
// one the adapter lists, tied to receive or receive_http, which the
// adapter declares.
func TestCredentialReceivingRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory adapter.Factory
		tie     map[string]model.Capability
		want    string
	}{
		{"unlisted purpose", adapterstest.New(), map[string]model.Capability{"socket": model.CapabilityReceive}, "does not list the purpose"},
		{"not a way of receiving", adapterstest.New(), map[string]model.Capability{"api": model.CapabilitySend}, "not a way of receiving"},
		{"undeclared", sendOnlyFactory{adapterstest.New()}, map[string]model.Capability{"api": model.CapabilityReceive}, "does not declare"},
		{"accepted", adapterstest.New(), map[string]model.Capability{"api": model.CapabilityReceive}, ""},
	} {
		a := adapterstest.New().Adapter()
		a.Factory = tc.factory
		a.CredentialPurposes = []string{"api"}
		a.CredentialReceiving = tc.tie
		err := adapters.NewRegistry().Register(a)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestServingNeedsReceive(t *testing.T) {
	r := adapters.NewRegistry()
	sendOnly := sendOnlyFactory{adapterstest.New()}
	if err := r.Register(adapters.Adapter{Factory: sendOnly, Serving: adapters.NewServing()}); err == nil || !strings.Contains(err.Error(), "does not declare receive") {
		t.Errorf("serving on an adapter without receive: %v", err)
	}
}

func TestReportsLinkNeedsReceive(t *testing.T) {
	r := adapters.NewRegistry()
	sendOnly := sendOnlyFactory{adapterstest.New()}
	if err := r.Register(adapters.Adapter{Factory: sendOnly, ReportsLink: true}); err == nil || !strings.Contains(err.Error(), "does not declare receive") {
		t.Errorf("a link reported by an adapter without receive: %v", err)
	}
}

func TestServingReportsToWatcher(t *testing.T) {
	s := adapters.NewServing()
	ref := model.ChannelRef{Tenant: "acme", ID: "ch-1"}
	s.Report(ref, nil) // nobody watches: dropped
	var got []error
	stop := s.Watch(ref, func(err error) { got = append(got, err) })
	s.Report(model.ChannelRef{Tenant: "acme", ID: "ch-2"}, nil)
	s.Report(ref, nil)
	s.Report(ref, errors.New("bind: address already in use"))
	stop()
	s.Report(ref, nil)
	if len(got) != 2 || got[0] != nil || got[1] == nil {
		t.Errorf("watcher saw %v", got)
	}
	var none *adapters.Serving
	none.Report(ref, nil)
}

// TestCredentialSendingRules: a purpose only sending resolves is one the
// adapter lists, on an adapter that declares send.
func TestCredentialSendingRules(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory adapter.Factory
		purpose []string
		want    string
	}{
		{"unlisted purpose", adapterstest.New(), []string{"callback"}, "does not list the purpose"},
		{"undeclared", noSendFactory{adapterstest.New()}, []string{"api"}, "does not declare"},
		{"accepted", adapterstest.New(), []string{"api"}, ""},
	} {
		a := adapterstest.New().Adapter()
		a.Factory = tc.factory
		a.CredentialPurposes = []string{"api"}
		a.CredentialSending = tc.purpose
		err := adapters.NewRegistry().Register(a)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// noSendFactory declares nothing it could send with.
type noSendFactory struct{ adapter.Factory }

func (f noSendFactory) Describe() adapter.Descriptor {
	d := f.Factory.Describe()
	d.Type = "test.heo.nosend"
	d.Capabilities = []model.Capability{model.CapabilityReceiveHTTP}
	return d
}

// TestCredentialAlternativesRules: alternatives are listed purposes, at
// least two to a group and each in one group at most; a kind is named for a
// listed purpose.
func TestCredentialAlternativesRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		purps []string
		oneOf [][]string
		kinds map[string][]ports.CredentialKind
		want  string
	}{
		{"unlisted alternative", []string{"a", "b"}, [][]string{{"a", "c"}}, nil, "does not list the purpose"},
		{"a group of one", []string{"a", "b"}, [][]string{{"a"}}, nil, "at least two"},
		{"a purpose twice", []string{"a", "b", "c"}, [][]string{{"a", "b"}, {"b", "c"}}, nil, "more than one group"},
		{"a purpose twice in a group", []string{"a", "b"}, [][]string{{"a", "a"}}, nil, "more than one group"},
		{"a kind for an unlisted purpose", []string{"a"}, nil, map[string][]ports.CredentialKind{"z": {ports.KindSecret}}, "does not list the purpose"},
		{"a purpose with no kind", []string{"a"}, nil, map[string][]ports.CredentialKind{"a": nil}, "names no kind"},
		{"accepted", []string{"a", "b"}, [][]string{{"a", "b"}}, map[string][]ports.CredentialKind{"a": {ports.KindSecret}}, ""},
	} {
		a := adapterstest.New().Adapter()
		a.CredentialPurposes, a.CredentialOneOf, a.CredentialKinds = tc.purps, tc.oneOf, tc.kinds
		err := adapters.NewRegistry().Register(a)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// TestExtensionsAreRegisteredOnce: two adapters of heo's may ship the
// same effect (a fake that stands in for the adapter that owns it, in
// a test build); uma registers a definition once and refuses a second,
// so the registry hands it on once, in registration order. Definitions
// that differ in anything but their name are both kept: uma refuses
// the conflict where it is composed.
func TestExtensionsAreRegisteredOnce(t *testing.T) {
	def := func(name string, v uint32) adapter.ExtensionDefinition {
		return adapter.ExtensionDefinition{Name: name, Version: v, Kind: model.ExtensionKindEffect,
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionConversationLabel}
	}
	a, b := adapterstest.New(), adapterstest.New()
	b.As("test.heo.fake.second")
	first, second := a.Adapter(), b.Adapter()
	first.Extensions = []adapter.ExtensionDefinition{def("x.one", 1), def("x.two", 1)}
	second.Extensions = []adapter.ExtensionDefinition{def("x.two", 1), def("x.three", 1)}
	r := adapters.NewRegistry()
	for _, ad := range []adapters.Adapter{first, second} {
		if err := r.Register(ad); err != nil {
			t.Fatal(err)
		}
	}
	var names []string
	for _, d := range r.Extensions() {
		names = append(names, d.Name)
	}
	if want := []string{"x.one", "x.two", "x.three"}; !slices.Equal(names, want) {
		t.Errorf("Extensions = %v, want %v", names, want)
	}

	// The same name in another version, or with another definition, is
	// not a duplicate.
	third := adapterstest.New()
	third.As("test.heo.fake.third")
	ad := third.Adapter()
	changed := def("x.one", 1)
	changed.AuthorizeAs = model.ActionWorkItemAct
	ad.Extensions = []adapter.ExtensionDefinition{def("x.one", 2), changed}
	if err := r.Register(ad); err != nil {
		t.Fatal(err)
	}
	if got := len(r.Extensions()); got != 5 {
		t.Errorf("Extensions = %d definitions, want 5 (a version and a changed definition are not duplicates)", got)
	}
}
