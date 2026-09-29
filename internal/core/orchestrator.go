package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harsha/relay/internal/agents"
)

// ExecutionMode controls how the coordinator schedules ready work.
type ExecutionMode string

const (
	ExecutionAuto       ExecutionMode = "auto"
	ExecutionSequential ExecutionMode = "sequential"
	ExecutionParallel   ExecutionMode = "parallel"
)

// AgentTask is one unit of work in an orchestration graph. Tasks that can
// touch the workspace must set ReadOnly to false; the scheduler never runs
// two such tasks at the same time.
type AgentTask struct {
	ID          string
	Agent       string
	Request     agents.Request
	DependsOn   []string
	ReadOnly    bool
	ContextFrom []string
	// ParallelSafe allows a mutating task to run with other mutating tasks when
	// WorkspacePaths prove that their ownership boundaries do not overlap.
	ParallelSafe   bool
	WorkspacePaths []string
}

type AgentTaskResult struct {
	TaskID      string        `json:"task_id"`
	Agent       string        `json:"agent"`
	Model       string        `json:"model,omitempty"`
	Result      agents.Result `json:"result"`
	PromptUsage agents.Usage  `json:"prompt_usage,omitempty"`
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	Skipped     bool          `json:"skipped,omitempty"`
	Error       string        `json:"error,omitempty"`
}

type OrchestrationResult struct {
	Mode       ExecutionMode           `json:"mode"`
	Results    []AgentTaskResult       `json:"results"`
	Messages   []AgentMessage          `json:"messages,omitempty"`
	Usage      agents.Usage            `json:"usage,omitempty"`
	AgentUsage map[string]agents.Usage `json:"agent_usage,omitempty"`
}

