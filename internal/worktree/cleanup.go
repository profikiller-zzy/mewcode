package worktree

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"
)

// ephemeralWorktreePatterns 用来识别可以自动清理的一次性 worktree。用户自己起名的
// worktree（例如 "my-feature"）永远不会匹配。
var ephemeralWorktreePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^agent-a[0-9a-f]{7}$`),
	regexp.MustCompile(`^wf_[0-9a-f]{8}-[0-9a-f]{3}-\d+$`),
	regexp.MustCompile(`^wf-\d+$`),
	regexp.MustCompile(`^bridge-[A-Za-z0-9_]+(-[A-Za-z0-9_]+)*$`),
	regexp.MustCompile(`^job-[a-zA-Z0-9._-]{1,55}-[0-9a-f]{8}$`),
}

func isEphemeralSlug(slug string) bool {
	for _, p := range ephemeralWorktreePatterns {
		if p.MatchString(slug) {
			return true
		}
	}
	return false
}

// CleanupStaleAgentWorktrees 清理早于 cutoffDate 的过期 agent/workflow worktree。
// 三层安全过滤：
//
// 1. 名字模式：只认 ephemeral slug 2. 时间 + session：跳过当前 session 和最近修改过的
// 3. 变更检查：有已跟踪改动或未推送 commit 的跳过。
func CleanupStaleAgentWorktrees(ctx context.Context, cutoffDate time.Time) int {
	cwd, _ := os.Getwd()
	gitRoot := FindCanonicalGitRoot(cwd)
	if gitRoot == "" {
		return 0
	}

	dir := WorktreesDir(gitRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}

	currentPath := ""
	if s := GetCurrentWorktreeSession(); s != nil {
		currentPath = s.WorktreePath
	}

	removed := 0
	for _, entry := range entries {
		slug := entry.Name()

		// 第 1 层：只认 ephemeral 模式。
		if !isEphemeralSlug(slug) {
			continue
		}

		worktreePath := WorktreePathFor(gitRoot, slug)
		if currentPath == worktreePath {
			continue
		}

		// 第 2 层：时间检查。
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoffDate) {
			continue
		}

		// 第 3 层：fail-closed 的变更检查。带 -uno：过期崩溃 Agent 的 worktree 里的
		// 未跟踪文件都是构建产物；跳过未跟踪文件的扫描在大仓库上能快 5-10 倍。
		statusOut, _, statusCode := runGit(ctx, worktreePath,
			"--no-optional-locks", "status", "--porcelain", "-uno")
		if statusCode != 0 || strings.TrimSpace(statusOut) != "" {
			continue
		}

		unpushedOut, _, unpushedCode := runGit(ctx, worktreePath,
			"rev-list", "--max-count=1", "HEAD", "--not", "--remotes")
		if unpushedCode != 0 || strings.TrimSpace(unpushedOut) != "" {
			continue
		}

		if RemoveAgentWorktree(ctx, worktreePath, WorktreeBranchName(slug), gitRoot) {
			removed++
		}
	}

	if removed > 0 {
		runGit(ctx, gitRoot, "worktree", "prune")
	}
	return removed
}

// StartCleanupLoop 在后台 goroutine 里周期性清理过期 worktree。立即返回；
// ctx 被取消时 goroutine 退出。
func StartCleanupLoop(ctx context.Context) {
	interval := GetStaleCleanupInterval()
	if interval <= 0 {
		return
	}
	cutoffHours := GetStaleCutoffHours()

	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-time.Duration(cutoffHours) * time.Hour)
				CleanupStaleAgentWorktrees(ctx, cutoff)
			}
		}
	}()
}

