package agents

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"mewcode/internal/agent"
	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/permissions"
	"mewcode/internal/teams"
	"mewcode/internal/tools"
	"mewcode/internal/worktree"
)

// sanitizeSlugSegment 把 [a-zA-Z0-9._-] 之外的字符替换成 '-'，并去掉多余的
// 分隔符，保证结果可以安全地用作 git 分支名。
var unsafeSlugChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeSlugSegment(s string) string {
	clean := unsafeSlugChars.ReplaceAllString(s, "-")
	clean = strings.Trim(clean, "-_.")
	if clean == "" {
		clean = "subagent"
	}
	if len(clean) > 40 {
		clean = clean[:40]
	}
	return clean
}

type SubAgentProgress struct {
	// AgentID 是一次 spawn 的稳定标识。并发跑多个子 Agent 时 description 可能
	// 重名，UI 靠它区分各自的活动块。空串时（旧调用方）退回 AgentDesc 匹配。
	AgentID   string
	AgentDesc string
	AgentType string
	ToolName  string
	ToolArgs  map[string]any
	Elapsed   float64
	IsError   bool
	Done      bool
	ToolCount int
	TotalTime float64
}

const ForkBoilerplateTag = "<fork_boilerplate>"

// ForkAgentType 是 fork 子 Agent 的类型名，拼进 QuerySource 用来识别 fork 出来的会话。
const ForkAgentType = "fork"

// GeneralPurposeAgentType 是 fork 关闭时，省略 subagent_type 的回退目标。
const GeneralPurposeAgentType = "general-purpose"

// ForkQuerySource 是 fork 子 Agent 的来源标记，形如 `agent:builtin:fork`。
// 它是判断「当前身处 fork 子 Agent」的首选信号，拿不到时再退回扫描对话历史里的
// ForkBoilerplateTag。
const ForkQuerySource = "agent:builtin:" + ForkAgentType

type AgentTool struct {
	Client        llm.Client
	ModelResolver func(string) (llm.Client, error)
	// ModelAliases 是当前 provider 可用的档位名（llm.AvailableModelAliases 的产出），
	// 只用于生成 model 参数的 enum：让模型只填这个端点真正认识的档位名，
	// 而不是照搬 Claude 的 sonnet/opus/haiku 去撞 model not found。
	// 为空时 schema 不给 enum，模型可以自由填 provider 支持的具体模型名。
	ModelAliases []string
	Registry     *tools.Registry
	Protocol     string
	ProgressCh   chan<- SubAgentProgress
	Loader       *AgentLoader
	Conversation *conversation.Manager // 父对话，fork 需要
	TeamMgr      *teams.TeamManager    // 可选，有它才支持 team_name 参数

	// ContextWindow / MaxOutputTokens 是当前 provider 的解析结果，spawn 时透传给
	// 子 Agent。不设的话 [agent.New] 会给子 Agent 写死 200000 的默认窗口 —— 与
	// provider 实际配置脱节：真实窗口更小的模型压缩来不及触发，更大的则被过早
	// 压缩、白白丢上下文。两者都是 Layer 2 压缩阈值的换算依据。
	ContextWindow   int
	MaxOutputTokens int

	// ParentChecker 是父 Agent 的权限检查器。Sandbox 和 RuleEngine 复用父级的；
	// 只有当 sub-agent 定义 / 调用里指定了不同的 permissionMode 时才覆盖 Mode。
	// 可选 —— 为 nil 时子 Agent 拿不到检查器（早期引导 / 测试场景）。
	ParentChecker *permissions.Checker

	// QuerySource 标识发起派生的 Agent，用于嵌套 fork 检测。主线程里为空；
	// 当这个 AgentTool 实例位于某个已派生的 sub-agent 内部时，设为 ForkQuerySource
	// （或 "agent:builtin:<type>"）。它抗压缩 —— 即使 fork 样板
	// 被从对话历史里摘要掉了也还在。
	QuerySource string

	// ForkDisabled 为真时，省略 subagent_type 不再 fork，而是回退到通用 agent。
	// 用「关闭」而不是「开启」语义，是为了让零值就是默认行为（fork 可用），
	// 每个构造点不必都显式赋值。
	ForkDisabled bool
}

