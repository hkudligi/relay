package freebuff_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/freebuff"
)

// fakeFreebuff installs a shell script named "freebuff" that mimics the real
// TUI's observable behavior for the adapter contract: it prints an input
// frame (readiness), then — once the pasted pointer names prompt.md — reads
// that file, appends progress lines to status.md, and writes result.md. The
// real TUI streams text ads and spinner output; the fake's extra stdout also
// exercises the PTY drain path.
func fakeFreebuff(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "freebuff")
	script := `#!/bin/sh
# Readiness frame like the real OpenTUI chat screen.
printf '┌─ input ─┐\n│ › _     │\n└─────────┘\n'
# Block until the pasted pointer arrives on stdin (the PTY).
while IFS= read -r line; do
  case "$line" in
    *prompt.md*)
      prompt_file=$(printf '%s' "$line" | grep -o '/[^ ]*prompt\.md')
      ;;
    *)
      continue
      ;;
  esac
  break
done
if [ -z "$prompt_file" ] || [ ! -f "$prompt_file" ]; then
  echo 'fake freebuff: no prompt.md pointer received' >&2
  exit 3
fi
channel_dir=$(dirname "$prompt_file")
status_file="$channel_dir/status.md"
result_file="$channel_dir/result.md"
{
  echo '# progress'
  echo 'reading task brief'
  echo 'working on the objective'
} >> "$status_file"
{
  echo '## result'
  echo 'answer-from-prompt-md'
} > "$result_file"
printf 'done\n'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartRunsTaskThroughFileChannel(t *testing.T) {
	workspace := t.TempDir()
	run, err := freebuff.New(fakeFreebuff(t)).Start(context.Background(), agents.Request{
		Prompt:    "do the work\n\n<rly-memory>{}</rly-memory>",
		Workspace: workspace,
		Sandbox:   agents.SandboxWorkspaceWrite,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events() {
	}
	result := run.Wait()
	if result.Err != nil {
		t.Fatalf("result = %+v", result)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if !strings.Contains(result.Response, "answer-from-prompt-md") {
		t.Fatalf("response = %q, want content from result.md", result.Response)
	}
	if !strings.HasPrefix(result.SessionID, "freebuff:") {
		t.Fatalf("session id = %q", result.SessionID)
	}
	// The prompt brief must carry the objective, protocol, and memory block.
	promptPath := strings.TrimPrefix(result.SessionID, "freebuff:") + "/prompt.md"
	brief, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(brief), "do the work") || !strings.Contains(string(brief), "Reporting protocol") || !strings.Contains(string(brief), "<rly-memory>") {
		t.Fatalf("brief = %q", string(brief))
	}
}

func TestStartStreamsStatusProgress(t *testing.T) {
	workspace := t.TempDir()
	run, err := freebuff.New(fakeFreebuff(t)).Start(context.Background(), agents.Request{
		Prompt:    "stream me",
		Workspace: workspace,
		Sandbox:   agents.SandboxReadOnly,
	})
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	sawResult := false
	for event := range run.Events() {
		switch event.Kind {
		case agents.EventProgress:
			if event.Type != "status.md" {
				t.Fatalf("progress type = %q, want status.md", event.Type)
			}
			messages = append(messages, event.Message)
		case agents.EventResult:
			if event.Type != "result.md" || !strings.Contains(event.Message, "answer-from-prompt-md") {
				t.Fatalf("result event = %+v, want response from result.md", event)
			}
			sawResult = true
		}
	}
	run.Wait()
	if !sawResult {
		t.Fatal("expected a result event from result.md")
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "reading task brief") || !strings.Contains(joined, "working on the objective") {
		t.Fatalf("status messages = %q", joined)
	}
}

func TestPruneStaleRunDirectoriesRemovesOnlyOldRunDirs(t *testing.T) {
	workspace := t.TempDir()
	root := filepath.Join(workspace, ".rly", "freebuff")
	stale := filepath.Join(root, "run-stale")
	fresh := filepath.Join(root, "run-fresh")
	other := filepath.Join(root, "not-a-run")
	for _, dir := range []string{stale, fresh, other} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stale, "result.md"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "prompt.md"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "keep.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldOther := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(other, oldOther, oldOther); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	pruned, err := freebuff.PruneStaleRunDirectories(workspace, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale run dir still present: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh run dir must survive: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non run-* directories must never be pruned: %v", err)
	}
}

func TestPruneStaleRunDirectoriesHandlesMissingRoot(t *testing.T) {
	pruned, err := freebuff.PruneStaleRunDirectories(filepath.Join(t.TempDir(), "missing"), time.Now())
	if err != nil || pruned != 0 {
		t.Fatalf("pruned = %d, err = %v, want 0, nil", pruned, err)
	}
}

func TestDetectReportsMissingBinary(t *testing.T) {
	installation := freebuff.New(filepath.Join(t.TempDir(), "nope")).Detect(context.Background())
	if installation.Available {
		t.Fatal("expected unavailable installation")
	}
	if installation.Error == "" {
		t.Fatal("expected detection error detail")
	}
}

func TestResumeIsUnsupported(t *testing.T) {
	adapter := freebuff.New(fakeFreebuff(t))
	if _, err := adapter.Resume(context.Background(), "", agents.Request{}); err == nil {
		t.Fatal("expected error for empty session")
	}
	if _, err := adapter.Resume(context.Background(), "sess-1", agents.Request{}); err == nil || !strings.Contains(err.Error(), "cannot be resumed") {
		t.Fatalf("resume err = %v", err)
	}
}

func TestDiscoverModelsReportsCatalogWithUnknownQuota(t *testing.T) {
	// Point discovery at a stub so no real CLI is needed. Model identity comes
	// from Freebuff's published catalog while quota remains UNKNOWN.
	dir := t.TempDir()
	stub := filepath.Join(dir, "freebuff")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho 0.0.183\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	rows := freebuff.New("").DiscoverModels(context.Background())
	if len(rows) < 2 {
		t.Fatalf("rows = %+v, want the published model catalog", rows)
	}
	for _, row := range rows {
		if !row.Installed || !row.Usable || row.Model == "" || row.Model == "UNKNOWN" {
			t.Fatalf("row = %+v, want a usable named model", row)
		}
		if row.RemainingPercent != nil || row.Confidence != agents.ConfidenceUnknown {
			t.Fatalf("row = %+v, want UNKNOWN quota", row)
		}
	}
}

func TestCancelTerminatesRun(t *testing.T) {
	workspace := t.TempDir()
	dir := t.TempDir()
	stub := filepath.Join(dir, "freebuff")
	// A TUI that never writes result.md; cancellation must end the run.
	script := "#!/bin/sh\nprintf '›\\n'; sleep 30\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run, err := freebuff.New("").Start(context.Background(), agents.Request{Prompt: "hang", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = run.Cancel()
	}()
	result := run.Wait()
	if result.Err == nil {
		t.Fatal("expected cancellation error, got success")
	}
}

func TestSessionEndedScreenStopsRunForFallback(t *testing.T) {
	workspace := t.TempDir()
	dir := t.TempDir()
	stub := filepath.Join(dir, "freebuff")
	script := `#!/bin/sh
printf '› _\n'
while IFS= read -r line; do
  case "$line" in
    *prompt.md*) break ;;
  esac
done
printf 'Session ended\nPress Enter to continue in a new session\n'
sleep 30
`
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run, err := freebuff.New("").Start(context.Background(), agents.Request{Prompt: "session ends", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for range run.Events() {
	}
	result := run.Wait()
	if result.Err == nil || !strings.Contains(result.Err.Error(), "session ended") {
		t.Fatalf("result = %+v, want session-ended failure", result)
	}
}
