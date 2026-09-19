// Package agents defines the vendor-neutral boundary between rly's control
// plane and coding-agent CLIs.
package agents

import (
	"context"
	"time"
)

type Sandbox string

const (
	SandboxReadOnly       Sandbox = "read-only"
	SandboxWorkspaceWrite Sandbox = "workspace-write"
)

type Installation struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`
}

type Capabilities struct {
	NonInteractive   bool `json:"non_interactive"`
	StructuredOutput bool `json:"structured_output"`
	SessionResume    bool `json:"session_resume"`
	Streaming        bool `json:"streaming"`
	Cancellation     bool `json:"cancellation"`
	UsageReporting   bool `json:"usage_reporting"`
	FileEditing      bool `json:"file_editing"`
}

type Request struct {
	Prompt       string
	Workspace    string
	Sandbox      Sandbox
	SkipGitCheck bool
}

type Usage struct {
	InputTokens     int64 `json:"input_tokens,omitempty"`
	CachedTokens    int64 `json:"cached_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens     int64 `json:"total_tokens,omitempty"`
}

type EventKind string

const (
	EventSession  EventKind = "session"
	EventProgress EventKind = "progress"
	EventMessage  EventKind = "message"
	EventUsage    EventKind = "usage"
	EventError    EventKind = "error"
	EventResult   EventKind = "result"
)

type Event struct {
	Kind      EventKind      `json:"kind"`
	Type      string         `json:"type,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Message   string         `json:"message,omitempty"`
	Usage     Usage          `json:"usage,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
	Time      time.Time      `json:"time"`
}

type Result struct {
	SessionID string `json:"session_id,omitempty"`
	Response  string `json:"response,omitempty"`
	Usage     Usage  `json:"usage,omitempty"`
	ExitCode  int    `json:"exit_code"`
	Error     string `json:"error,omitempty"`
	Err       error  `json:"-"`
}

type Run interface {
	Events() <-chan Event
	Wait() Result
	Cancel() error
}

type Adapter interface {
	Name() string
	Detect(context.Context) Installation
	Capabilities() Capabilities
	Start(context.Context, Request) (Run, error)
	Resume(context.Context, string, Request) (Run, error)
}
