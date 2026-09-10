package worktree

import "fmt"

// BuildWorktreeNotice 返回注入到 sub-agent prompt 里的提示文本（当它们跑在
// 独立 worktree 中时）。告诉子 Agent 把继承来的上下文里的路径
// 翻译成自己的路径，并重新读一遍文件。
func BuildWorktreeNotice(parentCwd, worktreeCwd string) string {
	return fmt.Sprintf(
		"You've inherited the conversation context above from a parent agent working in %s. "+
			"You are operating in an isolated git worktree at %s — same repository, same relative "+

			"file structure, separate working copy. Paths in the inherited context refer to the "+
			"parent's working directory; translate them to your worktree root. Re-read files before "+
			"editing if the parent may have modified them since they appear in the context. Your "+
			"changes stay in this worktree and will not affect the parent's files.",

		parentCwd, worktreeCwd,
	)
}

