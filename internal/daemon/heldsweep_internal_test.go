package daemon

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

// TestHeldSweepCadence: the held-operation sweep cadence reaches every
// worker's options; the default leaves uma's own cadence, "off" turns the
// sweep off, and a bad value is a configuration problem.
func TestHeldSweepCadence(t *testing.T) {
	v := viper.New()
	for k, val := range Defaults() {
		v.SetDefault(k, val)
	}
	cfg, probs := ReadWorkers(v)
	if len(probs) != 0 || cfg.HeldSweepEvery != 0 || workerOptions("o", cfg).SweepEvery != 0 {
		t.Fatalf("default: %+v %+v", cfg, probs)
	}
	for raw, want := range map[string]time.Duration{"30s": 30 * time.Second, "off": -1} {
		v.Set(KeyWorkersHeldSweepEvery, raw)
		cfg, probs = ReadWorkers(v)
		if len(probs) != 0 || workerOptions("o", cfg).SweepEvery != want {
			t.Fatalf("%q: %+v %+v", raw, cfg, probs)
		}
	}
	for _, raw := range []string{"soon", "-5s"} {
		v.Set(KeyWorkersHeldSweepEvery, raw)
		if _, probs = ReadWorkers(v); len(probs) != 1 || probs[0].Key != KeyWorkersHeldSweepEvery {
			t.Fatalf("%q: %+v", raw, probs)
		}
	}
}
