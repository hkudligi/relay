package core_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
	if len(events) != 5 {
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
	if len(events) != 5 || events[3].Type != "project_memory.updated" {
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