func (t *AgentTool) Name() string                 { return "Agent" }
func (t *AgentTool) Category() tools.ToolCategory { return tools.CategoryCommand }

// IsConcurrencySafe 声明 Agent 调用可以并发执行。同一轮里连续的多个 Agent 调用
// 会被 StreamingExecutor 归入同一并发批次并行跑，结果按提交顺序回填 ——
// fan-out / fan-in 由主循环的工具批次机制直接提供，无需额外协调。
// teammate 路径例外：建团队、注册成员、建 worktree 都是带副作用的注册动作，保持串行。
func (t *AgentTool) IsConcurrencySafe(args map[string]any) bool {
	teamName, _ := args["team_name"].(string)
	return teamName == "" || t.TeamMgr == nil
}

func (t *AgentTool) Description() string {
	desc := `Launch a sub-agent to handle a complex task.

There are two ordinary modes:
1. Omit "subagent_type" to fork a snapshot of the current conversation. The task prompt is appended to that copied context.
2. Set "subagent_type" to start a fresh-context agent from that role definition. The task prompt must contain all context the agent needs.

Ordinary sub-agent calls are synchronous: the caller waits for the final result in the tool result. Multiple independent Agent calls in one model response may run concurrently. To start a long-running team teammate instead, provide "team_name" and use SendMessage for follow-up communication.

This is ONE tool with multiple roles. Roles are NOT separate tools — choose one with "subagent_type". Available roles are:`

	if t.Loader != nil {
		for _, name := range t.Loader.ListNames() {
			def := t.Loader.Get(name)
			desc += "\n- " + name + ": " + def.WhenToUse
		}
	} else {
		desc += "\n- general-purpose: Full tool access for multi-step tasks (default)"
		desc += "\n- plan: Read-only tools for designing implementation plans"
		desc += "\n- explore: Read-only search agent for locating code"
	}

	desc += `

Example call shape:
{
  "name": "Agent",
  "input": {
    "description": "Short task label",
    "prompt": "Detailed task instructions"
  }
}

For a fresh-context role agent, write a detailed prompt explaining what it should do and why. For a fork, the prompt is added to the inherited conversation. When multiple agents may write files, pass isolation "worktree" to give each applicable agent a separate worktree.`
	return desc
}

// modelProperty 生成 model 参数的定义。enum 只列当前 provider 认识的档位名
// （见 llm.AvailableModelAliases）；没有可枚举的档位时干脆不给 enum，让模型
// 自由填 provider 支持的具体模型名。
func (t *AgentTool) modelProperty() map[string]any {
	prop := map[string]any{"type": "string"}
	if len(t.ModelAliases) > 0 {
		prop["enum"] = t.ModelAliases
		prop["description"] = "Model tier for this agent. One of: " +
			strings.Join(t.ModelAliases, ", ") + ". Omit to inherit the current model."
		return prop
	}
	prop["description"] = "Override the model for this agent. Must be a model name the current provider accepts; omit to inherit the current model."
	return prop
}

func (t *AgentTool) Schema() map[string]any {
	agentTypes := []string{"general-purpose", "plan", "explore"}
	if t.Loader != nil {
		agentTypes = t.Loader.ListNames()
	}

	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type":        "string",
					"description": "A short label for progress and result messages. This is not the task instructions sent to the sub-agent.",
				},
				// prompt 会作为本次subagent的user message，这是主agent分配任务时传入的
				"prompt": map[string]any{
					"type":        "string",
					"description": "The task instructions. For a fresh-context role agent, include all required context; for a fork, this is appended to the copied conversation; for a teammate, this is the initial assignment.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"enum":        agentTypes,
					"description": "Optional role for a fresh-context agent. Omit to fork a snapshot of the current conversation and append prompt as the task.",
				},
				"model": t.modelProperty(),
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the long-running teammate. Use only with team_name; this is the name used by SendMessage.",
				},
				"isolation": map[string]any{
					"type":        "string",
					"enum":        []string{"worktree"},
					"description": "For a role agent or teammate, create a dedicated git worktree so edits do not collide with the lead or peers. It is ignored by the current fork path.",
				},
				"plan_mode_required": map[string]any{
					"type":        "boolean",
					"description": "Only for team_name. When true, the teammate starts read-only, submits a plan, and waits for SendMessage approval before editing.",
				},
				"team_name": map[string]any{
					"type":        "string",
					"description": "Create a long-running teammate under this team. The caller returns immediately; use SendMessage for follow-up work and communication. If the team does not exist, it is created automatically.",
				},
				"mode": map[string]any{
					"type":        "string",
					"enum":        []string{"default", "acceptEdits", "plan", "bypassPermissions"},
					"description": "Permission mode override for a role agent or teammate. Forks inherit the parent permission mode.",
				},
			},
			"required": []string{"description", "prompt"},
		},
	}
}

