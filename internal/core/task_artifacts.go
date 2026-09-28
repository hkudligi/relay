package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
)

const taskArtifactSchemaVersion = 3

type taskArtifactState struct {
	SchemaVersion            int                          `json:"schema_version"`
	TaskID                   string                       `json:"task_id"`
	Objective                string                       `json:"objective"`
	Repository               string                       `json:"repository"`
	Phase                    model.TaskState              `json:"phase"`
	Status                   model.TaskState              `json:"status"`
	LastSuccessfulCheckpoint string                       `json:"last_successful_checkpoint"`
	NextRecommendedAction    string                       `json:"next_recommended_action"`
	Orchestration            DurableOrchestrationDecision `json:"orchestration"`
	Operations               taskOperationalState         `json:"operations"`
	CompletionGates          []completionGate             `json:"completion_gates"`
	RetryLimits              map[string]retryLimit        `json:"retry_limits"`
	ProviderAttempts         []providerAttemptArtifact    `json:"provider_attempts"`
	AgentResults             []agentResultArtifact        `json:"agent_results"`
	CreatedAt                time.Time                    `json:"created_at"`
	UpdatedAt                time.Time                    `json:"updated_at"`
}

type taskOperationalState struct {
	Observability  taskOperationalMetrics  `json:"observability"`
	CostLimits     taskCostLimits          `json:"cost_limits"`
	Cancellation   taskCancellationState   `json:"cancellation"`
	AccessControls taskAccessControls      `json:"access_controls"`
	Idempotency    []taskIdempotencyRecord `json:"idempotency"`
}

type taskOperationalMetrics struct {
	RunCount       int       `json:"run_count"`
	FailedRunCount int       `json:"failed_run_count"`
	TotalTokens    int64     `json:"total_tokens"`
	LastAgentAt    time.Time `json:"last_agent_at,omitempty"`
	LastEventAt    time.Time `json:"last_event_at,omitempty"`
}

type taskCostLimits struct {
	MaxTotalTokens int64 `json:"max_total_tokens,omitempty"`
}

type taskCancellationState struct {
	Requested      bool      `json:"requested"`
	RequestedBy    string    `json:"requested_by,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	IdempotencyKey string    `json:"idempotency_key,omitempty"`
	RequestedAt    time.Time `json:"requested_at,omitempty"`
}

type taskAccessControls struct {
	AllowedCancellationActors []string `json:"allowed_cancellation_actors"`
}

type taskIdempotencyRecord struct {
	Operation  string    `json:"operation"`
	Key        string    `json:"key"`
	Status     string    `json:"status"`
	RecordedAt time.Time `json:"recorded_at"`
}

type completionGate struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Phase       model.TaskState `json:"phase"`
	Required    bool            `json:"required"`
	Satisfied   bool            `json:"satisfied"`
	Evidence    string          `json:"evidence,omitempty"`
	SatisfiedAt time.Time       `json:"satisfied_at,omitempty"`
}

type retryLimit struct {
	MaxAttempts int `json:"max_attempts"`
	Attempts    int `json:"attempts"`
}

type agentResultArtifact struct {
	Role        string         `json:"role"`
	Adapter     string         `json:"adapter"`
	Status      string         `json:"status"`
	SessionID   string         `json:"session_id,omitempty"`
	ExitCode    int            `json:"exit_code"`
	Error       string         `json:"error,omitempty"`
	Usage       agents.Usage   `json:"usage,omitempty"`
	Summary     string         `json:"summary,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
	Data        map[string]any `json:"data,omitempty"`
}

