package core

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/model"
)

const (
	StrategyBalanced     = "balanced"
	StrategyConservative = "conservative"
	StrategyQualityFirst = "quality-first"

	RolePlanning       = "planner"
	RoleImplementation = "implementer"
	RoleReview         = "reviewer"
	RoleDebugging      = "debugger"
)

type RoutingPolicy struct {
	Strategy           string             `json:"strategy"`
	MinReservePercent  float64            `json:"min_reserve_percent"`
	UnknownQuota       string             `json:"unknown_quota"`
	RequireFileEditing bool               `json:"require_file_editing"`
	AgentWeights       map[string]float64 `json:"agent_weights,omitempty"`
}

func DefaultRoutingPolicy() RoutingPolicy {
	return RoutingPolicy{
		Strategy:          StrategyBalanced,
		MinReservePercent: 15.0,
		// Do not auto-route to a provider whose quota cannot be verified. An
		// UNKNOWN row can represent an expired provider just as easily as an
		// available one; callers can still explicitly select that adapter.
		UnknownQuota: "deny",
		AgentWeights: map[string]float64{"agy": 1.0, "codex": 0.9, "cursor": 0.8, "freebuff": 0.0},
	}
}

// Route evaluates all configured agent candidates using hard eligibility checks,
// token/quota availability, and role-specific efficacy scoring to produce an
// explainable routing decision.
func Route(
	ctx context.Context,
	role string,
	objective string,
	adapters map[string]agents.Adapter,
	inventory []agents.ModelAvailability,
	existingSessionAgent string,
	policy RoutingPolicy,
) (*model.RouteDecision, error) {
	if policy.Strategy == "" {
		policy.Strategy = StrategyBalanced
	}
	if policy.MinReservePercent <= 0 {
		policy.MinReservePercent = 15.0
	}
	if policy.UnknownQuota == "" {
		policy.UnknownQuota = "allow"
	}

	inventoryByAgent := make(map[string][]agents.ModelAvailability)
	for _, row := range inventory {
		inventoryByAgent[row.Agent] = append(inventoryByAgent[row.Agent], row)
	}
	semanticProfile := AnalyzeObjective(role, objective)

	names := make([]string, 0, len(adapters))
	for name := range adapters {
		names = append(names, name)
	}
	sort.Strings(names)

	candidates := make([]model.CandidateScore, 0, len(names))
	var eligibleCount int

	for _, name := range names {
		adapter := adapters[name]
		cand := evaluateCandidate(ctx, name, adapter, role, objective, semanticProfile, inventoryByAgent[name], existingSessionAgent, policy)
		if cand.Eligible {
			eligibleCount++
		}
		candidates = append(candidates, cand)
	}

	if eligibleCount == 0 {
		// If all candidates fell below reserve but have >0 quota, allow the best candidate with quota
		for i := range candidates {
			if candidates[i].Exclusion == "remaining quota below reserve threshold" && candidates[i].RemainingPercent != nil && *candidates[i].RemainingPercent > 0 {
				candidates[i].Eligible = true
				candidates[i].Exclusion = ""
				eligibleCount++
			}
		}
	}

	if eligibleCount == 0 {
		var reasons []string
		for _, c := range candidates {
			reasons = append(reasons, fmt.Sprintf("%s (%s)", c.Agent, c.Exclusion))
		}
		return &model.RouteDecision{
			Role:            role,
			Objective:       objective,
			SemanticProfile: &semanticProfile,
			Strategy:        policy.Strategy,
			CandidateScores: candidates,
		}, fmt.Errorf("%w: no eligible agent for %s role: %s", ErrAgentUnavailable, role, strings.Join(reasons, ", "))
	}

	// Sort eligible candidates by TotalScore DESC
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Eligible != candidates[j].Eligible {
			return candidates[i].Eligible
		}
		return candidates[i].TotalScore > candidates[j].TotalScore
	})

	selected := candidates[0]
	rationale := formatRationale(selected, policy.Strategy)

	return &model.RouteDecision{
		Role:            role,
		Objective:       objective,
		SemanticProfile: &semanticProfile,
		SelectedAgent:   selected.Agent,
		SelectedModel:   selected.SelectedModel,
		Rationale:       rationale,
		Strategy:        policy.Strategy,
		CandidateScores: candidates,
	}, nil
}

