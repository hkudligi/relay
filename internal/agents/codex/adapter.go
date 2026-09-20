package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harsha/relay/internal/agents"
)

type Adapter struct{ Command string }

func New(command string) *Adapter {
	if command == "" {
		command = "codex"
	}
	return &Adapter{Command: command}
}
func (a *Adapter) Name() string { return "codex" }
func (a *Adapter) Detect(ctx context.Context) agents.Installation {
	return detectAdapter(ctx, a.Command)
}
func (a *Adapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{NonInteractive: true, StructuredOutput: true, SessionResume: true, Streaming: true, Cancellation: true, UsageReporting: true, FileEditing: true}
}

func (a *Adapter) Start(ctx context.Context, r agents.Request) (agents.Run, error) {
	args := []string{"exec", "--json", "--color", "never", "--sandbox", sandbox(r.Sandbox), "-C", r.Workspace}
	if r.SkipGitCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if model := agents.ExecutionModel(r.Model); model != "" && model != "UNKNOWN" {
		args = append(args, "--model", model)
	}
	args = append(args, r.Prompt)
	return start(ctx, a.Command, args, r.Workspace)
}
func (a *Adapter) Resume(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("codex session id is required")
	}
	args := []string{"exec", "resume", "--json"}
	if r.SkipGitCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if model := agents.ExecutionModel(r.Model); model != "" && model != "UNKNOWN" {
		args = append(args, "--model", model)
	}
	args = append(args, session, r.Prompt)
	return start(ctx, a.Command, args, r.Workspace)
}
func sandbox(s agents.Sandbox) string {
	if s == agents.SandboxWorkspaceWrite {
		return "workspace-write"
	}
	return "read-only"
}

func normalize(line []byte) (agents.Event, bool, error) {
	var raw struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
		Item     struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
		Usage struct {
			Input     int64 `json:"input_tokens"`
			Cached    int64 `json:"cached_input_tokens"`
			Output    int64 `json:"output_tokens"`
			Reasoning int64 `json:"reasoning_output_tokens"`
		} `json:"usage"`
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return agents.Event{}, false, fmt.Errorf("decode codex event: %w", err)
	}
	switch raw.Type {
	case "thread.started":
		return agents.Event{Kind: agents.EventSession, Type: raw.Type, SessionID: raw.ThreadID}, true, nil
	case "item.completed":
		if raw.Item.Type == "agent_message" {
			return agents.Event{Kind: agents.EventMessage, Type: raw.Type, Message: raw.Item.Text}, true, nil
		}
		return agents.Event{Kind: agents.EventProgress, Type: raw.Item.Type}, true, nil
	case "turn.completed":
		u := agents.Usage{InputTokens: raw.Usage.Input, CachedTokens: raw.Usage.Cached, OutputTokens: raw.Usage.Output, ReasoningTokens: raw.Usage.Reasoning}
		// Codex reports reasoning output as a subset of output tokens.
		u.TotalTokens = u.InputTokens + u.OutputTokens
		return agents.Event{Kind: agents.EventResult, Type: raw.Type, Usage: u}, true, nil
	case "turn.failed", "error":
		message := parseErrorMessage(raw.Error)
		if message == "" {
			message = raw.Message
		}
		return agents.Event{Kind: agents.EventError, Type: raw.Type, Message: message}, true, nil
	default:
		return agents.Event{Kind: agents.EventProgress, Type: raw.Type}, raw.Type != "", nil
	}
}

func parseErrorMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msgObj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &msgObj); err == nil && msgObj.Message != "" {
		return msgObj.Message
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil && str != "" {
		return str
	}
	return string(raw)
}
