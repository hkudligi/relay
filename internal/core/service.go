package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
	"github.com/harsha/relay/internal/store"
)

var ErrAgentUnavailable = fmt.Errorf("agent unavailable")

type Execution struct {
	Task   *model.Task   `json:"task"`
	Result agents.Result `json:"result"`
}

type Service struct {
	store *store.SQLite
	now   func() time.Time
}

func New(s *store.SQLite) *Service { return &Service{store: s, now: time.Now} }

func (s *Service) StartTask(ctx context.Context, repo, objective string) (*model.Task, error) {
	now := s.now().UTC()
	id := newID("task")
	t := model.Task{ID: id, Repository: repo, Objective: objective, State: model.TaskCreated, Version: 1, CreatedAt: now, UpdatedAt: now}
	e := model.Event{ID: newID("evt"), TaskID: id, Sequence: 1, Type: "task.created", Actor: "user", Summary: objective, CreatedAt: now}
	if err := s.store.CreateTask(ctx, t, e); err != nil {
		return nil, err
	}
	now = s.now().UTC()
	e = model.Event{ID: newID("evt"), TaskID: id, Sequence: 2, Type: "task.state_changed", Actor: "coordinator", Summary: "task entered planning", Data: map[string]any{"from": model.TaskCreated, "to": model.TaskPlanning}, CreatedAt: now}
	if err := s.store.Transition(ctx, id, model.TaskCreated, model.TaskPlanning, e); err != nil {
		return nil, err
	}
	return s.store.Task(ctx, id)
}

func (s *Service) Status(ctx context.Context, repo string) (model.Status, error) {
	t, err := s.store.LatestTask(ctx, repo)
	if err == store.ErrNotFound {
		return model.Status{Repository: repo}, nil
	}
	return model.Status{Repository: repo, Task: t}, err
}
func (s *Service) Tasks(ctx context.Context, repo string) ([]model.Task, error) {
	return s.store.ListTasks(ctx, repo, 20)
}
func (s *Service) Trace(ctx context.Context, id string) ([]model.Event, error) {
	return s.store.Events(ctx, id)
}
func (s *Service) Task(ctx context.Context, id string) (*model.Task, error) {
	return s.store.Task(ctx, id)
}
func (s *Service) Session(ctx context.Context, taskID, adapter string) (string, error) {
	return s.store.LatestSession(ctx, taskID, adapter)
}

func (s *Service) Execute(ctx context.Context, task *model.Task, adapter agents.Adapter, request agents.Request, emit func(agents.Event)) (*Execution, error) {
	installation := adapter.Detect(ctx)
	if !installation.Available {
		summary := fmt.Sprintf("%s adapter unavailable", adapter.Name())
		if installation.Error != "" {
			summary += ": " + installation.Error
		}
		_ = s.store.AppendEvent(ctx, model.Event{ID: newID("evt"), TaskID: task.ID, Type: "agent.unavailable", Actor: "coordinator", Summary: summary, CreatedAt: s.now().UTC()})
		return nil, fmt.Errorf("%w: %s", ErrAgentUnavailable, summary)
	}
	started := s.now().UTC()
	if err := s.transition(ctx, task.ID, model.TaskPlanning, model.TaskRunning, "router", fmt.Sprintf("implementer → %s", adapter.Name()), map[string]any{"adapter": adapter.Name(), "version": installation.Version}); err != nil {
		return nil, err
	}
	request.Prompt = task.Objective
	request.Workspace = task.Repository
	run, err := adapter.Start(ctx, request)
	if err != nil {
		s.failExecution(ctx, task.ID, adapter.Name(), err)
		return nil, err
	}
	for event := range run.Events() {
		if emit != nil {
			emit(event)
		}
		switch event.Kind {
		case agents.EventSession:
			_ = s.store.AppendEvent(ctx, model.Event{ID: newID("evt"), TaskID: task.ID, Type: "agent.session_started", Actor: adapter.Name(), Summary: "session " + event.SessionID + " started", Data: map[string]any{"session_id": event.SessionID}, CreatedAt: s.now().UTC()})
		case agents.EventError:
			_ = s.store.AppendEvent(ctx, model.Event{ID: newID("evt"), TaskID: task.ID, Type: "agent.error", Actor: adapter.Name(), Summary: event.Message, CreatedAt: s.now().UTC()})
		}
	}
	result := run.Wait()
	completed := s.now().UTC()
	status := "COMPLETED"
	state := model.TaskCompleted
	if result.Err != nil {
		status = "FAILED"
		state = model.TaskFailed
	}
	record := model.RunRecord{ID: newID("run"), TaskID: task.ID, Adapter: adapter.Name(), SessionID: result.SessionID, Status: status, ExitCode: result.ExitCode, Response: result.Response, Usage: map[string]any{"input_tokens": result.Usage.InputTokens, "cached_tokens": result.Usage.CachedTokens, "output_tokens": result.Usage.OutputTokens, "reasoning_tokens": result.Usage.ReasoningTokens, "total_tokens": result.Usage.TotalTokens}, StartedAt: started, CompletedAt: completed}
	if err := s.store.RecordRun(ctx, record, task.Repository); err != nil {
		return nil, err
	}
	summary := fmt.Sprintf("%s completed the task", adapter.Name())
	if result.Err != nil {
		summary = fmt.Sprintf("%s failed: %v", adapter.Name(), result.Err)
	}
	if err := s.transition(ctx, task.ID, model.TaskRunning, state, adapter.Name(), summary, map[string]any{"session_id": result.SessionID, "exit_code": result.ExitCode, "total_tokens": result.Usage.TotalTokens}); err != nil {
		return nil, err
	}
	updated, err := s.store.Task(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	execution := &Execution{Task: updated, Result: result}
	if result.Err != nil {
		return execution, result.Err
	}
	return execution, nil
}

func (s *Service) transition(ctx context.Context, id string, from, to model.TaskState, actor, summary string, data map[string]any) error {
	return s.store.Transition(ctx, id, from, to, model.Event{ID: newID("evt"), TaskID: id, Type: "task.state_changed", Actor: actor, Summary: summary, Data: data, CreatedAt: s.now().UTC()})
}
func (s *Service) failExecution(ctx context.Context, id, actor string, cause error) {
	_ = s.transition(ctx, id, model.TaskRunning, model.TaskFailed, actor, "agent failed to start: "+cause.Error(), nil)
}

func newID(prefix string) string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("random id: %v", err))
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
