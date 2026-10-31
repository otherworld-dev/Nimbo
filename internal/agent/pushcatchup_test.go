package agent

import "testing"

// Deck #991: changes and the end of an outage went unnoticed until the next
// poll (up to 5 minutes) after push reconnected: a reconnect only updated the
// diagnostics. Events may have been missed while it was down, so a reconnect
// asks for a catch-up pass. The first connect at startup does not: startup
// runs its own full pass.
func TestPushReconnectTriggersACatchUp(t *testing.T) {
	e := &Engine{}
	calls := 0
	e.onFilesChanged = func() { calls++ }

	e.setPushState(true) // first connect
	if calls != 0 {
		t.Fatalf("first connect triggered %d catch-up(s), want 0", calls)
	}
	e.setPushState(true) // still up: no edge
	e.setPushState(false)
	e.setPushState(true) // reconnect
	if calls != 1 {
		t.Fatalf("reconnect triggered %d catch-up(s), want 1", calls)
	}
}
