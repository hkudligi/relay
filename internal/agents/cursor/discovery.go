package cursor

import (
	"context"
	"encoding/json"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
)

const cursorModelSource = "cursor agent --list-models; provider quota percentage unavailable"

// DiscoverModels lists models the Cursor CLI reports for this account.
// Cursor does not expose remaining quota percentages, so those values stay UNKNOWN.
func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "cursor --version", installation.Error)}
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(discoveryCtx, a.command(), "--list-models").Output()
	if err != nil {
		output, err = exec.CommandContext(discoveryCtx, a.command(), "models").Output()
	}
	if err != nil {
		row := agents.UnknownAvailability(installation, cursorModelSource, err.Error())
		row.Usable = installation.Available
		return []agents.ModelAvailability{row}
	}
	names := parseModelNames(output)
	if len(names) == 0 {
		names = []string{"UNKNOWN"}
	}
	sort.Strings(names)
	rows := make([]agents.ModelAvailability, 0, len(names))
	for _, name := range names {
		rows = append(rows, agents.ModelAvailability{
			Agent:      a.Name(),
			Model:      name,
			Version:    installation.Version,
			Installed:  true,
			Usable:     true,
			Confidence: agents.ConfidenceUnknown,
			DataSource: cursorModelSource,
		})
	}
	return rows
}

func parseModelNames(output []byte) []string {
	var payload struct {
		Models []struct {
			ID    string `json:"id"`
			Slug  string `json:"slug"`
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	var names []string
	seen := map[string]bool{}
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		names = append(names, id)
	}
	if json.Unmarshal(output, &payload) == nil {
		for _, item := range payload.Models {
			for _, id := range []string{item.ID, item.Slug, item.Model, item.Name} {
				if id != "" {
					add(id)
					break
				}
			}
		}
	}
	if len(names) == 0 {
		var list []string
		if json.Unmarshal(output, &list) == nil {
			for _, id := range list {
				add(id)
			}
		}
	}
	if len(names) > 0 {
		return names
	}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimLeft(line, "-*•")
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, " ") && !strings.Contains(line, "/") {
			if strings.HasPrefix(strings.ToLower(line), "available") || strings.HasPrefix(strings.ToLower(line), "model") {
				continue
			}
			if strings.Contains(line, " ") {
				continue
			}
		}
		if line == "" {
			continue
		}
		add(line)
	}
	return names
}
