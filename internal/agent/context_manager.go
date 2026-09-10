package agent

import (
	"context"

	"mewcode/internal/compact"
)

// ContextCoordinator 是 session 持有的策略，由 AgentRun 在某一轮迭代
// 发出下一次模型请求之前调用。工具结果进历史时
// 第 1 层已经跑过了；这个 coordinator 负责第 2 层的状态。
type ContextCoordinator interface {
	Prepare(context.Context, *AgentRun, int, []map[string]any) (string, error)
}

type contextCoordinator struct {
	tracking *compact.AutoCompactTrackingState
}

func (c *contextCoordinator) Prepare(ctx context.Context, run *AgentRun, _ int, toolSchemas []map[string]any) (string, error) {
	if c.tracking == nil {
		c.tracking = &compact.AutoCompactTrackingState{}
	}
	a := run.Agent
	return compact.ManageContext(
		ctx,
		run.Conversation,
		a.Client,
		a.WorkDir,
		run.SessionID,
		a.ContextWindow,
		a.MaxOutputTokens,
		c.tracking,
		a.RecoveryState,
		toolSchemas,
	)
}