func evaluateCandidate(
	ctx context.Context,
	name string,
	adapter agents.Adapter,
	role string,
	objective string,
	semanticProfile model.SemanticProfile,
	modelRows []agents.ModelAvailability,
	existingSessionAgent string,
	policy RoutingPolicy,
) model.CandidateScore {
	score := model.CandidateScore{
		Agent:           name,
		QuotaConfidence: agents.ConfidenceUnknown,
		Reliability:     0.90,
		CostFit:         0.85,
	}

	installation := adapter.Detect(ctx)
	if !installation.Available {
		score.Eligible = false
		score.Exclusion = "agent executable is not installed or available"
		if installation.Error != "" {
			score.Exclusion += ": " + installation.Error
		}
		return score
	}

	caps := adapter.Capabilities()
	if policy.RequireFileEditing && !caps.FileEditing {
		score.Eligible = false
		score.Exclusion = "adapter does not support file editing"
		return score
	}

	// 1. Token / Quota evaluation
	quotaHeadroom, remainingPercent, confidence, quotaEligible, exclusion := evaluateQuota(name, modelRows, policy)
	score.QuotaHeadroom = quotaHeadroom
	score.RemainingPercent = remainingPercent
	score.QuotaConfidence = confidence

	if !quotaEligible {
		score.Eligible = false
		score.Exclusion = exclusion
		return score
	}
	score.SelectedModel = selectModel(modelRows)

	// 2. Capability / Efficacy evaluation
	capabilityFit := evaluateEfficacy(name, role, objective, semanticProfile, caps)
	score.CapabilityFit = capabilityFit
	score.Details = map[string]any{
		"semantic_capability_fit": semanticCapabilityFit(caps, semanticProfile.RequiredCapabilities),
		"semantic_task_kind":      semanticProfile.TaskKind,
		"semantic_domains":        semanticProfile.Domains,
		"semantic_operations":     semanticProfile.Operations,
		"semantic_risks":          semanticProfile.Risks,
	}

	// 3. Session Context evaluation
	sessionValue := 0.0
	if existingSessionAgent == name && caps.SessionResume {
		sessionValue = 1.0
	}
	score.SessionValue = sessionValue

	// 4. Weight calculation based on strategy
	wCap, wSession, wQuota, wRel, wCost := strategyWeights(policy.Strategy)
	rawScore := (wCap * capabilityFit) + (wSession * sessionValue) + (wQuota * quotaHeadroom) + (wRel * score.Reliability) + (wCost * score.CostFit)
	score.AgentWeight = agentWeight(name, policy.AgentWeights)

	score.TotalScore = math.Round(rawScore*score.AgentWeight*1000) / 1000
	score.Eligible = true
	return score
}

func agentWeight(agent string, configured map[string]float64) float64 {
	if configured != nil {
		if weight, ok := configured[strings.ToLower(agent)]; ok && weight > 0 {
			return weight
		}
	}
	if weight, ok := DefaultRoutingPolicy().AgentWeights[strings.ToLower(agent)]; ok {
		return weight
	}
	return 1.0
}

