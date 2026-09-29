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
	if result.Usage.TotalTokens != 7 {
		t.Fatalf("total tokens=%d, want result usage 7", result.Usage.TotalTokens)
	}
	if result.AgentUsage["fake"].TotalTokens != 7 {
		t.Fatalf("agent tokens=%d, want 7", result.AgentUsage["fake"].TotalTokens)
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

// singleSessionAdapter models an adapter like freebuff: only one active
// session per account is permitted.
type singleSessionAdapter struct {
	orchestrationAdapter
}

func (a *singleSessionAdapter) Name() string { return "freebuff" }
func (a *singleSessionAdapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{NonInteractive: true, Streaming: true, Cancellation: true, FileEditing: true, SingleSession: true}
}

func TestOrchestratorSerializesRolesOnSingleSessionAdapter(t *testing.T) {
	// Two roles (e.g. planner and executor) both route to a single-session
	// agent. Even in parallel mode they must never run at the same time; the
	// second waits for the next round and reuses the one account session.
	adapter := &singleSessionAdapter{orchestrationAdapter{delay: 25 * time.Millisecond}}
	o := core.Orchestrator{Adapters: map[string]agents.Adapter{"freebuff": adapter}, Mode: core.ExecutionParallel}
	result, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "plan", Agent: "freebuff", ReadOnly: true, Request: agents.Request{Prompt: "plan"}},
		{ID: "execute", Agent: "freebuff", ReadOnly: true, Request: agents.Request{Prompt: "execute"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.max != 1 {
		t.Fatalf("max_concurrency=%d, want 1 for single-session adapter", adapter.max)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results=%d, want both roles completed", len(result.Results))
	}
}

func TestOrchestratorKeepsIndependentAdaptersParallelBesideSingleSessionTask(t *testing.T) {
	// A single-session task must not flatten the whole batch: other adapters
	// without the constraint still run concurrently with it.
	freebuff := &singleSessionAdapter{orchestrationAdapter{delay: 25 * time.Millisecond}}
	plain := &orchestrationAdapter{delay: 25 * time.Millisecond}
	o := core.Orchestrator{
		Adapters: map[string]agents.Adapter{"freebuff": freebuff, "plain": plain},
		Mode:     core.ExecutionParallel,
	}
	_, err := o.Run(context.Background(), []core.AgentTask{
		{ID: "one", Agent: "freebuff", ReadOnly: true, Request: agents.Request{Prompt: "one"}},
		{ID: "two", Agent: "freebuff", ReadOnly: true, Request: agents.Request{Prompt: "two"}},
		{ID: "three", Agent: "plain", ReadOnly: true, Request: agents.Request{Prompt: "three"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if freebuff.max != 1 {
		t.Fatalf("freebuff max_concurrency=%d, want 1", freebuff.max)
	}
	if plain.max != 1 {
		t.Fatalf("plain max_concurrency=%d, want 1", plain.max)
	}
	if len(plain.started) != 1 {
		t.Fatalf("plain started=%v, want its task launched in the first batch", plain.started)
	}
}
