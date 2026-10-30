package daemon

import "testing"

// TestCognitiveSupervisor_CloseIsIdempotent pins the double-close fix: a closer
// registry or defensive teardown that calls Close twice must not panic on the
// already-closed stop channel. The supervisor is never Started, so Close only
// closes stop and returns from the zero WaitGroup.
func TestCognitiveSupervisor_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newCognitiveSupervisor(nil, nil, nil, nil, nil, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
