package agents

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStartProcessStopsAfterTerminalAgentError(t *testing.T) {
	run, err := StartProcess(context.Background(), "sh", []string{"-c", `printf '%s\n' error; sleep 5`}, t.TempDir(), func(line []byte) (Event, bool, error) {
		if strings.TrimSpace(string(line)) == "error" {
			return Event{Kind: EventError, Message: "quota exhausted"}, true, nil
		}
		return Event{}, false, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	result := run.Wait()
	if result.Err == nil || result.Err.Error() != "quota exhausted" {
		t.Fatalf("result error = %v, want quota exhausted", result.Err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("terminal error took %s; child was not stopped promptly", elapsed)
	}
}