func evaluateQuota(agent string, rows []agents.ModelAvailability, policy RoutingPolicy) (float64, *float64, string, bool, string) {
	if len(rows) == 0 {
		switch policy.UnknownQuota {
		case "deny":
			// Some agents (agy, freebuff) publish named models but expose no
			// quota API. Treat any agent that has named usable models as
			// routable with unknown headroom; only deny agents that have no
			// model catalog at all.
			if hasNamedUsableModel(rows) {
				return 0.80, nil, agents.ConfidenceUnknown, true, ""
			}
			return 0, nil, agents.ConfidenceUnknown, false, "unknown quota denied by policy"
		case "conservative":
			return 0.50, nil, agents.ConfidenceUnknown, true, ""
		default:
			return 0.80, nil, agents.ConfidenceUnknown, true, ""
		}
	}

	var hasUsable bool
	var sawExactZero bool
	var bestRemaining *float64
	var bestReserveRemaining *float64
	bestRemainingIsReserve := false
	confidence := agents.ConfidenceUnknown

	for _, r := range rows {
		if r.Confidence == agents.ConfidenceExact {
			confidence = agents.ConfidenceExact
		}
		if !r.Usable {
			continue
		}
		hasUsable = true
		if r.RemainingPercent == nil {
			continue
		}
		val := *r.RemainingPercent
		if val <= 0 {
			sawExactZero = true
			continue
		}
		if r.Reserve || agents.IsReserveModel(r.Model) {
			if bestReserveRemaining == nil || val > *bestReserveRemaining {
				copied := val
				bestReserveRemaining = &copied
			}
			continue
		}
		if bestRemaining == nil || val > *bestRemaining {
			copied := val
			bestRemaining = &copied
		}
	}

	if !hasUsable {
		return 0, nil, confidence, false, "token quota exhausted (0% remaining)"
	}

	if bestRemaining == nil {
		bestRemaining = bestReserveRemaining
		bestRemainingIsReserve = bestRemaining != nil
	}

	if bestRemaining != nil {
		rem := *bestRemaining
		if rem <= 0.0 {
			return 0, bestRemaining, confidence, false, "token quota exhausted (0% remaining)"
		}
		// The configured reserve threshold protects ordinary quota. A reserve
		// bucket is already the fallback pool, so any positive balance must
		// remain eligible; otherwise Relay can report Codex unavailable while
		// Codex Reserve is still usable.
		if rem < policy.MinReservePercent && !bestRemainingIsReserve && policy.Strategy != StrategyQualityFirst {
			return 0.10, bestRemaining, confidence, false, "remaining quota below reserve threshold"
		}
		if bestRemainingIsReserve && rem < policy.MinReservePercent {
			return 0.10, bestRemaining, confidence, true, ""
		}
		headroom := (rem - policy.MinReservePercent) / (100.0 - policy.MinReservePercent)
		if headroom < 0 {
			headroom = 0
		}
		if headroom > 1 {
			headroom = 1
		}
		return math.Round(headroom*1000) / 1000, bestRemaining, confidence, true, ""
	}

	if sawExactZero {
		return 0, nil, confidence, false, "token quota exhausted (0% remaining)"
	}

	switch policy.UnknownQuota {
	case "deny":
		// Some agents (agy, freebuff) publish named models but expose no
		// quota API. Treat any agent that has named usable models as
		// routable with unknown headroom; only deny agents that have no
		// model catalog at all.
		if hasNamedUsableModel(rows) {
			return 0.80, nil, agents.ConfidenceUnknown, true, ""
		}
		return 0, nil, agents.ConfidenceUnknown, false, "unknown quota denied by policy"
	case "conservative":
		return 0.50, nil, agents.ConfidenceUnknown, true, ""
	default:
		return 0.80, nil, agents.ConfidenceUnknown, true, ""
	}
}

func hasNamedUsableModel(rows []agents.ModelAvailability) bool {
	for _, row := range rows {
		if row.Usable && strings.TrimSpace(row.Model) != "" && row.Model != "UNKNOWN" {
			return true
		}
	}
	return false
}

func selectModel(rows []agents.ModelAvailability) string {
	selected := pickUsableModel(rows, false)
	if selected == "" {
		selected = pickUsableModel(rows, true)
	}
	return selected
}

func pickUsableModel(rows []agents.ModelAvailability, reserve bool) string {
	var selected agents.ModelAvailability
	found := false
	for _, row := range rows {
		if !row.Usable || row.Model == "" || row.Model == "UNKNOWN" {
			continue
		}
		if (row.Reserve || agents.IsReserveModel(row.Model)) != reserve {
			continue
		}
		if !found || (row.RemainingPercent != nil && (selected.RemainingPercent == nil || *row.RemainingPercent > *selected.RemainingPercent)) ||
			(row.RemainingPercent == nil && selected.RemainingPercent == nil && row.Model < selected.Model) {
			selected = row
			found = true
		}
	}
	if found {
		return selected.Model
	}
	return ""
}

// IsQuotaExhausted reports whether an error indicates token quota exhaustion,
// rate limiting, or usage limits.
func IsQuotaExhausted(err error) bool {
	if err == nil {
		return false
	}
	return IsQuotaExhaustedMessage(err.Error())
}

// IsQuotaExhaustedMessage checks if a raw string message indicates quota or usage exhaustion.
func IsQuotaExhaustedMessage(msg string) bool {
	msg = strings.ToLower(msg)
	patterns := []string{
		"usage limit",
		"hit your usage limit",
		"quota exhausted",
		"token quota exhausted",
		"capacity exhausted",
		"no capacity",
		"at capacity",
		"over capacity",
		"model capacity",
		"server is busy",
		"insufficient quota",
		"exceeded your current quota",
		"resource_exhausted",
		"rate limit",
		"rate_limit",
		"purchase more credits",
		"upgrade to pro",
		"daily session limit",
		"out of sessions",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return containsToken(msg, "429")
}

// IsModelSelectionError reports provider failures where retrying the same
// model is unlikely to help. Callers can remove the failed model and try the
// next discovered model.
func IsModelSelectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, pattern := range []string{
		"model is unavailable",
		"model unavailable",
		"model not found",
		"unknown model",
		"invalid model",
		"unsupported model",
		"model does not exist",
		"failed to load model",
		"failed to initialize model",
	} {
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}

func containsToken(msg, token string) bool {
	for start := 0; start < len(msg); {
		idx := strings.Index(msg[start:], token)
		if idx == -1 {
			return false
		}
		idx += start
		beforeOK := idx == 0 || !isTokenChar(msg[idx-1])
		after := idx + len(token)
		afterOK := after == len(msg) || !isTokenChar(msg[after])
		if beforeOK && afterOK {
			return true
		}
		start = after
	}
	return false
}

func isTokenChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_'
}

