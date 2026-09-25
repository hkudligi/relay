package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/harsha/relay/internal/agents"
)

const (
	delegationOpen  = "<rly-subagents>"
	delegationClose = "</rly-subagents>"
)

// DelegatedTask is the machine-readable handoff Freebuff can return when a
// task is better handled by first-class vendor-agent runs.
type DelegatedTask struct {
	ID             string   `json:"id"`
	Agent          string   `json:"agent"`
	Model          string   `json:"model,omitempty"`
	Prompt         string   `json:"prompt"`
	WorkspacePaths []string `json:"workspace_paths"`
	ParallelSafe   bool     `json:"parallel_safe"`
}

// DelegationInstructions tells a coordinator-only agent how to ask rly to
// launch child agents. The child CLIs must never be launched from Freebuff's
// own shell: rly owns their lifecycle and streams their progress.
func DelegationInstructions() string {
	return `

Delegated vendor-agent execution:
If this task is best split across independent vendor coding agents, do not
launch codex, agy, cursor, or other agent CLIs yourself. Instead, return one
machine-readable block named <rly-subagents> containing a JSON array with this
shape:
<rly-subagents>
[{"id":"short-unique-id","agent":"codex|agy|cursor","model":"optional-model","prompt":"complete task instructions","workspace_paths":["relative/path/to/file.go"],"parallel_safe":true}]
</rly-subagents>

Each child must own a disjoint set of files or directories. Set
parallel_safe=true only when the ownership sets do not overlap. Put all actual
implementation instructions in the child prompt. Do not claim child work is
complete; rly will launch the children and collect their results. Omit the
block when no delegation is needed.`
}

func parseDelegatedTasks(response string) ([]DelegatedTask, error) {
	start := strings.LastIndex(response, delegationOpen)
	if start < 0 {
		return nil, nil
	}
	relEnd := strings.Index(response[start+len(delegationOpen):], delegationClose)
	if relEnd < 0 {
		return nil, fmt.Errorf("unterminated %s block", delegationOpen)
	}
	body := strings.TrimSpace(response[start+len(delegationOpen) : start+len(delegationOpen)+relEnd])
	var tasks []DelegatedTask
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tasks); err != nil {
		return nil, fmt.Errorf("decode %s block: %w", delegationOpen, err)
	}
	if err := validateDelegatedTasks(tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// ParseDelegatedTasks extracts and validates the optional Freebuff delegation
// block from an agent response.
func ParseDelegatedTasks(response string) ([]DelegatedTask, error) {
	return parseDelegatedTasks(response)
}

func validateDelegatedTasks(tasks []DelegatedTask) error {
	seen := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task.ID == "" || seen[task.ID] {
			return fmt.Errorf("delegated task IDs must be unique and non-empty")
		}
		if task.Agent == "" || task.Prompt == "" {
			return fmt.Errorf("delegated task %q must specify agent and prompt", task.ID)
		}
		if len(task.WorkspacePaths) == 0 {
			return fmt.Errorf("delegated task %q must declare workspace_paths", task.ID)
		}
		seen[task.ID] = true
	}
	return nil
}

func delegatedAgentTasks(tasks []DelegatedTask, workspace string, sandbox agents.Sandbox) []AgentTask {
	result := make([]AgentTask, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, AgentTask{
			ID: task.ID, Agent: task.Agent,
			Request:  agents.Request{Prompt: task.Prompt, Workspace: workspace, Model: task.Model, Sandbox: sandbox},
			ReadOnly: false, ParallelSafe: task.ParallelSafe, WorkspacePaths: task.WorkspacePaths,
		})
	}
	return result
}

// DelegatedAgentTasks converts validated declarations into scheduler tasks.
func DelegatedAgentTasks(tasks []DelegatedTask, workspace string, sandbox agents.Sandbox) []AgentTask {
	return delegatedAgentTasks(tasks, workspace, sandbox)
}