// AgentMessage is the bounded handoff text shared from one completed task to a
// downstream dependent task.
type AgentMessage struct {
	FromTaskID string `json:"from_task_id"`
	ToTaskID   string `json:"to_task_id"`
	Agent      string `json:"agent"`
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type CommunicationPolicy struct {
	// IncludeDependencyResults defaults to true; set DisableDependencyResults
	// when callers want scheduling without prompt handoffs.
	DisableDependencyResults bool
	MaxBytesPerDependency    int
}

// EventSink receives events from each process. It is called synchronously per
// event, so callers that do I/O should return quickly.
type EventSink func(task AgentTask, event agents.Event)

// Orchestrator schedules independent agent processes while keeping workspace
// writes serialized. The adapters are deliberately injected so the control
// plane remains independent of any vendor CLI.
type Orchestrator struct {
	Adapters      map[string]agents.Adapter
	Mode          ExecutionMode
	MaxParallel   int
	OnEvent       EventSink
	Communication CommunicationPolicy
}

var (
	ErrInvalidOrchestration = errors.New("invalid orchestration")
	ErrOrchestrationFailed  = errors.New("orchestration failed")
)

func (o Orchestrator) Run(ctx context.Context, tasks []AgentTask) (OrchestrationResult, error) {
	mode := o.Mode
	if mode == "" {
		mode = ExecutionAuto
	}
	if mode != ExecutionAuto && mode != ExecutionSequential && mode != ExecutionParallel {
		return OrchestrationResult{}, fmt.Errorf("%w: unknown execution mode %q", ErrInvalidOrchestration, mode)
	}
	if err := validateTasks(tasks, o.Adapters); err != nil {
		return OrchestrationResult{}, err
	}
	limit := o.MaxParallel
	if limit <= 0 {
		limit = len(tasks)
	}
	if limit < 1 {
		limit = 1
	}

	completed := make(map[string]bool, len(tasks))
	results := make(map[string]AgentTaskResult, len(tasks))
	messages := make([]AgentMessage, 0, len(tasks))
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for len(completed) < len(tasks) {
		for _, task := range tasks {
			if completed[task.ID] {
				continue
			}
			for _, dependency := range task.DependsOn {
				if completed[dependency] && (results[dependency].Error != "" || results[dependency].Skipped) {
					results[task.ID] = AgentTaskResult{TaskID: task.ID, Agent: task.Agent, Skipped: true, Error: "dependency failed"}
					completed[task.ID] = true
					break
				}
			}
		}
		ready := readyTasks(tasks, completed, results)
		if len(ready) == 0 {
			return o.result(mode, tasks, results, messages), fmt.Errorf("%w: dependency cycle or unresolved dependency", ErrInvalidOrchestration)
		}

		batch := o.chooseBatch(ready, mode, limit)
		prepared, handoffs := o.prepareBatch(batch, results)
		messages = append(messages, handoffs...)
		if len(prepared) == 0 {
			continue
		}
		batchResults, failed := o.runBatch(childCtx, prepared)
		for _, result := range batchResults {
			results[result.TaskID] = result
			completed[result.TaskID] = true
		}
		if failed {
			cancel()
			return o.result(mode, tasks, results, messages), fmt.Errorf("%w: one or more agent tasks failed", ErrOrchestrationFailed)
		}
	}
	return o.result(mode, tasks, results, messages), nil
}

func validateTasks(tasks []AgentTask, adapters map[string]agents.Adapter) error {
	if len(tasks) == 0 {
		return fmt.Errorf("%w: at least one task is required", ErrInvalidOrchestration)
	}
	seen := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task.ID == "" || seen[task.ID] {
			return fmt.Errorf("%w: task IDs must be unique and non-empty", ErrInvalidOrchestration)
		}
		if _, ok := adapters[task.Agent]; !ok {
			return fmt.Errorf("%w: adapter %q is not configured", ErrInvalidOrchestration, task.Agent)
		}
		seen[task.ID] = true
	}
	for _, task := range tasks {
		for _, dependency := range task.DependsOn {
			if !seen[dependency] {
				return fmt.Errorf("%w: task %q depends on unknown task %q", ErrInvalidOrchestration, task.ID, dependency)
			}
		}
		for _, source := range task.ContextFrom {
			if !seen[source] {
				return fmt.Errorf("%w: task %q requests context from unknown task %q", ErrInvalidOrchestration, task.ID, source)
			}
		}
	}
	return nil
}

func readyTasks(tasks []AgentTask, completed map[string]bool, results map[string]AgentTaskResult) []AgentTask {
	var ready []AgentTask
	for _, task := range tasks {
		if completed[task.ID] {
			continue
		}
		readyNow := true
		for _, dependency := range task.DependsOn {
			if !completed[dependency] {
				readyNow = false
				break
			}
			if results[dependency].Error != "" || results[dependency].Skipped {
				readyNow = false
				break
			}
		}
		if readyNow {
			ready = append(ready, task)
		}
	}
	return ready
}

// chooseBatch selects the next set of tasks to launch together. The scheduler
// keeps tasks on single-session adapters (e.g. freebuff) out of the same
// batch: their provider permits one active session per account, so two roles
// routed to the same adapter must reuse that session in later rounds instead
// of racing the vendor's instance lock.
func (o Orchestrator) chooseBatch(ready []AgentTask, mode ExecutionMode, limit int) []AgentTask {
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	if mode == ExecutionSequential {
		return ready[:1]
	}
	// Two tasks on the same single-session adapter would fight over the one
	// permitted account session. Keep only the first such task; the rest stay
	// ready and enter a later batch once the session is free again.
	if o.hasSingleSessionConflict(ready) {
		return o.tasksUpToFirstSingleSession(ready, limit)
	}
	// Parallel and auto modes fan out independent read-only work. Mutating
	// tasks may also fan out when every task explicitly opts in and declares
	// disjoint workspace ownership.
	batch := ready[:min(limit, len(ready))]
	for _, task := range batch {
		if !task.ReadOnly && !task.ParallelSafe {
			return []AgentTask{task}
		}
	}
	if hasMutatingTask(batch) && !disjointOwnership(batch) {
		for _, task := range batch {
			if !task.ReadOnly {
				return []AgentTask{task}
			}
		}
	}
	return batch
}

