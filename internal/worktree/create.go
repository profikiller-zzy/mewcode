package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WorktreesDir 是所有 MewCode 管理的 worktree 的存放地：repo 根目录下的
// 一个单一目录，它已经在 .gitignore 里了。
func WorktreesDir(repoRoot string) string {
	return filepath.Join(repoRoot, ".mewcode", "worktrees")
}

// WorktreePathFor 返回 slug 对应的 worktree 目录路径，嵌套的 slug
// 会先被拍平（`team/alice` → `team+alice`）。
func WorktreePathFor(repoRoot, slug string) string {
	return filepath.Join(WorktreesDir(repoRoot), FlattenSlug(slug))
}

// CreateResult 是 getOrCreateWorktree 的结果 —— Existed=true 表示我们快速恢复了
// 一个已有的 worktree（跳过了 `git worktree add` 和 performPostCreationSetup）；
// Existed=false 表示刚创建好，调用方还需要跑一遍创建后的 setup。
type CreateResult struct {
	WorktreePath   string
	WorktreeBranch string
	HeadCommit     string
	BaseBranch     string // Existed=true 时为空（恢复路径不算 baseBranch）
	Existed        bool
}

// getOrCreateWorktree 在 <repoRoot>/.mewcode/worktrees/ 下为给定 slug
// 创建一个新的 git worktree，如果已经存在则直接恢复它。
//
// 快速恢复路径：ReadWorktreeHeadSha 直接读 .git 指针文件（不起子进程，
// 也不向上遍历）。在一个有 16M 个对象的 repo 上，这能省掉每次恢复都会跑的
// `git fetch` commit-graph 扫描，大约 6-8 秒。
//
// 创建路径：只通过文件系统读取来解析默认分支（当本地已经知道
// origin/<default> 时就不跑 `git fetch`），然后执行
// `git worktree add -B worktree-<flat> <path> <baseBranch>`。
//
// `-B`（大写，不是 `-b`）：可以重置被删掉的 worktree 目录遗留的孤儿分支。
// 每次创建都能省掉一个 `git branch -D` 子进程。
func getOrCreateWorktree(ctx context.Context, repoRoot, slug string) (*CreateResult, error) {
	worktreePath := WorktreePathFor(repoRoot, slug)
	worktreeBranch := WorktreeBranchName(slug)

	// 快速恢复路径：worktree 已存在 → 跳过 fetch 和 add。
	existingHead, err := ReadWorktreeHeadSha(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("read worktree HEAD: %w", err)
	}
	if existingHead != "" {
		return &CreateResult{
			WorktreePath:   worktreePath,
			WorktreeBranch: worktreeBranch,
			HeadCommit:     existingHead,
			Existed:        true,
		}, nil
	}

	if err := os.MkdirAll(WorktreesDir(repoRoot), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktrees dir: %w", err)
	}

	// 解析 baseBranch + baseSha。如果本地已经存在 origin/<default>，就跳过 fetch ——
	// 在大 repo 里，fetch 还没碰到网络就先在本地 commit-graph 扫描上烧掉约 6-8 秒。
	// base 稍微旧一点没关系；用户想要最新的话可以在 worktree 里自己 pull。
	defaultBranch, err := GetDefaultBranch(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("get default branch: %w", err)
	}
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve git dir: %w", err)
	}
	var baseBranch, baseSha string
	if gitDir != "" {
		baseSha, _ = ResolveRef(gitDir, "refs/remotes/origin/"+defaultBranch)
	}
	if baseSha != "" {
		baseBranch = "origin/" + defaultBranch
	} else {
		// 本地不知道 origin/<default> —— 试着跑 `git fetch origin <default>`。如果失败
		// （离线、没有 remote），就回退到 HEAD，至少能从工作树当前的 commit
		// 上开分支，而不是直接报错。
		_, _, fetchCode := runGit(ctx, repoRoot, "fetch", "origin", defaultBranch)
		if fetchCode == 0 {
			baseBranch = "origin/" + defaultBranch
		} else {
			baseBranch = "HEAD"
		}
		// 通过子进程解析选中的 baseBranch 对应的 SHA —— resolveRef 在 worktree 共享的
		// commonDir 里没法直接找到 FETCH_HEAD 或 HEAD，不多做点工作是不行的。
		stdout, _, shaCode := runGit(ctx, repoRoot, "rev-parse", baseBranch)
		if shaCode != 0 {
			return nil, fmt.Errorf(`failed to resolve base branch %q: git rev-parse failed`, baseBranch)
		}
		baseSha = trimNewline(stdout)
	}

	// `-B`（大写）可以重置被删掉的 worktree 目录遗留的孤儿分支；用 `-b` 会
	// 报错。
	_, stderr, code := runGit(ctx, repoRoot, "worktree", "add", "-B", worktreeBranch, worktreePath, baseBranch)
	if code != 0 {
		return nil, fmt.Errorf("failed to create worktree: %s", stderr)
	}

	return &CreateResult{
		WorktreePath:   worktreePath,
		WorktreeBranch: worktreeBranch,
		HeadCommit:     baseSha,
		BaseBranch:     baseBranch,
		Existed:        false,
	}, nil
}

// trimNewline 去掉尾部的 CR/LF；相当于对单行命令 stdout 做一次 .trim。
// 放在本地是为了不为这点小事引入 strings 包。
func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
