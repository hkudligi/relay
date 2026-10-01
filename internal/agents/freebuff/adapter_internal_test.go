package freebuff

import (
	"os"
	"path/filepath"
	"strings"
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

func TestOpenVisibleTmuxMirrorStartsDetached(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "osascript.log")
	osascript := filepath.Join(dir, "osascript")
	script := "#!/bin/sh\nprintf '%s\\n' \"$2\" > " + shellQuote(logPath) + "\nsleep 2\n"
	if err := os.WriteFile(osascript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	started := time.Now()
	if err := openVisibleTmuxMirror("/tmp/tmux path", "sock name", "session name"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("mirror launch blocked for %s, want detached startup", elapsed)
	}

	deadline := time.Now().Add(time.Second)
	var appleScript string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil {
			appleScript = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if appleScript == "" {
		t.Fatal("fake osascript did not receive AppleScript")
	}
	for _, want := range []string{"/tmp/tmux path", "sock name", "session name", "attach-session -r", "System Events", "keystroke \"t\"", "selected tab of front window"} {
		if !strings.Contains(appleScript, want) {
			t.Fatalf("AppleScript = %q, want %q", appleScript, want)
		}
	}
}