// isSingleSessionAgent reports whether the task's adapter permits only one
// active session per account (freebuff does; its user-scoped lock fails fast
// with a session-conflict error when two runs race concurrently).
func (o Orchestrator) isSingleSessionAgent(agent string) bool {
	adapter, ok := o.Adapters[agent]
	if !ok {
		return false
	}
	return adapter.Capabilities().SingleSession
}

// hasSingleSessionConflict reports whether the ready list contains two or
// more tasks bound for the same single-session adapter.
func (o Orchestrator) hasSingleSessionConflict(ready []AgentTask) bool {
	counts := make(map[string]int, len(ready))
	for _, task := range ready {
		if o.isSingleSessionAgent(task.Agent) {
			counts[task.Agent]++
			if counts[task.Agent] > 1 {
				return true
			}
		}
	}
	return false
}

// tasksUpToFirstSingleSession builds the batch with at most one task per
// single-session adapter. Other ready tasks before it still fit in the batch
// and run concurrently with it; the remaining single-session tasks wait for
// the next scheduling round, reusing the one account session sequentially.
func (o Orchestrator) tasksUpToFirstSingleSession(ready []AgentTask, limit int) []AgentTask {
	seen := make(map[string]bool, len(ready))
	batch := make([]AgentTask, 0, len(ready))
	for _, task := range ready {
		if o.isSingleSessionAgent(task.Agent) {
			if seen[task.Agent] {
				continue
			}
			seen[task.Agent] = true
		}
		if len(batch) >= limit {
			break
		}
		batch = append(batch, task)
	}
	if len(batch) == 0 && len(ready) > 0 {
		batch = append(batch, ready[0])
	}
	return batch
}

func hasMutatingTask(tasks []AgentTask) bool {
	for _, task := range tasks {
		if !task.ReadOnly {
			return true
		}
	}
	return false
}

func disjointOwnership(tasks []AgentTask) bool {
	for i := range tasks {
		if tasks[i].ReadOnly || len(tasks[i].WorkspacePaths) == 0 {
			if !tasks[i].ReadOnly {
				return false
			}
			continue
		}
		for j := i + 1; j < len(tasks); j++ {
			if tasks[j].ReadOnly {
				continue
			}
			for _, left := range tasks[i].WorkspacePaths {
				for _, right := range tasks[j].WorkspacePaths {
					if pathsOverlap(left, right) {
						return false
					}
				}
			}
		}
	}
	return true
}

func pathsOverlap(left, right string) bool {
	left = filepath.ToSlash(filepath.Clean(left))
	right = filepath.ToSlash(filepath.Clean(right))
	return left == right || strings.HasPrefix(left, right+"/") || strings.HasPrefix(right, left+"/")
}

func (o Orchestrator) runBatch(ctx context.Context, tasks []AgentTask) ([]AgentTaskResult, bool) {
	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]AgentTaskResult, len(tasks))
	failed := false
	for i, task := range tasks {
		wg.Add(1)
		go func(index int, task AgentTask) {
			defer wg.Done()
			started := time.Now().UTC()
			result := AgentTaskResult{TaskID: task.ID, Agent: task.Agent, Model: task.Request.Model, PromptUsage: estimatePromptUsage(task.Request.Prompt), StartedAt: started}
			run, err := o.Adapters[task.Agent].Start(batchCtx, task.Request)
			if err != nil {
				result.Error = err.Error()
				result.Result = agents.Result{ExitCode: -1, Err: err, Error: err.Error()}
			} else {
				for event := range run.Events() {
					if o.OnEvent != nil {
						o.OnEvent(task, event)
					}
				}
				result.Result = run.Wait()
				if result.Result.Err != nil {
					result.Error = result.Result.Err.Error()
				}
			}
			result.CompletedAt = time.Now().UTC()
			mu.Lock()
			results[index] = result
			if result.Error != "" {
				failed = true
				cancel()
			}
			mu.Unlock()
		}(i, task)
	}
	wg.Wait()
	return results, failed
}

