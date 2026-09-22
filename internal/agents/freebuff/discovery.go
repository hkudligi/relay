package freebuff

import (
	"context"

	"github.com/harsha/relay/internal/agents"
)

const freebuffQuotaSource = "freebuff compiled model catalog; free-tier CLI exposes no quota surface"

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

// DiscoverModels inventories the Freebuff CLI for the pre-task model listing.
// Freebuff exposes no quota API or model-list command, so model identity comes
// from its stable published catalog while quota remains deliberately UNKNOWN.
func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "freebuff --version", installation.Error)}
	}
	rows := make([]agents.ModelAvailability, 0, len(freebuffModelCatalog))
	for _, model := range freebuffModelCatalog {
		rows = append(rows, agents.ModelAvailability{
			Agent:      a.Name(),
			Model:      model,
			Version:    installation.Version,
			Installed:  true,
			Usable:     true,
			Confidence: agents.ConfidenceUnknown,
			DataSource: freebuffQuotaSource,
		})
	}
	return rows
}
