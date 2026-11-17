package daemon

import (
	"testing"
	"time"
)

// TestJitterIsWithinTenPercent: the retry wait moves by at most 10%,
// and channels with different phases do not wait the same.
func TestJitterIsWithinTenPercent(t *testing.T) {
	base := 10 * time.Second
	seen := map[time.Duration]bool{}
	for i := range 20 {
		got := jitter(base, float64(i)/20)
		if got < 9*time.Second || got > 11*time.Second {
			t.Errorf("jitter(%s) = %s, outside +-10%%", base, got)
		}
		seen[got] = true
	}
	if len(seen) < 20 {
		t.Errorf("%d distinct waits for 20 phases, want 20", len(seen))
	}
}
