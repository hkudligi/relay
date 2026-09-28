package freebuff

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
)

const freebuffQuotaSource = "freebuff TUI startup balance"

var freebuffBalancePattern = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*/\s*([0-9]+(?:\.[0-9]+)?)\s+freebucks?\s+remaining`)

// These are the provider model IDs used by Freebuff's current regular picker.
// The CLI does not expose a catalog command, so keep this list aligned with
// Freebuff's published catalog instead of collapsing the provider to UNKNOWN.
var freebuffModelCatalog = []string{
	"deepseek/deepseek-v4-flash",
	"meta/muse-spark-1.3-contributor",
	"mimo/mimo-v2.5",
	"openai/gpt-5.6-luna",
	"upstage/solar-pro4",
	"z-ai/glm-5.3-flash",
}

// DiscoverModels inventories Freebuff's stable model catalog and probes the
// startup TUI for the account-level Freebucks balance. The balance is shared
// across models, so it is copied to each model row for routing purposes.
func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "freebuff --version", installation.Error)}
	}
	remaining, confidence, source := a.probeBalance(ctx)
	rows := make([]agents.ModelAvailability, 0, len(freebuffModelCatalog))
	for _, model := range freebuffModelCatalog {
		rows = append(rows, agents.ModelAvailability{
			Agent:            a.Name(),
			Model:            model,
			Version:          installation.Version,
			Installed:        true,
			Usable:           true,
			RemainingPercent: remaining,
			Confidence:       confidence,
			DataSource:       source,
		})
	}
	return rows
}

func (a *Adapter) probeBalance(ctx context.Context) (*float64, string, string) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance unavailable: tmux not installed"
	}
	workspace, err := os.Getwd()
	if err != nil {
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance unavailable: cannot resolve workspace"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	session := fmt.Sprintf("rly-limit-probe-%d", time.Now().UnixNano())
	start := exec.CommandContext(probeCtx, tmux, "new-session", "-d", "-s", session, a.Command, "--cwd", workspace)
	if output, err := start.CombinedOutput(); err != nil {
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance unavailable: " + strings.TrimSpace(string(output))
	}
	defer func() { _ = exec.Command("tmux", "kill-session", "-t", session).Run() }()
	timer := time.NewTimer(3 * time.Second)
	select {
	case <-timer.C:
	case <-probeCtx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance probe timed out"
	}
	capture := exec.CommandContext(probeCtx, tmux, "capture-pane", "-p", "-t", session, "-S", "-120")
	output, err := capture.CombinedOutput()
	if err != nil {
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance unavailable: " + strings.TrimSpace(string(output))
	}
	remaining, ok := parseFreebuffBalance(string(output))
	if !ok {
		return nil, agents.ConfidenceUnknown, "freebuff TUI balance not present"
	}
	return &remaining, agents.ConfidenceExact, freebuffQuotaSource
}

func parseFreebuffBalance(output string) (float64, bool) {
	matches := freebuffBalancePattern.FindStringSubmatch(stripANSI(output))
	if len(matches) != 3 {
		return 0, false
	}
	remaining, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, false
	}
	maximum, err := strconv.ParseFloat(matches[2], 64)
	if err != nil || maximum <= 0 || remaining < 0 {
		return 0, false
	}
	if remaining > maximum {
		remaining = maximum
	}
	return remaining / maximum * 100, true
}
