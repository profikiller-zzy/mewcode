package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestIsEphemeralSlug(t *testing.T) {
	tests := []struct {
		slug     string
		expected bool
	}{
		{"agent-a1234567", true},
		{"agent-aabcdef0", true},
		{"wf_12345678-abc-1", true},
		{"wf_12345678-abc-42", true},
		{"wf-1", true},
		{"wf-99", true},
		{"bridge-abc", true},
		{"bridge-abc_def-ghi", true},
		{"job-mytemplate-12345678", true},
		// 不应该匹配
		{"my-feature", false},
		{"agent-too-long", false},
		{"agent-a123", false},     // 太短
		{"agent-aGGGGGGG", false}, // 非十六进制
		{"wf_short", false},
	}
	for _, tt := range tests {
		got := isEphemeralSlug(tt.slug)
		if got != tt.expected {
			t.Errorf("isEphemeralSlug(%q) = %v, want %v", tt.slug, got, tt.expected)
		}
	}
}

func TestCleanupStaleAgentWorktrees(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// 加一个假的 remote 好让 --not --remotes 生效
	// （worktree 的 HEAD 能从 remote 到达，所以 rev-list 返回空）
	bare := t.TempDir()
	exec.Command("git", "init", "--bare", bare).Run()
	exec.Command("git", "-C", repo, "remote", "add", "origin", bare).Run()
	exec.Command("git", "-C", repo, "push", "origin", "master").Run()

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 创建一个 ephemeral 的 worktree
	result, err := CreateAgentWorktree(context.Background(), "agent-aaaaaaaa")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 把 mtime 设成 25 小时前
	past := time.Now().Add(-25 * time.Hour)
	os.Chtimes(result.WorktreePath, past, past)

	// 以 24 小时前为 cutoff 清理，应该会把它删掉
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	// 目录应该已经没了
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("stale worktree should be removed")
	}
}

func TestCleanupStaleAgentWorktrees_SkipsUserNamed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 创建一个用户命名的 worktree（非 ephemeral）
	_, err := getOrCreateWorktree(context.Background(), repo, "my-feature")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	wtPath := WorktreePathFor(repo, "my-feature")

	// 把 mtime 设到过去
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(wtPath, past, past)

	// 清理不应该删掉用户命名的 worktree
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 0 {
		t.Fatal("user-named worktree should not be cleaned up")
	}

	if _, err := os.Stat(wtPath); err != nil {
		t.Fatal("user-named worktree should still exist")
	}
}

func TestCleanupStaleAgentWorktrees_SkipsDirtyWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-abbbbbbb")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// 把它弄脏（产生未提交改动）
	os.WriteFile(filepath.Join(result.WorktreePath, "dirty.txt"), []byte("dirty"), 0o644)
	exec.Command("git", "-C", result.WorktreePath, "add", ".").Run()

	// 把 mtime 设到过去
	past := time.Now().Add(-48 * time.Hour)
	os.Chtimes(result.WorktreePath, past, past)

	// 清理应该跳过这个脏 worktree
	cutoff := time.Now().Add(-24 * time.Hour)
	removed := CleanupStaleAgentWorktrees(context.Background(), cutoff)
	if removed != 0 {
		t.Fatal("dirty worktree should not be cleaned up")
	}
}
