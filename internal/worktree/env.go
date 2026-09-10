package worktree

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// gitNoPromptEnv 返回本包派生的每个 git 子进程使用的基础环境变量，
// 并在末尾追加两个安全阀：
//
// GIT_TERMINAL_PROMPT=0: 阻止 git 打开 /dev/tty 做凭据提示
// （那样会把 CLI 卡住）。
// GIT_ASKPASS="": 关掉 askpass 图形界面程序
// （换个代码路径，效果一样）。
//
// 再配合 *exec.Cmd 上的 Stdin = nil，就堵死了 git 可能因交互
// 输入而阻塞的所有通道。
func gitNoPromptEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
}

// runGit 在 dir 里执行 `git <args.>`，关闭 stdin 并应用 no-prompt 环境。
// 返回 stdout、stderr 和退出码（进程没能启动时返回 -1）。非零退出也不抛错，
// 由调用方结合上下文判断 code != 0 算不算错误。
//
// ctx 会传递取消：取消 ctx 会杀掉 git 子进程。
func runGit(ctx context.Context, dir string, args ...string) (stdout, stderr string, code int) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitNoPromptEnv()
	cmd.Stdin = nil
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()
	if err == nil {
		return stdout, stderr, 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return stdout, stderr, ee.ExitCode()
	}
	// 进程启动失败（git 不在 PATH 上、dir 不存在等）。
	return stdout, stderr, -1
}

