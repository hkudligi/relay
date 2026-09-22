package agy

import (
	"context"
	"encoding/json"
	"log"
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
	if err := json.Unmarshal(output, &payload); err == nil && len(payload.Models) > 0 {
		for _, item := range payload.Models {
			if strings.TrimSpace(item.ID) != "" {
				names = append(names, item.ID)
			}
		}
	} else {
		// Fallback: agy may output a plain list (e.g., "model-id   description")
		cleanOutput := strings.ReplaceAll(string(output), "\r", "\n")
		lines := strings.Split(cleanOutput, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			// Remove spinner progress text if present, but keep any model info that may follow
			if strings.Contains(line, "Fetching available models") {
				line = strings.ReplaceAll(line, "Fetching available models", "")
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
			}
			// Remove any leading non‑ASCII/control characters (spinner glyphs) then take the first token as the model ID
			cleanLine := strings.TrimLeftFunc(line, func(r rune) bool {
				return r < 32 || r > 126
			})
			fields := strings.Fields(cleanLine)
			for _, f := range fields {
				if strings.Contains(f, "-") {
					names = append(names, f)
				}
			}
			// Debug: log the processed line and any names found
			log.Printf("agy fallback line=%q fields=%v names=%v", line, fields, names)

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
		rows = append(rows, agents.ModelAvailability{Agent: a.Name(), Model: name, Version: installation.Version, Installed: true, Usable: true, Confidence: agents.ConfidenceExact, DataSource: "agy models; provider quota unavailable"})
	}
	return rows
}
