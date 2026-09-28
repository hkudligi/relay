package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/core"
	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

type fakeAdapter struct{}

func (fakeAdapter) Name() string { return "fake" }
func (fakeAdapter) Detect(context.Context) agents.Installation {
	return agents.Installation{Name: "fake", Available: true, Version: "1.0"}
}
func (fakeAdapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{NonInteractive: true, SessionResume: true}
}
func (fakeAdapter) Start(context.Context, agents.Request) (agents.Run, error) {
	events := make(chan agents.Event, 2)
	events <- agents.Event{Kind: agents.EventSession, SessionID: "session-1"}
	events <- agents.Event{Kind: agents.EventResult, Message: "finished", Usage: agents.Usage{TotalTokens: 42}}
	close(events)
	done := make(chan agents.Result, 1)
	done <- agents.Result{SessionID: "session-1", Response: "finished", Usage: agents.Usage{TotalTokens: 42}, ExitCode: 0}
	close(done)
	return &fakeRun{events: events, done: done}, nil
}
func (fakeAdapter) Resume(context.Context, string, agents.Request) (agents.Run, error) {
	return nil, nil
}

type fakeRun struct {
	events chan agents.Event
	done   chan agents.Result
}

func (r *fakeRun) Events() <-chan agents.Event { return r.events }
func (r *fakeRun) Wait() agents.Result         { return <-r.done }
func (r *fakeRun) Cancel() error               { return nil }

type memoryAdapter struct {
	prompt   string
	response string
	err      error
}

func (a *memoryAdapter) Name() string { return "memory-fake" }
func (a *memoryAdapter) Detect(context.Context) agents.Installation {
	return agents.Installation{Name: a.Name(), Available: true, Version: "1.0"}
}
func (a *memoryAdapter) Capabilities() agents.Capabilities { return agents.Capabilities{} }
func (a *memoryAdapter) Start(_ context.Context, request agents.Request) (agents.Run, error) {
	a.prompt = request.Prompt
	events := make(chan agents.Event)
	close(events)
	done := make(chan agents.Result, 1)
	result := agents.Result{Response: a.response, ExitCode: 0, Err: a.err}
	if a.err != nil {
		result.ExitCode = 1
		result.Error = a.err.Error()
	}
	done <- result
	close(done)
	return &fakeRun{events: events, done: done}, nil
}
func (a *memoryAdapter) Resume(context.Context, string, agents.Request) (agents.Run, error) {
	return nil, nil
}

func TestStartTaskPersistsLifecycle(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "fix it")
	if err != nil {
		t.Fatal(err)
	}
	if task.State != model.TaskPlanning {
		t.Fatalf("state = %s", task.State)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("unexpected sequences: %+v", events)
	}
}

