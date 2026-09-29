package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
)

func TestColorizeDisabledReturnsPlainText(t *testing.T) {
	if got := colorize(false, ansiRed, "FAILED"); got != "FAILED" {
		t.Fatalf("colorize(false) = %q, want plain text", got)
	}
	if got := colorize(true, ansiRed, ""); got != "" {
		t.Fatalf("colorize(true, empty) = %q, want empty string", got)
	}
}

func TestColorizeEnabledWrapsInSGR(t *testing.T) {
	got := colorize(true, ansiGreen, "COMPLETED")
	if !strings.HasPrefix(got, ansiGreen) || !strings.HasSuffix(got, ansiReset) || !strings.Contains(got, "COMPLETED") {
		t.Fatalf("colorize(true) = %q, want SGR-wrapped text", got)
	}
}

func TestStatusColorSemanticPalette(t *testing.T) {
	cases := map[model.TaskState]string{
		model.TaskCompleted:      ansiGreen,
		model.TaskFailed:         ansiRed,
		model.TaskCancelled:      ansiRed,
		model.TaskWaitingForUser: ansiYellow,
		model.TaskBlocked:        ansiYellow,
		model.TaskRunning:        ansiBlue,
		model.TaskPlanning:       ansiBlue,
		model.TaskCreated:        ansiCyan,
	}
	for state, want := range cases {
		got := statusColor(true, state)
		if !strings.HasPrefix(got, want) {
			t.Fatalf("statusColor(%s) = %q, want prefix %s", state, got, want)
		}
		if !strings.HasSuffix(got, string(state)+ansiReset) {
			t.Fatalf("statusColor(%s) = %q, want state text + reset", state, got)
		}
	}
	if got := statusColor(false, model.TaskFailed); got != "FAILED" {
		t.Fatalf("statusColor(false) = %q, want plain state", got)
	}
}

func TestActorStatusColorSemanticPalette(t *testing.T) {
	cases := map[string]string{
		"error": ansiRed, "failed": ansiRed,
		"waiting": ansiYellow, "rerouting": ansiYellow,
		"running": ansiBlue, "streaming": ansiBlue, "working": ansiBlue, "responding": ansiBlue,
		"connected": ansiGreen,
		"queued":    ansiCyan,
	}
	for status, want := range cases {
		got := actorStatusColor(true, status)
		if !strings.HasPrefix(got, want) {
			t.Fatalf("actorStatusColor(%s) = %q, want prefix %s", status, got, want)
		}
	}
	if got := actorStatusColor(false, "running"); got != "running" {
		t.Fatalf("actorStatusColor(false) = %q, want plain status", got)
	}
}

func TestRunRendererPrintsResultEvents(t *testing.T) {
	var out strings.Builder
	renderer := &runRenderer{
		out:       &out,
		executor:  &runActorView{Role: "executor", Status: "running", Adapter: "freebuff"},
		started:   time.Now(),
		taskState: model.TaskRunning,
	}
	renderer.Event("executor", agents.Event{Kind: agents.EventResult, Type: "result.md", Message: "first line\nfinal answer"})
	if got := out.String(); !strings.Contains(got, "executor final answer") {
		t.Fatalf("output = %q, want final response line", got)
	}
	if renderer.executor.Status != "responding" || renderer.executor.Detail != "final answer" {
		t.Fatalf("executor = %+v, want responding with final answer detail", renderer.executor)
	}
}

func TestRunRendererFinishPrintsFullResult(t *testing.T) {
	var out strings.Builder
	renderer := &runRenderer{
		out:       &out,
		executor:  &runActorView{Role: "executor", Status: "running", Adapter: "freebuff"},
		started:   time.Now(),
		taskState: model.TaskRunning,
	}
	renderer.Event("executor", agents.Event{Kind: agents.EventResult, Type: "result.md", Message: "first line\nfinal answer"})
	renderer.Finish(model.TaskCompleted, "freebuff:/tmp/run")

	got := out.String()
	if !strings.Contains(got, "response\nfirst line\nfinal answer\n\n") {
		t.Fatalf("output = %q, want full response block", got)
	}
	if !strings.Contains(got, "done    COMPLETED (freebuff:/tmp/run)") {
		t.Fatalf("output = %q, want completion line", got)
	}
}

