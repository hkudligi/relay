package agy

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
)

// DiscoverModels uses agy's provider-backed model command when it returns
// structured output. Current agy releases do not expose quota percentages, so
// those values are intentionally UNKNOWN.
func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "agy --version", installation.Error)}
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(discoveryCtx, a.Command, "models").Output()
	if err != nil {
		row := agents.UnknownAvailability(installation, "agy models; quota API unavailable", err.Error())
		// Failure to enumerate models does not prove that the provider is
		// exhausted. Keep the installed adapter available with unknown quota so
		// routing can use it as a runtime fallback when another provider fails.
		row.Usable = installation.Available
		return []agents.ModelAvailability{row}
	}
	var payload struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	var names []string
	if json.Unmarshal(output, &payload) == nil {
		for _, item := range payload.Models {
			if strings.TrimSpace(item.ID) != "" {
				names = append(names, item.ID)
			}
		}
	}
	if len(names) == 0 {
		// Model output is not a stable machine-readable contract. Preserve the
		// successful usability check without guessing model identifiers.
		names = []string{"UNKNOWN"}
	}
	sort.Strings(names)
	rows := make([]agents.ModelAvailability, 0, len(names))
	for _, name := range names {
		rows = append(rows, agents.ModelAvailability{Agent: a.Name(), Model: name, Version: installation.Version, Installed: true, Usable: true, Confidence: agents.ConfidenceUnknown, DataSource: "agy models; provider quota percentage unavailable"})
	}
	return rows
}
