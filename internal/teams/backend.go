package teams

// DetectBackend 保留这个小入口以兼容调用方，但当前 Team 只支持进程内后端。
// 每个 teammate 使用独立 Agent、Conversation 和 goroutine；文件信箱、任务板
// 与 worktree 仍负责跨 goroutine 的协作和文件系统隔离。
func DetectBackend() TeamMode { return ModeInProcess }
