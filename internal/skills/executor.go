package skills

import (
	"context"

	"mewcode/internal/conversation"
	"mewcode/internal/tools"
)

// SkillHost 是 Executor 驱动 inline 模式 skill 所需要的那部分 Agent 状态。
// 由 *agent.Agent 实现；之所以在这里声明成接口，
// 是为了让 skills 包不必 import agent 包
// （一旦 LoadSkillTool 开始引用 skills.Catalog 就会造成循环依赖）。
type SkillHost interface {
	// ActivateSkill 记录 skill 的激活，用于追踪（/skills 列表和压缩恢复）。
	// body 不会每轮重新注入。
	ActivateSkill(name, body string)
	// ToolRegistry 暴露运行中的 tools.Registry，让 executor 能注册目录型工具。
	// 之所以叫 ToolRegistry（而不是 Registry），是因为 *agent.Agent
	// 已经有一个导出的 Registry 字段，而 Go 不允许
	// 方法名和字段名重名。
	ToolRegistry() *tools.Registry
}

// SkillForkHost 在 SkillHost 之上扩展出运行独立 sub-agent 的能力。
// 由 TUI 层实现（它持有 LLM client 和 agent 的构造器），
// 并传给 Executor.RunFork。把它和 SkillHost 拆开，
// 是为了让单元测试只桩掉 fork 相关的行为，
// 不必伪造整套 sub-agent 运行时。
type SkillForkHost interface {
	SkillHost
	// RunSubAgent 在一个全新的对话里把 `body` 作为第一条 user 消息跑起来，
	// 对话用 `seed` 初始化（已按 ForkContext 策略准备好），
	// 并返回最终的 assistant 文本。ctx 被取消时
	// 应当中止这个 sub-agent。
	RunSubAgent(ctx context.Context, body string, seed []conversation.Message, model string) (string, error)
	// SnapshotParentMessages 暴露父对话的消息，
	// 让 executor 能按 `fork_context` 构造 seed。
	// 实现可以返回浅拷贝；executor 不得修改这个切片。
	SnapshotParentMessages() []conversation.Message
}

// RunInline 在宿主 agent 上记录 skill 的激活，并返回渲染好的 prompt body。
// 调用方（slash 命令处理器）把返回的 body 作为一条 user 消息
// 提交到主对话里，它就作为普通消息留在那儿 ——
// 不会每轮重新注入。
func RunInline(_ context.Context, skill *Skill, args string, host SkillHost) (string, error) {
	body := skill.Render(args)
	host.ActivateSkill(skill.Meta.Name, body)
	return body, nil
}

// RunFork 在独立的 sub-agent 里执行这个 skill，并返回最终的 assistant 文本。
// 主对话不会被 sub-agent 改动；
// 调用方（slash 命令处理器）需要把返回的字符串
// 作为一条 assistant 消息插回主对话历史。
//
// 历史带多少由 skill.Meta.ForkContext 决定：
//   - "full":  用父对话的完整消息历史初始化 sub-agent
//   - "recent": 用父对话最后 5 条消息初始化
//   - "none":  不初始化（默认；像全新 session 一样隔离）
func RunFork(ctx context.Context, skill *Skill, args string, host SkillForkHost) (string, error) {
	body := skill.Render(args)
	seed := buildForkSeed(skill.Meta.ForkContext, host.SnapshotParentMessages())
	return host.RunSubAgent(ctx, body, seed, skill.Meta.Model)
}

// buildForkSeed 按 ForkContext 策略切出父对话的消息历史。
// 返回 nil 表示 "none" 或未知取值，
// 好让 sub-agent 从干净状态开始。
//
// "full" 目前不在 LLM 侧做总结 —— 它原样拷贝父切片。
// 将来如果上下文窗口成了瓶颈，可以改成走 compact.Summarise；
// 眼下让它和 "recent" 保持一致、
// 只是上限更高，已经够用了。
func buildForkSeed(mode string, parent []conversation.Message) []conversation.Message {
	switch mode {
	case "full":
		out := make([]conversation.Message, len(parent))
		copy(out, parent)
		return out
	case "recent":
		if len(parent) <= 5 {
			out := make([]conversation.Message, len(parent))
			copy(out, parent)
			return out
		}
		out := make([]conversation.Message, 5)
		copy(out, parent[len(parent)-5:])
		return out
	default:
		return nil
	}
}
