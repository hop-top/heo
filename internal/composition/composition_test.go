package composition_test

import (
	"context"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// TestEngineComposes builds and closes a uma engine from this module,
// proving the uma module graph resolves for heo.
func TestEngineComposes(t *testing.T) {
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
	engine, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{}, nil
		}),
		Clock: clock,
		IDs:   portstest.NewSequenceIDs(1),
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	if err := engine.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
