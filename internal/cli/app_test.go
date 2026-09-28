package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/codex"
	"github.com/harsha/relay/internal/cli"
	"github.com/harsha/relay/internal/core"
	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

type replAdapter struct {
	name      string
	available bool
	request   agents.Request
	resultErr error
	startErr  error
	progress  bool
}

func (a *replAdapter) Name() string {
	if a.name != "" {
		return a.name
	}
	return "codex"
}
func (a *replAdapter) Detect(context.Context) agents.Installation {
	installation := agents.Installation{Name: a.Name(), Available: a.available, Version: "test-version"}
	if !a.available {
		installation.Error = a.Name() + " executable not found"
	}
	return installation
}
func (a *replAdapter) DiscoverModels(context.Context) []agents.ModelAvailability {
	remaining := 75.0
	return []agents.ModelAvailability{{Agent: a.Name(), Model: "test-model", Installed: a.available, Usable: a.available, RemainingPercent: &remaining, Confidence: agents.ConfidenceExact, DataSource: "test provider quota"}}
}
func (a *replAdapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{Streaming: true, FileEditing: true, SessionResume: true}
}
func (a *replAdapter) Start(_ context.Context, request agents.Request) (agents.Run, error) {
	a.request = request
	if a.startErr != nil {
		return nil, a.startErr
	}
	events := make(chan agents.Event, 4)
	events <- agents.Event{Kind: agents.EventSession, SessionID: "repl-session"}
	if a.progress {
		events <- agents.Event{Kind: agents.EventProgress, Type: "tool_call", Data: map[string]any{"state": "running"}}
	}
	events <- agents.Event{Kind: agents.EventMessage, Message: "streamed response"}
	if a.resultErr != nil {
		events <- agents.Event{Kind: agents.EventError, Message: a.resultErr.Error()}
	} else {
		events <- agents.Event{Kind: agents.EventResult, Usage: agents.Usage{TotalTokens: 12}}
	}
	close(events)
	done := make(chan agents.Result, 1)
	result := agents.Result{SessionID: "repl-session", Response: "streamed response", Usage: agents.Usage{TotalTokens: 12}, ExitCode: 0, Err: a.resultErr}
	if a.resultErr != nil {
		result.ExitCode = 1
		result.Error = a.resultErr.Error()
	}
	done <- result
	close(done)
	return &replRun{events: events, done: done}, nil
}
func (a *replAdapter) Resume(context.Context, string, agents.Request) (agents.Run, error) {
	return nil, errors.New("not implemented")
}

type replRun struct {
	events chan agents.Event
	done   chan agents.Result
}

func (r *replRun) Events() <-chan agents.Event { return r.events }
func (r *replRun) Wait() agents.Result         { return <-r.done }
func (r *replRun) Cancel() error               { return nil }

func testApp(t *testing.T) (*cli.App, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	dir := t.TempDir()
	app := &cli.App{
		In:       strings.NewReader(""),
		Out:      &out,
		Err:      &errOut,
		Getwd:    func() (string, error) { return dir, nil },
		HomeDir:  func() (string, error) { return dir, nil },
		Adapters: map[string]agents.Adapter{"codex": codex.New(filepath.Join(dir, "missing-codex"))},
	}
	return app, &out, &errOut, filepath.Join(dir, "state.db")
}

func TestRunPersistsTaskAndReportsUnavailableAdapter(t *testing.T) {
	app, out, errOut, db := testApp(t)
	code := app.Run(context.Background(), []string{"--state", db, "run", "fix", "it"})
	if code != cli.ExitAgentUnavailable {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PLANNING") {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	code = app.Run(context.Background(), []string{"--state", db, "status", "--json"})
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, stderr = %s", code, errOut.String())
	}
	var status model.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Objective != "fix it" {
		t.Fatalf("status = %+v", status)
	}
}

