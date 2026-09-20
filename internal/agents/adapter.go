// Package agents defines the vendor-neutral boundary between rly's control
// plane and coding-agent CLIs.
package agents

import (
	"context"
	"strings"
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

const reserveModelSuffix = "#reserve"

// ModelAvailability is one row in the pre-task agent/model inventory. A nil
// RemainingPercent is deliberately serialized as null and rendered as UNKNOWN;
// callers must never infer quota from installation or model availability.
//
// Luna and Luna Reserve are different pools. Ordinary catalog rows use the
// model slug. A reserve fallback is a separate row with Reserve=true; its
// Model is the execution slug plus "#reserve" so routing can keep them apart
// while adapters still launch the underlying model.
type ModelAvailability struct {
	Agent            string   `json:"agent"`
	Model            string   `json:"model"`
	Version          string   `json:"version,omitempty"`
	Installed        bool     `json:"installed"`
	Usable           bool     `json:"usable"`
	Reserve          bool     `json:"reserve,omitempty"`
	RemainingPercent *float64 `json:"remaining_percent"`
	Confidence       string   `json:"confidence"`
	DataSource       string   `json:"data_source"`
	Error            string   `json:"error,omitempty"`
}

// ExecutionModel is the provider slug to pass to the agent CLI. Reserve
// inventory IDs are not real model names.
func ExecutionModel(model string) string {
	return strings.TrimSuffix(model, reserveModelSuffix)
}

// IsReserveModel reports whether an inventory/routing model ID is a reserve pool.
func IsReserveModel(model string) bool {
	return strings.HasSuffix(model, reserveModelSuffix)
}

// ReserveModelID builds a distinct inventory ID for a model's reserve pool.
func ReserveModelID(executionModel string) string {
	return executionModel + reserveModelSuffix
}

const (
	ConfidenceExact   = "EXACT"
	ConfidenceUnknown = "UNKNOWN"
)

// ModelDiscoverer is optional so third-party adapters implementing the
// original Adapter interface continue to work. The coordinator supplies a
// conservative UNKNOWN row for adapters that do not implement it.
type ModelDiscoverer interface {
	DiscoverModels(context.Context) []ModelAvailability
}

func UnknownAvailability(installation Installation, source, detail string) ModelAvailability {
	return ModelAvailability{
		Agent: installation.Name, Model: "UNKNOWN", Version: installation.Version, Installed: installation.Available,
		Usable: installation.Available, Confidence: ConfidenceUnknown, DataSource: source, Error: detail,
	}
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
	Prompt    string
	Workspace string
	// Model is the provider model selected by the coordinator. Empty and
	// UNKNOWN mean use the adapter's default model.
	Model        string
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
