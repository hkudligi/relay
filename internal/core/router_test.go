package core_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/core"
	"github.com/harsha/relay/internal/store"
)

type mockAdapter struct {
	name      string
	available bool
	caps      agents.Capabilities
}

func (m *mockAdapter) Name() string { return m.name }
func (m *mockAdapter) Detect(context.Context) agents.Installation {
	return agents.Installation{Name: m.name, Available: m.available, Version: "1.0"}
}
func (m *mockAdapter) Capabilities() agents.Capabilities { return m.caps }
func (m *mockAdapter) Start(context.Context, agents.Request) (agents.Run, error) {
	return nil, nil
}
func (m *mockAdapter) Resume(context.Context, string, agents.Request) (agents.Run, error) {
	return nil, nil
}

func TestRouteSelectsAgentWithAvailableTokensOverExhausted(t *testing.T) {
	codexQuota := 0.0 // 0% remaining tokens (exhausted)
	agyQuota := 80.0  // 80% remaining tokens

	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &codexQuota, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "gemini", Installed: true, Usable: true, RemainingPercent: &agyQuota, Confidence: agents.ConfidenceExact},
	}

	decision, err := core.Route(
		context.Background(),
		core.RoleImplementation,
		"fix concurrency bug",
		adapters,
		inventory,
		"",
		core.DefaultRoutingPolicy(),
	)
	if err != nil {
		t.Fatalf("unexpected route error: %v", err)
	}
	if decision.SelectedAgent != "agy" {
		t.Fatalf("expected agy to be selected due to codex token exhaustion, got %s", decision.SelectedAgent)
	}
	if decision.SelectedModel != "gemini" {
		t.Fatalf("expected selected model gemini, got %q", decision.SelectedModel)
	}

	// Verify candidate scores and exclusions
	for _, cand := range decision.CandidateScores {
		if cand.Agent == "codex" && cand.Eligible {
			t.Fatalf("expected codex to be ineligible due to 0%% tokens, but got: %+v", cand)
		}
		if cand.Agent == "agy" && (!cand.Eligible || cand.QuotaHeadroom <= 0) {
			t.Fatalf("expected agy to be eligible with positive quota headroom, got: %+v", cand)
		}
	}
}

func TestRouteDeniesUnknownQuotaByDefault(t *testing.T) {
	quota := 54.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5.6-luna", Installed: true, Usable: true, RemainingPercent: &quota, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "UNKNOWN", Installed: true, Usable: true, Confidence: agents.ConfidenceUnknown},
	}

	decision, err := core.Route(context.Background(), core.RoleImplementation, "create a file", adapters, inventory, "", core.DefaultRoutingPolicy())
	if err != nil {
		t.Fatalf("unexpected route error: %v", err)
	}
	if decision.SelectedAgent != "codex" {
		t.Fatalf("selected agent = %s, want codex", decision.SelectedAgent)
	}
}

func TestRouteOptimizesForEfficacyByRole(t *testing.T) {
	quota100 := 100.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &quota100, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "gemini", Installed: true, Usable: true, RemainingPercent: &quota100, Confidence: agents.ConfidenceExact},
	}

	// For planning role, AGY has higher planning efficacy
	planDecision, err := core.Route(
		context.Background(),
		core.RolePlanning,
		"design database architecture",
		adapters,
		inventory,
		"",
		core.DefaultRoutingPolicy(),
	)
	if err != nil {
		t.Fatalf("route planning error: %v", err)
	}
	if planDecision.SelectedAgent != "agy" {
		t.Fatalf("expected agy to be selected for planning efficacy, got %s", planDecision.SelectedAgent)
	}

	// For implementation role, Codex has higher coding efficacy
	implDecision, err := core.Route(
		context.Background(),
		core.RoleImplementation,
		"implement the payment service",
		adapters,
		inventory,
		"",
		core.DefaultRoutingPolicy(),
	)
	if err != nil {
		t.Fatalf("route implementation error: %v", err)
	}
	if implDecision.SelectedAgent != "codex" {
		t.Fatalf("expected codex to be selected for implementation efficacy, got %s", implDecision.SelectedAgent)
	}
}

func TestRouteConservativeStrategyPrioritizesTokensOverEfficacy(t *testing.T) {
	// Codex has 25% tokens (just above reserve), AGY has 90% tokens
	codexQuota := 25.0
	agyQuota := 90.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &codexQuota, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "gemini", Installed: true, Usable: true, RemainingPercent: &agyQuota, Confidence: agents.ConfidenceExact},
	}

	policy := core.RoutingPolicy{
		Strategy:          core.StrategyConservative,
		MinReservePercent: 15.0,
		UnknownQuota:      "allow",
	}

	decision, err := core.Route(
		context.Background(),
		core.RoleImplementation,
		"implement refactor",
		adapters,
		inventory,
		"",
		policy,
	)
	if err != nil {
		t.Fatalf("route error: %v", err)
	}
	// Under conservative strategy, AGY's 90% quota headroom outweighs Codex's slight coding efficacy lead
	if decision.SelectedAgent != "agy" {
		t.Fatalf("expected agy to be selected under conservative token strategy, got %s", decision.SelectedAgent)
	}
}