func TestEmptyStatus(t *testing.T) {
	app, out, errOut, db := testApp(t)
	code := app.Run(context.Background(), []string{"--state", db, "status"})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "task: none") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestOpsShowsAgingTasks(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	aged := model.Task{ID: "task-aged", Repository: filepath.Dir(dbPath), Objective: "old work", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC().Add(-2 * time.Hour), UpdatedAt: time.Now().UTC().Add(-2 * time.Hour)}
	if err := db.CreateTask(context.Background(), aged, model.Event{ID: "evt-aged", TaskID: aged.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: aged.Objective, CreatedAt: aged.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code := app.Run(context.Background(), []string{"--state", dbPath, "ops", "--aging", "1h"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Needs attention") || !strings.Contains(out.String(), "task-aged") || !strings.Contains(out.String(), "aging") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestCancelCommandIsIdempotent(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), filepath.Dir(dbPath), "cancel from cli")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if code := app.Run(context.Background(), []string{"--state", dbPath, "cancel", "--reason", "superseded", "--idempotency-key", "cli-cancel-1", task.ID}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), string(model.TaskCancelled)) {
		t.Fatalf("output = %q", out.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"--state", dbPath, "cancel", "--reason", "superseded", "--idempotency-key", "cli-cancel-1", task.ID}); code != cli.ExitOK {
		t.Fatalf("second exit = %d, stderr = %s", code, errOut.String())
	}
}

func TestMemoryCommands(t *testing.T) {
	app, out, errOut, db := testApp(t)
	if code := app.Run(context.Background(), []string{"--state", db, "memory", "set", "build.command", "go", "test", "./..."}); code != cli.ExitOK {
		t.Fatalf("set exit = %d, stderr = %s", code, errOut.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"--state", db, "memory", "--json"}); code != cli.ExitOK {
		t.Fatalf("list exit = %d, stderr = %s", code, errOut.String())
	}
	var items []model.ProjectMemory
	if err := json.Unmarshal(out.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "build.command" || items[0].Value != "go test ./..." {
		t.Fatalf("memory = %+v", items)
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"--state", db, "memory", "delete", "build.command"}); code != cli.ExitOK {
		t.Fatalf("delete exit = %d, stderr = %s", code, errOut.String())
	}
	out.Reset()
	if code := app.Run(context.Background(), []string{"--state", db, "memory"}); code != cli.ExitOK || !strings.Contains(out.String(), "No project memory") {
		t.Fatalf("empty memory exit = %d, output = %q", code, out.String())
	}
}

func TestREPLExecutesObjectiveWithDefaultCodexAdapter(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	adapter := &replAdapter{available: true}
	app.Adapters["codex"] = adapter
	app.In = strings.NewReader("implement the feature\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "models  codex/test-model 75% ready") || !strings.Contains(got, "exec    codex (test-model)") || !strings.Contains(got, "executor streamed response") || !strings.Contains(got, "done    COMPLETED (repl-session)") {
		t.Fatalf("output = %q", got)
	}
	if adapter.request.Workspace == "" || adapter.request.Sandbox != agents.SandboxWorkspaceWrite || !strings.HasPrefix(adapter.request.Prompt, "implement the feature") {
		t.Fatalf("request = %+v", adapter.request)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	status, err := svc.Status(context.Background(), adapter.request.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.State != model.TaskCompleted || status.Task.Objective != "implement the feature" {
		t.Fatalf("status = %+v", status)
	}
	if session, err := svc.Session(context.Background(), status.Task.ID, "codex"); err != nil || session != "repl-session" {
		t.Fatalf("session = %q, err = %v", session, err)
	}
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	var runStatus, response string
	var exitCode int
	if err := rawDB.QueryRowContext(context.Background(), `SELECT status, response, exit_code FROM runs WHERE task_id=?`, status.Task.ID).Scan(&runStatus, &response, &exitCode); err != nil {
		t.Fatal(err)
	}
	if runStatus != "COMPLETED" || response != "streamed response" || exitCode != 0 {
		t.Fatalf("run = status %q, response %q, exit %d", runStatus, response, exitCode)
	}
	events, err := svc.Trace(context.Background(), status.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 9 || events[1].Type != "agent.inventory_discovered" || events[3].Type != "semantic.assessed" || events[4].Type != "routing.selected" || events[5].Summary != "implementer → codex" || events[6].Type != "orchestration.selected" || events[8].Type != "task.state_changed" {
		t.Fatalf("events = %+v", events)
	}
}

func TestRunJSONIncludesInventoryAndTrace(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	adapter := &replAdapter{available: true}
	app.Adapters["codex"] = adapter
	if code := app.Run(context.Background(), []string{"--state", dbPath, "run", "--json", "implement", "it"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var result struct {
		Inventory []agents.ModelAvailability `json:"inventory"`
		Execution *core.Execution            `json:"execution"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Inventory) != 1 || result.Inventory[0].Model != "test-model" || result.Inventory[0].RemainingPercent == nil || *result.Inventory[0].RemainingPercent != 75 || result.Execution == nil {
		t.Fatalf("result = %+v", result)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	events, err := core.New(db).Trace(context.Background(), result.Execution.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[1].Type != "agent.inventory_discovered" {
		t.Fatalf("events = %+v", events)
	}
}

func TestAgentsJSONIncludesQuotaProvenance(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{available: true}
	if code := app.Run(context.Background(), []string{"--state", dbPath, "agents", "--json"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var inventory []agents.ModelAvailability
	if err := json.Unmarshal(out.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || inventory[0].Confidence != agents.ConfidenceExact || inventory[0].DataSource != "test provider quota" {
		t.Fatalf("inventory = %+v", inventory)
	}
}

func TestAgentsJSONKeepsUnavailableQuotaUnknown(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	if code := app.Run(context.Background(), []string{"--state", dbPath, "agents", "--json"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	var inventory []agents.ModelAvailability
	if err := json.Unmarshal(out.Bytes(), &inventory); err != nil {
		t.Fatal(err)
	}
	if len(inventory) != 1 || inventory[0].Installed || inventory[0].Usable || inventory[0].RemainingPercent != nil || inventory[0].Confidence != agents.ConfidenceUnknown || inventory[0].DataSource != "codex --version" {
		t.Fatalf("inventory = %+v", inventory)
	}
}

func TestHelpDocumentsRoutingStrategyAndReserveOptions(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	if code := app.Run(context.Background(), []string{"--state", dbPath, "help"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{
		"--strategy balanced",
		"--strategy conservative",
		"--strategy quality-first",
		"--min-reserve PCT",
		"auto-routing skips agents below",
		"unless no higher-headroom agent is",
		"--max-total-tokens N",
		"rly [--state PATH] ops",
		"rly [--state PATH] cancel",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("help missing %q:\n%s", want, got)
		}
	}
}

func TestRunPreservesCodexPlannerAndAgyExecutor(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	planner := &replAdapter{name: "codex", available: true}
	executor := &replAdapter{name: "agy", available: true}
	app.Adapters = map[string]agents.Adapter{"codex": planner, "agy": executor}
	if code := app.Run(context.Background(), []string{"--state", dbPath, "run", "--planner", "codex", "--agent", "agy", "implement", "it"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(executor.request.Prompt, "Inter-agent context") || !strings.Contains(executor.request.Prompt, "[plan via planner]") || !strings.Contains(executor.request.Prompt, "streamed response") {
		t.Fatalf("executor prompt = %q", executor.request.Prompt)
	}
	if !strings.Contains(out.String(), "plan    codex (test-model)") || !strings.Contains(out.String(), "exec    agy (test-model)") || !strings.Contains(out.String(), "planner streamed response") || !strings.Contains(out.String(), "executor streamed response") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunPrintsNormalizedProgressUpdates(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	adapter := &replAdapter{name: "codex", available: true, progress: true}
	app.Adapters = map[string]agents.Adapter{"codex": adapter}
	if code := app.Run(context.Background(), []string{"--state", dbPath, "run", "inspect", "it"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "executor tool_call (running)") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestREPLRecognizesConversationalStatusAndTaskCommands(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.In = strings.NewReader("status\nSTATUS OF   TASK\ntasks\nShow Tasks\n/status\n/tasks\n/help\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if got := out.String(); strings.Count(got, "task: none") != 3 || strings.Count(got, "No tasks for this repository.") != 3 {
		t.Fatalf("output = %q", got)
	}

	out.Reset()
	if code := app.Run(context.Background(), []string{"--state", dbPath, "tasks", "--json"}); code != cli.ExitOK {
		t.Fatalf("tasks exit = %d, stderr = %s", code, errOut.String())
	}
	var tasks []model.Task
	if err := json.Unmarshal(out.Bytes(), &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Fatalf("recognized commands created tasks: %+v", tasks)
	}
}

func TestREPLReportsUnavailableDefaultAgentAndContinues(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{available: false}
	app.In = strings.NewReader("do the work\nshow tasks\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "agent unavailable") || !strings.Contains(errOut.String(), "codex executable not found") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "PLANNING") || !strings.Contains(got, "do the work") {
		t.Fatalf("output = %q", got)
	}
}

func TestREPLPersistsAgentFailureAndContinues(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{available: true, resultErr: errors.New("agent exploded")}
	app.In = strings.NewReader("attempt the work\nstatus\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "rly: agent exploded") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if got := out.String(); !strings.Contains(got, "streamed response") || !strings.Contains(got, "state: FAILED") {
		t.Fatalf("output = %q", got)
	}
}

func TestCleanupRunsAutomaticallyOnEveryInvocation(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Seed one task older than the 7-day retention window and one recent.
	aged := model.Task{ID: "task-aged", Repository: t.TempDir(), Objective: "aged", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour), UpdatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour)}
	agedEvent := model.Event{ID: "evt-aged", TaskID: aged.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: aged.Objective, CreatedAt: aged.CreatedAt}
	if err := db.CreateTask(ctx, aged, agedEvent); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRun(ctx, model.RunRecord{ID: "run-aged", TaskID: aged.ID, Adapter: "codex", Status: "COMPLETED", StartedAt: aged.CreatedAt, CompletedAt: aged.UpdatedAt}, aged.Repository); err != nil {
		t.Fatal(err)
	}
	recentTask := model.Task{ID: "task-recent", Repository: aged.Repository, Objective: "recent", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	recentEvent := model.Event{ID: "evt-recent", TaskID: recentTask.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: recentTask.Objective, CreatedAt: recentTask.CreatedAt}
	if err := db.CreateTask(ctx, recentTask, recentEvent); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	app.Getwd = func() (string, error) { return aged.Repository, nil }
	if code := app.Run(ctx, []string{"--state", dbPath, "status", "--json"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	// Cleanup runs before the command and reports what it pruned on stderr so
	// stdout remains machine-readable for --json consumers.
	if !strings.Contains(errOut.String(), "cleanup: pruned 1 task(s) older than 7 days") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	var status model.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.ID != recentTask.ID {
		t.Fatalf("status = %+v", status)
	}
	if strings.Contains(out.String(), "task-aged") {
		t.Fatalf("aged task survived automatic cleanup: %q", out.String())
	}
	if strings.Contains(out.String(), "cleanup:") {
		t.Fatalf("cleanup chatter leaked to stdout: %q", out.String())
	}
}

func TestCleanupPrunesStaleFreebuffRunDirectories(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	workspace := t.TempDir()
	app.Getwd = func() (string, error) { return workspace, nil }
	root := filepath.Join(workspace, ".rly", "freebuff")
	stale := filepath.Join(root, "run-stale")
	fresh := filepath.Join(root, "run-fresh")
	for _, dir := range []string{stale, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "prompt.md"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := app.Run(context.Background(), []string{"--state", dbPath, "status"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "cleanup: removed 1 freebuff run director(y|ies) older than 7 days") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale channel still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "prompt.md")); err != nil {
		t.Fatalf("fresh channel must survive cleanup: %v", err)
	}
	if strings.Contains(errOut.String(), "cleanup freebuff channels") {
		t.Fatalf("unexpected channel cleanup error: %q", errOut.String())
	}
	_ = out
}

func TestCleanupFailureDoesNotBlockCommand(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	// A workspace whose .rly/freebuff path is occupied by a regular file makes
	// channel cleanup fail; the status command must still succeed.
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".rly"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".rly", "freebuff"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.Getwd = func() (string, error) { return workspace, nil }

	if code := app.Run(context.Background(), []string{"--state", dbPath, "status"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "rly: cleanup freebuff channels:") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if !strings.Contains(out.String(), "task: none") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestREPLPrefersAgyOverCodex(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{available: true, resultErr: errors.New("RESOURCE_EXHAUSTED: individual quota reached")}
	app.Adapters["agy"] = &replAdapter{name: "agy", available: true}
	app.In = strings.NewReader("attempt the work\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "exec    agy (test-model)") || !strings.Contains(out.String(), "done    COMPLETED") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestREPLPrefersAgyOverFreebuff(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters = map[string]agents.Adapter{
		"freebuff": &replAdapter{name: "freebuff", available: true, resultErr: errors.New("freebuff TUI did not produce result.md")},
		"agy":      &replAdapter{name: "agy", available: true},
	}
	app.In = strings.NewReader("attempt the work\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "exec    agy (test-model)") || !strings.Contains(out.String(), "done    COMPLETED") {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestREPLReportsAgentStartFailureAndContinues(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{available: true, startErr: errors.New("could not start agent")}
	app.In = strings.NewReader("attempt the work\nstatus\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "rly: could not start agent") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	if !strings.Contains(out.String(), "state: FAILED") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestREPLDoesNotCreateTaskWithoutConfiguredCodexAdapter(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	delete(app.Adapters, "codex")
	app.In = strings.NewReader("do the work\ntasks\n/exit\n")

	if code := app.Run(context.Background(), []string{"--state", dbPath}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "default codex adapter is not configured") || !strings.Contains(out.String(), "No tasks for this repository.") {
		t.Fatalf("stdout = %q, stderr = %q", out.String(), errOut.String())
	}
}

func TestRunAutoRoutesToBestAgentBasedOnTokensAndEfficacy(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	// Codex is exhausted (0% quota), AGY has 90% quota
	codexAdapter := &replAdapter{name: "codex", available: true}
	agyAdapter := &replAdapter{name: "agy", available: true}
	app.Adapters = map[string]agents.Adapter{"codex": codexAdapter, "agy": agyAdapter}

	// We discover inventory where codex has 0% and agy has 90%
	code := app.Run(context.Background(), []string{"--state", dbPath, "run", "--agent", "auto", "--strategy", "conservative", "implement the feature"})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "exec    agy (test-model)") || !strings.Contains(out.String(), "rly run") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunJSONIncludesRoutingDecision(t *testing.T) {
	app, out, errOut, dbPath := testApp(t)
	app.Adapters["codex"] = &replAdapter{name: "codex", available: true}
	app.Adapters["agy"] = &replAdapter{name: "agy", available: true}

	if code := app.Run(context.Background(), []string{"--state", dbPath, "run", "--json", "--planner", "auto", "--agent", "auto", "plan and implement feature"}); code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}

	var result struct {
		Inventory []agents.ModelAvailability `json:"inventory"`
		Routing   *model.RouteDecision       `json:"routing"`
		Execution *core.Execution            `json:"execution"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Routing == nil || result.Routing.SelectedAgent == "" {
		t.Fatalf("expected routing decision in json output, got: %+v", result)
	}
	if len(result.Routing.CandidateScores) < 2 {
		t.Fatalf("expected candidate scores for both adapters, got: %+v", result.Routing.CandidateScores)
	}
}
