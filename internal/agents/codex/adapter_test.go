package codex_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/codex"
)

func fakeCodex(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 9.9.9'; exit 0; fi
printf '%s\n' '{"type":"thread.started","thread_id":"thread-123"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":2}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartNormalizesCodexStream(t *testing.T) {
	adapter := codex.New(fakeCodex(t))
	installation := adapter.Detect(context.Background())
	if !installation.Available || !strings.Contains(installation.Version, "9.9.9") {
		t.Fatalf("installation = %+v", installation)
	}
	run, err := adapter.Start(context.Background(), agents.Request{Prompt: "work", Workspace: t.TempDir(), Sandbox: agents.SandboxWorkspaceWrite, SkipGitCheck: true})
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
	if result.SessionID != "thread-123" || result.Response != "done" || result.Usage.TotalTokens != 13 {
		t.Fatalf("result = %+v", result)
	}
	if len(kinds) != 3 {
		t.Fatalf("events = %v", kinds)
	}
}

func TestResumeRequiresSession(t *testing.T) {
	_, err := codex.New(fakeCodex(t)).Resume(context.Background(), "", agents.Request{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("expected error")
	}
}
