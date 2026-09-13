package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectAgentWorktreeIsBoundedAndReportsChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	initTestRepo(t, repo)
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	result, err := CreateAgentWorktree(context.Background(), "agent-review01")
	if err != nil {
		t.Fatalf("create worktree failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(result.WorktreePath, "init.txt"), []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := InspectAgentWorktree(context.Background(), result.GitRoot, result.WorktreePath, 32)
	if err != nil {
		t.Fatalf("inspect worktree failed: %v", err)
	}
	if info.Clean {
		t.Fatal("expected dirty worktree")
	}
	if info.ChangedFiles != 1 {
		t.Fatalf("changed files = %d, want 1", info.ChangedFiles)
	}
	if len(info.Diff) <= 32 || !strings.Contains(info.Diff, "diff truncated") {
		t.Fatalf("diff was not bounded: %q", info.Diff)
	}
}

func TestInspectAgentWorktreeRejectsOutsideManagedDirectory(t *testing.T) {
	repo := t.TempDir()
	initTestRepo(t, repo)
	if _, err := InspectAgentWorktree(context.Background(), repo, filepath.Join(repo, "README.md"), 100); err == nil {
		t.Fatal("expected unmanaged path to be rejected")
	}
}
