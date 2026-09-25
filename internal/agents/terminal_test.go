package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestKillTerminalPIDDoesNotKillExitedProcess(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	elapsed := func() time.Duration {
		start := time.Now()
		killTerminalPID(path)
		return time.Since(start)
	}()
	if elapsed > 100*time.Millisecond {
		t.Fatalf("killTerminalPID waited %s after finding an exited process", elapsed)
	}
}

func TestStartProcessCancelUnblocksFullEventChannel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run, err := StartProcess(ctx, "sh", []string{"-c", "while true; do echo event; done"}, t.TempDir(), func([]byte) (Event, bool, error) {
		return Event{Kind: EventProgress}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	time.Sleep(50 * time.Millisecond)
	if err := run.Cancel(); err != nil {
		t.Fatal(err)
	}
	result := make(chan Result, 1)
	go func() { result <- run.Wait() }()
	select {
	case <-result:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait blocked after cancellation with an undrained event channel")
	}
}

func TestWriteTerminalLifecycleState(t *testing.T) {
	dir := t.TempDir()
	paths := terminalLifecyclePaths{
		dir:     dir,
		script:  filepath.Join(dir, "run.sh"),
		stdout:  filepath.Join(dir, "stdout.jsonl"),
		stderr:  filepath.Join(dir, "stderr.log"),
		display: filepath.Join(dir, "display.log"),
		pid:     filepath.Join(dir, "pid"),
		exit:    filepath.Join(dir, "exit"),
		state:   filepath.Join(dir, "state.json"),
	}
	started := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	if err := writeTerminalLifecycleState(paths, terminalLifecycleState{
		Version:      1,
		Status:       "starting",
		Command:      "codex",
		Args:         []string{"--json", "do work"},
		Workspace:    "/repo",
		LifecycleDir: dir,
		Paths:        paths.relativePaths(),
		StartedAt:    started,
		UpdatedAt:    started,
	}); err != nil {
		t.Fatal(err)
	}

	var state terminalLifecycleState
	data, err := os.ReadFile(paths.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "starting" || state.Command != "codex" || state.Paths["stdout"] != "stdout.jsonl" {
		t.Fatalf("state = %+v", state)
	}
}

func TestCollectTerminalProcessUpdatesLifecycleState(t *testing.T) {
	dir := t.TempDir()
	paths := terminalLifecyclePaths{
		dir:     dir,
		script:  filepath.Join(dir, "run.sh"),
		stdout:  filepath.Join(dir, "stdout.jsonl"),
		stderr:  filepath.Join(dir, "stderr.log"),
		display: filepath.Join(dir, "display.log"),
		pid:     filepath.Join(dir, "pid"),
		exit:    filepath.Join(dir, "exit"),
		state:   filepath.Join(dir, "state.json"),
	}
	started := time.Now().UTC()
	if err := writeTerminalLifecycleState(paths, terminalLifecycleState{
		Version:      1,
		Status:       "starting",
		Command:      "codex",
		Workspace:    "/repo",
		LifecycleDir: dir,
		Paths:        paths.relativePaths(),
		StartedAt:    started,
		UpdatedAt:    started,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.stdout, []byte("session\nresult\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.exit, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := make(chan Event, 32)
	done := make(chan Result, 1)
	collectTerminalProcess(context.Background(), paths, func(line []byte) (Event, bool, error) {
		switch string(line) {
		case "session":
			return Event{Kind: EventSession, SessionID: "sess_123"}, true, nil
		case "result":
			return Event{Kind: EventResult, Message: "done"}, true, nil
		default:
			return Event{}, false, nil
		}
	}, events, done)

	result := <-done
	if result.Err != nil {
		t.Fatalf("result error = %v", result.Err)
	}
	var state terminalLifecycleState
	data, err := os.ReadFile(paths.state)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "completed" || state.SessionID != "sess_123" || state.ExitCode != 0 || state.CompletedAt == nil {
		t.Fatalf("state = %+v", state)
	}
}
