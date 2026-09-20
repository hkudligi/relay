package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/harsha/relay/internal/agents"
)

const codexQuotaSource = "codex app-server account/rateLimits/read"

type catalogResponse struct {
	Models []struct {
		Slug       string `json:"slug"`
		Visibility string `json:"visibility"`
	} `json:"models"`
}

type rateLimitWindow struct {
	UsedPercent int `json:"usedPercent"`
}

type rateLimitSnapshot struct {
	LimitID         string           `json:"limitId"`
	LimitName       string           `json:"limitName"`
	NormalModelSlug string           `json:"normalModelSlug"`
	Primary         *rateLimitWindow `json:"primary"`
	Secondary       *rateLimitWindow `json:"secondary"`
	IndividualLimit *struct {
		RemainingPercent int `json:"remainingPercent"`
	} `json:"individualLimit"`
}

type rateLimitsResponse struct {
	OrdinaryUsageAllowed *bool                        `json:"ordinaryUsageAllowed"`
	RateLimits           rateLimitSnapshot            `json:"rateLimits"`
	ByLimitID            map[string]rateLimitSnapshot `json:"rateLimitsByLimitId"`
}

func (a *Adapter) DiscoverModels(ctx context.Context) []agents.ModelAvailability {
	installation := a.Detect(ctx)
	if !installation.Available {
		return []agents.ModelAvailability{agents.UnknownAvailability(installation, "codex --version", installation.Error)}
	}

	discoveryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(discoveryCtx, a.Command, "debug", "models").Output()
	if err != nil {
		row := agents.UnknownAvailability(installation, "codex debug models", err.Error())
		row.Usable = false
		return []agents.ModelAvailability{row}
	}
	var catalog catalogResponse
	if err := json.Unmarshal(output, &catalog); err != nil {
		row := agents.UnknownAvailability(installation, "codex debug models", "decode model catalog: "+err.Error())
		row.Usable = false
		return []agents.ModelAvailability{row}
	}

	quota, quotaErr := a.readRateLimits(discoveryCtx)
	models := make([]string, 0, len(catalog.Models))
	hidden := make([]string, 0, 2)
	for _, item := range catalog.Models {
		if item.Slug == "" {
			continue
		}
		if item.Visibility == "hide" {
			hidden = append(hidden, item.Slug)
			continue
		}
		models = append(models, item.Slug)
	}
	sort.Strings(models)
	if len(models) == 0 {
		models = append(models, "UNKNOWN")
	}
	rows := make([]agents.ModelAvailability, 0, len(models)+1)
	for _, model := range models {
		row := agents.ModelAvailability{Agent: a.Name(), Model: model, Version: installation.Version, Installed: true, Usable: true, Confidence: agents.ConfidenceUnknown, DataSource: codexQuotaSource}
		if quotaErr != nil {
			row.Error = quotaErr.Error()
		} else {
			ordinaryAllowed := true
			if quota.OrdinaryUsageAllowed != nil && !*quota.OrdinaryUsageAllowed {
				ordinaryAllowed = false
			}
			snapshot := quota.RateLimits
			modelSpecificLimit := false
			if candidate, ok := selectModelLimit(quota.ByLimitID, model); ok {
				snapshot = candidate
				modelSpecificLimit = true
			}
			// ordinaryUsageAllowed describes the account's base/ordinary
			// allowance. A model-specific limit is authoritative for that
			// model: some models (for example Luna) can remain available after
			// the base allowance has been exhausted.
			if !ordinaryAllowed && !modelSpecificLimit {
				row.Usable = false
				row.Error = "usage limit reached: ordinary usage not allowed"
			}
			if remaining, ok := remainingPercent(snapshot); ok {
				row.Confidence = agents.ConfidenceExact
				if !ordinaryAllowed && !modelSpecificLimit {
					zero := 0.0
					row.RemainingPercent = &zero
				} else {
					row.RemainingPercent = &remaining
					if remaining == 0 {
						row.Usable = false
					}
				}
			} else {
				if ordinaryAllowed {
					row.Error = "provider returned no quota window"
				}
			}
		}
		rows = append(rows, row)
	}
	if quotaErr == nil {
		rows = append(rows, reserveAvailability(installation, append(append([]string{}, models...), hidden...), quota.ByLimitID)...)
	}
	return rows
}

// matchesModelLimit joins an ordinary rate-limit bucket to a catalog model.
// Reserve buckets are a different pool and must not be folded into Luna's
// remaining percent; they are emitted as separate inventory rows.
func matchesModelLimit(limitID string, snapshot rateLimitSnapshot, model string) bool {
	if isReserveLimit(limitID, snapshot.LimitName) {
		return false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false
	}
	if slug := strings.ToLower(strings.TrimSpace(snapshot.NormalModelSlug)); slug != "" && slug == model {
		return true
	}
	name := strings.ToLower(snapshot.LimitName)
	for _, id := range []string{limitID, snapshot.LimitID} {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		if id == model {
			return true
		}
		shortID := normalizeLimitID(id)
		if shortID != "" && shortID != "reserve" && strings.HasSuffix(model, "-"+shortID) {
			return true
		}
	}
	return strings.Contains(name, "luna") && !strings.Contains(name, "reserve") && strings.Contains(model, "luna")
}

func isReserveLimit(limitID, limitName string) bool {
	return strings.Contains(strings.ToLower(limitID), "reserve") || strings.Contains(strings.ToLower(limitName), "reserve")
}