func (t *AgentTool) selectClient(specModel, overrideModel string) llm.Client {
	model := overrideModel
	if model == "" {
		model = specModel
	}
	if model == "" || model == "inherit" {
		return t.Client
	}
	if t.ModelResolver != nil {
		if c, err := t.ModelResolver(model); err == nil {
			return c
		}
	}
	return t.Client
}

func (t *AgentTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	description, _ := args["description"].(string)
	prompt, _ := args["prompt"].(string)
	if description == "" || prompt == "" {
		return tools.ToolResult{Output: "Error: description and prompt are required", IsError: true}
	}

	subagentType, _ := args["subagent_type"].(string)
	modelOverride, _ := args["model"].(string)
	agentName, _ := args["name"].(string)
	teamName, _ := args["team_name"].(string)
	modeOverride, _ := args["mode"].(string)
	isolation, _ := args["isolation"].(string)
	planModeRequired, _ := args["plan_mode_required"].(bool)

	if modeOverride != "" && !validPermissionModes[modeOverride] {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: invalid mode '%s'. Valid: default, acceptEdits, plan, bypassPermissions", modeOverride),
			IsError: true,
		}
	}

	// 团队成员路径：显式给了 team_name，这次派生就变成该团队 backend 下
	// 长期运行的 teammate。teammate 一启动 Lead 就拿回控制权，后续协调走
	// SendMessage / mailbox 通知。
	if teamName != "" && t.TeamMgr != nil {
		return t.runAsTeammate(ctx, teamName, agentName, description, prompt, modelOverride, subagentType, isolation, planModeRequired)
	}

	// 省略 subagent_type 时的走向由配置决定：fork 开着就继承父对话，关着就当成
	// 没指定类型，回退到通用 agent。这里不报错，模型只是没填一个可选参数，
	// 为此中断一次调用不值得，回退到通用 agent 一样能把活干了。
	if subagentType == "" && t.ForkDisabled {
		subagentType = GeneralPurposeAgentType
	}

	// 没有指定 subagent_type则走fork subagent
	if subagentType == "" {
		return t.runFork(ctx, description, prompt, modelOverride)
	}

	// 定义路径：从 loader 或内置表里解析 spec。
	var spec SubAgentSpec
	if t.Loader != nil {
		def := t.Loader.Get(subagentType)
		if def == nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: %s", subagentType, strings.Join(t.Loader.ListNames(), ", ")),
				IsError: true,
			}
		}
		spec = def.ToSpec()
	} else {
		s, ok := BuiltinSpecs[subagentType]
		if !ok {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error: unknown agent type '%s'. Available: general-purpose, plan, explore", subagentType),
				IsError: true,
			}
		}
		spec = s
	}

	// 单次调用的 mode 覆盖优先于定义里的 permissionMode。
	if modeOverride != "" {
		spec.PermissionMode = modeOverride
	}

	// 同步执行：主 Agent 阻塞在这条工具调用上直到子 Agent 结束。同一轮里连续
	// 派出的多个 Agent 调用会并发跑（见 IsConcurrencySafe），全部结束后结果
	// 一起作为工具结果回到主对话 —— 不再有后台异步路径。
	return t.runSync(ctx, spec, description, prompt, modelOverride, isolation)
}

