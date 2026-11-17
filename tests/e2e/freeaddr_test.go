package e2e

import (
	"fmt"
	"testing"
)

// TestFreeAddrNeverRepeats: freeAddr picks a port by listening and
// closing, so the kernel may hand the same port to consecutive picks; a
// daemon given one port for two of its listeners exits before it is
// ready. No address is returned twice, whatever the kernel returns.
func TestFreeAddrNeverRepeats(t *testing.T) {
	// A source that repeats each address three times in a row, as a
	// kernel reusing the port it just freed would.
	var n int
	next := func() (string, error) {
		n++
		return fmt.Sprintf("127.0.0.1:%d", (n-1)/3), nil
	}
	seen := map[string]bool{}
	for i := range 20 {
		a := pickAddr(t, next)
		if seen[a] {
			t.Fatalf("pick %d: %s returned twice", i, a)
		}
		seen[a] = true
	}
	for i := range 50 {
		a := freeAddr(t)
		if seen[a] {
			t.Fatalf("freeAddr %d: %s returned twice", i, a)
		}
		seen[a] = true
	}
}
