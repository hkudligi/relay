package freebuff

import (
	"testing"
	"time"
)

func TestResetWatchdogsAfterSleepExcludesSleepGap(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	started := base
	lastActivity := base.Add(30 * time.Second)
	lastHeartbeat := base.Add(45 * time.Second)
	now := base.Add(11 * time.Minute)

	updatedStarted, updatedActivity, updatedHeartbeat := resetWatchdogsAfterSleep(
		started,
		lastActivity,
		lastHeartbeat,
		now,
		10*time.Minute,
	)

	if got, want := updatedStarted, started.Add(10*time.Minute); !got.Equal(want) {
		t.Fatalf("started = %s, want %s", got, want)
	}
	if !updatedActivity.Equal(now) || !updatedHeartbeat.Equal(now) {
		t.Fatalf("watchdog anchors = %s, %s, want both %s", updatedActivity, updatedHeartbeat, now)
	}
}

func TestResetWatchdogsAfterShortGapDoesNothing(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	started := base
	lastActivity := base.Add(30 * time.Second)
	lastHeartbeat := base.Add(45 * time.Second)
	now := base.Add(90 * time.Second)

	updatedStarted, updatedActivity, updatedHeartbeat := resetWatchdogsAfterSleep(
		started,
		lastActivity,
		lastHeartbeat,
		now,
		90*time.Second,
	)

	if !updatedStarted.Equal(started) || !updatedActivity.Equal(lastActivity) || !updatedHeartbeat.Equal(lastHeartbeat) {
		t.Fatalf("short gap changed watchdog anchors: %s, %s, %s", updatedStarted, updatedActivity, updatedHeartbeat)
	}
}
