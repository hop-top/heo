package channels_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters"
	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/heo/internal/channels"
	"hop.top/heo/internal/credentials"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// mapDiag reports each purpose with the status it is given, missing when
// it is given none, and keeps the requests it was asked.
type mapDiag struct {
	status map[string]credentials.Status
	asked  []credentials.Request
}

func (d *mapDiag) Check(_ context.Context, req credentials.Request) credentials.Diagnosis {
	d.asked = append(d.asked, req)
	st := d.status[req.Purpose]
	if st == "" {
		st = credentials.StatusMissing
	}
	return credentials.Diagnosis{Reference: req.Reference, Purpose: req.Purpose, Status: st, Message: string(st)}
}

const (
	purposeSecret = "bot.client_secret"      // #nosec G101 -- purpose name, not a credential
	purposeCert   = "bot.client_certificate" // #nosec G101 -- purpose name, not a credential
)

// oneOfWorld is a channel whose adapter proves itself with a client secret
// or a certificate, exactly one of the two.
func oneOfWorld(t *testing.T, probe func(context.Context, model.Channel) *adapters.CredentialProblem) *world {
	t.Helper()
	reg := adapters.NewRegistry()
	a := adapterstest.New().Adapter()
	a.CredentialPurposes = []string{purposeSecret, purposeCert}
	a.CredentialOneOf = [][]string{{purposeSecret, purposeCert}}
	a.CredentialKinds = map[string][]ports.CredentialKind{
		purposeSecret: {ports.KindSecret}, purposeCert: {ports.KindClientCert},
	}
	a.ProbeCredential = probe
	if err := reg.Register(a); err != nil {
		t.Fatal(err)
	}
	w := newWorldWith(t, reg)
	w.createTyped("bot", adapterstest.Type, adapterstest.Config{Account: "a"})
	return w
}

func (w *world) checkWith(d *mapDiag) channels.ChannelCheck {
	w.t.Helper()
	got, err := w.as(operator).Check(context.Background(), "bot", channels.CheckOptions{Remote: true, Credentials: d})
	if err != nil {
		w.t.Fatal(err)
	}
	return got[0]
}

func credentialFindings(c channels.ChannelCheck) []channels.Finding {
	var out []channels.Finding
	for _, f := range c.Findings {
		if strings.HasPrefix(f.Scope, "credential:") {
			out = append(out, f)
		}
	}
	return out
}

