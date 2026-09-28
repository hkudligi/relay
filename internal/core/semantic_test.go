package core

import (
	"context"
	"testing"

	"github.com/harsha/relay/internal/agents"
)

func TestDeterministicSemanticLayerClassifiesRoutingMonitoringTask(t *testing.T) {
	layer := DeterministicSemanticLayer{}
	profile, err := layer.AnalyzeTask(context.Background(), SemanticTaskInput{
		Role:      RolePlanning,
		Objective: "Come up with a semantic layer for intelligent routing and monitoring using BERT or a local micro LLM",
	})
	if err != nil {
		t.Fatalf("AnalyzeTask error = %v", err)
	}
	if profile.TaskKind != "design" {
		t.Fatalf("TaskKind = %q, want design", profile.TaskKind)
	}
	assertContains(t, profile.Domains, "semantic-routing")
	assertContains(t, profile.Domains, "monitoring")
	assertContains(t, profile.Operations, "classify")
	assertContains(t, profile.RequiredCapabilities, SemanticCapabilityStructuredOutput)
}

func TestLocalSemanticLayerMergesBestExample(t *testing.T) {
	layer := NewLocalSemanticLayer([]SemanticExample{
		{
			Text:                 "watchdog stalled background workflow checkpoint resume cancellation",
			Domains:              []string{"monitoring"},
			Operations:           []string{"monitor", "orchestrate"},
			RequiredCapabilities: []string{SemanticCapabilitySessionResume, SemanticCapabilityCancellation, SemanticCapabilityLongContext},
			Signals:              []string{"blocked"},
			LongRunning:          true,
			NeedsReview:          true,
		},
	})
	profile, err := layer.AnalyzeTask(context.Background(), SemanticTaskInput{
		Role:      RoleImplementation,
		Objective: "add a watchdog for stalled workflow resume checkpoints",
	})
	if err != nil {
		t.Fatalf("AnalyzeTask error = %v", err)
	}
	if profile.Backend != "local-lexical" {
		t.Fatalf("Backend = %q, want local-lexical", profile.Backend)
	}
	if !profile.LongRunning || !profile.NeedsReview {
		t.Fatalf("profile = %+v, want long-running review profile", profile)
	}
	assertContains(t, profile.Domains, "monitoring")
	assertContains(t, profile.Operations, "orchestrate")
	assertContains(t, profile.RequiredCapabilities, SemanticCapabilitySessionResume)
	assertContains(t, profile.Signals, "blocked")
}

func TestSemanticDurableHintsUsesProfileSignals(t *testing.T) {
	hints := semanticDurableHints(AnalyzeObjective(RoleImplementation, "detect stalled background workflow retry after process restart with checkpoint resume"))
	if !hints.ProcessIndependentRetries || !hints.ReliableWorkerRecovery {
		t.Fatalf("hints = %+v, want durable retry/recovery hints", hints)
	}
}

func TestSemanticCapabilityFitRewardsStructuredOutput(t *testing.T) {
	required := []string{SemanticCapabilityFileEditing, SemanticCapabilityStructuredOutput}
	got := semanticCapabilityFit(agents.Capabilities{FileEditing: true, StructuredOutput: true}, required)
	if got != 1.0 {
		t.Fatalf("fit = %v, want 1.0", got)
	}
	partial := semanticCapabilityFit(agents.Capabilities{FileEditing: true}, required)
	if partial >= got {
		t.Fatalf("partial fit = %v, want below %v", partial, got)
	}
}

func TestSemanticLayerObservesQuotaPressure(t *testing.T) {
	layer := DeterministicSemanticLayer{}
	obs := layer.ObserveEvent(context.Background(), agents.Event{Kind: agents.EventError, Message: "429 rate limit: model at capacity"})
	if obs.Signal != "quota-pressure" {
		t.Fatalf("Signal = %q, want quota-pressure", obs.Signal)
	}
	if obs.SuggestedAction == "" {
		t.Fatal("expected suggested action")
	}
}

func assertContains(t *testing.T, items []string, want string) {
	t.Helper()
	for _, item := range items {
		if item == want {
			return
		}
	}
	t.Fatalf("%q not found in %#v", want, items)
}
