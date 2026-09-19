package codex

import (
	"context"

	"github.com/harsha/relay/internal/agents"
)

// These small bridges keep process execution private to the agents package
// while adapters remain independently testable packages.
func start(ctx context.Context, command string, args []string, dir string) (agents.Run, error) {
	return agents.StartProcess(ctx, command, args, dir, normalize)
}
func detectAdapter(ctx context.Context, command string) agents.Installation {
	return agents.Detect(ctx, "codex", command)
}
