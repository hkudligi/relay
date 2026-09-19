package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/harsha/relay/internal/agents"
	"github.com/harsha/relay/internal/agents/codex"
	"github.com/harsha/relay/internal/cli"
	"github.com/harsha/relay/internal/model"
)

func testApp(t *testing.T) (*cli.App, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	dir := t.TempDir()
	app := &cli.App{
		In:       strings.NewReader(""),
		Out:      &out,
		Err:      &errOut,
		Getwd:    func() (string, error) { return dir, nil },
		HomeDir:  func() (string, error) { return dir, nil },
		Adapters: map[string]agents.Adapter{"codex": codex.New(filepath.Join(dir, "missing-codex"))},
	}
	return app, &out, &errOut, filepath.Join(dir, "state.db")
}

func TestRunPersistsTaskAndReportsUnavailableAdapter(t *testing.T) {
	app, out, errOut, db := testApp(t)
	code := app.Run(context.Background(), []string{"--state", db, "run", "fix", "it"})
	if code != cli.ExitAgentUnavailable {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "PLANNING") {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	code = app.Run(context.Background(), []string{"--state", db, "status", "--json"})
	if code != cli.ExitOK {
		t.Fatalf("status exit = %d, stderr = %s", code, errOut.String())
	}
	var status model.Status
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Task == nil || status.Task.Objective != "fix it" {
		t.Fatalf("status = %+v", status)
	}
}

func TestEmptyStatus(t *testing.T) {
	app, out, errOut, db := testApp(t)
	code := app.Run(context.Background(), []string{"--state", db, "status"})
	if code != cli.ExitOK {
		t.Fatalf("exit = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "task: none") {
		t.Fatalf("output = %q", out.String())
	}
}