type providerAttemptArtifact struct {
	Attempt     int            `json:"attempt"`
	Role        string         `json:"role"`
	Adapter     string         `json:"adapter"`
	Model       string         `json:"model,omitempty"`
	Status      string         `json:"status"`
	SessionID   string         `json:"session_id,omitempty"`
	ExitCode    int            `json:"exit_code"`
	Error       string         `json:"error,omitempty"`
	ErrorDetail map[string]any `json:"error_detail,omitempty"`
	TracePaths  []string       `json:"trace_paths,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
}

func newTaskArtifactState(task model.Task) taskArtifactState {
	now := task.UpdatedAt
	return taskArtifactState{
		SchemaVersion:            taskArtifactSchemaVersion,
		TaskID:                   task.ID,
		Objective:                task.Objective,
		Repository:               task.Repository,
		Phase:                    task.State,
		Status:                   task.State,
		LastSuccessfulCheckpoint: "task-artifacts-initialized",
		NextRecommendedAction:    "Capture a concrete plan and acceptance criteria.",
		Orchestration:            localOrchestrationDecision(),
		Operations: taskOperationalState{
			AccessControls: taskAccessControls{AllowedCancellationActors: []string{"user", "coordinator"}},
			Idempotency:    []taskIdempotencyRecord{},
		},
		CompletionGates: []completionGate{
			{ID: "objective_recorded", Description: "User objective is recorded in objective.md.", Phase: model.TaskPlanning, Required: true, Satisfied: true, Evidence: "objective.md", SatisfiedAt: now},
			{ID: "artifact_state_initialized", Description: "Machine-readable workflow state exists with phases, gates, retry limits, and agent results.", Phase: model.TaskPlanning, Required: true, Satisfied: true, Evidence: "state.json", SatisfiedAt: now},
			{ID: "agent_result_recorded", Description: "At least one structured agent result is recorded before completion.", Phase: model.TaskRunning, Required: true},
			{ID: "terminal_state_recorded", Description: "The workflow reached a terminal task state with durable evidence.", Phase: model.TaskCompleted, Required: true},
		},
		RetryLimits: map[string]retryLimit{
			"planner":     {MaxAttempts: 2},
			"implementer": {MaxAttempts: 3},
			"verifier":    {MaxAttempts: 2},
			"reviewer":    {MaxAttempts: 2},
		},
		ProviderAttempts: []providerAttemptArtifact{},
		AgentResults:     []agentResultArtifact{},
		CreatedAt:        task.CreatedAt,
		UpdatedAt:        task.UpdatedAt,
	}
}

func taskArtifactRoot(task model.Task) string {
	return filepath.Join(task.Repository, ".relay", "tasks", task.ID)
}

func writeTaskArtifactState(root string, state taskArtifactState) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode task state artifact: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(root, "state.json"), encoded, 0o600); err != nil {
		return fmt.Errorf("write task state artifact: %w", err)
	}
	return nil
}

func readTaskArtifactState(task model.Task) (taskArtifactState, string, error) {
	root := taskArtifactRoot(task)
	raw, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		return taskArtifactState{}, root, err
	}
	var state taskArtifactState
	if err := json.Unmarshal(raw, &state); err != nil {
		return taskArtifactState{}, root, fmt.Errorf("decode task state artifact: %w", err)
	}
	if state.SchemaVersion < taskArtifactSchemaVersion {
		state = migrateLegacyTaskArtifactState(task, state)
	}
	if state.RetryLimits == nil {
		state.RetryLimits = newTaskArtifactState(task).RetryLimits
	}
	if state.AgentResults == nil {
		state.AgentResults = []agentResultArtifact{}
	}
	if state.ProviderAttempts == nil {
		state.ProviderAttempts = []providerAttemptArtifact{}
	}
	if state.Orchestration.Backend == "" {
		state.Orchestration = localOrchestrationDecision()
	}
	if len(state.Operations.AccessControls.AllowedCancellationActors) == 0 {
		state.Operations.AccessControls.AllowedCancellationActors = []string{"user", "coordinator"}
	}
	if state.Operations.Idempotency == nil {
		state.Operations.Idempotency = []taskIdempotencyRecord{}
	}
	return state, root, nil
}

func migrateLegacyTaskArtifactState(task model.Task, state taskArtifactState) taskArtifactState {
	next := newTaskArtifactState(task)
	if state.TaskID != "" {
		next.TaskID = state.TaskID
	}
	if state.Objective != "" {
		next.Objective = state.Objective
	}
	if state.Repository != "" {
		next.Repository = state.Repository
	}
	if state.Phase != "" {
		next.Phase = state.Phase
	}
	if state.Status != "" {
		next.Status = state.Status
	}
	if state.LastSuccessfulCheckpoint != "" {
		next.LastSuccessfulCheckpoint = state.LastSuccessfulCheckpoint
	}
	if state.NextRecommendedAction != "" {
		next.NextRecommendedAction = state.NextRecommendedAction
	}
	if state.Orchestration.Backend != "" {
		next.Orchestration = state.Orchestration
	}
	next.AgentResults = state.AgentResults
	next.ProviderAttempts = state.ProviderAttempts
	next.RetryLimits = state.RetryLimits
	next.CompletionGates = state.CompletionGates
	next.Operations = state.Operations
	if len(next.Operations.AccessControls.AllowedCancellationActors) == 0 {
		next.Operations.AccessControls.AllowedCancellationActors = []string{"user", "coordinator"}
	}
	return next
}

func updateTaskArtifactState(task model.Task, mutate func(*taskArtifactState)) error {
	info, err := os.Stat(task.Repository)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect repository for task artifacts: %w", err)
	}
	if !info.IsDir() {
		return nil
	}
	state, root, err := readTaskArtifactState(task)
	if err != nil {
		if os.IsNotExist(err) {
			state = newTaskArtifactState(task)
			root = taskArtifactRoot(task)
		} else {
			return err
		}
	}
	mutate(&state)
	state.UpdatedAt = time.Now().UTC()
	return writeTaskArtifactState(root, state)
}

func recordArtifactTransition(task model.Task, to model.TaskState, summary string, at time.Time) error {
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		state.Phase = to
		state.Status = to
		if to != model.TaskFailed || !strings.Contains(state.NextRecommendedAction, "Retry") {
			state.NextRecommendedAction = nextActionForState(to)
		}
		if isSuccessfulCheckpoint(to) {
			state.LastSuccessfulCheckpoint = strings.ToLower(string(to))
		}
		if isTerminalState(to) {
			satisfyGate(state, "terminal_state_recorded", summary, at)
		}
	})
}

func recordArtifactAgentResult(task model.Task, role, adapter, status string, result agents.Result, started, completed time.Time, data map[string]any) error {
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		item := agentResultArtifact{
			Role:        role,
			Adapter:     adapter,
			Status:      status,
			SessionID:   result.SessionID,
			ExitCode:    result.ExitCode,
			Error:       result.Error,
			Usage:       result.Usage,
			Summary:     summarizeAgentResponse(result.Response),
			StartedAt:   started,
			CompletedAt: completed,
			Data:        data,
		}
		if result.Err != nil && item.Error == "" {
			item.Error = result.Err.Error()
		}
		state.AgentResults = append(state.AgentResults, item)
		state.Operations.Observability.RunCount++
		state.Operations.Observability.TotalTokens += result.Usage.TotalTokens
		state.Operations.Observability.LastAgentAt = completed
		state.Operations.Observability.LastEventAt = completed
		satisfyGate(state, "agent_result_recorded", "state.json", completed)
		if status == "FAILED" {
			state.Operations.Observability.FailedRunCount++
			limit := state.RetryLimits[role]
			limit.Attempts++
			state.RetryLimits[role] = limit
			state.NextRecommendedAction = retryNextAction(role, limit)
		}
	})
}

func recordArtifactProviderAttempt(task model.Task, role, adapter, selectedModel, status string, result agents.Result, started, completed time.Time, errorDetail map[string]any, tracePaths []string) error {
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		item := providerAttemptArtifact{
			Attempt:     len(state.ProviderAttempts) + 1,
			Role:        role,
			Adapter:     adapter,
			Model:       selectedModel,
			Status:      status,
			SessionID:   result.SessionID,
			ExitCode:    result.ExitCode,
			Error:       result.Error,
			ErrorDetail: errorDetail,
			TracePaths:  tracePaths,
			StartedAt:   started,
			CompletedAt: completed,
		}
		if result.Err != nil && item.Error == "" {
			item.Error = result.Err.Error()
		}
		state.ProviderAttempts = append(state.ProviderAttempts, item)
		state.Operations.Observability.LastEventAt = completed
	})
}

func recordArtifactOrchestrationDecision(task model.Task, decision DurableOrchestrationDecision) error {
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		state.Orchestration = decision
	})
}

func recordArtifactCostLimit(task model.Task, maxTotalTokens int64) error {
	if maxTotalTokens <= 0 {
		return nil
	}
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		state.Operations.CostLimits.MaxTotalTokens = maxTotalTokens
	})
}

func recordArtifactCancellation(task model.Task, actor, reason, idempotencyKey string, at time.Time) error {
	return updateTaskArtifactState(task, func(state *taskArtifactState) {
		state.Operations.Cancellation = taskCancellationState{
			Requested:      true,
			RequestedBy:    actor,
			Reason:         reason,
			IdempotencyKey: idempotencyKey,
			RequestedAt:    at,
		}
		state.Operations.Observability.LastEventAt = at
		if idempotencyKey != "" {
			state.Operations.Idempotency = append(state.Operations.Idempotency, taskIdempotencyRecord{Operation: "cancel", Key: idempotencyKey, Status: "applied", RecordedAt: at})
		}
	})
}

func satisfyGate(state *taskArtifactState, id, evidence string, at time.Time) {
	for i := range state.CompletionGates {
		if state.CompletionGates[i].ID == id {
			state.CompletionGates[i].Satisfied = true
			state.CompletionGates[i].Evidence = evidence
			state.CompletionGates[i].SatisfiedAt = at
			return
		}
	}
}

func nextActionForState(state model.TaskState) string {
	switch state {
	case model.TaskPlanning:
		return "Capture a concrete plan and acceptance criteria."
	case model.TaskRunning:
		return "Record structured agent results and run focused validation."
	case model.TaskVerifying:
		return "Run verification commands and attach evidence."
	case model.TaskReviewing:
		return "Review correctness, scope, maintainability, and acceptance criteria."
	case model.TaskCompleted:
		return "Task is complete; preserve artifacts as recovery evidence."
	case model.TaskFailed:
		return "Inspect the failed structured result and retry only within the recorded retry limit."
	case model.TaskWaitingForUser:
		return "Wait for the user decision recorded in questions.md."
	case model.TaskBlocked:
		return "Escalate with evidence before attempting more work."
	default:
		return ""
	}
}

func isSuccessfulCheckpoint(state model.TaskState) bool {
	return state == model.TaskPlanning || state == model.TaskRunning || state == model.TaskVerifying || state == model.TaskReviewing || state == model.TaskCompleted
}

func isTerminalState(state model.TaskState) bool {
	return state == model.TaskCompleted || state == model.TaskFailed || state == model.TaskCancelled || state == model.TaskBlocked
}

func retryNextAction(role string, limit retryLimit) string {
	if limit.MaxAttempts > 0 && limit.Attempts >= limit.MaxAttempts {
		return fmt.Sprintf("%s retry limit reached; escalate with evidence instead of retrying indefinitely.", role)
	}
	return fmt.Sprintf("Retry %s if the failure is transient; attempts used: %d of %d.", role, limit.Attempts, limit.MaxAttempts)
}

func summarizeAgentResponse(response string) string {
	response = strings.Join(strings.Fields(response), " ")
	const max = 240
	if len(response) <= max {
		return response
	}
	return response[:max-15] + " [truncated]"
}
