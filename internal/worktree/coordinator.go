package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

const defaultWorktreeInspectBytes = 20000

// WorktreeInspection 是 Lead 审查一个 Worker worktree 时需要的有限信息。
// Diff 有大小上限，避免一次工具调用把整个仓库灌进 Lead 上下文。
type WorktreeInspection struct {
	WorktreePath string
	Branch       string
	HeadCommit   string
	RootCommit   string
	Clean        bool
	ChangedFiles int
	Commits      int
	Status       string
	Diff         string
}

// InspectAgentWorktree 只读取指定的 MewCode worktree，不执行任何写操作。
func InspectAgentWorktree(ctx context.Context, gitRoot, worktreePath string, maxBytes int) (*WorktreeInspection, error) {
	if err := validateManagedWorktree(gitRoot, worktreePath); err != nil {
		return nil, err
	}
	if maxBytes <= 0 {
		maxBytes = defaultWorktreeInspectBytes
	}

	branchOut, stderr, code := runGit(ctx, worktreePath, "branch", "--show-current")
	if code != 0 {
		return nil, fmt.Errorf("read worktree branch failed: %s", strings.TrimSpace(stderr))
	}
	headOut, stderr, code := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if code != 0 {
		return nil, fmt.Errorf("read worktree HEAD failed: %s", strings.TrimSpace(stderr))
	}
	rootHeadOut, _, rootCode := runGit(ctx, gitRoot, "rev-parse", "HEAD")
	if rootCode != 0 {
		return nil, fmt.Errorf("read repository HEAD failed")
	}

	status, stderr, code := runGit(ctx, worktreePath, "status", "--short")
	if code != 0 {
		return nil, fmt.Errorf("read worktree status failed: %s", strings.TrimSpace(stderr))
	}
	status = strings.TrimSpace(status)

	commits := 0
	rootCommit := strings.TrimSpace(rootHeadOut)
	if IsValidGitSha(strings.TrimSpace(headOut)) && IsValidGitSha(rootCommit) {
		countOut, _, countCode := runGit(ctx, worktreePath, "rev-list", "--count", rootCommit+"..HEAD")
		if countCode == 0 {
			commits, _ = strconv.Atoi(strings.TrimSpace(countOut))
		}
	}

	diff := ""
	if IsValidGitSha(strings.TrimSpace(headOut)) && IsValidGitSha(rootCommit) {
		committed, _, committedCode := runGit(ctx, worktreePath, "diff", "--no-ext-diff", "--unified=3", rootCommit+"...HEAD")
		if committedCode == 0 {
			diff = committed
		}
	}
	// 已提交 diff 不包含工作区未提交内容；补上这部分，Lead 才能判断是否可以安全合并。
	working, _, workingCode := runGit(ctx, worktreePath, "diff", "--no-ext-diff", "--unified=3", "HEAD")
	if workingCode == 0 && strings.TrimSpace(working) != "" {
		if diff != "" {
			diff += "\n\n--- uncommitted changes ---\n\n"
		}
		diff += working
	}
	diff = truncateOutput(diff, maxBytes)

	return &WorktreeInspection{
		WorktreePath: worktreePath,
		Branch:       strings.TrimSpace(branchOut),
		HeadCommit:   strings.TrimSpace(headOut),
		RootCommit:   rootCommit,
		Clean:        status == "",
		ChangedFiles: countStatusLines(status),
		Commits:      commits,
		Status:       status,
		Diff:         diff,
	}, nil
}

