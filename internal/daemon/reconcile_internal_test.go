package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"hop.top/heo/internal/adapters/adapterstest"
	"hop.top/uma/model"
)

func entry(id, rev, account string) inbound {
	ch := model.Channel{
		Ref: model.ChannelRef{Tenant: "acme", ID: model.ID(id)}, Key: id, Type: adapterstest.Type,
		Revision: model.Revision(rev), Configuration: adapterstest.Config{Account: account}.Configuration(),
	}
	return inbound{ch: ch, ad: adapterstest.New().Adapter(), known: true, account: account}
}

func ids(ins []inbound) []string {
	out := make([]string, 0, len(ins))
	for _, in := range ins {
		out = append(out, string(in.ch.Ref.ID)+"@"+string(in.ch.Revision))
	}
	slices.Sort(out)
	return out
}

func TestDiffPlan(t *testing.T) {
	refused := entry("waiting", "r1", "acct-w")
	refused.claimErr = errors.New("held")
	cur := []inbound{
		entry("same", "r1", "acct-s"),
		entry("changed", "r1", "acct-c"),
		entry("removed", "r1", "acct-r"),
		refused,
	}
	next := []inbound{
		entry("same", "r1", "acct-s"),
		entry("changed", "r2", "acct-c"),
		entry("added", "r1", "acct-a"),
		entry("waiting", "r1", "acct-w"),
	}
	d := diffPlan(cur, next)
	if got, want := ids(d.keep), []string{"same@r1"}; !slices.Equal(got, want) {
		t.Errorf("keep = %v, want %v", got, want)
	}
	if got, want := ids(d.stop), []string{"changed@r1", "removed@r1", "waiting@r1"}; !slices.Equal(got, want) {
		t.Errorf("stop = %v, want %v", got, want)
	}
	if got, want := ids(d.start), []string{"added@r1", "changed@r2", "waiting@r1"}; !slices.Equal(got, want) {
		t.Errorf("start = %v, want %v", got, want)
	}
	if !d.gone("removed") || d.gone("changed") || d.gone("waiting") || d.gone("same") {
		t.Errorf("gone: removed %v, changed %v", d.gone("removed"), d.gone("changed"))
	}
	if d.empty() {
		t.Error("diff with changes reported empty")
	}
	if d := diffPlan(next[:1], next[:1]); !d.empty() || len(d.keep) != 1 {
		t.Errorf("no change = %+v", d)
	}
}

func TestAssignAccounts(t *testing.T) {
	keep := []inbound{entry("incumbent", "r1", "acct-1")}
	start := []inbound{
		entry("b-dup", "r1", "acct-1"),
		entry("c-new", "r1", "acct-2"),
		entry("d-same-new", "r1", "acct-2"),
		entry("e-foreign", "r1", "acct-3"),
	}
	noAccount := entry("f-none", "r1", "")
	noAccount.accountErr = errors.New("configuration names no account")
	start = append(start, noAccount)
	var claimed []string
	claim := func(in inbound) error {
		if in.account == "acct-3" {
			return errors.New("received by heo/other (pid 1)")
		}
		claimed = append(claimed, in.account)
		return nil
	}
	assignAccounts(keep, start, claim)
	reasons := map[string]string{}
	for _, in := range start {
		if in.claimErr != nil {
			reasons[string(in.ch.Ref.ID)] = in.claimErr.Error()
		}
	}
	if r := reasons["b-dup"]; !strings.Contains(r, `channel "incumbent"`) || !strings.Contains(r, "already receives this provider account") {
		t.Errorf("duplicate of the incumbent = %q", r)
	}
	if r := reasons["d-same-new"]; !strings.Contains(r, `channel "c-new"`) {
		t.Errorf("second new channel on one account = %q", r)
	}
	if r := reasons["e-foreign"]; !strings.Contains(r, "received by heo/other") {
		t.Errorf("account another daemon holds = %q", r)
	}
	if _, ok := reasons["c-new"]; ok {
		t.Errorf("first new channel refused: %q", reasons["c-new"])
	}
	if _, ok := reasons["f-none"]; ok {
		t.Error("a channel without an account was given a claim refusal")
	}
	if !slices.Equal(claimed, []string{"acct-2"}) {
		t.Errorf("claimed %v, want only the free account", claimed)
	}
}
