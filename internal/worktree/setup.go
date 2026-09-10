package worktree

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// performPostCreationSetup 把主仓库里的 settings、hooks、符号链接以及
// 被 gitignore 的文件同步到新建的 worktree 中。
func performPostCreationSetup(ctx context.Context, repoRoot, worktreePath string) {
	// A. 复制 settings.local.json。
	copySettingsLocal(repoRoot, worktreePath)

	// B. 配置 git hooks 路径。
	configureHooksPath(ctx, repoRoot, worktreePath)

	// C. 为大目录创建符号链接（通过配置开启）。
	symlinkDirectories(repoRoot, worktreePath, getSymlinkDirectories())

	// D. 从 .worktreeinclude 复制被 gitignore 的文件。
	CopyWorktreeIncludeFiles(ctx, repoRoot, worktreePath)
}

// copySettingsLocal 把主仓库的 .mewcode/settings.local.json 复制到 worktree。
// 这样可以把本地设置（可能包含密钥）同步过去。
func copySettingsLocal(repoRoot, worktreePath string) {
	relPath := filepath.Join(".mewcode", "settings.local.json")
	src := filepath.Join(repoRoot, relPath)
	dst := filepath.Join(worktreePath, relPath)

	srcData, err := os.ReadFile(src)
	if err != nil {
		return // ENOENT 没关系 —— 本来就没有本地设置要复制
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(dst, srcData, 0o644)
}

// configureHooksPath 在 worktree 里设置 core.hooksPath，让主仓库的 git hooks
// 可以共用。优先使用 .husky/，其次才是 .git/hooks/。
func configureHooksPath(ctx context.Context, repoRoot, worktreePath string) {
	candidates := []string{
		filepath.Join(repoRoot, ".husky"),
		filepath.Join(repoRoot, ".git", "hooks"),
	}
	var hooksPath string
	for _, c := range candidates {
		info, err := os.Stat(c)
		if err == nil && info.IsDir() {
			hooksPath = c
			break
		}
	}
	if hooksPath == "" {
		return
	}
	_, _, code := runGit(ctx, worktreePath, "config", "core.hooksPath", hooksPath)
	if code != 0 {
		// 尽力而为 —— 不要因为这一步失败就让整个 setup 挂掉。
		return
	}
}

// symlinkDirectories 把 repoRoot 下的目录以符号链接的方式挂到 worktreePath，
// 避免磁盘膨胀（比如 node_modules、vendor）。
func symlinkDirectories(repoRoot, worktreePath string, dirs []string) {
	for _, dir := range dirs {
		if strings.Contains(dir, "..") {
			continue // 防止路径穿越
		}
		src := filepath.Join(repoRoot, dir)
		dst := filepath.Join(worktreePath, dir)
		// 建符号链接是尽力而为：源可能不存在，目标可能已经存在。
		_ = os.Symlink(src, dst)
	}
}

// getSymlinkDirectories 返回配置好的需要建符号链接的目录列表。
// 通过 settings.worktree.symlinkDirectories 配置；能读到配置就读，默认为空。
func getSymlinkDirectories() []string {
	return worktreeConfig.SymlinkDirectories
}

// CopyWorktreeIncludeFiles 把 .worktreeinclude 里指定的、被 gitignore 的文件
// 从基础仓库复制到 worktree。使用 gitignore 语法的模式匹配。
func CopyWorktreeIncludeFiles(ctx context.Context, repoRoot, worktreePath string) ([]string, error) {
	includeFile := filepath.Join(repoRoot, ".worktreeinclude")
	data, err := os.ReadFile(includeFile)
	if err != nil {
		return nil, nil // 没有 .worktreeinclude → 没有要复制的东西
	}

	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if len(patterns) == 0 {
		return nil, nil
	}

	// 用 git ls-files 列出被 gitignore 的文件。
	stdout, _, code := runGit(ctx, repoRoot,
		"ls-files", "--others", "--ignored", "--exclude-standard", "--directory")
	if code != 0 || strings.TrimSpace(stdout) == "" {
		return nil, nil
	}

	entries := strings.Split(strings.TrimSpace(stdout), "\n")

	// 简单的模式匹配：对每个被 gitignore 的文件，检查是否有 .worktreeinclude
	// 里的模式能匹配上。基础 glob 匹配用 filepath.Match，目录模式用前缀匹配。
	var toCopy []string
	for _, entry := range entries {
		if entry == "" {
			continue
		}
		// 跳过折叠的目录（以 / 结尾的）。
		if strings.HasSuffix(entry, "/") {
			continue
		}
		if matchesWorktreeInclude(entry, patterns) {
			toCopy = append(toCopy, entry)
		}
	}

	var copied []string
	for _, rel := range toCopy {
		src := filepath.Join(repoRoot, rel)
		dst := filepath.Join(worktreePath, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		if err := copyFileContents(src, dst); err != nil {
			continue
		}
		copied = append(copied, rel)
	}
	return copied, nil
}

// matchesWorktreeInclude 检查某个文件路径是否匹配 .worktreeinclude 里的任一模式。
// 支持精确匹配、basename 匹配以及基础 glob 模式。
func matchesWorktreeInclude(path string, patterns []string) bool {
	base := filepath.Base(path)
	for _, p := range patterns {
		p = strings.TrimPrefix(p, "/")
		// 精确匹配。
		if p == path || p == base {
			return true
		}
		// 对完整路径做 glob 匹配。
		if matched, _ := filepath.Match(p, path); matched {
			return true
		}
		// 对 basename 做 glob 匹配。
		if matched, _ := filepath.Match(p, base); matched {
			return true
		}
		// 目录模式的前缀匹配。
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// WorktreeConfig 保存 worktree 相关配置。从 config.yaml 或默认值填充。
var worktreeConfig = struct {
	SymlinkDirectories    []string
	StaleCleanupInterval  int // 单位秒；0 = 关闭
	StaleCutoffHours      int // 单位小时；默认 720（30 天）
}{
	StaleCutoffHours: 720,
}

// SetWorktreeConfig 让 TUI/CLI 启动时可以注入配置值。
func SetWorktreeConfig(symlinkDirs []string, cleanupIntervalSec, cutoffHours int) {
	worktreeConfig.SymlinkDirectories = symlinkDirs
	worktreeConfig.StaleCleanupInterval = cleanupIntervalSec
	if cutoffHours > 0 {
		worktreeConfig.StaleCutoffHours = cutoffHours
	}
}

// GetStaleCutoffHours 返回配置的过期阈值，单位为小时。
func GetStaleCutoffHours() int {
	return worktreeConfig.StaleCutoffHours
}

// GetStaleCleanupInterval 返回配置的清理间隔，单位为秒。
func GetStaleCleanupInterval() int {
	return worktreeConfig.StaleCleanupInterval
}

// FindCanonicalGitRoot 穿透 worktree 找到主仓库根目录。
// 在 worktree 内部调用时，会顺着 .git 指针找到 commondir。
func FindCanonicalGitRoot(startDir string) string {
	gitDir, err := ResolveGitDir(startDir)
	if err != nil || gitDir == "" {
		return ""
	}
	// 如果 gitDir 里有 commondir 指针，顺着它找到主仓库。
	commonDir, err := GetCommonDir(gitDir)
	if err != nil || commonDir == "" {
		// gitDir 就是主仓库的 .git 目录；仓库根目录是它的上一级。
		return filepath.Dir(gitDir)
	}
	// commonDir 指向主仓库的 .git；仓库根目录是它的上一级。
	return filepath.Dir(commonDir)
}

