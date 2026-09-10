package worktree

import (
	"context"
	"strconv"
	"strings"
)

// ChangeSummary 保存 countWorktreeChanges 的结果。
type ChangeSummary struct {
	ChangedFiles int
	Commits      int
}

// HasWorktreeChanges 在 worktree 有未提交改动、或者自 headCommit 之后有新提交时
// 返回 true。git 执行失败时也返回 true（fail-closed）。
func HasWorktreeChanges(ctx context.Context, worktreePath, headCommit string) bool {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return true // 出错即保守处理
	}
	if strings.TrimSpace(stdout) != "" {
		return true
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", headCommit+"..HEAD")
	if code != 0 {
		return true // 出错即保守处理
	}
	n, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return true // 出错即保守处理
	}
	return n > 0
}

// CountWorktreeChanges 返回详细的变更摘要；状态无法可靠确定时返回 nil。
// 把它当安全闸门用的调用方必须把 nil 当成「未知，按不安全处理」
// （fail-closed）。
func CountWorktreeChanges(ctx context.Context, worktreePath, originalHeadCommit string) *ChangeSummary {
	stdout, _, code := runGit(ctx, worktreePath, "status", "--porcelain")
	if code != 0 {
		return nil // 出错即保守处理
	}
	changedFiles := 0
	for _, line := range strings.Split(stdout, "\n") {
		if strings.TrimSpace(line) != "" {
			changedFiles++
		}
	}

	if originalHeadCommit == "" {
		// 没有基线 commit 就没法数提交数。fail-closed。
		return nil
	}

	stdout, _, code = runGit(ctx, worktreePath, "rev-list", "--count", originalHeadCommit+"..HEAD")
	if code != 0 {
		return nil // 出错即保守处理
	}
	commits, err := strconv.Atoi(strings.TrimSpace(stdout))
	if err != nil {
		return nil
	}

	return &ChangeSummary{
		ChangedFiles: changedFiles,
		Commits:      commits,
	}
}

