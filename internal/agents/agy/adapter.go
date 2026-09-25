package agy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	} else {
		// Do not rely on Agy's default mode for implementation tasks. Without
		// an explicit write mode, the model can inspect the repository and
		// return SUCCESS without ever applying the requested edits.
		args = append(args, "--mode", "accept-edits")
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
		Model          string `json:"model"`
		ConversationID string `json:"conversation_id"`
		Message        string `json:"message"`
		ErrorMessage   string `json:"error_message"`
		Error          string `json:"error"`
		Init           struct {
			ConversationID string `json:"conversation_id"`
			Model          string `json:"model"`
		} `json:"init"`
		Step struct {
			StepType     string `json:"step_type"`
			State        string `json:"state"`
			Text         string `json:"text_delta"`
			Message      string `json:"message"`
			Error        string `json:"error"`
			ErrorMessage string `json:"error_message"`
			Usage        usage  `json:"usage"`
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
		model := raw.Model
		if model == "" {
			model = raw.Init.Model
		}
		data := map[string]any{}
		if model != "" {
			data["model"] = model
		}
		return agents.Event{Kind: agents.EventSession, Type: raw.Event, SessionID: id, Data: data}, true, nil
	case "step_update":
		message := raw.Step.Text
		if message == "" {
			message = raw.Step.Message
		}
		if message == "" {
			message = raw.Step.ErrorMessage
		}
		if message == "" {
			message = raw.Step.Error
		}
		if message == "" {
			message = raw.ErrorMessage
		}
		if message == "" {
			message = raw.Message
		}
		if message == "" {
			message = raw.Error
		}
		kind := agents.EventProgress
		switch {
		case raw.Step.StepType == "error_message" || raw.Step.ErrorMessage != "" || raw.Step.Error != "" || strings.EqualFold(raw.Step.State, "error") || strings.EqualFold(raw.Step.State, "failed"):
			kind = agents.EventError
		case raw.Step.StepType == "agent_response" && message != "":
			kind = agents.EventMessage
		}
		return agents.Event{Kind: kind, Type: raw.Step.StepType, Message: message, Usage: commonUsage(raw.Step.Usage), Data: map[string]any{"state": raw.Step.State}}, true, nil
	case "result":
		if raw.Result.Status != "SUCCESS" {
			return agents.Event{Kind: agents.EventError, Type: raw.Result.Status, SessionID: raw.Result.ConversationID, Message: raw.Result.Error}, true, nil
		}
		return agents.Event{Kind: agents.EventResult, Type: raw.Result.Status, SessionID: raw.Result.ConversationID, Message: raw.Result.Response, Usage: commonUsage(raw.Result.Usage)}, true, nil
	default:
		return agents.Event{}, false, nil
	}
}
