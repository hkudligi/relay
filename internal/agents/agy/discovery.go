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

// DiscoverModels uses agy's provider-backed model command and /usage command.
// /usage reports shared buckets, so each discovered model is mapped to the
// provider bucket covering its model family.
func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "agy --version", installation.Error)}
	}
	modelsCtx, modelsCancel := context.WithTimeout(ctx, 8*time.Second)
	defer modelsCancel()
	output, err := exec.CommandContext(modelsCtx, a.Command, "models").Output()
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
	seen := map[string]bool{}
	var names []string
	addName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		names = append(names, name)
	}
	if err := json.Unmarshal(output, &payload); err == nil && len(payload.Models) > 0 {
		for _, item := range payload.Models {
			addName(item.ID)
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
			if len(fields) == 0 {
				continue
			}
			addName(strings.Trim(fields[0], ",:;"))
		}
	}
	if len(names) == 0 {
		// Model output is not a stable machine-readable contract. Preserve the
		// successful usability check without guessing model identifiers.
		names = []string{"UNKNOWN"}
	}
	// Keep the usage probe on its own deadline. Agy may spend several seconds
	// starting up and fetching models, and /usage is a separate invocation.
	usageCtx, usageCancel := context.WithTimeout(ctx, 8*time.Second)
	defer usageCancel()
	usageByFamily := discoverUsage(usageCtx, a.Command)
	sort.Strings(names)
	rows := make([]agents.ModelAvailability, 0, len(names))
	for _, name := range names {
		remaining, ok := usageByFamily[agyModelFamily(name)]
		confidence := agents.ConfidenceUnknown
		source := "agy models; /usage unavailable for model family"
		if ok {
			confidence = agents.ConfidenceExact
			source = "agy /usage grouped weekly limit"
		}
		rows = append(rows, agents.ModelAvailability{Agent: a.Name(), Model: name, Version: installation.Version, Installed: true, Usable: true, RemainingPercent: remaining, Confidence: confidence, DataSource: source})
	}
	return rows
}

type usageBucket struct {
	RemainingFraction *float64 `json:"remaining_fraction"`
}

type usageGroup struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Buckets     []usageBucket `json:"buckets"`
}

func discoverUsage(ctx context.Context, command string) map[string]*float64 {
	output, err := exec.CommandContext(ctx, command, "-p", "/usage", "--output-format", "stream-json").Output()
	if err != nil {
		return nil
	}
	var groups []usageGroup
	for _, line := range strings.Split(string(output), "\n") {
		var event struct {
			Command struct {
				Data struct {
					Groups []usageGroup `json:"groups"`
				} `json:"data"`
			} `json:"command"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && len(event.Command.Data.Groups) > 0 {
			groups = event.Command.Data.Groups
			break
		}
	}
	result := map[string]*float64{}
	for _, group := range groups {
		if len(group.Buckets) == 0 || group.Buckets[0].RemainingFraction == nil {
			continue
		}
		remaining := *group.Buckets[0].RemainingFraction * 100
		if remaining < 0 {
			remaining = 0
		}
		if remaining > 100 {
			remaining = 100
		}
		value := remaining
		family := agyModelFamily(group.Name + " " + group.Description)
		if family != "" {
			result[family] = &value
		}
	}
	return result
}

func agyModelFamily(name string) string {
	value := strings.ToLower(name)
	if strings.Contains(value, "gemini") {
		return "gemini"
	}
	if strings.Contains(value, "claude") || strings.Contains(value, "gpt") {
		return "third-party"
	}
	return ""
}
