package cursor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverModelsParsesJSONList(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	script := `#!/bin/sh
case "$1" in
  --version) echo '2026.9.11-abc' ;;
  --list-models) echo '{"models":[{"id":"composer-2.5"},{"id":"gpt-5"}]}' ;;
esac
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := New(command).DiscoverModels(context.Background())
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Agent != "cursor" || rows[0].Model != "composer-2.5" || !rows[0].Usable || rows[0].RemainingPercent != nil {
		t.Fatalf("row = %+v", rows[0])
	}
	if rows[1].Model != "gpt-5" {
		t.Fatalf("row = %+v", rows[1])
	}
}

func TestDiscoverModelsKeepsUnknownQuotaWhenListingFails(t *testing.T) {
	command := filepath.Join(t.TempDir(), "agent")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2026.9.11-abc'; exit 0; fi
exit 1
`
	if err := os.WriteFile(command, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := New(command).DiscoverModels(context.Background())
	if len(rows) != 1 || !rows[0].Usable || rows[0].Model != "UNKNOWN" || rows[0].RemainingPercent != nil {
		t.Fatalf("rows = %+v", rows)
	}
}