func TestTasksShareDurableProjectHistory(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	repo := t.TempDir()
	first, err := svc.StartTask(context.Background(), repo, "first attempt")
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.StartTask(context.Background(), repo, "retry attempt")
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectID == "" || second.ProjectID != first.ProjectID {
		t.Fatalf("project IDs = %q and %q, want one durable project", first.ProjectID, second.ProjectID)
	}
	project, tasks, err := svc.Project(context.Background(), first.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if project.TaskCount != 2 || len(tasks) != 2 {
		t.Fatalf("project history = project=%+v tasks=%d, want two tasks", project, len(tasks))
	}
}

func TestStartTaskWithInventoryRecordsDiscoveryBeforePlanning(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	remaining := 42.0
	task, err := svc.StartTaskWithInventory(context.Background(), "/repo", "fix it", []agents.ModelAvailability{{Agent: "codex", Model: "gpt-test", Installed: true, Usable: true, RemainingPercent: &remaining, Confidence: agents.ConfidenceExact, DataSource: "provider"}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Type != "task.created" || events[1].Type != "agent.inventory_discovered" || events[2].Type != "task.state_changed" {
		t.Fatalf("events = %+v", events)
	}
	rows, ok := events[1].Data["inventory"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("inventory event data = %#v", events[1].Data)
	}
}

func TestStartTaskInitializesArtifactWorkspace(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	task, err := core.New(db).StartTask(context.Background(), repo, "fix the resumable workflow")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(repo, ".relay", "tasks", task.ID)
	for _, name := range []string{
		"objective.md",
		"plan.md",
		"state.json",
		"decisions.md",
		"findings.md",
		"progress.md",
		"questions.md",
		"evidence",
		"handoffs",
	} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		SchemaVersion int             `json:"schema_version"`
		TaskID        string          `json:"task_id"`
		Objective     string          `json:"objective"`
		Repository    string          `json:"repository"`
		Phase         model.TaskState `json:"phase"`
		Status        model.TaskState `json:"status"`
		Orchestration struct {
			Backend string `json:"backend"`
			Durable bool   `json:"durable"`
		} `json:"orchestration"`
		CompletionGates []struct {
			ID        string `json:"id"`
			Required  bool   `json:"required"`
			Satisfied bool   `json:"satisfied"`
		} `json:"completion_gates"`
		RetryLimits map[string]struct {
			MaxAttempts int `json:"max_attempts"`
			Attempts    int `json:"attempts"`
		} `json:"retry_limits"`
		AgentResults []struct{} `json:"agent_results"`
		Operations   struct {
			AccessControls struct {
				AllowedCancellationActors []string `json:"allowed_cancellation_actors"`
			} `json:"access_controls"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.TaskID != task.ID || state.Objective != task.Objective || state.Repository != repo || state.Phase != model.TaskPlanning || state.Status != model.TaskPlanning {
		t.Fatalf("state artifact = %+v, task = %+v", state, task)
	}
	if state.SchemaVersion != 3 {
		t.Fatalf("schema_version = %d, want 3", state.SchemaVersion)
	}
	if state.Orchestration.Backend != "local" || state.Orchestration.Durable {
		t.Fatalf("orchestration = %+v", state.Orchestration)
	}
	if len(state.CompletionGates) < 4 || state.CompletionGates[0].ID != "objective_recorded" || !state.CompletionGates[0].Satisfied {
		t.Fatalf("completion gates = %+v", state.CompletionGates)
	}
	if state.RetryLimits["implementer"].MaxAttempts != 3 || state.RetryLimits["implementer"].Attempts != 0 {
		t.Fatalf("retry limits = %+v", state.RetryLimits)
	}
	if len(state.AgentResults) != 0 {
		t.Fatalf("agent results should start empty: %+v", state.AgentResults)
	}
	if len(state.Operations.AccessControls.AllowedCancellationActors) == 0 {
		t.Fatalf("operations access controls missing: %+v", state.Operations)
	}
	objective, err := os.ReadFile(filepath.Join(root, "objective.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(objective), "fix the resumable workflow") {
		t.Fatalf("objective artifact = %q", objective)
	}
}

func TestOperationsDashboardFlagsFailedBlockedAndAgingTasks(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := "/repo"
	svc := core.New(db)
	ctx := context.Background()
	if _, err := svc.StartTask(ctx, repo, "fresh work"); err != nil {
		t.Fatal(err)
	}
	aged := model.Task{ID: "task-aged", Repository: repo, Objective: "old planning", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC().Add(-2 * time.Hour), UpdatedAt: time.Now().UTC().Add(-2 * time.Hour)}
	if err := db.CreateTask(ctx, aged, model.Event{ID: "evt-aged", TaskID: aged.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: aged.Objective, CreatedAt: aged.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	failed := model.Task{ID: "task-failed", Repository: repo, Objective: "failed work", State: model.TaskFailed, Version: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := db.CreateTask(ctx, failed, model.Event{ID: "evt-failed", TaskID: failed.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: failed.Objective, CreatedAt: failed.CreatedAt}); err != nil {
		t.Fatal(err)
	}

	dashboard, err := svc.Operations(ctx, repo, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.Counts[model.TaskPlanning] != 2 || dashboard.Counts[model.TaskFailed] != 1 {
		t.Fatalf("counts = %+v", dashboard.Counts)
	}
	reasons := map[string]string{}
	for _, item := range dashboard.Tasks {
		reasons[item.ID] = item.Reason
	}
	if reasons["task-aged"] != "aging" || reasons["task-failed"] != "failed" {
		t.Fatalf("dashboard tasks = %+v", dashboard.Tasks)
	}
}

func TestCancelTaskIsIdempotentAndRecordsArtifact(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), repo, "cancel me")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := svc.CancelTask(context.Background(), task.ID, "user", "no longer needed", "cancel-1")
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != model.TaskCancelled {
		t.Fatalf("state = %s", cancelled.State)
	}
	if _, err := svc.CancelTask(context.Background(), task.ID, "user", "no longer needed", "cancel-1"); err != nil {
		t.Fatal(err)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cancelEvents int
	for _, event := range events {
		if event.Data["idempotency_key"] == "cancel-1" {
			cancelEvents++
		}
	}
	if cancelEvents != 1 {
		t.Fatalf("cancel events = %d, events = %+v", cancelEvents, events)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".relay", "tasks", task.ID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Status     model.TaskState `json:"status"`
		Operations struct {
			Cancellation struct {
				Requested      bool   `json:"requested"`
				RequestedBy    string `json:"requested_by"`
				Reason         string `json:"reason"`
				IdempotencyKey string `json:"idempotency_key"`
			} `json:"cancellation"`
			Idempotency []struct {
				Operation string `json:"operation"`
				Key       string `json:"key"`
			} `json:"idempotency"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != model.TaskCancelled || !state.Operations.Cancellation.Requested || state.Operations.Cancellation.IdempotencyKey != "cancel-1" || len(state.Operations.Idempotency) != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestCancelTaskRejectsUnauthorizedActor(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "cancel me")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelTask(context.Background(), task.ID, "adapter", "", ""); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatalf("err = %v, want ErrAccessDenied", err)
	}
}

func TestExecuteHonorsMaxTotalTokens(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), repo, strings.Repeat("large objective ", 20))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(context.Background(), task, fakeAdapter{}, agents.Request{MaxTotalTokens: 1}, nil); !errors.Is(err, core.ErrCostLimitExceeded) {
		t.Fatalf("err = %v, want ErrCostLimitExceeded", err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".relay", "tasks", task.ID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Status     model.TaskState `json:"status"`
		Operations struct {
			CostLimits struct {
				MaxTotalTokens int64 `json:"max_total_tokens"`
			} `json:"cost_limits"`
		} `json:"operations"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != model.TaskFailed || state.Operations.CostLimits.MaxTotalTokens != 1 {
		t.Fatalf("state = %+v", state)
	}
}

func TestExecuteUpdatesStageTwoArtifactState(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), repo, "implement it")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Execute(context.Background(), task, fakeAdapter{}, agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".relay", "tasks", task.ID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Phase                    model.TaskState `json:"phase"`
		Status                   model.TaskState `json:"status"`
		LastSuccessfulCheckpoint string          `json:"last_successful_checkpoint"`
		NextRecommendedAction    string          `json:"next_recommended_action"`
		CompletionGates          []struct {
			ID        string `json:"id"`
			Satisfied bool   `json:"satisfied"`
			Evidence  string `json:"evidence"`
		} `json:"completion_gates"`
		AgentResults []struct {
			Role      string       `json:"role"`
			Adapter   string       `json:"adapter"`
			Status    string       `json:"status"`
			SessionID string       `json:"session_id"`
			ExitCode  int          `json:"exit_code"`
			Usage     agents.Usage `json:"usage"`
			Summary   string       `json:"summary"`
		} `json:"agent_results"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Phase != model.TaskCompleted || state.Status != model.TaskCompleted || state.LastSuccessfulCheckpoint != "completed" {
		t.Fatalf("state = %+v", state)
	}
	if state.NextRecommendedAction == "" {
		t.Fatal("next recommended action was not recorded")
	}
	gates := map[string]bool{}
	for _, gate := range state.CompletionGates {
		gates[gate.ID] = gate.Satisfied
	}
	if !gates["agent_result_recorded"] || !gates["terminal_state_recorded"] {
		t.Fatalf("completion gates = %+v", state.CompletionGates)
	}
	if len(state.AgentResults) != 1 {
		t.Fatalf("agent results = %+v", state.AgentResults)
	}
	result := state.AgentResults[0]
	if result.Role != "implementer" || result.Adapter != "fake" || result.Status != "COMPLETED" || result.SessionID != "session-1" || result.ExitCode != 0 || result.Usage.TotalTokens != 42 || result.Summary != "finished" {
		t.Fatalf("agent result = %+v", result)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type == "orchestration.selected" {
			found = true
			if event.Data["backend"] != "local" || event.Data["durable"] != false {
				t.Fatalf("orchestration event = %+v", event)
			}
		}
	}
	if !found {
		t.Fatalf("missing orchestration.selected event: %+v", events)
	}
}

func TestAssessDurableOrchestrationRequiresTemporalForStageThreeTriggers(t *testing.T) {
	decision := core.AssessDurableOrchestration([]core.AgentTask{
		{ID: "inspect-api", Agent: "fake", ReadOnly: true},
		{ID: "inspect-ui", Agent: "fake", ReadOnly: true},
	}, core.DurableOrchestrationHints{LongWait: true, WaitingForUser: true})
	if decision.Backend != core.OrchestrationBackendTemporal || !decision.Durable {
		t.Fatalf("decision = %+v", decision)
	}
	for _, want := range []string{"parallel branches", "long waits", "human-input signals"} {
		if !containsRequirement(decision.Requirements, want) {
			t.Fatalf("requirements = %+v, want %q", decision.Requirements, want)
		}
	}
}

func containsRequirement(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestFailedExecuteUpdatesRetryAccounting(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := t.TempDir()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), repo, "implement it")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &memoryAdapter{response: "could not finish", err: errors.New("boom")}
	if _, err := svc.Execute(context.Background(), task, adapter, agents.Request{}, nil); err == nil {
		t.Fatal("expected execution error")
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".relay", "tasks", task.ID, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Status      model.TaskState `json:"status"`
		RetryLimits map[string]struct {
			MaxAttempts int `json:"max_attempts"`
			Attempts    int `json:"attempts"`
		} `json:"retry_limits"`
		AgentResults []struct {
			Role   string `json:"role"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"agent_results"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != model.TaskFailed {
		t.Fatalf("status = %s", state.Status)
	}
	if state.RetryLimits["implementer"].Attempts != 1 || state.RetryLimits["implementer"].MaxAttempts != 3 {
		t.Fatalf("retry limits = %+v", state.RetryLimits)
	}
	if len(state.AgentResults) != 1 || state.AgentResults[0].Role != "implementer" || state.AgentResults[0].Status != "FAILED" || state.AgentResults[0].Error != "boom" {
		t.Fatalf("agent results = %+v", state.AgentResults)
	}
}

func TestStatusWithoutTask(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	status, err := core.New(db).Status(context.Background(), "/empty")
	if err != nil {
		t.Fatal(err)
	}
	if status.Task != nil {
		t.Fatalf("unexpected task: %+v", status.Task)
	}
}

func TestExecutePersistsSessionAndCompletesTask(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "implement it")
	if err != nil {
		t.Fatal(err)
	}
	execution, err := svc.Execute(context.Background(), task, fakeAdapter{}, agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Task.State != model.TaskCompleted {
		t.Fatalf("state = %s", execution.Task.State)
	}
	session, err := svc.Session(context.Background(), task.ID, "fake")
	if err != nil {
		t.Fatal(err)
	}
	if session != "session-1" {
		t.Fatalf("session = %q", session)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 || events[3].Type != "orchestration.selected" {
		t.Fatalf("events = %d: %+v", len(events), events)
	}
}

func TestExecuteWithPlanRunsPlannerThenExecutor(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "implement it")
	if err != nil {
		t.Fatal(err)
	}
	planner := &memoryAdapter{response: "1. inspect files\n2. implement change\n<rly-memory>{\"upsert\":[{\"key\":\"fake.key\",\"value\":\"ignored\"}]}</rly-memory>"}
	executor := &memoryAdapter{response: "done\n<rly-memory>{\"upsert\":[{\"key\":\"feature.done\",\"value\":\"true\"}]}</rly-memory>"}
	var planEmits, execEmits []agents.Event
	if _, err := svc.ExecuteWithPlan(context.Background(), task, planner, executor, "", agents.Request{Sandbox: agents.SandboxWorkspaceWrite}, func(e agents.Event) { planEmits = append(planEmits, e) }, func(e agents.Event) { execEmits = append(execEmits, e) }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(planner.prompt, "read-only analysis phase") || !strings.Contains(planner.prompt, "Write the handoff as instructions for the executor") {
		t.Fatalf("planner prompt did not enforce plan-only handoff: %q", planner.prompt)
	}
	if !strings.Contains(executor.prompt, "You are the implementation specialist") || !strings.Contains(executor.prompt, "you are responsible for making the actual repository changes") {
		t.Fatalf("executor prompt did not claim implementation responsibility: %q", executor.prompt)
	}
	if !strings.Contains(executor.prompt, "Inter-agent context") || !strings.Contains(executor.prompt, "[plan via planner]") || !strings.Contains(executor.prompt, "1. inspect files") {
		t.Fatalf("executor prompt = %q", executor.prompt)
	}
	if strings.Contains(executor.prompt, "fake.key") {
		t.Fatalf("executor prompt contained unstripped memory update from planner: %q", executor.prompt)
	}
	if taskState, err := svc.Status(context.Background(), "/repo"); err != nil || taskState.Task.State != model.TaskCompleted {
		t.Fatalf("status = %+v, err = %v", taskState, err)
	}
	mem, err := svc.Memory(context.Background(), "/repo")
	if err != nil || len(mem) != 1 || mem[0].Key != "feature.done" {
		t.Fatalf("memory = %+v, err = %v", mem, err)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 4 || events[2].Type != "planning.started" {
		t.Fatalf("events = %+v", events)
	}
	if events[2].Data["role"] != "planner" {
		t.Fatalf("planning event data = %+v", events[2].Data)
	}
}

func TestProjectMemoryIsLoadedUpdatedAndDurable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	svc := core.New(db)
	if err := svc.SetMemory(context.Background(), "/repo", "test.command", "go test ./..."); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMemory(context.Background(), "/other", "private.fact", "not for repo"); err != nil {
		t.Fatal(err)
	}
	task, err := svc.StartTask(context.Background(), "/repo", "choose the cache")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &memoryAdapter{response: `Implemented it.
<rly-memory>{"upsert":[{"key":"architecture.cache","value":"Use SQLite for the local cache."}],"delete":["test.command"]}</rly-memory>`}
	execution, err := svc.Execute(context.Background(), task, adapter, agents.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Result.Response != "Implemented it." {
		t.Fatalf("response = %q", execution.Result.Response)
	}
	if !strings.Contains(adapter.prompt, `{"key":"test.command","value":"go test ./..."}`) {
		t.Fatalf("prompt did not contain project memory: %q", adapter.prompt)
	}
	if strings.Contains(adapter.prompt, "private.fact") {
		t.Fatalf("prompt leaked another repository's memory: %q", adapter.prompt)
	}
	items, err := svc.Memory(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Key != "architecture.cache" || items[0].SourceTaskID != task.ID {
		t.Fatalf("memory = %+v", items)
	}
	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 || events[3].Type != "orchestration.selected" || events[4].Type != "project_memory.updated" {
		t.Fatalf("events = %+v", events)
	}
	nextTask, err := svc.StartTask(context.Background(), "/repo", "use the cache")
	if err != nil {
		t.Fatal(err)
	}
	nextAdapter := &memoryAdapter{response: "Done."}
	if _, err := svc.Execute(context.Background(), nextTask, nextAdapter, agents.Request{}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nextAdapter.prompt, `{"key":"architecture.cache","value":"Use SQLite for the local cache."}`) {
		t.Fatalf("future task did not receive updated memory: %q", nextAdapter.prompt)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err = core.New(db).Memory(context.Background(), "/repo")
	if err != nil || len(items) != 1 || items[0].Value != "Use SQLite for the local cache." {
		t.Fatalf("reopened memory = %+v, err = %v", items, err)
	}
}

func TestMemoryValidation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	if err := svc.SetMemory(context.Background(), "/repo", "Invalid Key", "value"); err == nil {
		t.Fatal("expected invalid key error")
	}
	if err := svc.SetMemory(context.Background(), "/repo", "valid.key", ""); err == nil {
		t.Fatal("expected empty value error")
	}
}

func TestMalformedAgentMemoryUpdateIsIgnored(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "work")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &memoryAdapter{response: `Done.
<rly-memory>{"upsert":[{"key":"valid.key","value":"value"},{"key":"INVALID","value":"bad"}]}</rly-memory>`}
	execution, err := svc.Execute(context.Background(), task, adapter, agents.Request{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(execution.Result.Response, "<rly-memory>") {
		t.Fatalf("invalid update should remain visible in response: %q", execution.Result.Response)
	}
	items, err := svc.Memory(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("invalid update changed memory: %+v", items)
	}
}

func TestFailedRunDoesNotUpdateMemory(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "work")
	if err != nil {
		t.Fatal(err)
	}
	adapter := &memoryAdapter{response: `<rly-memory>{"upsert":[{"key":"should.not.persist","value":"unverified"}]}</rly-memory>`, err: errors.New("agent failed")}
	if _, err := svc.Execute(context.Background(), task, adapter, agents.Request{}, nil); err == nil {
		t.Fatal("expected run error")
	}
	items, err := svc.Memory(context.Background(), "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("failed run changed memory: %+v", items)
	}
}

func TestCleanupOldRunsPrunesHistoryOlderThanRetention(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	ctx := context.Background()
	recent, err := svc.StartTask(ctx, "/repo", "recent work")
	if err != nil {
		t.Fatal(err)
	}
	// Seed a task whose update timestamp lies outside the 7-day window
	// directly through the store, since the service clock is not injectable.
	agedTask := model.Task{ID: "task-aged", Repository: "/repo", Objective: "aged work", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour), UpdatedAt: time.Now().UTC().Add(-8 * 24 * time.Hour)}
	agedEvent := model.Event{ID: "evt-aged", TaskID: agedTask.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: agedTask.Objective, CreatedAt: agedTask.CreatedAt}
	if err := db.CreateTask(ctx, agedTask, agedEvent); err != nil {
		t.Fatal(err)
	}

	pruned, err := svc.CleanupOldRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if _, err := svc.Task(ctx, agedTask.ID); err != store.ErrNotFound {
		t.Fatalf("aged task err = %v, want ErrNotFound", err)
	}
	if _, err := svc.Task(ctx, recent.ID); err != nil {
		t.Fatalf("recent task should survive cleanup: %v", err)
	}
}

func TestCleanupOldRunsWithRetentionRejectsNegativePeriod(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	if _, err := svc.CleanupOldRunsWithRetention(context.Background(), -time.Hour); err == nil {
		t.Fatal("expected negative retention error")
	}
}

func TestCleanupOldRunsWithRetentionUsesConfiguredPeriod(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	svc := core.New(db)
	ctx := context.Background()
	// A task last updated 3 days ago survives the default 7-day window but
	// must be pruned once the retention period is tightened to 2 days.
	task := model.Task{ID: "task-three-days", Repository: "/repo", Objective: "three day old work", State: model.TaskPlanning, Version: 1, CreatedAt: time.Now().UTC().Add(-3 * 24 * time.Hour), UpdatedAt: time.Now().UTC().Add(-3 * 24 * time.Hour)}
	event := model.Event{ID: "evt-three-days", TaskID: task.ID, Sequence: 1, Type: "task.created", Actor: "user", Summary: task.Objective, CreatedAt: task.CreatedAt}
	if err := db.CreateTask(ctx, task, event); err != nil {
		t.Fatal(err)
	}

	pruned, err := svc.CleanupOldRunsWithRetention(ctx, 2*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if _, err := svc.Task(ctx, task.ID); err != store.ErrNotFound {
		t.Fatalf("task err = %v, want ErrNotFound", err)
	}
}
