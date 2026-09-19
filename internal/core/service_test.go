package core_test

import (
	"context"
	"path/filepath"
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