func TestRouteQualityFirstStrategyPrioritizesEfficacy(t *testing.T) {
	// Codex has 25% tokens, AGY has 90% tokens
	codexQuota := 25.0
	agyQuota := 90.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &codexQuota, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "gemini", Installed: true, Usable: true, RemainingPercent: &agyQuota, Confidence: agents.ConfidenceExact},
	}

	policy := core.RoutingPolicy{
		Strategy:          core.StrategyQualityFirst,
		MinReservePercent: 15.0,
		UnknownQuota:      "allow",
	}

	decision, err := core.Route(
		context.Background(),
		core.RoleImplementation,
		"implement critical code",
		adapters,
		inventory,
		"",
		policy,
	)
	if err != nil {
		t.Fatalf("route error: %v", err)
	}
	// Under quality-first strategy, Codex's higher implementation efficacy wins
	if decision.SelectedAgent != "codex" {
		t.Fatalf("expected codex to be selected under quality-first strategy, got %s", decision.SelectedAgent)
	}
}

func TestRouteSessionContextBonus(t *testing.T) {
	quota := 80.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true, SessionResume: true}},
		"agy":   &mockAdapter{name: "agy", available: true, caps: agents.Capabilities{FileEditing: true, SessionResume: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &quota, Confidence: agents.ConfidenceExact},
		{Agent: "agy", Model: "gemini", Installed: true, Usable: true, RemainingPercent: &quota, Confidence: agents.ConfidenceExact},
	}

	// AGY has an existing active session on this task
	decision, err := core.Route(
		context.Background(),
		core.RoleImplementation,
		"follow up on previous change",
		adapters,
		inventory,
		"agy", // existing session agent
		core.DefaultRoutingPolicy(),
	)
	if err != nil {
		t.Fatalf("route error: %v", err)
	}
	if decision.SelectedAgent != "agy" {
		t.Fatalf("expected existing session on agy to be preferred, got %s", decision.SelectedAgent)
	}
}

func TestServiceRouteRecordsTraceEvent(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := core.New(db)
	task, err := svc.StartTask(context.Background(), "/repo", "build feature")
	if err != nil {
		t.Fatal(err)
	}

	quota := 90.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5", Installed: true, Usable: true, RemainingPercent: &quota, Confidence: agents.ConfidenceExact},
	}

	decision, err := svc.Route(
		context.Background(),
		task.ID,
		core.RoleImplementation,
		task.Objective,
		adapters,
		inventory,
		core.DefaultRoutingPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedAgent != "codex" {
		t.Fatalf("selected = %s", decision.SelectedAgent)
	}

	events, err := svc.Trace(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var sawRoutingEvent bool
	for _, e := range events {
		if e.Type == "routing.selected" && e.Actor == "router" {
			sawRoutingEvent = true
			if e.Data["selected_agent"] != "codex" {
				t.Fatalf("event data = %+v", e.Data)
			}
		}
	}
	if !sawRoutingEvent {
		t.Fatalf("expected routing.selected event in trace, got: %+v", events)
	}
}

func TestRoutePrefersOrdinaryModelOverLunaReserve(t *testing.T) {
	ordinary := 30.0
	reserve := 90.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5.1-codex", Installed: true, Usable: true, RemainingPercent: &ordinary, Confidence: agents.ConfidenceExact},
		{Agent: "codex", Model: "gpt-5.6-luna", Installed: true, Usable: false, RemainingPercent: new(float64), Confidence: agents.ConfidenceExact},
		{Agent: "codex", Model: agents.ReserveModelID("gpt-5.6-luna"), Installed: true, Usable: true, Reserve: true, RemainingPercent: &reserve, Confidence: agents.ConfidenceExact},
	}
	decision, err := core.Route(context.Background(), core.RoleImplementation, "implement the feature", adapters, inventory, "", core.DefaultRoutingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedModel != "gpt-5.1-codex" {
		t.Fatalf("selected model = %q, want ordinary gpt-5.1-codex not luna reserve", decision.SelectedModel)
	}
}

func TestRouteFallsBackToLunaReserveWhenOrdinaryQuotaIsExhausted(t *testing.T) {
	zero := 0.0
	reserve := 80.0
	adapters := map[string]agents.Adapter{
		"codex": &mockAdapter{name: "codex", available: true, caps: agents.Capabilities{FileEditing: true}},
	}
	inventory := []agents.ModelAvailability{
		{Agent: "codex", Model: "gpt-5.1-codex", Installed: true, Usable: false, RemainingPercent: &zero, Confidence: agents.ConfidenceExact},
		{Agent: "codex", Model: "gpt-5.6-luna", Installed: true, Usable: false, RemainingPercent: &zero, Confidence: agents.ConfidenceExact},
		{Agent: "codex", Model: "gpt-reserve", Installed: true, Usable: true, Reserve: true, RemainingPercent: &reserve, Confidence: agents.ConfidenceExact},
	}
	decision, err := core.Route(context.Background(), core.RoleImplementation, "implement the feature", adapters, inventory, "", core.DefaultRoutingPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if decision.SelectedAgent != "codex" || decision.SelectedModel != "gpt-reserve" {
		t.Fatalf("selected = %s %s, want codex gpt-reserve", decision.SelectedAgent, decision.SelectedModel)
	}
}