// applyProviderLimits 把 provider 的窗口 / 输出上限透传给子 Agent。Layer 2 的
// 压缩阈值靠这两个值换算，而 agent.New 写死的默认窗口是 200000，与真实模型无关。
// 为 0（未配置）时保持默认值不动。
func (t *AgentTool) applyProviderLimits(sub *agent.Agent) {
	if t.ContextWindow > 0 {
		sub.ContextWindow = t.ContextWindow
	}
	if t.MaxOutputTokens > 0 {
		sub.MaxOutputTokens = t.MaxOutputTokens
	}
}

func (t *AgentTool) runSync(ctx context.Context, spec SubAgentSpec, description, prompt, modelOverride, isolation string) tools.ToolResult {
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, spec.DisallowedTools, false)
	client := t.selectClient(spec.Model, modelOverride)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = deriveSubAgentChecker(t.ParentChecker, spec.PermissionMode)
	t.applyProviderLimits(subAgent)
	if spec.MaxTurns > 0 {
		subAgent.MaxIterations = spec.MaxTurns
	} else {
		subAgent.MaxIterations = 200
	}

	// Worktree 隔离：给 sub-agent 创建一个独立的 worktree。
	var wtResult *worktree.AgentWorktreeResult
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		var err error
		wtResult, err = worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating agent worktree: %s", err),
				IsError: true,
			}
		}
		subAgent.WorkDir = wtResult.WorktreePath

		// 把 worktree 提示注入 prompt。
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	conv := conversation.NewManager()
	if spec.SystemPromptOverride != "" {
		conv.AddSystemReminder(spec.SystemPromptOverride)
	}
	// initialPrompt 会被拼在第一轮 user 消息前面。
	if spec.InitialPrompt != "" {
		conv.AddUserMessage(spec.InitialPrompt)
	}
	conv.AddUserMessage(prompt)

	// 同步阻塞到子 Agent 结束，结果作为工具返回值直接回到主 Agent。
	outcome := runSubAgentToCompletion(ctx, subAgent, conv, t.ProgressCh, newSubAgentID(), description, spec.Name)

	if outcome.errMsg != "" {
		// 失败也带上已产出的部分结果：子 Agent 失败前的工作对主 Agent 仍然有
		// 价值，一句错误信息把它扔掉等于白跑。
		msg := fmt.Sprintf("Agent failed: %s", outcome.errMsg)
		if outcome.output != "" {
			msg += "\n\nPartial output before failure:\n" + outcome.output
		}
		if kept := finishWorktree(ctx, wtResult); kept {
			msg += fmt.Sprintf("\n\nWorktree kept at %s (branch %s) — has uncommitted changes or new commits.",
				wtResult.WorktreePath, wtResult.WorktreeBranch)
		}
		return tools.ToolResult{Output: msg, IsError: true}
	}

	result := outcome.output
	if result == "" {
		result = "(agent produced no output)"
	}

	// Worktree 清理：干净的就自动删掉，脏的就保留。上面的失败路径同样会清理，
	// 失败的运行不会泄漏 worktree。
	if kept := finishWorktree(ctx, wtResult); kept {
		result += fmt.Sprintf("\n\nWorktree kept at %s (branch %s) — has uncommitted changes or new commits.",
			wtResult.WorktreePath, wtResult.WorktreeBranch)
	}

	return tools.ToolResult{
		Output: fmt.Sprintf("Agent \"%s\" completed in %s.\n\n%s", description, outcome.elapsed.Round(time.Millisecond), result),
	}
}

// finishWorktree 在子 Agent 结束后处理它的隔离 worktree：有未提交改动或新提交
// 就保留（返回 true），干净就删掉。wtResult 为 nil 时是空操作，返回 false。
// 成功与失败路径都要调用，否则失败的运行会泄漏 worktree。
func finishWorktree(ctx context.Context, wtResult *worktree.AgentWorktreeResult) bool {
	if wtResult == nil {
		return false
	}
	if worktree.HasWorktreeChanges(ctx, wtResult.WorktreePath, wtResult.HeadCommit) {
		return true
	}
	worktree.RemoveAgentWorktree(ctx, wtResult.WorktreePath, wtResult.WorktreeBranch, wtResult.GitRoot)
	return false
}

// subAgentOutcome 是一次同步子 Agent 运行的产出。失败时 output 仍携带失败前
// 已产出的部分结果，调用方把它连同错误一起返回给主 Agent，避免信息丢失。
type subAgentOutcome struct {
	output    string
	errMsg    string
	toolCount int
	elapsed   time.Duration
}

