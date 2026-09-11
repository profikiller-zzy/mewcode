package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// canonicalPath 解析路径里的符号链接。macOS 上 t.TempDir() 返回 /var/...，
// 而 git（以及 FindCanonicalGitRoot）输出的是解析后的 /private/var/...；
// 直接比较字符串会因环境差异失败。
func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}

func TestCreateAgentWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	// CreateAgentWorktree 需要在 git 仓库内部调用
	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-a1234567")
	if err != nil {
		t.Fatalf("CreateAgentWorktree failed: %v", err)
	}

	// 返回值经 FindCanonicalGitRoot 解析过真实路径，断言前先对齐。
	canonicalRepo := canonicalPath(t, repo)
	expectedPath := filepath.Join(canonicalRepo, ".mewcode", "worktrees", "agent-a1234567")
	if result.WorktreePath != expectedPath {
		t.Fatalf("expected path %q, got %q", expectedPath, result.WorktreePath)
	}
	if result.GitRoot != canonicalRepo {
		t.Fatalf("expected git root %q, got %q", canonicalRepo, result.GitRoot)
	}
	if result.HeadCommit == "" {
		t.Fatal("expected non-empty head commit")
	}

	// 目录应该已经存在
	if _, err := os.Stat(result.WorktreePath); err != nil {
		t.Fatalf("worktree directory not created: %v", err)
	}

	// 不应该设置 session 单例（agent worktree 是没有 session 的）
	if s := GetCurrentWorktreeSession(); s != nil {
		t.Fatal("CreateAgentWorktree should not touch global session")
	}
}

func TestCreateAgentWorktree_Resume(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	// 第一次调用是创建
	r1, err := CreateAgentWorktree(context.Background(), "agent-a7777777")
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// 第二次调用应该走恢复路径（并刷新 mtime）
	r2, err := CreateAgentWorktree(context.Background(), "agent-a7777777")
	if err != nil {
		t.Fatalf("second call failed: %v", err)
	}
	if r2.WorktreePath != r1.WorktreePath {
		t.Fatal("resume should return same path")
	}
}

func TestRemoveAgentWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	repo := t.TempDir()
	initTestRepo(t, repo)

	origDir, _ := os.Getwd()
	defer os.Chdir(origDir)
	os.Chdir(repo)

	result, err := CreateAgentWorktree(context.Background(), "agent-aabcdef0")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	ok := RemoveAgentWorktree(context.Background(), result.WorktreePath, result.WorktreeBranch, result.GitRoot)
	if !ok {
		t.Fatal("RemoveAgentWorktree returned false")
	}

	// 目录应该已经没了
	if _, err := os.Stat(result.WorktreePath); !os.IsNotExist(err) {
		t.Fatal("worktree directory should be removed")
	}
}

func TestRemoveAgentWorktree_NoGitRoot(t *testing.T) {
	ok := RemoveAgentWorktree(context.Background(), "/tmp/nonexistent", "branch", "")
	if ok {
		t.Fatal("expected false when gitRoot is empty")
	}
}