func TestRunRendererFinishPrintsFinalResponseWhenResultEventHasNoMessage(t *testing.T) {
	var out strings.Builder
	renderer := &runRenderer{
		out:       &out,
		executor:  &runActorView{Role: "executor", Status: "running", Adapter: "codex"},
		started:   time.Now(),
		taskState: model.TaskRunning,
	}
	renderer.Event("executor", agents.Event{Kind: agents.EventMessage, Message: "codex final answer"})
	renderer.Event("executor", agents.Event{Kind: agents.EventResult, Type: "turn.completed", Usage: agents.Usage{TotalTokens: 12}})
	renderer.SetResult("executor", "codex final answer")
	renderer.Finish(model.TaskCompleted, "thread-123")

	got := out.String()
	if !strings.Contains(got, "response\ncodex final answer\n\n") {
		t.Fatalf("output = %q, want final response block", got)
	}
	if !strings.Contains(got, "done    COMPLETED (thread-123)") {
		t.Fatalf("output = %q, want completion line", got)
	}
}

func TestActorLineColorMatchesPlainLayout(t *testing.T) {
	actor := &runActorView{Role: "executor", Status: "running", Adapter: "codex", Model: "gpt-5.6"}
	plain := actorLineColor(false, actor)
	if got := actorLine(actor); got != plain {
		t.Fatalf("actorLine = %q, actorLineColor(false) = %q, want identical", got, plain)
	}
	if !strings.HasPrefix(plain, "executor  running") {
		t.Fatalf("plain actor line = %q, want unchanged layout", plain)
	}
	colored := actorLineColor(true, actor)
	if !strings.Contains(colored, ansiBlue) {
		t.Fatalf("colored actor line = %q, want SGR sequence", colored)
	}
	if stripped := stripSGR(colored); stripped != plain {
		t.Fatalf("stripped colored line = %q, want %q", stripped, plain)
	}
}

func TestSupportsColorRespectsEnvironment(t *testing.T) {
	// Pipes, files, and in-memory buffers never receive color regardless of env.
	var buffer strings.Builder
	if supportsColor(&buffer) {
		t.Fatalf("supportsColor(buffer) = true, want false for non-terminal writer")
	}
}

// stripSGR removes ANSI SGR sequences so colored output can be compared with
// the plain form.
func stripSGR(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); {
		if value[i] == 0x1b && i+1 < len(value) && value[i+1] == '[' {
			j := i + 2
			for j < len(value) && !isSGRTerminator(value[j]) {
				j++
			}
			if j < len(value) {
				i = j + 1
				continue
			}
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String()
}

func isSGRTerminator(c byte) bool {
	return c >= 0x40 && c <= 0x7e
}

func TestPanelLinesColoredWhenEnabled(t *testing.T) {
	renderer := &runRenderer{
		out:         os.Stdout,
		interactive: true,
		color:       true,
		taskID:      "task-1",
		taskState:   model.TaskRunning,
		objective:   "fix it",
		started:     time.Now(),
		planner:     &runActorView{Role: "planner", Status: "running", Adapter: "codex"},
		executor:    &runActorView{Role: "executor", Status: "queued", Adapter: "agy"},
	}
	for _, line := range renderer.panelLines() {
		if strings.Contains(line, "RUNNING") && !strings.Contains(line, ansiBlue) {
			t.Fatalf("state line %q missing blue SGR", line)
		}
	}
	stripped := stripSGR(strings.Join(renderer.panelLines(), "\n"))
	if !strings.Contains(stripped, "task-1") || !strings.Contains(stripped, "RUNNING") {
		t.Fatalf("panel content lost after strip: %q", stripped)
	}
}

func TestPanelLinesPlainWhenDisabled(t *testing.T) {
	renderer := &runRenderer{
		out:         os.Stdout,
		interactive: true,
		color:       false,
		taskID:      "task-1",
		taskState:   model.TaskFailed,
		objective:   "fix it",
		started:     time.Now(),
		executor:    &runActorView{Role: "executor", Status: "failed", Adapter: "codex"},
	}
	for _, line := range renderer.panelLines() {
		if strings.ContainsRune(line, 0x1b) {
			t.Fatalf("plain panel line contains escape: %q", line)
		}
	}
}
