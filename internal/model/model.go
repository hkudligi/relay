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
