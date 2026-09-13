package teams

import (
	"context"
	"fmt"

	"mewcode/internal/agent"
	"mewcode/internal/llm"
	"mewcode/internal/permissions"
	"mewcode/internal/tools"
)

// TeammateSpawnConfig 收集 SpawnTeammate 需要的全部参数。
// 所有 teammate 都在当前进程的独立 goroutine 中运行；Client、Registry、
// Protocol、Workdir 和 Checker 会在 goroutine 启动前绑定到该成员自己的 Agent。
type TeammateSpawnConfig struct {
	Team       *Team
	MemberName string
	Task       string
	Addendum   string

	Client   llm.Client
	Registry *tools.Registry
	Protocol string

	Workdir string
	Checker *permissions.Checker
}

// SpawnResult 承载进程内 teammate 的事件流。
type SpawnResult struct {
	Mode    TeamMode
	EventCh <-chan agent.AgentEvent
}

// SpawnTeammate 在当前进程中为 Team 启动一个独立 goroutine。
// Team 仍然保留文件信箱和任务板，以便不同 Agent goroutine 之间通过持久化
// 消息协作；worktree 也仍然由调用方在启动前创建并绑定。
func SpawnTeammate(ctx context.Context, cfg TeammateSpawnConfig) (*SpawnResult, error) {
	if cfg.Team == nil {
		return nil, fmt.Errorf("SpawnTeammate: team is required")
	}
	if cfg.MemberName == "" {
		return nil, fmt.Errorf("SpawnTeammate: member name is required")
	}

	// 旧 config.json 可能记录过 tmux/iTerm；加载后统一降级为当前唯一后端。
	cfg.Team.Mode = ModeInProcess
	GetNameRegistry().Register(cfg.MemberName, cfg.MemberName)
	ch := StartInProcessMemberWithConfig(
		cfg.Team.workerContext(),
		cfg.Team,
		cfg.MemberName,
		cfg.Client,
		cfg.Registry,
		cfg.Protocol,
		cfg.Task,
		cfg.Addendum,
		cfg.Workdir,
		cfg.Checker,
	)
	return &SpawnResult{Mode: ModeInProcess, EventCh: ch}, nil
}
