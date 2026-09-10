package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreesDir(t *testing.T) {
	got := WorktreesDir("/tmp/repo")
	want := filepath.Join("/tmp/repo", ".mewcode", "worktrees")
	if got != want {
		t.Errorf("WorktreesDir = %q, want %q", got, want)
	}
}

func TestWorktreePathFor_FlattensNestedSlugs(t *testing.T) {
	got := WorktreePathFor("/tmp/repo", "team/alice")
	want := filepath.Join("/tmp/repo", ".mewcode", "worktrees", "team+alice")
	if got != want {
		t.Errorf("WorktreePathFor(team/alice) = %q, want %q", got, want)
	}
}

func TestGetOrCreateWorktree_RoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	repo := t.TempDir()
	initBareRepoWithCommit(t, repo)

	ctx := context.Background()

	// 第一次调用：新建一个 worktree。
	r1, err := getOrCreateWorktree(ctx, repo, "feature-x")
	if err != nil {
		t.Fatalf("first getOrCreateWorktree: %v", err)
	}
	if r1.Existed {
		t.Errorf("first call: Existed=true, want false")
	}
	if r1.WorktreeBranch != "worktree-feature-x" {
		t.Errorf("WorktreeBranch = %q, want worktree-feature-x", r1.WorktreeBranch)
	}
	if !strings.HasSuffix(r1.WorktreePath, filepath.Join(".mewcode", "worktrees", "feature-x")) {
		t.Errorf("WorktreePath = %q, missing expected suffix", r1.WorktreePath)
	}
	if !IsValidGitSha(r1.HeadCommit) {
		t.Errorf("HeadCommit = %q, not a valid SHA", r1.HeadCommit)
	}
	if _, err := os.Stat(filepath.Join(r1.WorktreePath, ".git")); err != nil {
		t.Errorf(".git pointer not present in worktree: %v", err)
	}

	// 第二次调用同样的 slug：快速恢复，返回 Existed=true。
	r2, err := getOrCreateWorktree(ctx, repo, "feature-x")
	if err != nil {
		t.Fatalf("second getOrCreateWorktree: %v", err)
	}
	if !r2.Existed {
		t.Errorf("second call: Existed=false, want true (fast resume)")
	}
	if r2.HeadCommit != r1.HeadCommit {
		t.Errorf("resume HeadCommit = %q, want same as create (%q)", r2.HeadCommit, r1.HeadCommit)
	}

	// 手动删掉 worktree 目录；下一次调用应该重新走完整的
	// 创建流程（并且 -B 会重置那个孤立的分支）。
	if out, err := exec.Command("git", "-C", repo, "worktree", "remove", "--force", r1.WorktreePath).CombinedOutput(); err != nil {
		t.Fatalf("cleanup git worktree remove: %v\n%s", err, out)
	}
	r3, err := getOrCreateWorktree(ctx, repo, "feature-x")
	if err != nil {
		t.Fatalf("third getOrCreateWorktree (after remove): %v", err)
	}
	if r3.Existed {
		t.Errorf("third call (after remove): Existed=true, want false")
	}
}

func TestGetOrCreateWorktree_NestedSlug(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	repo := t.TempDir()
	initBareRepoWithCommit(t, repo)
	r, err := getOrCreateWorktree(context.Background(), repo, "team-refactor/alice")
	if err != nil {
		t.Fatalf("getOrCreateWorktree: %v", err)
	}
	if r.WorktreeBranch != "worktree-team-refactor+alice" {
		t.Errorf("WorktreeBranch = %q, want worktree-team-refactor+alice", r.WorktreeBranch)
	}
	if !strings.HasSuffix(r.WorktreePath, filepath.Join(".mewcode", "worktrees", "team-refactor+alice")) {
		t.Errorf("WorktreePath flatten mismatch: %q", r.WorktreePath)
	}
}
