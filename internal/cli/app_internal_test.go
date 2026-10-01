package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsGitRepository(t *testing.T) {
	workspace := t.TempDir()
	if isGitRepository(context.Background(), workspace) {
		t.Fatal("uninitialized workspace reported as a Git repository")
	}
	if err := exec.Command("git", "-C", workspace, "init").Run(); err != nil {
		t.Fatal(err)
	}
	if !isGitRepository(context.Background(), filepath.Clean(workspace)) {
		t.Fatal("initialized workspace was not detected as a Git repository")
	}
}
