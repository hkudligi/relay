package agy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/agy"
)

func fakeAGY(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '1.2.4'; exit 0; fi
printf '%s\n' '{"event":"init","conversation_id":"conv-123","init":{"cwd":"/tmp"}}'
printf '%s\n' '{"event":"step_update","step_update":{"step_type":"agent_response","state":"DONE","text_delta":"answer"}}'
printf '%s\n' '{"event":"result","result":{"conversation_id":"conv-123","status":"SUCCESS","response":"answer","usage":{"input_tokens":11,"output_tokens":4,"thinking_tokens":2,"cache_read_tokens":3,"total_tokens":17}}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartNormalizesAGYStream(t *testing.T) {
	run, err := agy.New(fakeAGY(t)).Start(context.Background(), agents.Request{Prompt: "work", Workspace: t.TempDir(), Sandbox: agents.SandboxReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events() {
	}
	result := run.Wait()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.SessionID != "conv-123" || result.Response != "answer" || result.Usage.TotalTokens != 17 {
		t.Fatalf("result = %+v", result)
	}
}

func TestResumeRequiresSession(t *testing.T) {
	_, err := agy.New(fakeAGY(t)).Resume(context.Background(), "", agents.Request{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestStartUsesExplicitAcceptEditsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
case " $* " in
  *" --mode accept-edits "*) ;;
  *) exit 9 ;;
esac
printf '%s\n' '{"event":"result","result":{"conversation_id":"conv-write","status":"SUCCESS","response":"edited"}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	run, err := agy.New(path).Start(context.Background(), agents.Request{Prompt: "edit", Workspace: t.TempDir(), Sandbox: agents.SandboxWorkspaceWrite})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events() {
	}
	if result := run.Wait(); result.Err != nil {
		t.Fatalf("write-mode run failed: %v", result.Err)
	}
}
