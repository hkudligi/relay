package core_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/core"
)

type orchestrationAdapter struct {
	mu       sync.Mutex
	active   int
	max      int
	started  []string
	delay    time.Duration
	response string
	usage    agents.Usage
}

func (a *orchestrationAdapter) Name() string { return "fake" }
func (a *orchestrationAdapter) Detect(context.Context) agents.Installation {
	return agents.Installation{Name: a.Name(), Available: true}
}
func (a *orchestrationAdapter) Capabilities() agents.Capabilities { return agents.Capabilities{} }
func (a *orchestrationAdapter) Resume(context.Context, string, agents.Request) (agents.Run, error) {
	return nil, nil
}
func (a *orchestrationAdapter) Start(ctx context.Context, request agents.Request) (agents.Run, error) {
	a.mu.Lock()
	a.active++
	if a.active > a.max {
		a.max = a.active
	}
	a.started = append(a.started, request.Prompt)
	a.mu.Unlock()
	return &orchestrationRun{ctx: ctx, adapter: a, delay: a.delay, response: a.response, usage: a.usage}, nil
}

type orchestrationRun struct {
	ctx      context.Context
	adapter  *orchestrationAdapter
	delay    time.Duration
	response string
	usage    agents.Usage
}

func (r *orchestrationRun) Events() <-chan agents.Event {
	ch := make(chan agents.Event)
	go func() { close(ch) }()
	return ch
}
func (r *orchestrationRun) Wait() agents.Result {
	defer func() {
		r.adapter.mu.Lock()
		r.adapter.active--
		r.adapter.mu.Unlock()
	}()
	select {
	case <-time.After(r.delay):
	case <-r.ctx.Done():
		return agents.Result{ExitCode: -1, Err: r.ctx.Err(), Error: r.ctx.Err().Error()}
	}
	response := r.response
	if response == "" {
		response = "ok"
	}
	return agents.Result{ExitCode: 0, Response: response, Usage: r.usage}
}
func (r *orchestrationRun) Cancel() error { return nil }

func TestOrchestratorRunsIndependentReadOnlyTasksInParallel(t *testing.T) {
	adapter := &orchestrationAdapter{delay: 40 * time.Millisecond}
	o := core.Orchestrator{Adapters: map[string]agents.Adapter{"fake": adapter}, Mode: core.ExecutionAuto}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "a", Agent: "fake", ReadOnly: true, Request: agents.Request{Prompt: "a"}},
		{ID: "b", Agent: "fake", ReadOnly: true, Request: agents.Request{Prompt: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 || adapter.max != 2 {
		t.Fatalf("results=%d max_concurrency=%d, want 2 and 2", len(result.Results), adapter.max)
	}
}

func TestOrchestratorPassesDependencyResultsToDownstreamTask(t *testing.T) {
	adapter := &orchestrationAdapter{response: "planner notes: touch router and tests"}
	o := core.Orchestrator{Adapters: map[string]agents.Adapter{"fake": adapter}, Mode: core.ExecutionSequential}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "plan", Agent: "fake", ReadOnly: true, Request: agents.Request{Prompt: "plan"}},
		{ID: "implement", Agent: "fake", DependsOn: []string{"plan"}, Request: agents.Request{Prompt: "implement"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(adapter.started) != 2 {
		t.Fatalf("started=%v, want two tasks", adapter.started)
	}
	if got := adapter.started[1]; !contains(got, "Inter-agent context") || !contains(got, "planner notes") {
		t.Fatalf("downstream prompt did not include handoff context:\n%s", got)
	}
	if len(result.Messages) != 1 || result.Messages[0].FromTaskID != "plan" || result.Messages[0].ToTaskID != "implement" {
		t.Fatalf("messages=%v, want plan -> implement handoff", result.Messages)
	}
}

func TestOrchestratorTracksPromptAndAgentTokenUsage(t *testing.T) {
	adapter := &orchestrationAdapter{usage: agents.Usage{OutputTokens: 7, TotalTokens: 7}}
	o := core.Orchestrator{Adapters: map[string]agents.Adapter{"fake": adapter}, Mode: core.ExecutionSequential}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "a", Agent: "fake", ReadOnly: true, Request: agents.Request{Prompt: "12345678"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Usage.TotalTokens != 9 {
		t.Fatalf("total tokens=%d, want estimated prompt 2 + result 7", result.Usage.TotalTokens)
	}
	if result.AgentUsage["fake"].TotalTokens != 9 {
		t.Fatalf("agent tokens=%d, want 9", result.AgentUsage["fake"].TotalTokens)
	}
}

func TestOrchestratorStopsBeforeExceedingTokenBudget(t *testing.T) {
	adapter := &orchestrationAdapter{}
	o := core.Orchestrator{
		Adapters:    map[string]agents.Adapter{"fake": adapter},
		Mode:        core.ExecutionSequential,
		TokenBudget: core.TokenBudget{MaxTotalTokens: 1},
	}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "a", Agent: "fake", ReadOnly: true, Request: agents.Request{Prompt: "12345678"}},
	})
	if err == nil {
		t.Fatal("expected budget error")
	}
	if len(adapter.started) != 0 {
		t.Fatalf("started=%v, want no launched tasks", adapter.started)
	}
	if len(result.Results) != 1 || !result.Results[0].Skipped {
		t.Fatalf("results=%+v, want skipped budget result", result.Results)
	}
}

func contains(value, needle string) bool {
	return strings.Contains(value, needle)
}

func TestOrchestratorSerializesWorkspaceWritesAndHonorsDependencies(t *testing.T) {
	adapter := &orchestrationAdapter{delay: 10 * time.Millisecond}
	o := core.Orchestrator{Adapters: map[string]agents.Adapter{"fake": adapter}, Mode: core.ExecutionParallel}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "write", Agent: "fake", Request: agents.Request{Prompt: "write"}},
		{ID: "after", Agent: "fake", DependsOn: []string{"write"}, Request: agents.Request{Prompt: "after"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.max != 1 || len(result.Results) != 2 {
		t.Fatalf("max_concurrency=%d results=%d, want 1 and 2", adapter.max, len(result.Results))
	}
	if len(adapter.started) != 2 || adapter.started[0] != "write" || !strings.HasPrefix(adapter.started[1], "after") {
		t.Fatalf("started=%v, want dependency order", adapter.started)
	}
}
