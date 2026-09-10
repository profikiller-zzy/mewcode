package worktree

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxWorktreeSlugLength 把 worktree 名称限制在 64 个字符以内。
const MaxWorktreeSlugLength = 64

// validWorktreeSlugSegment 是按 '/' 切分 slug 之后，对每个分段套用的白名单正则。
var validWorktreeSlugSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ValidateWorktreeSlug 校验 worktree 的 slug，防止路径穿越和目录逃逸。
// slug 会通过 filepath.Join 拼进 `.mewcode/worktrees/<slug>`，而 filepath.Join
// 会规范化 `.` 分段 —— 所以 `./././target` 会逃出 worktrees 目录。同理，
// 绝对路径（以 `/` 或 `C:\` 开头）会把前缀整个丢掉。
//
// 允许用正斜杠做嵌套（比如 `asm/feature-foo`）；每个分段都会独立按白名单校验，
// 所以 `.` / `..` 分段和盘符字符依然会被拒绝。
//
//
// 同步返回 —— 调用方依赖它在任何副作用（git 命令、hook 执行、chdir）
// 之前跑完。
func ValidateWorktreeSlug(slug string) error {
	if len(slug) > MaxWorktreeSlugLength {
		return fmt.Errorf(
			"Invalid worktree name: must be %d characters or fewer (got %d)",
			MaxWorktreeSlugLength, len(slug),
		)
	}
	// 开头或结尾的 `/` 会让 filepath.Join 产出绝对路径或者一个悬空的分段。
	// 拆开逐个校验分段可以同时挡掉这两种情况（空分段过不了正则），
	// 又仍然允许 `user/feature`。
	for _, segment := range strings.Split(slug, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf(
				`Invalid worktree name %q: must not contain "." or ".." path segments`,
				slug,
			)
		}
		if !validWorktreeSlugSegment.MatchString(segment) {
			return fmt.Errorf(
				`Invalid worktree name %q: each "/"-separated segment must be non-empty and contain only letters, digits, dots, underscores, and dashes`,
				slug,
			)
		}
	}
	return nil
}

// FlattenSlug 把嵌套的 slug 拍平（`user/feature` → `user+feature`），
// 分支名和目录路径都用它。这两个位置
// 出现嵌套都不安全：
// git refs：`worktree-user`（文件）和 `worktree-user/feature`（需要目录）
// 构成 D/F 冲突，git 会直接拒绝。
// directory：`.mewcode/worktrees/user/feature/` 位于 `user` worktree 内部；
// 对父级执行 `git worktree remove` 会把它下面还有未提交改动的
// 子目录一起删掉。
//
// `+` 在 git 分支名和文件系统路径里都合法，但不在 slug 分段的白名单
// （[a-zA-Z0-9._-]）里，所以这个映射是单射的。
func FlattenSlug(slug string) string {
	return strings.ReplaceAll(slug, "/", "+")
}

// WorktreeBranchName 返回 slug 对应 worktree 的 git 分支名。
// 格式："worktree-<flattenedSlug>"。
func WorktreeBranchName(slug string) string {
	return "worktree-" + FlattenSlug(slug)
}

