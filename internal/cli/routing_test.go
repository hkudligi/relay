package cli

import (
	"testing"

	"github.com/harsha/relay/internal/agents"
)

func TestFilterFailedModelRemovesUnknownProvider(t *testing.T) {
	remaining := 54.0
	inventory := []agents.ModelAvailability{
		{Agent: "agy", Model: "UNKNOWN", Usable: true},
		{Agent: "codex", Model: "gpt-5.6-luna", Usable: true, RemainingPercent: &remaining},
	}

	filtered := filterFailedModel(inventory, "agy", "")
	if len(filtered) != 1 || filtered[0].Agent != "codex" {
		t.Fatalf("filtered inventory = %+v, want only codex", filtered)
	}
}

func TestFilterFailedModelPreservesOtherKnownModels(t *testing.T) {
	inventory := []agents.ModelAvailability{
		{Agent: "agy", Model: "model-a", Usable: true},
		{Agent: "agy", Model: "model-b", Usable: true},
		{Agent: "codex", Model: "gpt-5.6-luna", Usable: true},
	}

	filtered := filterFailedModel(inventory, "agy", "model-a")
	if len(filtered) != 2 || filtered[0].Model != "model-b" || filtered[1].Agent != "codex" {
		t.Fatalf("filtered inventory = %+v, want model-b and codex", filtered)
	}
}

func TestFilterFailedModelKeepsLunaReserveWhenOrdinaryLunaFails(t *testing.T) {
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5.6-luna", Usable: false},
		{Agent: "codex", Model: "gpt-reserve", Usable: true, Reserve: true},
	}
	filtered := filterFailedModel(inventory, "codex", "gpt-5.6-luna")
	if len(filtered) != 1 || filtered[0].Model != "gpt-reserve" {
		t.Fatalf("filtered inventory = %+v, want gpt-reserve row", filtered)
	}
}
