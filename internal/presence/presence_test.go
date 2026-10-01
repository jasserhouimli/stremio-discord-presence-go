package presence

import (
	"testing"
	"time"
)

// tickKey drives the elapsed-timer basis; it must not touch the network.
func TestTimerFreezesOnPauseAndExcludesPause(t *testing.T) {
	m := New("test-client-id")

	m.tickKey("playing|Dune", StatePlaying, true)
	first := m.stamp()
	if first.IsZero() {
		t.Fatal("expected start timestamp after playing")
	}

	time.Sleep(20 * time.Millisecond)
	m.tickKey("playing|Dune", StatePaused, false) // entering pause freezes
	m.tickKey("playing|Dune", StatePaused, false)
	time.Sleep(60 * time.Millisecond)
	m.tickKey("playing|Dune", StatePlaying, true) // resume shifts start forward
	shifted := m.stamp()
	if !shifted.After(first) {
		t.Errorf("resume should shift start forward to exclude pause: first=%v shifted=%v", first, shifted)
	}

	m.tickKey("playing|Other Title", StatePlaying, true) // new title resets
	reset := m.stamp()
	if !reset.After(shifted) {
		t.Errorf("new title should reset timer: shifted=%v reset=%v", shifted, reset)
	}
}
