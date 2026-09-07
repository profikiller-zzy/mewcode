package memory

import (
	"os"
	"path/filepath"
	"strings"
)

// AutoMemEntrypointName 是每个项目记忆索引的文件名。
const AutoMemEntrypointName = "MEMORY.md"

// GetAutoMemPath 返回指定项目根目录的自动记忆目录：<projectRoot>/.mewcode/memory/。
//
// 保留末尾分隔符，确保基于前缀的路径匹配不会误把 …/memoryxyz 当成记忆目录。
//
// MewCode 将记忆与其他项目状态放在 .mewcode/ 下，便于 IDE 展示和编辑器直接打开。
//
// 解析顺序：
//  1. MEWCODE_REMOTE_MEMORY_DIR 环境变量（CI/容器中将记忆放到其他位置的出口）
//  2. <projectRoot>/.mewcode/memory
func GetAutoMemPath(projectRoot string) string {
	if override := os.Getenv("MEWCODE_REMOTE_MEMORY_DIR"); override != "" {
		return strings.TrimRight(override, string(filepath.Separator)) + string(filepath.Separator)
	}
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		abs = projectRoot
	}
	return filepath.Join(abs, ".mewcode", "memory") + string(filepath.Separator)
}

// GetAutoMemEntrypoint 返回自动记忆目录中的 MEMORY.md 路径。
func GetAutoMemEntrypoint(projectRoot string) string {
	dir := GetAutoMemPath(projectRoot)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsAutoMemPath 检查绝对路径是否位于项目级或用户级自动记忆目录中，
// 用于路径沙箱判断是否允许写入记忆目录。
func IsAutoMemPath(absolutePath, projectRoot string) bool {
	abs := filepath.Clean(absolutePath)
	if dir := GetAutoMemPath(projectRoot); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	if dir := GetUserAutoMemPath(); dir != "" {
		if strings.HasPrefix(abs+string(filepath.Separator), dir) {
			return true
		}
	}
	return false
}

// GetUserAutoMemPath 返回用户级自动记忆目录 ~/.mewcode/memory/，
// 用于跨项目跟随用户的 user/feedback 记忆（例如编码偏好）。
// 如果无法解析用户主目录，则返回空字符串。
//
// 保留末尾分隔符，供基于前缀的路径匹配使用。
func GetUserAutoMemPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".mewcode", "memory") + string(filepath.Separator)
}

// GetUserAutoMemEntrypoint 返回 ~/.mewcode/memory/MEMORY.md 的路径。
func GetUserAutoMemEntrypoint() string {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, AutoMemEntrypointName)
}

// IsUserAutoMemPath 检查绝对路径是否位于用户级记忆目录中，
// 用于区分用户作用域和项目作用域（沙箱已同时接受两者，这里用于路由）。
func IsUserAutoMemPath(absolutePath string) bool {
	dir := GetUserAutoMemPath()
	if dir == "" {
		return false
	}
	abs := filepath.Clean(absolutePath)
	return strings.HasPrefix(abs+string(filepath.Separator), dir)
}
