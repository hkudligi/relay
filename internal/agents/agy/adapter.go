package agy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/harsha/relay/internal/agents"
)

type Adapter struct{ Command string }

func New(command string) *Adapter {
	if command == "" {
		command = "agy"
	}
	return &Adapter{Command: command}
}
func (a *Adapter) Name() string { return "agy" }
func (a *Adapter) Detect(ctx context.Context) agents.Installation {
	return agents.Detect(ctx, "agy", a.Command)
}
func (a *Adapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{NonInteractive: true, StructuredOutput: true, SessionResume: true, Streaming: true, Cancellation: true, UsageReporting: true, FileEditing: true}
}

func (a *Adapter) Start(ctx context.Context, r agents.Request) (agents.Run, error) {
	return a.run(ctx, "", r)
}
func (a *Adapter) Resume(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("agy conversation id is required")
	}
	return a.run(ctx, session, r)
}
func (a *Adapter) run(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	args := []string{"-p", r.Prompt, "--output-format", "stream-json"}
	if model := agents.ExecutionModel(r.Model); model != "" && model != "UNKNOWN" {
		args = append(args, "--model", model)
	}
	if session != "" {
		args = append(args, "--conversation", session)
	}
	if r.Sandbox == agents.SandboxReadOnly {
		args = append(args, "--mode", "plan")
	}
	args = append(args, "--sandbox")
	return agents.StartProcess(ctx, a.Command, args, r.Workspace, normalize)
}

type usage struct {
	Input    int64 `json:"input_tokens"`
	Output   int64 `json:"output_tokens"`
	Thinking int64 `json:"thinking_tokens"`
	Cached   int64 `json:"cache_read_tokens"`
	Total    int64 `json:"total_tokens"`
}

func commonUsage(u usage) agents.Usage {
	return agents.Usage{InputTokens: u.Input, CachedTokens: u.Cached, OutputTokens: u.Output, ReasoningTokens: u.Thinking, TotalTokens: u.Total}
}

func normalize(line []byte) (agents.Event, bool, error) {
	var raw struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		Init           struct {
			ConversationID string `json:"conversation_id"`
		} `json:"init"`
		Step struct {
			StepType string `json:"step_type"`
			State    string `json:"state"`
			Text     string `json:"text_delta"`
			Usage    usage  `json:"usage"`
		} `json:"step_update"`
		Result struct {
			ConversationID string `json:"conversation_id"`
			Status         string `json:"status"`
			Response       string `json:"response"`
			Error          string `json:"error"`
			Usage          usage  `json:"usage"`
		} `json:"result"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return agents.Event{}, false, fmt.Errorf("decode agy event: %w", err)
	}
	switch raw.Event {
	case "init":
		id := raw.ConversationID
		if id == "" {
			id = raw.Init.ConversationID
		}
		return agents.Event{Kind: agents.EventSession, Type: raw.Event, SessionID: id}, true, nil
	case "step_update":
		kind := agents.EventProgress
		if raw.Step.StepType == "agent_response" && raw.Step.Text != "" {
			kind = agents.EventMessage
		}
		return agents.Event{Kind: kind, Type: raw.Step.StepType, Message: raw.Step.Text, Usage: commonUsage(raw.Step.Usage), Data: map[string]any{"state": raw.Step.State}}, true, nil
	case "result":
		if raw.Result.Status != "SUCCESS" {
			return agents.Event{Kind: agents.EventError, Type: raw.Result.Status, SessionID: raw.Result.ConversationID, Message: raw.Result.Error}, true, nil
		}
		return agents.Event{Kind: agents.EventResult, Type: raw.Result.Status, SessionID: raw.Result.ConversationID, Message: raw.Result.Response, Usage: commonUsage(raw.Result.Usage)}, true, nil
	default:
		return agents.Event{}, false, nil
	}
}
