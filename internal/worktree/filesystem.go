// Package worktree 文件系统辅助：在不启动 git 子进程的前提下读取 git 状态。
//
// 覆盖范围：解析 .git 目录（含 worktree/子模块）、解析 HEAD、通过
// 松散文件和 packed-refs 解析 ref。
//
// 正确性说明（已对照 git 源码核实）：
// HEAD: `ref: refs/heads/<branch>\n` 或裸 SHA（refs/files-backend.c）
// Packed-refs: `<sha> <refname>\n`，跳过 `#` 和 `^` 开头的行（packed-backend.c）
// git 文件（worktree）：`gitdir: <path>\n`，路径可为相对路径（setup.c）
package worktree

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safeRefName 允许 ASCII 字母数字以及 '/'、'.'、'_'、'+'、'-'、'@'。用于校验从
// .git/ 读到的 ref/分支名，这样被篡改的 HEAD 或 ref 文件就无法
// 注入路径穿越、参数前缀或 shell 元字符。
var safeRefName = regexp.MustCompile(`^[a-zA-Z0-9/._+@-]+$`)

// IsSafeRefName 校验 ref/分支名是否可以安全地用于路径拼接、作为 git 位置参数，
// 以及拼进 shell 命令。
func IsSafeRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") {
		return false
	}
	if strings.Contains(name, "..") {
		return false
	}
	// 拒绝单点（.）和空的路径分段。
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == "" {
			return false
		}
	}
	return safeRefName.MatchString(name)
}

var sha1Pattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// IsValidGitSha 判断 s 是否为完整长度的 SHA-1（40 位十六进制）或
// SHA-256（64 位十六进制）git object id。git 从不往 HEAD 或 ref 文件里写缩写 SHA。
func IsValidGitSha(s string) bool {
	return sha1Pattern.MatchString(s) || sha256Pattern.MatchString(s)
}

// ResolveGitDir 解析以 root 为根的仓库真正的 .git 目录。处理 .git
// 是包含 `gitdir: <path>` 的文件的 worktree/子模块情况。当 root 下没有 .git
// 条目（不是仓库）时返回 ("", nil) —— 调用方把空串当作「不是 git 仓库」。
// 错误只留给调用方关心的 IO 失败（这里指除 ENOENT 之外的文件系统错误）。
//
// 去掉了记忆化（Go 的调用方在更上层做缓存）。
func ResolveGitDir(root string) (string, error) {
	gitPath := filepath.Join(root, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if !st.IsDir() {
		// Worktree 或子模块：.git 是一个带 `gitdir: <path>` 的文件。git 通过
		// strbuf_rtrim 去掉尾部空白（setup.c read_gitfile_gently）；strings.TrimSpace 与之等价。
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return "", err
		}
		content := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(content, "gitdir:") {
			return "", nil
		}
		rel := strings.TrimSpace(strings.TrimPrefix(content, "gitdir:"))
		// 相对于 root（.git 指针文件所在的目录）解析。
		if filepath.IsAbs(rel) {
			return rel, nil
		}
		return filepath.Clean(filepath.Join(root, rel)), nil
	}
	return gitPath, nil
}

// GetCommonDir 读取 worktree 的 gitDir 里的 `commondir` 文件，找出共享的 git
// 目录。在 worktree 中，它指向主仓库的 .git 目录。
// 若不存在 commondir 文件（普通仓库）则返回 ("", nil)。
func GetCommonDir(gitDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if filepath.IsAbs(content) {
		return content, nil
	}
	return filepath.Clean(filepath.Join(gitDir, content)), nil
}

// gitHead 是 <gitDir>/HEAD 的解析结果。
type gitHead struct {
	// branch 在 HEAD 处于某个分支上时非空。
	branch string
	// sha 在 HEAD 处于 detached（裸 SHA）或某个非常规 symref 已被解析时非空。
	sha string
}

// readGitHead 解析 <gitDir>/HEAD，判断当前分支或 detached 的 SHA。当 HEAD 不存在
// 或格式损坏时返回 (nil, nil) —— 调用方把它当作「不是 worktree」/「不是仓库」。
// 除 ENOENT 之外的 IO 错误会向上传递。
//
// HEAD 格式（依据 refs/files-backend.c）：
// `ref: refs/heads/<branch>\n` —— 在分支上
// `ref: <other-ref>\n` —— 非常规 symref（例如 bisect 期间）
// `<hex-sha>\n` —— detached HEAD（例如 rebase 期间）
func readGitHead(gitDir string) (*gitHead, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	content := strings.TrimSpace(string(raw))
	if strings.HasPrefix(content, "ref:") {
		ref := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
		if strings.HasPrefix(ref, "refs/heads/") {
			name := strings.TrimPrefix(ref, "refs/heads/")
			if !IsSafeRefName(name) {
				return nil, nil
			}
			return &gitHead{branch: name}, nil
		}
		// 非常规 symref（不是本地分支）—— 解析成 SHA。
		if !IsSafeRefName(ref) {
			return nil, nil
		}
		sha, err := ResolveRef(gitDir, ref)
		if err != nil {
			return nil, err
		}
		return &gitHead{sha: sha}, nil
	}
	// 裸 SHA（detached HEAD）。做校验是为了防止被篡改的 HEAD
	// 把 shell 元字符带进下游场景。
	if !IsValidGitSha(content) {
		return nil, nil
	}
	return &gitHead{sha: content}, nil
}

