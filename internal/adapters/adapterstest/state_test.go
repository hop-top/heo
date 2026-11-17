package adapterstest

import (
	"testing"

	"hop.top/uma/model"
)

// TestStateLimitsSurviveTheProbe: both the capability report and the
// permission probe carry the state limits, since uma merges the probe over
// the report and a probe without them leaves the verb "no readable limits".
func TestStateLimitsSurviveTheProbe(t *testing.T) {
	f := New()
	f.DeclareState(StateOptions{})
	i := &instance{f: f}
	for name, get := range map[string]func() ([]model.CapabilityStatus, error){
		"capabilities": func() ([]model.CapabilityStatus, error) { return i.Capabilities(t.Context()) },
		"probe":        func() ([]model.CapabilityStatus, error) { return i.Probe(t.Context()) },
	} {
		sts, err := get()
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, st := range sts {
			if _, ok := f.stateLimits(st.Name); !ok {
				continue
			}
			n++
			if _, err := model.ParseStateLimits(st.Limits); err != nil {
				t.Errorf("%s: %s limits unreadable: %v", name, st.Name, err)
			}
		}
		if n != 5 {
			t.Errorf("%s: %d state capabilities, want 5", name, n)
		}
	}
}
