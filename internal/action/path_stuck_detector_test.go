package action

import (
	"testing"
	"time"

	"github.com/hectorgimenez/d2go/pkg/data"
	"github.com/hectorgimenez/d2go/pkg/data/area"
)

func TestPathStuckDetector_Update(t *testing.T) {
	detector := NewPathStuckDetector()
	pos1 := data.Position{X: 100, Y: 100}
	pos2 := data.Position{X: 110, Y: 110}
	testArea := area.BloodMoor

	// First update - should not be stuck
	isStuck := detector.Update(pos1, testArea)
	if isStuck {
		t.Error("Should not be stuck on first update")
	}

	// Same position - should not be stuck yet (timer not expired)
	isStuck = detector.Update(pos1, testArea)
	if isStuck {
		t.Error("Should not be stuck immediately")
	}

	// Wait for stuck timeout
	time.Sleep(PathStuckTimeout + 100*time.Millisecond)

	// Same position after timeout - should be stuck
	isStuck = detector.Update(pos1, testArea)
	if !isStuck {
		t.Error("Should be stuck after timeout")
	}

	// Different position - should reset and not be stuck
	isStuck = detector.Update(pos2, testArea)
	if isStuck {
		t.Error("Should not be stuck after moving")
	}
}

func TestPathStuckDetector_AreaChange(t *testing.T) {
	detector := NewPathStuckDetector()
	pos := data.Position{X: 100, Y: 100}
	area1 := area.BloodMoor
	area2 := area.ColdPlains

	// Start in area1
	detector.Update(pos, area1)

	// Wait a bit (not long enough to trigger stuck)
	time.Sleep(2 * time.Second)

	// Change to area2 - should reset timer and all state
	isStuck := detector.Update(pos, area2)
	if isStuck {
		t.Error("Should not be stuck after area change")
	}

	// Verify timer was reset (should be zero after Reset())
	if !detector.stuckSince.IsZero() {
		t.Error("Timer should have been reset to zero after area change")
	}
}

func TestPathStuckDetector_OnStuckDetected(t *testing.T) {
	detector := NewPathStuckDetector()
	currentPos := data.Position{X: 100, Y: 100}
	nextPos := data.Position{X: 107, Y: 107}

	// Initially no blacklisted points
	if detector.HasBlacklistedPoints() {
		t.Error("Should have no blacklisted points initially")
	}

	// Blacklist positions after stuck detection
	detector.OnStuckDetected(currentPos, nextPos)

	// Should now have blacklisted points
	if !detector.HasBlacklistedPoints() {
		t.Error("Should have blacklisted points after stuck detection")
	}

	// Should have 2 blacklisted points (current + next)
	blacklisted := detector.GetBlacklistedPoints()
	if len(blacklisted) != 2 {
		t.Errorf("Expected 2 blacklisted points, got %d", len(blacklisted))
	}

	// Points within radius should be blacklisted
	nearbyPos := data.Position{X: 103, Y: 103}
	if !detector.IsPointBlacklisted(nearbyPos) {
		t.Error("Nearby point should be blacklisted")
	}

	// Points outside radius should not be blacklisted
	farPos := data.Position{X: 150, Y: 150}
	if detector.IsPointBlacklisted(farPos) {
		t.Error("Far point should not be blacklisted")
	}
}

func TestPathStuckDetector_Reset(t *testing.T) {
	detector := NewPathStuckDetector()
	pos := data.Position{X: 100, Y: 100}
	nextPos := data.Position{X: 107, Y: 107}
	testArea := area.BloodMoor

	// Create some state
	detector.Update(pos, testArea)
	detector.OnStuckDetected(pos, nextPos)

	if !detector.HasBlacklistedPoints() {
		t.Error("Should have blacklisted points before reset")
	}

	// Reset
	detector.Reset()

	// All state should be cleared
	if detector.HasBlacklistedPoints() {
		t.Error("Should have no blacklisted points after reset")
	}

	if !detector.stuckSince.IsZero() {
		t.Error("Stuck timer should be zero after reset")
	}

	if detector.lastPosition != (data.Position{}) {
		t.Error("Last position should be zero after reset")
	}
}

func TestPathStuckDetector_EnableDisable(t *testing.T) {
	detector := NewPathStuckDetector()
	pos := data.Position{X: 100, Y: 100}
	testArea := area.BloodMoor

	// Should be enabled by default
	if !detector.IsEnabled() {
		t.Error("Should be enabled by default")
	}

	// Disable
	detector.Disable()
	if detector.IsEnabled() {
		t.Error("Should be disabled after Disable()")
	}

	// Update while disabled - should not detect stuck even after timeout
	detector.Update(pos, testArea)
	time.Sleep(PathStuckTimeout + 100*time.Millisecond)
	isStuck := detector.Update(pos, testArea)
	if isStuck {
		t.Error("Should not detect stuck when disabled")
	}

	// Enable
	detector.Enable()
	if !detector.IsEnabled() {
		t.Error("Should be enabled after Enable()")
	}
}

func TestPathStuckDetector_SamePositionBlacklisting(t *testing.T) {
	detector := NewPathStuckDetector()
	pos := data.Position{X: 100, Y: 100}

	// When current and next are the same, should only blacklist once
	detector.OnStuckDetected(pos, pos)

	blacklisted := detector.GetBlacklistedPoints()
	// Should only create 1 entry when positions are the same (implementation behavior)
	if len(blacklisted) != 1 {
		t.Errorf("Expected 1 blacklisted point when positions are same, got %d", len(blacklisted))
	}

	if !detector.IsPointBlacklisted(pos) {
		t.Error("Position should be blacklisted")
	}
}

func TestPathStuckDetector_MultipleStuckDetections(t *testing.T) {
	detector := NewPathStuckDetector()
	pos1 := data.Position{X: 100, Y: 100}
	pos2 := data.Position{X: 200, Y: 200}
	nextPos1 := data.Position{X: 107, Y: 107}
	nextPos2 := data.Position{X: 207, Y: 207}

	// First stuck detection
	detector.OnStuckDetected(pos1, nextPos1)
	firstCount := len(detector.GetBlacklistedPoints())

	// Second stuck detection at different location
	detector.OnStuckDetected(pos2, nextPos2)
	secondCount := len(detector.GetBlacklistedPoints())

	if secondCount <= firstCount {
		t.Error("Should accumulate blacklisted points from multiple stuck detections")
	}

	// Both positions should be blacklisted
	if !detector.IsPointBlacklisted(pos1) {
		t.Error("First stuck position should remain blacklisted")
	}
	if !detector.IsPointBlacklisted(pos2) {
		t.Error("Second stuck position should be blacklisted")
	}
}
