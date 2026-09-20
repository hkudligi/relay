package cursor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harsha/relay/internal/agents"
	cursorcli "github.com/harsha/relay/internal/agents/cursor"
)

func fakeCursor(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2026.9.11-abc'; exit 0; fi
printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-123","model":"composer-2.5"}'
printf '%s\n' '{"type":"assistant","session_id":"chat-123","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"chat-123"}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartNormalizesCursorStream(t *testing.T) {
	adapter := cursorcli.New(fakeCursor(t))
	installation := adapter.Detect(context.Background())
	if !installation.Available || !strings.Contains(installation.Version, "2026.9.11") {
		t.Fatalf("installation = %+v", installation)
	}
	run, err := adapter.Start(context.Background(), agents.Request{Prompt: "work", Workspace: t.TempDir(), Sandbox: agents.SandboxWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []agents.EventKind
	for event := range run.Events() {
		kinds = append(kinds, event.Kind)
	}
	result := run.Wait()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.SessionID != "chat-123" || result.Response != "done" {
		t.Fatalf("result = %+v", result)
	}
	if len(kinds) != 3 {
		t.Fatalf("events = %v", kinds)
	}
}

func TestResumeRequiresSession(t *testing.T) {
	_, err := cursorcli.New(fakeCursor(t)).Resume(context.Background(), "", agents.Request{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStartPreservesUsageLimitError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2026.9.11-abc'; exit 0; fi
printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"rate limit exceeded","session_id":"chat-err"}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := cursorcli.New(path).Start(context.Background(), agents.Request{Prompt: "work", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events() {
	}
	result := run.Wait()
	if result.Err == nil || !strings.Contains(result.Err.Error(), "rate limit") {
		t.Fatalf("result = %+v", result)
	}
}
