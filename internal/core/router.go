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
	Strategy           string  `json:"strategy"`
	MinReservePercent  float64 `json:"min_reserve_percent"`
	UnknownQuota       string  `json:"unknown_quota"`
	RequireFileEditing bool    `json:"require_file_editing"`
}

func DefaultRoutingPolicy() RoutingPolicy {
	return RoutingPolicy{
		Strategy:          StrategyBalanced,
		MinReservePercent: 15.0,
		// Do not auto-route to a provider whose quota cannot be verified. An
		// UNKNOWN row can represent an expired provider just as easily as an
		// available one; callers can still explicitly select that adapter.
		UnknownQuota: "deny",
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

	names := make([]string, 0, len(adapters))
	for name := range adapters {
		names = append(names, name)
	}
	sort.Strings(names)

	candidates := make([]model.CandidateScore, 0, len(names))
	var eligibleCount int

	for _, name := range names {
		adapter := adapters[name]
		cand := evaluateCandidate(ctx, name, adapter, role, objective, inventoryByAgent[name], existingSessionAgent, policy)
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
	capabilityFit := evaluateEfficacy(name, role, objective, caps)
	score.CapabilityFit = capabilityFit

	// 3. Session Context evaluation
	sessionValue := 0.0
	if existingSessionAgent == name && caps.SessionResume {
		sessionValue = 1.0
	}
	score.SessionValue = sessionValue

	// 4. Weight calculation based on strategy
	wCap, wSession, wQuota, wRel, wCost := strategyWeights(policy.Strategy)
	rawScore := (wCap * capabilityFit) + (wSession * sessionValue) + (wQuota * quotaHeadroom) + (wRel * score.Reliability) + (wCost * score.CostFit)

	score.TotalScore = math.Round(rawScore*1000) / 1000
	score.Eligible = true
	return score
}

func evaluateQuota(agent string, rows []agents.ModelAvailability, policy RoutingPolicy) (float64, *float64, string, bool, string) {
	if len(rows) == 0 {
		switch policy.UnknownQuota {
		case "deny":
			// Freebuff publishes named models but exposes no quota API. Treat that
			// catalog as routable with unknown headroom; an actual UNKNOWN row is
			// still denied, as are other providers without measured quota.
			if agent == "freebuff" && hasNamedUsableModel(rows) {
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
		// Freebuff publishes named models but exposes no quota API. Treat that
		// catalog as routable with unknown headroom; an actual UNKNOWN row is
		// still denied, as are other providers without measured quota.
		if agent == "freebuff" && hasNamedUsableModel(rows) {
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
		"insufficient quota",
		"exceeded your current quota",
		"resource_exhausted",
		"rate limit",
		"rate_limit",
		"429",
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
	return false
}

func evaluateEfficacy(agentName, role, objective string, caps agents.Capabilities) float64 {
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
