package agent

import (
	"context"

	"mewcode/internal/compact"
)

// ContextCoordinator is session-owned policy invoked by AgentRun immediately
// before an iteration sends its next model request. Layer 1 has already run
// when tool results entered history; this coordinator owns Layer 2 state.
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
