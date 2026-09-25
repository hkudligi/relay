package agy_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/harsha/relay/internal/agents/agy"
)

func TestDiscoverModelsFallbackCleansPlainTextOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '1.2.4'; exit 0; fi
if [ "$1" = "models" ]; then
  printf '%s\n' '⠋ Fetching available models'
  printf '%s\n' 'claude-sonnet-4         high-quality coding model'
  printf '%s\n' 'gpt-5-codex            another-code-focused description'
  printf '%s\n' 'claude-sonnet-4         duplicate row'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	rows := agy.New(path).DiscoverModels(context.Background())
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want exactly two parsed models", rows)
	}
	if rows[0].Model != "claude-sonnet-4" || rows[1].Model != "gpt-5-codex" {
		t.Fatalf("rows = %+v, want only model IDs", rows)
	}
}