// MergeAgentWorktree 将指定 Worker worktree 的分支整合到 gitRoot。
// strategy 支持 merge 和 cherry-pick；两者都要求主仓库与 Worker 工作区干净。
// 调用方必须先通过 Team 成员元数据解析 worktreePath，不能传任意路径。
func MergeAgentWorktree(ctx context.Context, gitRoot, worktreePath, strategy, commit string) (string, error) {
	if err := validateManagedWorktree(gitRoot, worktreePath); err != nil {
		return "", err
	}
	branchOut, stderr, code := runGit(ctx, worktreePath, "branch", "--show-current")
	if code != 0 {
		return "", fmt.Errorf("read worktree branch failed: %s", strings.TrimSpace(stderr))
	}
	branch := strings.TrimSpace(branchOut)
	if !strings.HasPrefix(branch, "worktree-") || !IsSafeRefName(branch) {
		return "", fmt.Errorf("refuse to merge unmanaged worktree branch %q", branch)
	}
	if status, _, statusCode := runGit(ctx, gitRoot, "status", "--porcelain"); statusCode != 0 || strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("main repository has uncommitted changes; merge aborted")
	}
	if status, _, statusCode := runGit(ctx, worktreePath, "status", "--porcelain"); statusCode != 0 || strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("worktree has uncommitted changes; commit them before merging")
	}

	var output string
	err := withRepositoryLock(ctx, gitRoot, func() error {
		var stderr string
		var code int
		switch strategy {
		case "", "merge":
			output, stderr, code = runGit(ctx, gitRoot, "merge", "--no-edit", "--no-ff", branch)
		case "cherry-pick":
			if !IsValidGitSha(commit) {
				return fmt.Errorf("cherry-pick requires a full commit SHA")
			}
			if _, _, ancestorCode := runGit(ctx, gitRoot, "merge-base", "--is-ancestor", commit, branch); ancestorCode != 0 {
				return fmt.Errorf("commit %s is not reachable from worker branch %s", commit, branch)
			}
			output, stderr, code = runGit(ctx, gitRoot, "cherry-pick", commit)
		default:
			return fmt.Errorf("unsupported merge strategy %q (use merge or cherry-pick)", strategy)
		}
		if code != 0 {
			// 不把主仓库留在 MERGE_HEAD/CHERRY_PICK_HEAD 状态；冲突交给
			// Worker 在它自己的 worktree 里解决后重新提交。
			if strategyName(strategy) == "merge" {
				_, _, _ = runGit(ctx, gitRoot, "merge", "--abort")
			} else {
				_, _, _ = runGit(ctx, gitRoot, "cherry-pick", "--abort")
			}
			return fmt.Errorf("%s failed: %s", strategyName(strategy), strings.TrimSpace(stderr))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	head, _, _ := runGit(ctx, gitRoot, "rev-parse", "HEAD")
	return fmt.Sprintf("%s\nmain_head=%s", strings.TrimSpace(output), strings.TrimSpace(head)), nil
}

func strategyName(strategy string) string {
	if strategy == "" {
		return "merge"
	}
	return strategy
}

func validateManagedWorktree(gitRoot, worktreePath string) error {
	if gitRoot == "" || worktreePath == "" {
		return fmt.Errorf("git root and worktree path are required")
	}
	root, err := filepath.Abs(filepath.Clean(gitRoot))
	if err != nil {
		return err
	}
	path, err := filepath.Abs(filepath.Clean(worktreePath))
	if err != nil {
		return err
	}
	managedRoot := filepath.Clean(WorktreesDir(root))
	managedResolved, resolveErr := filepath.EvalSymlinks(managedRoot)
	if resolveErr != nil {
		return fmt.Errorf("managed worktrees directory unavailable: %w", resolveErr)
	}
	pathResolved, resolveErr := filepath.EvalSymlinks(path)
	if resolveErr != nil {
		return fmt.Errorf("worktree %q is unavailable: %w", worktreePath, resolveErr)
	}
	rel, err := filepath.Rel(managedResolved, pathResolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("worktree path %q is outside managed worktrees", worktreePath)
	}
	info, err := os.Stat(pathResolved)
	if err != nil {
		return fmt.Errorf("worktree %q is unavailable: %w", worktreePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("worktree path %q is not a directory", worktreePath)
	}
	return nil
}

func countStatusLines(status string) int {
	if strings.TrimSpace(status) == "" {
		return 0
	}
	count := 0
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func truncateOutput(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n\n[diff truncated at %d bytes]", maxBytes)
}