// TestCheckRemoteExactlyOneOfAlternativeCredentials: of purposes the
// adapter lists as alternatives, one is stored and the other is not asked
// for: no finding, and the credential listed is the stored one. Neither
// stored is one finding naming both; both stored is a conflict naming
// both, since the host cannot know which the operator means.
func TestCheckRemoteExactlyOneOfAlternativeCredentials(t *testing.T) {
	probed := 0
	w := oneOfWorld(t, func(context.Context, model.Channel) *adapters.CredentialProblem { probed++; return nil })

	for name, tc := range map[string]struct {
		status map[string]credentials.Status
		listed []string
	}{
		"the secret":                       {map[string]credentials.Status{purposeSecret: credentials.StatusOK}, []string{purposeSecret}},
		"the certificate":                  {map[string]credentials.Status{purposeCert: credentials.StatusOK}, []string{purposeCert}},
		"a rejected certificate is stored": {map[string]credentials.Status{purposeCert: credentials.StatusRejected}, []string{purposeCert}},
	} {
		probed = 0
		d := &mapDiag{status: tc.status}
		c := w.checkWith(d)
		var listed []string
		for _, cd := range c.Remote.Credentials {
			listed = append(listed, cd.Purpose)
		}
		if !slices.Equal(listed, tc.listed) || len(credentialFindings(c)) != 0 && tc.status[purposeCert] != credentials.StatusRejected {
			t.Errorf("%s: credentials %v, findings %+v; want %v listed and no finding", name, listed, c.Findings, tc.listed)
		}
		if probed != 1 {
			t.Errorf("%s: probed %d times, want once", name, probed)
		}
	}

	// Neither: one finding, on the first purpose, naming both.
	probed = 0
	c := w.checkWith(&mapDiag{})
	fs := credentialFindings(c)
	if len(fs) != 1 || fs[0].Scope != "credential:"+purposeSecret || fs[0].Code != string(credentials.StatusMissing) || fs[0].Severity != channels.SeverityError ||
		!strings.Contains(fs[0].Message, purposeSecret) || !strings.Contains(fs[0].Message, purposeCert) || c.OK {
		t.Errorf("neither stored = %+v ok=%v, want one error naming both purposes", fs, c.OK)
	}
	if len(c.Remote.Credentials) != 2 || probed != 0 {
		t.Errorf("neither stored: %d credentials listed, %d probes, want both listed and no probe", len(c.Remote.Credentials), probed)
	}

	// Both: a conflict naming both; the host would not know which to use.
	c = w.checkWith(&mapDiag{status: map[string]credentials.Status{purposeSecret: credentials.StatusOK, purposeCert: credentials.StatusOK}})
	fs = credentialFindings(c)
	if len(fs) != 1 || fs[0].Code != string(model.Conflict) || fs[0].Severity != channels.SeverityError ||
		!strings.Contains(fs[0].Message, purposeSecret) || !strings.Contains(fs[0].Message, purposeCert) || c.OK {
		t.Errorf("both stored = %+v ok=%v, want one conflict naming both purposes", fs, c.OK)
	}
	if probed != 0 {
		t.Errorf("both stored: probed %d times, want none", probed)
	}

	// A stored certificate that is broken is stored: it still conflicts with a
	// secret, and says what is wrong with it.
	c = w.checkWith(&mapDiag{status: map[string]credentials.Status{purposeSecret: credentials.StatusOK, purposeCert: credentials.StatusMalformed}})
	codes := map[string]string{}
	for _, f := range credentialFindings(c) {
		codes[f.Scope] = f.Code
	}
	if codes["credential:"+purposeCert] != string(credentials.StatusMalformed) || !slices.Contains(codeValues(codes), string(model.Conflict)) {
		t.Errorf("secret and a malformed certificate = %v, want the malformation and the conflict", codes)
	}

	// An expired certificate alone is the one finding, not 'missing' for the secret too.
	c = w.checkWith(&mapDiag{status: map[string]credentials.Status{purposeCert: credentials.StatusExpired}})
	fs = credentialFindings(c)
	if len(fs) != 1 || fs[0].Scope != "credential:"+purposeCert || fs[0].Code != string(credentials.StatusExpired) {
		t.Errorf("expired certificate alone = %+v, want one expired finding", fs)
	}
}

func codeValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestCheckRemoteAsksForTheKindsAPurposeAccepts: a purpose that accepts one
// kind is diagnosed as that kind, so a credential of another kind stored
// where the purpose looks (the reference's shared entry) is not mistaken
// for the one it wants.
func TestCheckRemoteAsksForTheKindsAPurposeAccepts(t *testing.T) {
	w := oneOfWorld(t, nil)
	d := &mapDiag{}
	w.checkWith(d)
	got := map[string][]ports.CredentialKind{}
	for _, r := range d.asked {
		got[r.Purpose] = r.Accept
	}
	if !slices.Equal(got[purposeSecret], []ports.CredentialKind{ports.KindSecret}) || !slices.Equal(got[purposeCert], []ports.CredentialKind{ports.KindClientCert}) {
		t.Errorf("asked %v, want each purpose's own kind", got)
	}
}

// TestCheckRemoteProbeWarning: a probe that finds the credential works but
// has something to say (a certificate close to its not_after) is a warning
// finding: the channel is still ok.
func TestCheckRemoteProbeWarning(t *testing.T) {
	w := oneOfWorld(t, func(context.Context, model.Channel) *adapters.CredentialProblem {
		return &adapters.CredentialProblem{Purpose: purposeCert, Code: "expires_soon", Message: "the certificate expires in 12 days", Warning: true}
	})
	c := w.checkWith(&mapDiag{status: map[string]credentials.Status{purposeCert: credentials.StatusOK}})
	f, found := findingFor(c, "credential:"+purposeCert)
	if !found || f.Severity != channels.SeverityWarning || f.Code != "expires_soon" || !c.OK {
		t.Errorf("finding %+v (found %v) ok=%v, want a warning and an ok channel", f, found, c.OK)
	}
}
