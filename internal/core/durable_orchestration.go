package core

// DurableOrchestrationBackend identifies the execution engine that should own
// a task graph's lifecycle.
type DurableOrchestrationBackend string

const (
	OrchestrationBackendLocal    DurableOrchestrationBackend = "local"
	OrchestrationBackendTemporal DurableOrchestrationBackend = "temporal"
)

// DurableOrchestrationHints describe requirements that are not always visible
// from the in-memory task graph.
type DurableOrchestrationHints struct {
	ProcessIndependentRetries bool
	LongWait                  bool
	Timer                     bool
	WaitingForUser            bool
	ReliableWorkerRecovery    bool
}

// DurableOrchestrationDecision is persisted before agent work starts so a
// future workflow worker can resume from the same backend decision.
type DurableOrchestrationDecision struct {
	Backend      DurableOrchestrationBackend `json:"backend"`
	Durable      bool                        `json:"durable"`
	Reasons      []string                    `json:"reasons,omitempty"`
	Requirements []string                    `json:"requirements,omitempty"`
}

// AssessDurableOrchestration selects local in-process orchestration for small,
// bounded graphs and Temporal for work that needs durable workflow semantics.
func AssessDurableOrchestration(tasks []AgentTask, hints DurableOrchestrationHints) DurableOrchestrationDecision {
	var requirements []string
	if hints.ProcessIndependentRetries {
		requirements = append(requirements, "process-independent retries")
	}
	if hints.LongWait {
		requirements = append(requirements, "long waits")
	}
	if hints.Timer {
		requirements = append(requirements, "timers")
	}
	if hints.WaitingForUser {
		requirements = append(requirements, "human-input signals")
	}
	if hints.ReliableWorkerRecovery {
		requirements = append(requirements, "reliable worker recovery")
	}
	if hasParallelBranches(tasks) {
		requirements = append(requirements, "parallel branches")
	}
	if len(requirements) > 0 {
		return DurableOrchestrationDecision{
			Backend:      OrchestrationBackendTemporal,
			Durable:      true,
			Reasons:      []string{"task graph requires durable orchestration"},
			Requirements: requirements,
		}
	}
	return DurableOrchestrationDecision{
		Backend: OrchestrationBackendLocal,
		Durable: false,
		Reasons: []string{"bounded task graph can run in the local coordinator"},
	}
}

func localOrchestrationDecision() DurableOrchestrationDecision {
	return AssessDurableOrchestration(nil, DurableOrchestrationHints{})
}

func hasParallelBranches(tasks []AgentTask) bool {
	if len(tasks) < 2 {
		return false
	}
	for i := range tasks {
		for j := i + 1; j < len(tasks); j++ {
			if independent(tasks[i], tasks[j]) {
				return true
			}
		}
	}
	return false
}

func independent(a, b AgentTask) bool {
	return !containsString(a.DependsOn, b.ID) && !containsString(b.DependsOn, a.ID)
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