// runSubAgentToCompletion 启动一个子 Agent 并阻塞到它结束。期间把工具执行转发成
// 进度事件（UI 用，best-effort，消费不过来就丢）；权限请求一律拒绝（headless，
// 没有 UI 可问，自动 deny 让子 Agent 的 executeSingleTool 不至于卡在 respCh 上）。
// 同一轮里并发的多个 Agent 调用各自阻塞在自己的事件流上，互不影响。
func runSubAgentToCompletion(ctx context.Context, sub *agent.Agent, conv *conversation.Manager, progressCh chan<- SubAgentProgress, agentID, desc, agentType string) subAgentOutcome {
	start := time.Now()
	var output strings.Builder
	toolCount := 0
	ch := sub.Run(ctx, conv)

	for ev := range ch {
		switch e := ev.(type) {
		case agent.StreamText:
			output.WriteString(e.Text)
		case agent.PermissionRequestEvent:
			// respCh 的 buffer=1，这次发送不会阻塞。
			e.ResponseCh <- agent.PermDeny
		case agent.ToolResultEvent:
			toolCount++
			emitProgress(progressCh, ctx, SubAgentProgress{
				AgentID:   agentID,
				AgentDesc: desc,
				AgentType: agentType,
				ToolName:  e.ToolName,
				ToolArgs:  map[string]any{"_summary": e.Output},
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(progressCh, ctx, SubAgentProgress{
				AgentID:   agentID,
				AgentDesc: desc,
				AgentType: agentType,
				Done:      true,
				ToolCount: toolCount,
				TotalTime: time.Since(start).Seconds(),
				IsError:   true,
			})
			return subAgentOutcome{
				output:    output.String(),
				errMsg:    e.Message,
				toolCount: toolCount,
				elapsed:   time.Since(start),
			}
		}
	}

	emitProgress(progressCh, ctx, SubAgentProgress{
		AgentID:   agentID,
		AgentDesc: desc,
		AgentType: agentType,
		Done:      true,
		ToolCount: toolCount,
		TotalTime: time.Since(start).Seconds(),
	})
	return subAgentOutcome{
		output:    output.String(),
		toolCount: toolCount,
		elapsed:   time.Since(start),
	}
}

// newSubAgentID 生成一次 spawn 的稳定标识，供 UI 在并发时区分各子 Agent 的
// 活动块。用随机值而不是自增计数器：fork 子 Agent 手里的 AgentTool 是父实例的
// 浅拷贝，两边计数器会从同一初值出发，撞号后 UI 会把两个块混成一个。
func newSubAgentID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "sub-" + hex.EncodeToString(b)
}

func (t *AgentTool) runFork(ctx context.Context, description, prompt, modelOverride string) tools.ToolResult {
	if t.Conversation == nil {
		return tools.ToolResult{Output: "Error: fork requires parent conversation context", IsError: true}
	}

	// 嵌套 fork 防护，分两层：
	// (1) 主手段：querySource —— 在 fork 子 Agent 内部构造 AgentTool 实例时设置。
	//     抗压缩；能覆盖对话历史被重写或摘要掉的情况。
	//
	// (2) 兜底：扫描消息里有没有 ForkBoilerplateTag。
	if t.QuerySource == ForkQuerySource {
		return tools.ToolResult{
			Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
			IsError: true,
		}
	}
	for _, msg := range t.Conversation.GetMessages() {
		if strings.Contains(msg.Content, ForkBoilerplateTag) {
			return tools.ToolResult{
				Output:  "Error: cannot fork from a forked agent. Use subagent_type to spawn a definition-based agent instead.",
				IsError: true,
			}
		}
	}

	// 构造 fork 对话：复制父消息 + 补齐未完成的 tool_use + 追加任务。
	forkedConv := buildForkedConversation(t.Conversation, prompt)

	client := t.selectClient("", modelOverride)
	// fork 原样继承父 Agent 的工具池，这样发出去的请求前缀和父 Agent 逐字节一致，
	// prompt 缓存才命中得上。其中的 Agent 工具被换成一份 QuerySource=ForkQuerySource
	// 的浅拷贝，于是再往下 fork 会在 runFork 的首道检查那里被拒。
	subRegistry := cloneRegistryForFork(t.Registry)

	subAgent := agent.New(client, subRegistry, t.Protocol)
	subAgent.Checker = t.ParentChecker // fork 原样继承父 Agent 的权限状态
	t.applyProviderLimits(subAgent)
	subAgent.MaxIterations = 200

	// fork 同步运行：主 Agent 阻塞在这条工具调用上直到 fork 结束，结果作为工具
	// 返回值直接进入主对话。同一轮里连续派出的多个 fork 会被 StreamingExecutor
	// 归入同一并发批次并行执行，全部结束后结果一起回填 —— fan-in 由主循环提供。
	outcome := runSubAgentToCompletion(ctx, subAgent, forkedConv, t.ProgressCh, newSubAgentID(), description, ForkAgentType)

	if outcome.errMsg != "" {
		// 失败也带上已产出的部分结果，理由同 runSync。
		msg := fmt.Sprintf("Forked agent failed: %s", outcome.errMsg)
		if outcome.output != "" {
			msg += "\n\nPartial output before failure:\n" + outcome.output
		}
		return tools.ToolResult{Output: msg, IsError: true}
	}

	result := outcome.output
	if result == "" {
		result = "(agent produced no output)"
	}
	return tools.ToolResult{
		Output: fmt.Sprintf("Forked agent \"%s\" completed in %s.\n\n%s", description, outcome.elapsed.Round(time.Millisecond), result),
	}
}

// emitProgress 发送一个 SubAgentProgress 事件，且绝不阻塞调用方。如果消费方
// （TUI）跟不上，事件直接丢掉 —— 进度只是尽力而为的 UI 反馈，不承担关键
// 状态。这里如果用阻塞发送，ProgressCh 的 buffer 一满就会让 sub-agent 循环
// 死锁，进而导致 ESC / ctx cancel 永远不生效，因为 sub-agent 卡在了这次发送上，
// 而不是卡在能感知 ctx 的地方。
func emitProgress(ch chan<- SubAgentProgress, ctx context.Context, p SubAgentProgress) {
	if ch == nil {
		return
	}
	select {
	case ch <- p:
	case <-ctx.Done():
	default:
		// 消费方跟不上了。丢掉这个事件，也别把 sub-agent 的事件循环卡住。
	}
}

// deriveSubAgentChecker 把 sub-agent 的 permissionMode 透传给派生出来的 agent：
// `mode` 覆盖 agent 定义里的 permissionMode，再喂给 sub-agent 的
// ToolPermissionContext。Sandbox 和 RuleEngine 是共用的 —— 只换 Mode，
// 这样 sub-agent 的工具调用会命中另一套判定矩阵，而权限状态不会分叉。
//
// 没有要求覆盖时原样返回父级 checker。
func deriveSubAgentChecker(parent *permissions.Checker, modeOverride string) *permissions.Checker {
	if parent == nil {
		return nil
	}
	if modeOverride == "" || permissions.PermissionMode(modeOverride) == parent.Mode {
		return parent
	}
	return permissions.NewChecker(parent.Sandbox, parent.RuleEngine, permissions.PermissionMode(modeOverride))
}

// cloneRegistryForFork 返回的 registry 原样复制父级，唯一区别是把其中的
// *AgentTool 实例换成 QuerySource 设为 ForkQuerySource 的浅拷贝。
// 这样 fork 子 Agent 看到的工具定义和父 Agent 在协议层完全一样（prompt 缓存因此命中），
// 但它再想 fork 时会在调用那一刻被 runFork 里的 QuerySource 检查拦下。
func cloneRegistryForFork(reg *tools.Registry) *tools.Registry {
	forked := tools.NewRegistry()
	for _, tool := range reg.ListTools() {
		if at, ok := tool.(*AgentTool); ok {
			clone := *at
			clone.QuerySource = ForkQuerySource
			forked.Register(&clone)
			continue
		}
		forked.Register(tool)
	}
	return forked
}

const forkBoilerplate = ForkBoilerplateTag + `
You are a forked worker process. You are NOT the main agent.
Rules (non-negotiable):
1. Do NOT fork again.
2. Do NOT converse, ask questions, or request confirmation.
3. Use tools directly: read files, search code, make changes.
4. Stay strictly within your assigned task scope.
5. Final report must be under 500 characters, starting with "Scope:".
` + "</fork_boilerplate>"

func buildForkedConversation(parent *conversation.Manager, task string) *conversation.Manager {
	forked := conversation.NewManager()
	msgs := parent.GetMessages()

	// 逐字节重放：把 thinking block 和 tool_use 一起保留，这样 API 请求前缀和
	// 父 Agent 完全一致（参见「保留所有 content block（thinking、text 和每个 tool_use）」）。
	// 少了 thinking block 会改变 assistant 消息的形状，prompt cache 就打不中了。
	for _, msg := range msgs {
		if len(msg.ToolUses) > 0 && len(msg.ToolResults) == 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
			var placeholders []conversation.ToolResultBlock
			for _, tu := range msg.ToolUses {
				placeholders = append(placeholders, conversation.ToolResultBlock{
					ToolUseID: tu.ToolUseID,
					Content:   "(tool execution interrupted by fork)",
					IsError:   false,
				})
			}
			forked.AddToolResultsMessage(placeholders)
		} else if len(msg.ToolUses) > 0 {
			forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, msg.ToolUses)
		} else if len(msg.ToolResults) > 0 {
			forked.AddToolResultsMessage(msg.ToolResults)
		} else if msg.Role == "assistant" {
			if len(msg.ThinkingBlocks) > 0 {
				forked.AddAssistantFull(msg.Content, msg.ThinkingBlocks, nil)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		} else {
			forked.AddUserMessage(msg.Content)
		}
	}

	// 把 fork 样板和任务作为 user 消息追加在后面。
	forked.AddUserMessage(forkBoilerplate + "\n\nYour task:\n" + task)
	return forked
}

