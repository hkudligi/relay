package model

import "time"

type TaskState string

const (
	TaskCreated        TaskState = "CREATED"
	TaskPlanning       TaskState = "PLANNING"
	TaskRunning        TaskState = "RUNNING"
	TaskVerifying      TaskState = "VERIFYING"
	TaskReviewing      TaskState = "REVIEWING"
	TaskCompleted      TaskState = "COMPLETED"
	TaskWaitingForUser TaskState = "WAITING_FOR_USER"
	TaskBlocked        TaskState = "BLOCKED"
	TaskFailed         TaskState = "FAILED"
	TaskCancelled      TaskState = "CANCELLED"
)

type Task struct {
	ID         string    `json:"id"`
	Repository string    `json:"repository"`
	Objective  string    `json:"objective"`
	State      TaskState `json:"state"`
	Version    int64     `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Event struct {
	ID        string         `json:"id"`
	TaskID    string         `json:"task_id"`
	Sequence  int64          `json:"sequence"`
	Type      string         `json:"type"`
	Actor     string         `json:"actor"`
	Summary   string         `json:"summary"`
	Data      map[string]any `json:"data,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type Status struct {
	Repository string `json:"repository"`
	Task       *Task  `json:"task,omitempty"`
}

type RunRecord struct {
	ID          string         `json:"id"`
	TaskID      string         `json:"task_id"`
	Adapter     string         `json:"adapter"`
	SessionID   string         `json:"session_id,omitempty"`
	Status      string         `json:"status"`
	ExitCode    int            `json:"exit_code"`
	Response    string         `json:"response,omitempty"`
	Usage       map[string]any `json:"usage,omitempty"`
	StartedAt   time.Time      `json:"started_at"`
	CompletedAt time.Time      `json:"completed_at"`
}

// ProjectMemory is a durable, repository-scoped fact or decision that is
// supplied to agents on future tasks.
type ProjectMemory struct {
	Repository   string    `json:"repository"`
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	SourceTaskID string    `json:"source_task_id,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// MemoryUpdate is the constrained format agents use to maintain project
// memory after a successful task.
type MemoryUpdate struct {
	Upsert []MemoryEntry `json:"upsert,omitempty"`
	Delete []string      `json:"delete,omitempty"`
}

type MemoryEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// CandidateScore contains the explainable evaluation score for an agent
// candidate considered during task routing.
type CandidateScore struct {
	Agent            string         `json:"agent"`
	SelectedModel    string         `json:"selected_model,omitempty"`
	Eligible         bool           `json:"eligible"`
	Exclusion        string         `json:"exclusion,omitempty"`
	TotalScore       float64        `json:"total_score"`
	CapabilityFit    float64        `json:"capability_fit"`
	QuotaHeadroom    float64        `json:"quota_headroom"`
	SessionValue     float64        `json:"session_value"`
	Reliability      float64        `json:"reliability"`
	CostFit          float64        `json:"cost_fit"`
	RemainingPercent *float64       `json:"remaining_percent,omitempty"`
	QuotaConfidence  string         `json:"quota_confidence"`
	Details          map[string]any `json:"details,omitempty"`
}

// RouteDecision is the explainable decision produced by the coordinator router
// selecting the best agent for a task role based on token availability and efficacy.
type RouteDecision struct {
	Role            string           `json:"role"`
	Objective       string           `json:"objective"`
	SelectedAgent   string           `json:"selected_agent"`
	SelectedModel   string           `json:"selected_model,omitempty"`
	Rationale       string           `json:"rationale"`
	Strategy        string           `json:"strategy"`
	CandidateScores []CandidateScore `json:"candidate_scores"`
}
