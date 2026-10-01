package main

import (
	"testing"
	"time"
)

func TestPauseHeuristicOffByDefault(t *testing.T) {
	now := time.Now()
	stalled := now.Add(-1 * time.Hour)
	if shouldMarkPaused(0, stalled, now) {
		t.Error("pauseAfterSec=0 must never mark paused, even after a long stall")
	}
	if shouldMarkPaused(-5, stalled, now) {
		t.Error("negative pauseAfterSec must never mark paused")
	}
}

func TestPauseHeuristicThresholdWhenEnabled(t *testing.T) {
	now := time.Now()
	if !shouldMarkPaused(12, now.Add(-13*time.Second), now) {
		t.Error("stall past threshold should mark paused when enabled")
	}
	if shouldMarkPaused(12, now.Add(-5*time.Second), now) {
		t.Error("stall under threshold must not mark paused")
	}
}
