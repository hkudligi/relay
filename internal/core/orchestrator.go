package core

import (
	"context"
	"errors"
	"fmt"
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
}

type AgentTaskResult struct {
	TaskID      string        `json:"task_id"`
	Agent       string        `json:"agent"`
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
	Budget     *TokenBudgetSnapshot    `json:"budget,omitempty"`
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

type TokenBudget struct {
	MaxTotalTokens    int64
	MaxTokensPerAgent map[string]int64
}

type TokenBudgetSnapshot struct {
	MaxTotalTokens    int64            `json:"max_total_tokens,omitempty"`
	MaxTokensPerAgent map[string]int64 `json:"max_tokens_per_agent,omitempty"`
	UsedTotalTokens   int64            `json:"used_total_tokens,omitempty"`
	UsedByAgent       map[string]int64 `json:"used_by_agent,omitempty"`
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
	TokenBudget   TokenBudget
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
	ledger := newTokenLedger(o.TokenBudget)
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
			return o.result(mode, tasks, results, messages, ledger), fmt.Errorf("%w: dependency cycle or unresolved dependency", ErrInvalidOrchestration)
		}

		batch := chooseBatch(ready, mode, limit)
		prepared, handoffs, skipped, budgetFailed := o.prepareBatch(batch, results, ledger)
		for _, result := range skipped {
			results[result.TaskID] = result
			completed[result.TaskID] = true
		}
		messages = append(messages, handoffs...)
		if budgetFailed {
			cancel()
			return o.result(mode, tasks, results, messages, ledger), fmt.Errorf("%w: token budget exhausted", ErrOrchestrationFailed)
		}
		if len(prepared) == 0 {
			continue
		}
		batchResults, failed := o.runBatch(childCtx, prepared)
		for _, result := range batchResults {
			ledger.add(result.Agent, result.Result.Usage)
			results[result.TaskID] = result
			completed[result.TaskID] = true
		}
		if failed {
			cancel()
			return o.result(mode, tasks, results, messages, ledger), fmt.Errorf("%w: one or more agent tasks failed", ErrOrchestrationFailed)
		}
	}
	return o.result(mode, tasks, results, messages, ledger), nil
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

func chooseBatch(ready []AgentTask, mode ExecutionMode, limit int) []AgentTask {
	sort.Slice(ready, func(i, j int) bool { return ready[i].ID < ready[j].ID })
	if mode == ExecutionSequential {
		return ready[:1]
	}
	// Parallel and auto modes fan out only independent read-only work. A
	// mutating task is always isolated in its own batch to protect the shared
	// workspace, even when the caller requests parallel scheduling.
	for _, task := range ready {
		if !task.ReadOnly {
			return []AgentTask{task}
		}
	}
	return ready[:min(limit, len(ready))]
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
			result := AgentTaskResult{TaskID: task.ID, Agent: task.Agent, PromptUsage: estimatePromptUsage(task.Request.Prompt), StartedAt: started}
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

func (o Orchestrator) prepareBatch(tasks []AgentTask, results map[string]AgentTaskResult, ledger *tokenLedger) ([]AgentTask, []AgentMessage, []AgentTaskResult, bool) {
	prepared := make([]AgentTask, 0, len(tasks))
	var messages []AgentMessage
	var skipped []AgentTaskResult
	trial := ledger.clone()
	for _, task := range tasks {
		next, handoffs := o.attachDependencyContext(task, results)
		promptUsage := estimatePromptUsage(next.Request.Prompt)
		if err := trial.canSpend(task.Agent, promptUsage); err != nil {
			skipped = append(skipped, AgentTaskResult{TaskID: task.ID, Agent: task.Agent, PromptUsage: promptUsage, Skipped: true, Error: err.Error(), StartedAt: time.Now().UTC(), CompletedAt: time.Now().UTC()})
			return nil, nil, skipped, true
		}
		trial.add(task.Agent, promptUsage)
		next.Request.Model = agents.ExecutionModel(next.Request.Model)
		prepared = append(prepared, next)
		messages = append(messages, handoffs...)
	}
	*ledger = *trial
	return prepared, messages, skipped, false
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

func (o Orchestrator) result(mode ExecutionMode, tasks []AgentTask, results map[string]AgentTaskResult, messages []AgentMessage, ledger *tokenLedger) OrchestrationResult {
	return OrchestrationResult{
		Mode:       mode,
		Results:    orderedResults(tasks, results),
		Messages:   messages,
		Usage:      ledger.totalUsage,
		AgentUsage: ledger.agentUsage,
		Budget:     ledger.snapshot(),
	}
}

type tokenLedger struct {
	budget      TokenBudget
	totalUsage  agents.Usage
	agentUsage  map[string]agents.Usage
	usedByAgent map[string]int64
}

func newTokenLedger(budget TokenBudget) *tokenLedger {
	return &tokenLedger{budget: budget, agentUsage: make(map[string]agents.Usage), usedByAgent: make(map[string]int64)}
}

func (l *tokenLedger) clone() *tokenLedger {
	next := &tokenLedger{
		budget:      l.budget,
		totalUsage:  l.totalUsage,
		agentUsage:  make(map[string]agents.Usage, len(l.agentUsage)),
		usedByAgent: make(map[string]int64, len(l.usedByAgent)),
	}
	for agent, usage := range l.agentUsage {
		next.agentUsage[agent] = usage
	}
	for agent, used := range l.usedByAgent {
		next.usedByAgent[agent] = used
	}
	return next
}

func (l *tokenLedger) canSpend(agent string, usage agents.Usage) error {
	tokens := usage.TotalTokens
	if tokens <= 0 {
		return nil
	}
	if l.budget.MaxTotalTokens > 0 && l.totalUsage.TotalTokens+tokens > l.budget.MaxTotalTokens {
		return fmt.Errorf("total token budget exceeded")
	}
	if l.budget.MaxTokensPerAgent != nil && l.budget.MaxTokensPerAgent[agent] > 0 && l.usedByAgent[agent]+tokens > l.budget.MaxTokensPerAgent[agent] {
		return fmt.Errorf("%s token budget exceeded", agent)
	}
	return nil
}

func (l *tokenLedger) add(agent string, usage agents.Usage) {
	l.totalUsage = addUsage(l.totalUsage, usage)
	current := l.agentUsage[agent]
	l.agentUsage[agent] = addUsage(current, usage)
	l.usedByAgent[agent] += usage.TotalTokens
}

func (l *tokenLedger) snapshot() *TokenBudgetSnapshot {
	if l.budget.MaxTotalTokens <= 0 && len(l.budget.MaxTokensPerAgent) == 0 {
		return nil
	}
	maxByAgent := make(map[string]int64, len(l.budget.MaxTokensPerAgent))
	for agent, max := range l.budget.MaxTokensPerAgent {
		maxByAgent[agent] = max
	}
	usedByAgent := make(map[string]int64, len(l.usedByAgent))
	for agent, used := range l.usedByAgent {
		usedByAgent[agent] = used
	}
	return &TokenBudgetSnapshot{MaxTotalTokens: l.budget.MaxTotalTokens, MaxTokensPerAgent: maxByAgent, UsedTotalTokens: l.totalUsage.TotalTokens, UsedByAgent: usedByAgent}
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
