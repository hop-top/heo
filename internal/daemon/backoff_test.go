package daemon

import (
	"testing"
	"time"
)

// TestReceiveBackoffResetsAfterHealthyRun: a receiver that ran longer
// than the longest wait starts its backoff over; one that failed
// quickly keeps backing off.
func TestReceiveBackoffResetsAfterHealthyRun(t *testing.T) {
	if got := receiveBackoff(maxReceiveBackoff, maxReceiveBackoff); got != minReceiveBackoff {
		t.Errorf("after a healthy run = %v, want %v", got, minReceiveBackoff)
	}
	if got := receiveBackoff(time.Minute, time.Second); got != time.Minute {
		t.Errorf("after a quick failure = %v, want the current wait", got)
	}
}
