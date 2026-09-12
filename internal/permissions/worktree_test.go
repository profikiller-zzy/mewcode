package permissions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathSandboxForWorktreeRebasesProjectRoot(t *testing.T) {
	project, err := os.MkdirTemp("/var/tmp", "mewcode-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	worktree := filepath.Join(project, ".mewcode", "worktrees", "agent-a")
	shared := t.TempDir()
	sandbox := NewPathSandbox(project, shared)
	rebased := sandbox.ForWorktree(worktree)

	if ok, _ := rebased.Check(filepath.Join(worktree, "main.go")); !ok {
		t.Fatal("worktree path should be allowed")
	}
	if ok, _ := rebased.Check(filepath.Join(project, "main.go")); ok {
		t.Fatal("parent project path should not be allowed")
	}
	if ok, _ := rebased.Check(filepath.Join(shared, "memory.md")); !ok {
		t.Fatal("unrelated shared root should remain allowed")
	}
}

func TestCheckerForWorktreeKeepsProjectPolicyAndRebasesLocalPolicy(t *testing.T) {
	project, err := os.MkdirTemp("/var/tmp", "mewcode-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(project) })
	worktree := filepath.Join(project, ".mewcode", "worktrees", "agent-a")
	rules := NewRuleEngine(project)
	checker := NewChecker(NewPathSandbox(project), rules, ModeDefault)
	rebased := checker.ForWorktree(worktree)

	if rebased.RuleEngine == rules {
		t.Fatal("worktree checker should have a private rule engine instance")
	}
	if rebased.RuleEngine.ProjectPath != rules.ProjectPath {
		t.Fatal("project policy path should remain shared")
	}
	wantLocal := filepath.Join(worktree, ".mewcode", "permissions.local.yaml")
	if rebased.RuleEngine.LocalPath != wantLocal {
		t.Fatalf("local policy path = %q, want %q", rebased.RuleEngine.LocalPath, wantLocal)
	}
}
