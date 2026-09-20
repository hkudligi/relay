package cursor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/harsha/relay/internal/agents"
)

type Adapter struct{ Command string }

func New(command string) *Adapter {
	if command == "" {
		command = "agent"
	}
	return &Adapter{Command: command}
}

func (a *Adapter) Name() string { return "cursor" }

func (a *Adapter) command() string {
	if a.Command != "" && a.Command != "agent" {
		return a.Command
	}
	if _, err := exec.LookPath(a.Command); err == nil {
		return a.Command
	}
	if _, err := exec.LookPath("cursor-agent"); err == nil {
		return "cursor-agent"
	}
	if a.Command != "" {
		return a.Command
	}
	return "agent"
}

func (a *Adapter) Detect(ctx context.Context) agents.Installation {
	command := a.command()
	installation := agents.Detect(ctx, "cursor", command)
	installation.Name = "cursor"
	installation.Command = command
	return installation
}

func (a *Adapter) Capabilities() agents.Capabilities {
	return agents.Capabilities{NonInteractive: true, StructuredOutput: true, SessionResume: true, Streaming: true, Cancellation: true, UsageReporting: false, FileEditing: true}
}

func (a *Adapter) Start(ctx context.Context, r agents.Request) (agents.Run, error) {
	return a.run(ctx, "", r)
}

func (a *Adapter) Resume(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	if session == "" {
		return nil, fmt.Errorf("cursor session id is required")
	}
	return a.run(ctx, session, r)
}

func (a *Adapter) run(ctx context.Context, session string, r agents.Request) (agents.Run, error) {
	args := []string{"-p", "--output-format", "stream-json", "--trust"}
	if r.Workspace != "" {
		args = append(args, "--workspace", r.Workspace)
	}
	if model := agents.ExecutionModel(r.Model); model != "" && model != "UNKNOWN" {
		args = append(args, "--model", model)
	}
	if r.Sandbox == agents.SandboxReadOnly {
		args = append(args, "--mode", "plan")
	} else {
		args = append(args, "--force")
	}
	if session != "" {
		args = append(args, "--resume", session)
	}
	args = append(args, r.Prompt)
	return agents.StartProcess(ctx, a.command(), args, r.Workspace, normalize)
}

func normalize(line []byte) (agents.Event, bool, error) {
	var raw struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		Model     string `json:"model"`
		Message   struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return agents.Event{}, false, fmt.Errorf("decode cursor event: %w", err)
	}
	switch raw.Type {
	case "system":
		if raw.Subtype == "init" {
			return agents.Event{Kind: agents.EventSession, Type: raw.Type, SessionID: raw.SessionID, Data: map[string]any{"model": raw.Model}}, true, nil
		}
		return agents.Event{Kind: agents.EventProgress, Type: raw.Subtype, SessionID: raw.SessionID}, true, nil
	case "assistant":
		text := assistantText(raw.Message.Content)
		if text == "" {
			return agents.Event{Kind: agents.EventProgress, Type: raw.Type, SessionID: raw.SessionID}, true, nil
		}
		return agents.Event{Kind: agents.EventMessage, Type: raw.Type, SessionID: raw.SessionID, Message: text}, true, nil
	case "tool_call":
		return agents.Event{Kind: agents.EventProgress, Type: raw.Subtype, SessionID: raw.SessionID}, true, nil
	case "result":
		if raw.IsError || raw.Subtype == "error" {
			message := strings.TrimSpace(raw.Result)
			if message == "" {
				message = "cursor agent failed"
			}
			return agents.Event{Kind: agents.EventError, Type: raw.Type, SessionID: raw.SessionID, Message: message}, true, nil
		}
		return agents.Event{Kind: agents.EventResult, Type: raw.Subtype, SessionID: raw.SessionID, Message: raw.Result}, true, nil
	case "user":
		return agents.Event{}, false, nil
	default:
		return agents.Event{Kind: agents.EventProgress, Type: raw.Type, SessionID: raw.SessionID}, raw.Type != "", nil
	}
}

func assistantText(content []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var parts []string
	for _, block := range content {
		if block.Type == "text" || block.Type == "" {
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "")
}