func reserveExecutionModel(id string, snap rateLimitSnapshot, catalog []string) string {
	// Codex meters Luna Reserve on a hidden model slug (gpt-reserve). The
	// snapshot's normalModelSlug is the display model (gpt-5.6-luna), which
	// still consumes ordinary usage and is rejected when that pool is empty.
	for _, candidate := range []string{snap.LimitName, id, snap.NormalModelSlug} {
		if isReserveModelSlug(candidate) {
			return strings.TrimSpace(candidate)
		}
	}
	for _, model := range catalog {
		if isReserveModelSlug(model) {
			return model
		}
	}
	if isReserveLimit(id, snap.LimitName) {
		return "gpt-reserve"
	}
	return ""
}

func isReserveModelSlug(model string) bool {
	slug := strings.ToLower(strings.TrimSpace(model))
	return slug == "gpt-reserve" || strings.HasPrefix(slug, "gpt-") && strings.Contains(slug, "reserve") && !strings.Contains(slug, "luna")
}

func reserveAvailability(installation agents.Installation, catalog []string, limits map[string]rateLimitSnapshot) []agents.ModelAvailability {
	if len(limits) == 0 {
		return nil
	}
	ids := make([]string, 0, len(limits))
	for id := range limits {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]agents.ModelAvailability, 0, 1)
	for _, id := range ids {
		snap := limits[id]
		if !isReserveLimit(id, snap.LimitName) {
			continue
		}
		execModel := reserveExecutionModel(id, snap, catalog)
		if execModel == "" || execModel == "UNKNOWN" {
			continue
		}
		row := agents.ModelAvailability{
			Agent:      installation.Name,
			Model:      execModel,
			Version:    installation.Version,
			Installed:  true,
			Usable:     true,
			Reserve:    true,
			Confidence: agents.ConfidenceUnknown,
			DataSource: codexQuotaSource + " (" + id + ")",
		}
		if remaining, ok := remainingPercent(snap); ok {
			row.Confidence = agents.ConfidenceExact
			row.RemainingPercent = &remaining
			if remaining == 0 {
				row.Usable = false
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// selectModelLimit chooses the ordinary quota bucket for a catalog model.
func selectModelLimit(limits map[string]rateLimitSnapshot, model string) (rateLimitSnapshot, bool) {
	var selected rateLimitSnapshot
	selectedID := ""
	selectedRemaining := -1.0
	selectedKnown := false
	found := false
	for limitID, candidate := range limits {
		if !matchesModelLimit(limitID, candidate, model) {
			continue
		}
		remaining, known := remainingPercent(candidate)
		better := !found
		if found {
			switch {
			case known && !selectedKnown:
				better = true
			case !known && selectedKnown:
				better = false
			case remaining > selectedRemaining:
				better = true
			case remaining < selectedRemaining:
				better = false
			case limitID < selectedID:
				better = true
			default:
				better = false
			}
		}
		if better {
			selected = candidate
			selectedID = limitID
			selectedRemaining = remaining
			selectedKnown = known
			found = true
		}
	}
	return selected, found
}

func normalizeLimitID(limitID string) string {
	shortID := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(limitID)), "gpt-")
	for _, suffix := range []string{"-reserve", "_reserve", ":reserve"} {
		shortID = strings.TrimSuffix(shortID, suffix)
	}
	return shortID
}

func remainingPercent(snapshot rateLimitSnapshot) (float64, bool) {
	remaining := 101
	for _, window := range []*rateLimitWindow{snapshot.Primary, snapshot.Secondary} {
		if window == nil {
			continue
		}
		value := 100 - window.UsedPercent
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		if value < remaining {
			remaining = value
		}
	}
	if snapshot.IndividualLimit != nil {
		value := snapshot.IndividualLimit.RemainingPercent
		if value < 0 {
			value = 0
		}
		if value > 100 {
			value = 100
		}
		if value < remaining {
			remaining = value
		}
	}
	if remaining == 101 {
		return 0, false
	}
	return float64(remaining), true
}

func (a *Adapter) readRateLimits(ctx context.Context) (rateLimitsResponse, error) {
	cmd := exec.CommandContext(ctx, a.Command, "app-server", "--listen", "stdio://")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return rateLimitsResponse{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return rateLimitsResponse{}, err
	}
	if err := cmd.Start(); err != nil {
		return rateLimitsResponse{}, err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	reader := bufio.NewReader(stdout)
	if _, err := rpcCall(stdin, reader, 1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "rly", "version": "1"}}); err != nil {
		return rateLimitsResponse{}, fmt.Errorf("initialize app-server: %w", err)
	}
	if err := json.NewEncoder(stdin).Encode(map[string]any{"method": "initialized"}); err != nil {
		return rateLimitsResponse{}, err
	}
	raw, err := rpcCall(stdin, reader, 2, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return rateLimitsResponse{}, err
	}
	var result rateLimitsResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, fmt.Errorf("decode rate limits: %w", err)
	}
	return result, nil
}

func rpcCall(w io.Writer, r *bufio.Reader, id int, method string, params any) (json.RawMessage, error) {
	if err := json.NewEncoder(w).Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var response struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(line, &response) != nil || response.ID != id {
			continue
		}
		if len(response.Error) > 0 && string(response.Error) != "null" {
			return nil, fmt.Errorf("app-server error: %s", response.Error)
		}
		return response.Result, nil
	}
}