// ResolveRef 把 git ref（例如 `refs/heads/main`）解析成 commit SHA。先查松散 ref
// 文件，再回退到 packed-refs。会跟随 symref（例如 `ref: refs/remotes/origin/main`）。
//
// 对 worktree 来说，ref 位于公共 gitdir（`commondir` 文件指向的目录）里，
// 而不是 worktree 自己的 gitdir。我们先查 worktree 的 gitdir，再回退到公共目录。
func ResolveRef(gitDir, ref string) (string, error) {
	sha, err := resolveRefInDir(gitDir, ref)
	if err != nil {
		return "", err
	}
	if sha != "" {
		return sha, nil
	}
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "", err
	}
	if commonDir != "" && commonDir != gitDir {
		return resolveRefInDir(commonDir, ref)
	}
	return "", nil
}

// resolveRefInDir 在单个 git 目录内解析 ref（不做 commonDir 回退）。
func resolveRefInDir(dir, ref string) (string, error) {
	// 先试松散 ref 文件。
	raw, err := os.ReadFile(filepath.Join(dir, ref))
	if err == nil {
		content := strings.TrimSpace(string(raw))
		if strings.HasPrefix(content, "ref:") {
			target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
			if !IsSafeRefName(target) {
				return "", nil
			}
			// 递归跟随 symref 链。传 `dir`（而不是 gitDir），这样 resolveRef 的
			// commonDir 回退才会从同一个起点开始。
			return ResolveRef(dir, target)
		}
		if !IsValidGitSha(content) {
			return "", nil
		}
		return content, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	// 回退到 packed-refs。
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		spaceIdx := strings.IndexByte(line, ' ')
		if spaceIdx == -1 {
			continue
		}
		if line[spaceIdx+1:] == ref {
			sha := line[:spaceIdx]
			if !IsValidGitSha(sha) {
				return "", nil
			}
			return sha, nil
		}
	}
	return "", nil
}

// ReadRawSymref 读取原始 symref 文件，提取已知前缀之后的分支名。若 ref
// 不存在、不是 symref 或不匹配前缀，则返回 ("", nil)。
// 只检查松散文件 —— packed-refs 不存 symref。
func ReadRawSymref(gitDir, refPath, branchPrefix string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(gitDir, refPath))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	content := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(content, "ref:") {
		return "", nil
	}
	target := strings.TrimSpace(strings.TrimPrefix(content, "ref:"))
	if !strings.HasPrefix(target, branchPrefix) {
		return "", nil
	}
	name := strings.TrimPrefix(target, branchPrefix)
	if !IsSafeRefName(name) {
		return "", nil
	}
	return name, nil
}

// GetDefaultBranch 通过从公共 gitdir 读取 refs/remotes/origin/HEAD（一个 symref）
// 来确定仓库的默认分支，回退策略是依次尝试 `main` 和 `master`，
// 最后兜底返回 "main"。
//
// 纯文件系统读取 —— 不启动 git 子进程，不联网。
func GetDefaultBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return "main", err
	}
	if gitDir == "" {
		return "main", nil
	}
	// refs/remotes/ 位于 commonDir，而不在每个 worktree 自己的 gitDir 里。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return "main", err
	}
	if commonDir == "" {
		commonDir = gitDir
	}
	branch, err := ReadRawSymref(commonDir, "refs/remotes/origin/HEAD", "refs/remotes/origin/")
	if err != nil {
		return "main", err
	}
	if branch != "" {
		return branch, nil
	}
	for _, candidate := range []string{"main", "master"} {
		sha, err := ResolveRef(commonDir, "refs/remotes/origin/"+candidate)
		if err != nil {
			return "main", err
		}
		if sha != "" {
			return candidate, nil
		}
	}
	return "main", nil
}

// GetCurrentBranch 读取 <repoRoot>/.git/HEAD 并返回当前分支名，HEAD 处于
// detached 时返回 ""。纯文件系统读取；（用空串
// 而不是哨兵值 "HEAD" 来区分 detached HEAD）。
func GetCurrentBranch(repoRoot string) (string, error) {
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil || gitDir == "" {
		return "", err
	}
	head, err := readGitHead(gitDir)
	if err != nil || head == nil {
		return "", err
	}
	return head.branch, nil
}

// ReadWorktreeHeadSha 读取 git worktree 目录（不是主仓库）的 HEAD SHA。与
// ResolveGitDir+readGitHead 串联不同，它直接把 `<worktreePath>/.git` 当作
// `gitdir:` 指针文件读取，不做向上遍历。当 worktree 不存在（`.git`
// 指针 ENOENT）或格式损坏时返回 ("", nil)；调用方把空串当作「不是合法的 worktree」。
//
// 性能目标：≤10ms（纯文件系统读取，无子进程）。在 1600 万 object 的仓库上，
// `git rev-parse HEAD` 光进程启动开销就要 ~15ms。
func ReadWorktreeHeadSha(worktreePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	ptr := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(ptr, "gitdir:") {
		return "", nil
	}
	rel := strings.TrimSpace(strings.TrimPrefix(ptr, "gitdir:"))
	var gitDir string
	if filepath.IsAbs(rel) {
		gitDir = rel
	} else {
		gitDir = filepath.Clean(filepath.Join(worktreePath, rel))
	}
	head, err := readGitHead(gitDir)
	if err != nil {
		return "", err
	}
	if head == nil {
		return "", nil
	}
	if head.branch != "" {
		return ResolveRef(gitDir, "refs/heads/"+head.branch)
	}
	return head.sha, nil
}