func (o Orchestrator) prepareBatch(tasks []AgentTask, results map[string]AgentTaskResult) ([]AgentTask, []AgentMessage) {
	prepared := make([]AgentTask, 0, len(tasks))
	var messages []AgentMessage
	for _, task := range tasks {
		next, handoffs := o.attachDependencyContext(task, results)
		next.Request.Model = agents.ExecutionModel(next.Request.Model)
		prepared = append(prepared, next)
		messages = append(messages, handoffs...)
	}
	return prepared, messages
}

func (o Orchestrator) attachDependencyContext(task AgentTask, results map[string]AgentTaskResult) (AgentTask, []AgentMessage) {
	if o.Communication.DisableDependencyResults {
		return task, nil
	}
	sources := task.ContextFrom
	if len(sources) == 0 {
		sources = task.DependsOn
	}
	if len(sources) == 0 {
		return task, nil
	}
	limit := o.Communication.MaxBytesPerDependency
	if limit <= 0 {
		limit = 6000
	}
	var b strings.Builder
	var messages []AgentMessage
	for _, source := range sources {
		result, ok := results[source]
		if !ok || result.Result.Response == "" {
			continue
		}
		content, truncated := truncateForHandoff(result.Result.Response, limit)
		messages = append(messages, AgentMessage{FromTaskID: source, ToTaskID: task.ID, Agent: result.Agent, Content: content, Truncated: truncated})
		if b.Len() == 0 {
			b.WriteString("\n\nInter-agent context:\n")
		}
		fmt.Fprintf(&b, "\n[%s via %s]\n%s\n", source, result.Agent, content)
	}
	if b.Len() > 0 {
		task.Request.Prompt += b.String()
	}
	return task, messages
}

func truncateForHandoff(value string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(value) <= maxBytes {
		return value, false
	}
	if maxBytes < 32 {
		return value[:maxBytes], true
	}
	return value[:maxBytes-16] + "\n[truncated]\n", true
}

func estimatePromptUsage(prompt string) agents.Usage {
	tokens := int64((len(prompt) + 3) / 4)
	return agents.Usage{InputTokens: tokens, TotalTokens: tokens}
}

func (o Orchestrator) result(mode ExecutionMode, tasks []AgentTask, results map[string]AgentTaskResult, messages []AgentMessage) OrchestrationResult {
	ordered := orderedResults(tasks, results)
	var total agents.Usage
	byAgent := make(map[string]agents.Usage)
	for _, result := range ordered {
		total = addUsage(total, result.Result.Usage)
		byAgent[result.Agent] = addUsage(byAgent[result.Agent], result.Result.Usage)
	}
	if len(byAgent) == 0 {
		byAgent = nil
	}
	return OrchestrationResult{
		Mode:       mode,
		Results:    ordered,
		Messages:   messages,
		Usage:      total,
		AgentUsage: byAgent,
	}
}

func addUsage(a, b agents.Usage) agents.Usage {
	return agents.Usage{
		InputTokens:     a.InputTokens + b.InputTokens,
		CachedTokens:    a.CachedTokens + b.CachedTokens,
		OutputTokens:    a.OutputTokens + b.OutputTokens,
		ReasoningTokens: a.ReasoningTokens + b.ReasoningTokens,
		TotalTokens:     a.TotalTokens + b.TotalTokens,
	}
}

func orderedResults(tasks []AgentTask, results map[string]AgentTaskResult) []AgentTaskResult {
	ordered := make([]AgentTaskResult, 0, len(results))
	for _, task := range tasks {
		if result, ok := results[task.ID]; ok {
			ordered = append(ordered, result)
		}
	}
	return ordered
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