// runAsTeammate 在一个已有的 Team 上登记一个长期运行的团队成员。和
// runSync 不同，这条路径从不让 Lead 阻塞等成员的输出：Lead 总是
// 立刻返回，之后通过 SendMessage 和团队 mailbox 里的 idle 通知来协调。
// backend（in-process / tmux / iTerm）由 teams.SpawnTeammate 根据 Team.Mode 选择。
//
// 当 isolation == "worktree" 且配了 WorktreeMgr 时，teammate 会拿到一个专属的
// git worktree，这样它改文件就不会和别的成员撞车。
func (t *AgentTool) runAsTeammate(
	ctx context.Context,
	teamName, memberName, description, prompt, modelOverride, subagentType, isolation string,
	planModeRequired bool,
) tools.ToolResult {
	// 团队不存在就顺手建一个：coordinator 模式下 TeamCreate 不在白名单里，
	// 要求 Lead 先建团队再派人，它会卡在第一步。
	team := t.TeamMgr.GetTeam(teamName)
	if team == nil {
		team = t.TeamMgr.CreateTeamFull(teamName, teams.DetectBackend(), teams.LeadName, description)
	}

	if memberName == "" {
		memberName = sanitizeSlugSegment(description)
	}
	if _, exists := team.Members[memberName]; exists {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error: team '%s' already has a member named '%s'", teamName, memberName),
			IsError: true,
		}
	}

	// 设了 subagent_type 就解析出 spec，好让 teammate 遵守同类型 sub-agent
	// 的禁用列表。没有 spec 时就把整个 registry 交给 teammate。
	var spec SubAgentSpec
	if subagentType != "" {
		if t.Loader != nil {
			if def := t.Loader.Get(subagentType); def != nil {
				spec = def.ToSpec()
			}
		} else if s, ok := BuiltinSpecs[subagentType]; ok {
			spec = s
		}
	}

	teammateDisallowed := append(append([]string{}, spec.DisallowedTools...), TeammateDisallowedTools...)
	subRegistry := FilterToolsForAgent(t.Registry, spec.Tools, teammateDisallowed, false)
	// 队友协作工具：以队友自己的名字发消息，并注入团队共享任务板工具
	// （覆盖继承来的个人版同名工具，让队友之间共享同一份任务列表）。
	subRegistry.Register(&teams.SendMessageTool{TeamMgr: t.TeamMgr, SenderName: memberName})
	subRegistry.Register(&teams.TaskCreateTool{TeamMgr: t.TeamMgr, TeamName: teamName, AgentName: memberName})
	subRegistry.Register(&teams.TaskGetTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskListTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	subRegistry.Register(&teams.TaskUpdateTool{TeamMgr: t.TeamMgr, TeamName: teamName})
	client := t.selectClient(spec.Model, modelOverride)

	var otherMembers []string
	for n := range team.Members {
		otherMembers = append(otherMembers, n)
	}
	addendum := teams.BuildTeammateAddendum(teamName, memberName, otherMembers)

	var workdir string
	if isolation == "worktree" {
		slug := generateAgentSlug(description)
		wtResult, err := worktree.CreateAgentWorktree(ctx, slug)
		if err != nil {
			return tools.ToolResult{
				Output:  fmt.Sprintf("Error creating teammate worktree: %s", err),
				IsError: true,
			}
		}
		workdir = wtResult.WorktreePath
		parentCwd, _ := os.Getwd()
		notice := worktree.BuildWorktreeNotice(parentCwd, wtResult.WorktreePath)
		prompt = notice + "\n\n" + prompt
	}

	team.SetMemberMeta(memberName, subagentType, modelOverride, workdir)

	// 标了 plan_mode_required 的队友以计划模式启动：只能读不能改，
	// 写出计划交 Lead 审批，通过后才切回正常权限。
	teammateChecker := t.ParentChecker
	if planModeRequired && t.ParentChecker != nil {
		teammateChecker = permissions.NewChecker(
			t.ParentChecker.Sandbox, t.ParentChecker.RuleEngine, permissions.ModePlan)
	}

	result, err := teams.SpawnTeammate(ctx, teams.TeammateSpawnConfig{
		Team:       team,
		MemberName: memberName,
		Checker:    teammateChecker,
		Task:       prompt,
		Addendum:   addendum,
		Client:     client,
		Registry:   subRegistry,
		Protocol:   t.Protocol,
		Workdir:    workdir,
	})
	if err != nil {
		return tools.ToolResult{
			Output:  fmt.Sprintf("Error spawning teammate: %v", err),
			IsError: true,
		}
	}

	// in-process 派生会返回一个活的 event channel；在后台把它抽干，免得 goroutine
	// 卡在满的 chan 上。Lead 能看到的进度走 mailbox，不靠这里的抽干。
	if result.Mode == teams.ModeInProcess && result.EventCh != nil {
		go drainTeammateEvents(memberName, result.EventCh, t.ProgressCh)
	}

	backendHint := string(result.Mode)
	if result.PaneID != "" {
		backendHint += " pane=" + result.PaneID
	}
	if workdir != "" {
		backendHint += " worktree=" + workdir
	}
	return tools.ToolResult{
		Output: fmt.Sprintf(
			"Teammate \"%s\" started on team \"%s\" [%s]. Use SendMessage to talk to it; its idle notifications will arrive as system reminders.",
			memberName, teamName, backendHint,
		),
	}
}

// drainTeammateEvents 消费 teammate 的事件流，让生产侧永远不会卡在满的
// channel 上。设了 ProgressCh 时，工具/错误事件会转发过去，
// 好让父级 UI 显示活动状态。
func drainTeammateEvents(name string, ch <-chan agent.AgentEvent, progressCh chan<- SubAgentProgress) {
	for ev := range ch {
		if progressCh == nil {
			continue
		}
		switch e := ev.(type) {
		case agent.ToolResultEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  e.ToolName,
				Elapsed:   e.Elapsed.Seconds(),
				IsError:   e.IsError,
			})
		case agent.ErrorEvent:
			emitProgress(progressCh, context.Background(), SubAgentProgress{
				AgentDesc: name,
				AgentType: "teammate",
				ToolName:  "error",
				IsError:   true,
			})
		}
	}
}

// generateAgentSlug 为 sub-agent 的 worktree 生成一个匹配 ^agent-a[0-9a-f]{7}$ 的 slug。
func generateAgentSlug(description string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "agent-a" + hex.EncodeToString(b)[:7]
}