func evaluateEfficacy(agentName, role, objective string, semanticProfile model.SemanticProfile, caps agents.Capabilities) float64 {
	base := 0.80
	agentLower := strings.ToLower(agentName)
	objLower := strings.ToLower(objective)

	switch role {
	case RolePlanning:
		if strings.Contains(agentLower, "agy") {
			base = 0.96
		} else if strings.Contains(agentLower, "cursor") {
			base = 0.92
		} else if strings.Contains(agentLower, "freebuff") {
			base = 0.90
		} else if strings.Contains(agentLower, "codex") {
			base = 0.88
		}
		if strings.Contains(objLower, "plan") || strings.Contains(objLower, "architect") || strings.Contains(objLower, "design") {
			base += 0.02
		}
	case RoleImplementation:
		if strings.Contains(agentLower, "codex") {
			base = 0.96
		} else if strings.Contains(agentLower, "cursor") {
			base = 0.94
		} else if strings.Contains(agentLower, "freebuff") {
			base = 0.92
		} else if strings.Contains(agentLower, "agy") {
			base = 0.90
		}
		if caps.FileEditing {
			base += 0.02
		}
		if strings.Contains(objLower, "fix") || strings.Contains(objLower, "implement") || strings.Contains(objLower, "refactor") {
			base += 0.02
		}
	case RoleDebugging:
		if strings.Contains(agentLower, "agy") {
			base = 0.93
		} else if strings.Contains(agentLower, "cursor") {
			base = 0.92
		} else if strings.Contains(agentLower, "freebuff") {
			base = 0.91
		} else if strings.Contains(agentLower, "codex") {
			base = 0.90
		}
	case RoleReview:
		if strings.Contains(agentLower, "agy") {
			base = 0.94
		} else if strings.Contains(agentLower, "cursor") {
			base = 0.93
		} else if strings.Contains(agentLower, "freebuff") {
			base = 0.91
		} else if strings.Contains(agentLower, "codex") {
			base = 0.90
		}
	default:
		if strings.Contains(agentLower, "codex") {
			base = 0.92
		} else if strings.Contains(agentLower, "cursor") {
			base = 0.91
		} else if strings.Contains(agentLower, "freebuff") {
			base = 0.90
		} else if strings.Contains(agentLower, "agy") {
			base = 0.90
		}
	}

	if base > 1.0 {
		base = 1.0
	}
	semanticFit := semanticCapabilityFit(caps, semanticProfile.RequiredCapabilities)
	if semanticFit < 1.0 {
		base -= (1.0 - semanticFit) * 0.12
	}
	if semanticProfile.NeedsReview && caps.StructuredOutput {
		base += 0.02
	}
	if semanticProfile.LongRunning && !caps.SessionResume {
		base -= 0.04
	}
	if base < 0 {
		base = 0
	}
	if base > 1.0 {
		base = 1.0
	}
	return math.Round(base*1000) / 1000
}

func strategyWeights(strategy string) (float64, float64, float64, float64, float64) {
	switch strings.ToLower(strategy) {
	case StrategyConservative:
		// capability: 0.25, session: 0.10, quota: 0.40, reliability: 0.10, cost: 0.15
		return 0.25, 0.10, 0.40, 0.10, 0.15
	case StrategyQualityFirst:
		// capability: 0.70, session: 0.15, quota: 0.05, reliability: 0.10, cost: 0.00
		return 0.70, 0.15, 0.05, 0.10, 0.00
	default: // StrategyBalanced
		// capability: 0.35, session: 0.20, quota: 0.20, reliability: 0.15, cost: 0.10
		return 0.35, 0.20, 0.20, 0.15, 0.10
	}
}

func formatRationale(score model.CandidateScore, strategy string) string {
	var quotaInfo string
	if score.RemainingPercent != nil {
		quotaInfo = fmt.Sprintf("%.0f%% remaining (%s)", *score.RemainingPercent, score.QuotaConfidence)
	} else {
		quotaInfo = fmt.Sprintf("headroom %.2f (%s)", score.QuotaHeadroom, score.QuotaConfidence)
	}
	return fmt.Sprintf("highest score %.2f [%s] (capability: %.2f, quota: %s, session: %.1f)",
		score.TotalScore, strategy, score.CapabilityFit, quotaInfo, score.SessionValue)
}
