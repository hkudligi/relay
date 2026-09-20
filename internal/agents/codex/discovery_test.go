package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemainingPercentUsesConstrainingProviderWindow(t *testing.T) {
	got, ok := remainingPercent(rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 20}, Secondary: &rateLimitWindow{UsedPercent: 65}})
	if !ok || got != 35 {
		t.Fatalf("remaining = %v, ok = %v", got, ok)
	}
}

func TestRemainingPercentUnknownWithoutProviderWindow(t *testing.T) {
	if _, ok := remainingPercent(rateLimitSnapshot{}); ok {
		t.Fatal("expected quota to remain unknown")
	}
}

func TestMatchesModelLimitAcceptsShortProviderLimitID(t *testing.T) {
	if !matchesModelLimit("luna", rateLimitSnapshot{}, "gpt-5.6-luna") {
		t.Fatal("expected short Luna limit ID to match the full model slug")
	}
}

func TestMatchesModelLimitDoesNotTreatReserveAsLunaQuota(t *testing.T) {
	if matchesModelLimit("gpt-reserve", rateLimitSnapshot{LimitName: "Luna Reserve"}, "gpt-5.6-luna") {
		t.Fatal("gpt-reserve is a separate pool and must not match ordinary Luna quota")
	}
	if matchesModelLimit("luna-reserve", rateLimitSnapshot{}, "gpt-5.6-luna") {
		t.Fatal("luna-reserve is a separate pool and must not match ordinary Luna quota")
	}
}

func TestSelectModelLimitIgnoresReserveBuckets(t *testing.T) {
	limits := map[string]rateLimitSnapshot{
		"luna":         {NormalModelSlug: "gpt-5.6-luna", Primary: &rateLimitWindow{UsedPercent: 100}},
		"luna-reserve": {Primary: &rateLimitWindow{UsedPercent: 20}},
	}
	selected, ok := selectModelLimit(limits, "gpt-5.6-luna")
	if !ok {
		t.Fatal("expected the ordinary Luna quota bucket")
	}
	remaining, ok := remainingPercent(selected)
	if !ok || remaining != 0 {
		t.Fatalf("selected remaining = %v, ok = %v; want ordinary Luna at 0%%", remaining, ok)
	}
}

func TestSelectModelLimitUsesOrdinaryLunaWhenReserveAlsoExists(t *testing.T) {
	limits := map[string]rateLimitSnapshot{
		"luna":        {NormalModelSlug: "gpt-5.6-luna", Primary: &rateLimitWindow{UsedPercent: 40}},
		"gpt-reserve": {LimitID: "gpt-reserve", LimitName: "Luna Reserve", Primary: &rateLimitWindow{UsedPercent: 0}},
	}
	selected, ok := selectModelLimit(limits, "gpt-5.6-luna")
	if !ok {
		t.Fatal("expected a Luna quota bucket")
	}
	remaining, ok := remainingPercent(selected)
	if !ok || remaining != 60 {
		t.Fatalf("selected remaining = %v, ok = %v; want ordinary Luna bucket at 60%%", remaining, ok)
	}
}

func TestDiscoverModelsUsesProviderCatalogAndExactQuota(t *testing.T) {
	command := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
case "$1" in
  --version) echo 'codex-cli test' ;;
  debug) echo '{"models":[{"slug":"gpt-a","visibility":"list"},{"slug":"gpt-b","visibility":"list"}]}' ;;
  app-server)
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
        *'"method":"account/rateLimits/read"'*) echo '{"id":2,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":30},"secondary":{"usedPercent":70}},"rateLimitsByLimitId":null}}' ;;
      esac
    done
    ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := New(command).DiscoverModels(context.Background())
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, row := range rows {
		if !row.Installed || !row.Usable || row.RemainingPercent == nil || *row.RemainingPercent != 30 || row.Confidence != "EXACT" || row.DataSource != codexQuotaSource {
			t.Fatalf("row = %+v", row)
		}
	}
}

func TestDiscoverModelsKeepsModelSpecificLimitUsableWhenBaseIsExhausted(t *testing.T) {
	command := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
case "$1" in
  --version) echo 'codex-cli test' ;;
  debug) echo '{"models":[{"slug":"gpt-base","visibility":"list"},{"slug":"gpt-5.6-luna","visibility":"list"}]}' ;;
  app-server)
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
        *'"method":"account/rateLimits/read"'*) echo '{"id":2,"result":{"ordinaryUsageAllowed":false,"rateLimits":{"primary":{"usedPercent":100},"secondary":{"usedPercent":100}},"rateLimitsByLimitId":{"luna":{"normalModelSlug":"gpt-5.6-luna","primary":{"usedPercent":20},"secondary":{"usedPercent":30}}}}}' ;;
      esac
    done
    ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := New(command).DiscoverModels(context.Background())
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, row := range rows {
		switch row.Model {
		case "gpt-base":
			if row.Usable || row.RemainingPercent == nil || *row.RemainingPercent != 0 {
				t.Fatalf("base row = %+v", row)
			}
		case "gpt-5.6-luna":
			if !row.Usable || row.RemainingPercent == nil || *row.RemainingPercent != 70 {
				t.Fatalf("luna row = %+v", row)
			}
		}
	}
}

func TestDiscoverModelsLaunchesGptReserveNotLunaWhenOrdinaryQuotaIsExhausted(t *testing.T) {
	command := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
case "$1" in
  --version) echo 'codex-cli test' ;;
  debug) echo '{"models":[{"slug":"gpt-5.1-codex","visibility":"list"},{"slug":"gpt-5.6-luna","visibility":"list"},{"slug":"gpt-reserve","visibility":"hide"}]}' ;;
  app-server)
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) echo '{"id":1,"result":{}}' ;;
        *'"method":"account/rateLimits/read"'*) echo '{"id":2,"result":{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","primary":{"usedPercent":0},"secondary":{"usedPercent":100},"rateLimitReachedType":"rate_limit_reached"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":0},"secondary":{"usedPercent":100}},"base_model_inference":{"limitId":"base_model_inference","limitName":"gpt-reserve","normalModelSlug":"gpt-5.6-luna","primary":{"usedPercent":68}}}}}' ;;
      esac
    done
    ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := New(command).DiscoverModels(context.Background())
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	sawReserve := false
	for _, row := range rows {
		switch row.Model {
		case "gpt-5.1-codex":
			if row.Usable || row.RemainingPercent == nil || *row.RemainingPercent != 0 || row.Reserve {
				t.Fatalf("codex row = %+v", row)
			}
		case "gpt-5.6-luna":
			if row.Usable || row.Reserve {
				t.Fatalf("ordinary luna row = %+v; reserve must not be folded into Luna", row)
			}
		case "gpt-reserve":
			sawReserve = true
			if !row.Reserve || !row.Usable || row.RemainingPercent == nil || *row.RemainingPercent != 32 {
				t.Fatalf("reserve row = %+v; want usable gpt-reserve remaining 32%%", row)
			}
		default:
			t.Fatalf("unexpected model %q", row.Model)
		}
	}
	if !sawReserve {
		t.Fatalf("missing luna reserve row: %+v", rows)
	}
}
