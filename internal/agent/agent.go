package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"mewcode/internal/compact"
	"mewcode/internal/conversation"
	"mewcode/internal/filehistory"
	"mewcode/internal/hooks"
	"mewcode/internal/llm"
	"mewcode/internal/permissions"
	"mewcode/internal/planfile"
	"mewcode/internal/prompt"
	"mewcode/internal/session"
	"mewcode/internal/toolresult"
	"mewcode/internal/tools"
)

const (
	maxTokensCeiling          = 64000
	maxOutputTokensRecoveries = 3
)

type Agent struct {
	Client        llm.Client      // llm 客户端
	Registry      *tools.Registry // 工具注册中心
	Protocol      string          // 协议标识
	WorkDir       string          // 工作目录
	MaxIterations int             // 最大迭代次数，0 不限
	ContextWindow int             // 上下文窗口，默认 200000
	// MaxOutputTokens 是模型的最大输出预算，Layer 2 用它算出压缩阈值所依据的
	// 有效窗口。取 0 时回落到 compact 内部的
	// summaryOutputReserve 默认值。
	MaxOutputTokens int
	Checker         *permissions.Checker // 权限检查器
	Hooks           *hooks.Engine
	// SessionID 标识本 Agent 追加写入的磁盘会话日志。设置后，Layer 2 压缩会往
	// 该会话里写一条 compact_boundary 记录，之后恢复会话就能直接
	// 重建压缩后的状态，而不必重放
	// 压缩前的完整记录。留空则不持久化边界（测试、
	// 一次性调用方）。
	SessionID      string
	NotificationFn func() []string
	// ToolNameFilter 非 nil 时，Name 判定返回 false 的工具不会出现在发给 LLM 的
	// schema 里。每轮迭代开头都会重新查一次这个过滤器，所以调用方可以随时开关
	// Coordinator 模式（比如 Team 创建/解散时），不必重启 Agent。
	Instructions  string
	MemoryContent string
	// SkillSection 是可用 Skill 的清单文本。它跟着项目走，所以不进系统提示词，
	// 而是和指令、记忆一起放进首条 system-reminder。
	SkillSection string
	// SkillDeltaFn 返回本轮新出现的 Skill 清单（已通知过的不再返回）。
	// 会话中途装了新 Skill 时只补这几条，不重发整份清单，也不动系统提示词。
	SkillDeltaFn func() string
	// MemoryRecallCh 非阻塞 memory recall：prefetch 与主 LLM 调用并行，
	// 工具执行后从 channel 读取并注入
	MemoryRecallCh <-chan RecallResult
	ToolNameFilter func(name string) bool
	// CoordinatorActiveFn 非 nil 时，用来报告 Coordinator 模式当前是否生效。
	// 每轮迭代都会和 ToolNameFilter 一起查，这样调度指引恰好在工具集被收窄时出现，
	// Team 一解散也就随之消失。
	CoordinatorActiveFn func() bool
	// OnLoopComplete 非 nil 时，在 Agent 到达 LoopComplete（最后一条助手消息、
	// 且没有待执行的工具调用）后以 fire-and-forget 方式调用。ch09 的后台记忆提取用它。
	// 它取代了原来的 stopHooks 分发器；失败静默处理，绝不能阻塞主循环。
	// 回调拿到的是正在使用的 conversation —— 不要在别的 goroutine 里改它。
	OnLoopComplete  func(conv *conversation.Manager)
	FileHistory     *filehistory.History
	compactTracking compact.AutoCompactTrackingState
	// RecoveryState 保存着重建工作上下文所需的快照，供 Layer 2 把对话
	// 压成摘要之后使用：最近读过的文件和调用过的 skill。
	// 这个结构体是并发安全的，
	// 所以流式执行器可以从多个 goroutine 往里写。
	RecoveryState *compact.RecoveryState
	// Trace 记录完整的 run 级轨迹。它是可选的；对于交互式会话，
	// AgentLoop 默认会装一个持久化 recorder。
	Trace   *TraceRecorder
	eventCh chan AgentEvent
	// activeSkills 记录本次会话里已经激活的 Skill SOP（name → body）。
	// /skills 用它展示当前激活了哪些，RecoveryState 靠它跨压缩存活。
	// body 只作为一条消息注入对话一次 —— 不会每轮重复注入。
	activeSkills map[string]string
	// announcedDeferred 是上一次告诉模型的延迟工具清单，按字典序。跟当前清单一比
	// 就知道工具池有没有变，没变就不重发那条提醒。
	announcedDeferred []string
	// recallMu 保护下面两个字段：工具执行分散在多个 goroutine 里写，记忆召回的
	// prefetch 又在自己的 goroutine 里读写。
	recallMu sync.Mutex
	// recentToolNames 是最近调用过的工具名，去重后按调用顺序保留。传给记忆召回
	// 的选择器，让它跳过这些工具的用法说明类记忆，但涉及坑和警告的仍然要选。
	recentToolNames []string
	// surfacedMemPaths 记录本次会话已经注入过的记忆文件路径，召回前做预过滤，
	// 避免同一条记忆每轮重新占用选择器的名额。
	surfacedMemPaths map[string]struct{}
}

// RecallResult 是一次记忆召回的产出：渲染好的 system-reminder 正文，以及
// 选中的记忆文件路径。路径要等正文真正注入对话时才记为已注入。
type RecallResult struct {
	Reminder string
	Paths    []string
}

// maxRecentTools 是传给记忆召回选择器的最近工具名上限。
const maxRecentTools = 10

// RecordRecentTool 记下刚执行完的工具名。重复调用同一个工具时把它移到末尾，
// 这样列表反映的是最近使用顺序而不是首次使用顺序。
func (a *Agent) RecordRecentTool(name string) {
	if name == "" {
		return
	}
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	if i := slices.Index(a.recentToolNames, name); i >= 0 {
		a.recentToolNames = slices.Delete(a.recentToolNames, i, i+1)
	}
	a.recentToolNames = append(a.recentToolNames, name)
	if len(a.recentToolNames) > maxRecentTools {
		a.recentToolNames = a.recentToolNames[1:]
	}
}

// RecallHints 返回记忆召回要用的两份状态的快照：最近用过的工具名，以及本次会话
// 已经注入过的记忆路径。返回的是副本，调用方可以在自己的 goroutine 里随便用。
func (a *Agent) RecallHints() ([]string, map[string]struct{}) {
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	tools := slices.Clone(a.recentToolNames)
	surfaced := make(map[string]struct{}, len(a.surfacedMemPaths))
	for p := range a.surfacedMemPaths {
		surfaced[p] = struct{}{}
	}
	return tools, surfaced
}

// MarkMemoriesSurfaced 记下这一轮注入了哪些记忆，下一轮召回时先把它们排除掉。
func (a *Agent) MarkMemoriesSurfaced(paths []string) {
	if len(paths) == 0 {
		return
	}
	a.recallMu.Lock()
	defer a.recallMu.Unlock()
	if a.surfacedMemPaths == nil {
		a.surfacedMemPaths = make(map[string]struct{}, len(paths))
	}
	for _, p := range paths {
		a.surfacedMemPaths[p] = struct{}{}
	}
}

// deferredReminderMarker 是延迟工具清单提醒的固定开头。用它在历史里回认这条提醒
// 还在不在：compact 把历史压成摘要之后原来那条就没了，得重发一遍。
const deferredReminderMarker = "The following deferred tools are available via ToolSearch."

// ActivateSkill 记录一次 skill 激活。body 会留着给 /skills 列表和压缩恢复用，
// 但不会每轮重新注入 —— 它就作为一条普通消息待在对话里。
func (a *Agent) ActivateSkill(name, body string) {
	if a.activeSkills == nil {
		a.activeSkills = make(map[string]string)
	}
	a.activeSkills[name] = body
}

// ClearActiveSkills 清掉所有固定住的 SOP。/clear 会调它，这样新对话不会把上一个
// 任务的 SOP 带过来。一次都没激活过 skill 时调用也是安全的。
func (a *Agent) ClearActiveSkills() {
	a.activeSkills = nil
}

// GetActiveSkills 返回当前固定住的 SOP 的副本（name → body）。测试和 /skills
// 都用它来展示当前激活了哪些。
func (a *Agent) GetActiveSkills() map[string]string {
	out := make(map[string]string, len(a.activeSkills))
	for k, v := range a.activeSkills {
		out[k] = v
	}
	return out
}

// SetToolFilter 给当前对话装一个工具可见性过滤器。每轮迭代开头都会查一次
// 这个过滤器，所以调用方可以随时开关 Coordinator 模式，不必重启 Agent。
// 传 nil 会清掉之前设置的过滤器。
func (a *Agent) SetToolFilter(allow func(name string) bool) {
	a.ToolNameFilter = allow
}

// ToolRegistry 返回正在使用的工具注册中心。之所以叫 ToolRegistry 而不是直接叫
// Registry（尽管后者和字段名一致），是为了避开 Go 不允许的方法/字段同名冲突。
// 对应 skills.SkillHost 接口的约定。
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.Registry
}

func New(client llm.Client, registry *tools.Registry, protocol string) *Agent {
	wd, _ := os.Getwd()
	return &Agent{
		Client:        client,
		Registry:      registry,
		Protocol:      protocol,
		WorkDir:       wd,
		MaxIterations: 0,
		ContextWindow: 200000,
		RecoveryState: compact.NewRecoveryState(),
	}
}

// SetSessionID 把磁盘会话日志的 id 挂到 Agent 上，这样 Layer 2 压缩才能把
// compact_boundary 记录写进 TUI 正在追加普通消息的那个会话里。
// 由 TUI 在 Agent 构造完成后立刻调用
// （resume 切换会话之后会再调一次）。
func (a *Agent) SetSessionID(id string) { a.SessionID = id }

// currentToolSchemas 构造下一次 API 调用要用的 schema 列表，
// 并遵守当前生效的 ToolNameFilter（比如 Teams 的 coordinator 模式）。
// 压缩恢复附件（列出压缩后还剩哪些工具）和真正的 Stream 调用共用它，
// 这样两边看到的东西
// 始终一致。
func (a *Agent) currentToolSchemas() []map[string]any {
	// GetAllSchemas 这里需要理解为这次调用模型api决定传给模型的工具定义
	schemas := a.Registry.GetAllSchemas(a.Protocol)
	if a.ToolNameFilter == nil {
		return schemas
	}
	// 过滤器就是唯一依据，不留任何例外分支
	return filterSchemasByName(schemas, a.ToolNameFilter)
}

// Run 是给现有 TUI 和 sub-agent 调用方用的兼容入口。
// ReAct 循环本身在 AgentRun 上。
func (a *Agent) Run(ctx context.Context, conv *conversation.Manager) <-chan AgentEvent {
	return a.NewRun(RunOptions{SessionID: a.SessionID, Conversation: conv, SessionHooks: true}).Start(ctx)
}

func (r *AgentRun) Start(ctx context.Context) <-chan AgentEvent {
	ch := make(chan AgentEvent, 32)

	go func() {
		defer close(ch)
		a := r.Agent
		conv := r.Conversation
		finishReason := RunFailed
		finalReply := ""
		defer func() {
			conv.EndRun()
			r.setFinishReason(finishReason)
			finishEvent := TraceEvent{Type: TraceRunFinished, Source: TraceSourceRun, Reason: finishReason}
			if finalReply != "" {
				preview, truncated := tracePreview(finalReply)
				finishEvent.Payload = map[string]any{"final_reply_preview": preview, "truncated": truncated}
			}
			r.record(ctx, finishEvent)
		}()
		conv.BeginRun(r.ID)
		ctx = llm.WithStreamObserver(ctx, func(lifecycle llm.RequestLifecycle) {
			event := TraceEvent{
				Source:    TraceSourceModel,
				Iteration: lifecycle.Metadata.Iteration,
				RequestID: lifecycle.Metadata.RequestID,
			}
			if lifecycle.Phase == llm.RequestStarted {
				event.Type = TraceModelRequestStarted
			} else {
				event.Type = TraceModelRequestFinished
				event.Payload = map[string]any{
					"stop_reason":   lifecycle.StopReason,
					"input_tokens":  lifecycle.Usage.InputTokens,
					"output_tokens": lifecycle.Usage.OutputTokens,
				}
				if lifecycle.Err != nil {
					event.Payload["error"] = lifecycle.Err.Error()
				}
			}
			r.record(ctx, event)
		})
		r.record(ctx, TraceEvent{Type: TraceRunStarted, Source: TraceSourceRun})
		if r.Prompt != "" {
			conv.AddUserMessage(r.Prompt)
			a.persistLastMessage(conv, r.SessionID)
		}
		if r.sessionHooks {
			defer a.emitHook(hooks.EventSessionEnd, "", nil)
			a.emitHook(hooks.EventSessionStart, "", nil)
		}

		conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)

		var totalInput, totalOutput int
		maxTokensEscalated := false
		outputRecoveries := 0

		for iteration := 1; ; iteration++ {
			if a.MaxIterations > 0 && iteration > a.MaxIterations {
				ch <- ErrorEvent{Message: fmt.Sprintf("Agent reached maximum iterations (%d)", a.MaxIterations)}
				finishReason = RunMaxIterations
				r.record(ctx, TraceEvent{Type: TraceError, Source: TraceSourceRun, Iteration: iteration, Payload: map[string]any{"message": "maximum iterations reached"}})
				return
			}

			if ctx.Err() != nil {
				finishReason = RunCancelled
				return
			}

			// 每轮迭代只算一次工具 schema 列表，这样压缩恢复附件
			// （压缩触发时）和下面真正的 Stream 调用对「接了哪些工具」
			// 看法一致。skill 过滤器只会在两轮之间变，
			// 不会在一轮之内变。
			toolSchemas := a.currentToolSchemas()
			iterationCtx := llm.WithStreamMetadata(ctx, llm.StreamMetadata{
				SessionID: r.SessionID,
				RunID:     r.ID,
				Iteration: iteration,
			})

			// Plan mode：注入结构化的工作流提醒。
			if a.Checker != nil && a.Checker.Mode == permissions.ModePlan {
				planPath := planfile.GetOrCreatePlanPath(a.WorkDir)
				a.Checker.PlanFilePath = planPath
				planExists := planfile.PlanExists(a.WorkDir)
				reminder := prompt.BuildPlanModeReminder(planPath, planExists, iteration)
				conv.AddSystemReminder(reminder)
			}

			// Coordinator 模式：工具集被收窄的同时注入调度指引。
			// 走 system-reminder 而不是系统提示词：长会话里开头那份约束会被淹没，
			// 每轮追加一次才拉得回来，而且系统提示词是缓存前缀，动它整段都要重新计费。
			if a.CoordinatorActiveFn != nil && a.CoordinatorActiveFn() {
				conv.AddSystemReminder(prompt.CoordinatorReminder(iteration))
			}

			if a.NotificationFn != nil {
				for _, note := range a.NotificationFn() {
					conv.AddSystemReminder(note)
				}
			}

			a.emitHook(hooks.EventTurnStart, "", nil)

			// 会话中途新增的 Skill：只补新出现的那几条，不重发整份清单，
			// 更不动系统提示词，避免把缓存前缀顶掉。
			if a.SkillDeltaFn != nil {
				if delta := a.SkillDeltaFn(); delta != "" {
					conv.AddSystemReminder("The following skills became available:\n" + delta)
				}
			}

			// 这里只将 deferred tool 的名字注入到 system-reminder，所以模型可以通过toolSearch查询完整tool schema
			// dispatch 模式下这些工具永远不会进 tools[]，必须额外告诉
			// 模型调用要走 mcp_call，否则它读完 schema 也不知道从哪儿调。
			//
			// 只在需要的时候发，不每轮重发。这条提醒是 append 进历史的，发过一次
			// 就一直在上下文里，之后每轮再发一遍只是拿同样的内容占窗口：六十来个
			// MCP 工具一份清单五百多 token，四十轮下来就是两万多。
			//
			// 两种情况要重发：池子变了（MCP 是异步连上的，服务器也可能掉线重连），
			// 或者历史里那条已经被 compact 压掉了。后者靠回扫历史发现，这样就不用
			// 在 compact 那边额外挂钩子。
			if deferredNames := a.Registry.GetDeferredToolNames(); len(deferredNames) > 0 {
				poolChanged := !slices.Equal(a.announcedDeferred, deferredNames)
				if poolChanged || !conv.HasReminderContaining(deferredReminderMarker) {
					reminder := deferredReminderMarker + " Their schemas are NOT loaded - use ToolSearch with query \"select:<name>[,<name>...]\" to load tool schemas"
					if a.Registry.McpLoadingMode == tools.McpLoadingDispatch {
						reminder += ", then invoke them with the mcp_call tool"
					} else {
						reminder += " before calling them"
					}
					conv.AddSystemReminder(reminder + ":\n" + strings.Join(deferredNames, "\n"))
					a.announcedDeferred = deferredNames
				}
			}

			a.emitHook(hooks.EventPreSend, "", nil)

			// Layer 2: 自动压缩
			// Layer 1（工具结果预算）在结果入历史时已处理完，历史里的内容就是最终大小，直接用其消息估算 token
			if msg, err := r.Context.Prepare(iterationCtx, r, iteration, toolSchemas); err != nil {
				r.record(ctx, TraceEvent{Type: TraceError, Source: TraceSourceContext, Iteration: iteration, Payload: map[string]any{"message": err.Error()}})
			} else if msg != "" {
				ch <- CompactEvent{Message: msg}
				r.record(ctx, TraceEvent{Type: TraceContextCompacted, Source: TraceSourceContext, Iteration: iteration, Payload: map[string]any{"message": msg}})
				conv.ClearUsageAnchor()
				conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)
			}

			requestID := newRuntimeID("req")
			events, errs := llm.StreamOnce(iterationCtx, a.Client, llm.StreamRequest{
				Metadata:     llm.StreamMetadata{SessionID: r.SessionID, RunID: r.ID, RequestID: requestID, Iteration: iteration},
				Conversation: conv,
				Tools:        toolSchemas,
			})

			var text string
			var toolCalls []llm.ToolCallComplete
			var thinkingBlocks []conversation.ThinkingBlock
			var stopReason string
			var usage llm.UsageInfo

			executor := NewStreamingExecutor(a.Registry, ch)
			executor.SetObserver(
				func(tc toolCallInfo) {
					r.record(ctx, TraceEvent{Type: TraceToolExecutionStarted, Source: TraceSourceTool, Iteration: iteration, RequestID: requestID, ToolCallID: tc.toolID, ToolName: tc.toolName})
				},
				func(result toolExecResult) {
					r.record(ctx, TraceEvent{Type: TraceToolExecutionEnded, Source: TraceSourceTool, Iteration: iteration, RequestID: requestID, ToolCallID: result.toolID, ToolName: result.toolName, Payload: map[string]any{
						"result_ref":   "session:" + r.SessionID + "#tool:" + result.toolID,
						"output_chars": len(result.output),
						"is_error":     result.isError,
						"elapsed_ms":   result.elapsed.Milliseconds(),
					}})
				},
			)

			for ev := range events {
				switch e := ev.(type) {
				case llm.ThinkingDelta:
					ch <- ThinkingText{Text: e.Text}
					r.record(ctx, TraceEvent{Type: TraceThinkingContent, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"delta": e.Text}})
				case llm.ThinkingComplete:
					thinkingBlocks = append(thinkingBlocks, conversation.ThinkingBlock{
						Thinking:  e.Thinking,
						Signature: e.Signature,
					})
				case llm.TextDelta:
					text += e.Text
					ch <- StreamText{Text: e.Text}
					r.record(ctx, TraceEvent{Type: TraceTextMessageContent, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"delta": e.Text}})
				case llm.ToolCallStart:
					ch <- ToolUseEvent{ToolID: e.ToolID, ToolName: e.ToolName}
					r.record(ctx, TraceEvent{Type: TraceToolCallStarted, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, ToolCallID: e.ToolID, ToolName: e.ToolName})
				case llm.ToolCallDelta:
					// 忽略
				case llm.ToolCallComplete:
					toolCalls = append(toolCalls, e)
					ch <- ToolUseEvent{
						ToolID:   e.ToolID,
						ToolName: e.ToolName,
						Args:     e.Arguments,
					}
					r.record(ctx, TraceEvent{Type: TraceToolCallArguments, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, ToolCallID: e.ToolID, ToolName: e.ToolName, Payload: traceToolArguments(e.Arguments)})
					r.record(ctx, TraceEvent{Type: TraceToolCallFinished, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, ToolCallID: e.ToolID, ToolName: e.ToolName})
					// 收集工具调用，等流式结束后按安全性分批执行
					executor.Submit(toolCallInfo{
						toolID:    e.ToolID,
						toolName:  e.ToolName,
						arguments: e.Arguments,
					})
				case llm.StreamEnd:
					stopReason = e.StopReason
					usage = e.Usage
				}
			}
			if ctx.Err() != nil {
				finishReason = RunCancelled
				return
			}
			a.emitHook(hooks.EventPostReceive, text, nil)

			// 处理流式错误。
			select {
			case err := <-errs:
				if err != nil {
					if retry, compacted := a.handleStreamError(iterationCtx, ch, conv, err); retry {
						r.record(ctx, TraceEvent{Type: TraceRetry, Source: TraceSourceRun, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"error": err.Error(), "compacted": compacted}})
						if compacted {
							conv.ClearUsageAnchor()
							conv.InjectLongTermMemory(a.Instructions, a.MemoryContent, a.SkillSection)
						}
						continue // 重试本轮
					}
					if ctx.Err() != nil {
						finishReason = RunCancelled
					}
					ch <- ErrorEvent{Message: err.Error()}
					r.record(ctx, TraceEvent{Type: TraceError, Source: TraceSourceModel, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"message": err.Error()}})
					return
				}
			default:
			}

			totalInput += usage.InputTokens
			totalOutput += usage.OutputTokens
			ch <- UsageEvent{InputTokens: totalInput, OutputTokens: totalOutput}

			anchorAfterAssistant := func() {
				conv.RecordUsageAnchor(
					usage.InputTokens,
					usage.OutputTokens,
					usage.CacheReadTokens,
					usage.CacheCreationTokens,
				)
			}

			// 处理 max_tokens 结束原因。
			if stopReason == "max_tokens" {
				if !maxTokensEscalated {
					// 第一次命中：静默上调。
					if setter, ok := a.Client.(llm.MaxTokensSetter); ok {
						setter.SetMaxOutputTokens(maxTokensCeiling)
						maxTokensEscalated = true
					}
					if text != "" {
						conv.AddAssistantFull(text, thinkingBlocks, nil)
						a.persistLastMessage(conv, r.SessionID)
						anchorAfterAssistant()
						conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Do not apologize or repeat previous content. Pick up mid-thought if needed.")
					}
					ch <- RetryEvent{Reason: "max_tokens escalation", Wait: 0}
					r.record(ctx, TraceEvent{Type: TraceRetry, Source: TraceSourceRun, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"reason": "max_tokens escalation"}})
					continue
				} else if outputRecoveries < maxOutputTokensRecoveries {
					// 多轮恢复。
					outputRecoveries++
					conv.AddAssistantFull(text, thinkingBlocks, nil)
					a.persistLastMessage(conv, r.SessionID)
					anchorAfterAssistant()
					conv.AddUserMessage("Output token limit hit. Resume directly from where you stopped. Break remaining work into smaller pieces.")
					ch <- RetryEvent{Reason: fmt.Sprintf("max_tokens recovery %d/%d", outputRecoveries, maxOutputTokensRecoveries), Wait: 0}
					r.record(ctx, TraceEvent{Type: TraceRetry, Source: TraceSourceRun, Iteration: iteration, RequestID: requestID, Payload: map[string]any{"reason": "max_tokens recovery", "attempt": outputRecoveries}})
					continue
				}
				// 次数用尽：落回正常结束流程。
			} else {
				// 本轮成功，重置恢复计数器。
				outputRecoveries = 0
			}

			if len(toolCalls) == 0 {
				finalReply = text
				conv.AddAssistantFull(text, thinkingBlocks, nil)
				a.persistLastMessage(conv, r.SessionID)
				if a.FileHistory != nil {
					summary := text
					if len(summary) > 60 {
						summary = summary[:60] + "..."
					}
					a.FileHistory.MakeSnapshot(conv.Len(), summary)
				}
				ch <- LoopComplete{TotalTurns: iteration}
				finishReason = RunCompleted
				if a.OnLoopComplete != nil {
					go a.OnLoopComplete(conv)
				}
				return
			}

			var toolUses []conversation.ToolUseBlock
			for _, tc := range toolCalls {
				toolUses = append(toolUses, conversation.ToolUseBlock{
					ToolUseID: tc.ToolID,
					ToolName:  tc.ToolName,
					Arguments: tc.Arguments,
				})
			}
			conv.AddAssistantFull(text, thinkingBlocks, toolUses)
			a.persistLastMessage(conv, r.SessionID)
			// 助手消息已经就位，把真实用量锚定到对话上；
			// 之后的工具结果和下一条用户消息
			// 都在这个基线上增量估算。
			anchorAfterAssistant()

			// 按安全性分批执行：只读工具并发，写/命令工具串行
			results := executor.ExecuteAll(ctx, a)

			// 溢写文件的回读结果豁免溢写：把模型刚读回来的内容再写盘换成
			// 预览，模型就永远看不到全文，还会在「读回、溢写」之间打转。
			exempt := make(map[string]bool)
			for _, tc := range toolCalls {
				if toolresult.IsSpillReadback(tc.ToolName, tc.Arguments, a.WorkDir, r.SessionID) {
					exempt[tc.ToolID] = true
				}
			}

			var toolResults []conversation.ToolResultBlock
			for _, result := range results {
				ch <- ToolResultEvent{
					ToolID:   result.toolID,
					ToolName: result.toolName,
					Output:   result.output,
					IsError:  result.isError,
					Elapsed:  result.elapsed,
				}
				content := result.output
				if len(content) > tools.MaxOutputChars && !exempt[result.toolID] {
					// 单条超限：写盘换预览。写盘失败会原样保留，同一块磁盘
					// 聚合预算也不必再试，所以两种结果都标记豁免。
					content = toolresult.PersistLargeResult(a.WorkDir, r.SessionID, result.toolID, result.output)
					exempt[result.toolID] = true
				}
				toolResults = append(toolResults, conversation.ToolResultBlock{
					ToolUseID:     result.toolID,
					Content:       content,
					IsError:       result.isError,
					ContentBlocks: result.contentBlocks,
				})
			}

			// 聚合预算：一轮并行工具的结果落在同一条消息里，单条阈值管不住
			// 合计超限的情况。进历史前把整批处理完，消息一出生就是终态。
			toolresult.ApplyBudget(toolResults, exempt, a.WorkDir, r.SessionID)

			exitPlanCalled := false
			for _, tc := range toolCalls {
				if tc.ToolName == "ExitPlanMode" {
					exitPlanCalled = true
					break
				}
			}
			conv.AddToolResultsMessage(toolResults)
			a.persistLastMessage(conv, r.SessionID)

			// 非阻塞 memory recall：工具执行完后检查 prefetch 是否就绪
			if a.MemoryRecallCh != nil {
				select {
				case recall := <-a.MemoryRecallCh:
					if recall.Reminder != "" {
						conv.AddSystemReminder(recall.Reminder)
						// 真正进了对话才算「已注入」。这一轮没消费掉的召回结果
						// 不留痕，下一轮召回时这些记忆还能参选。
						a.MarkMemoriesSurfaced(recall.Paths)
						r.record(ctx, TraceEvent{Type: TraceMemoryRecalled, Source: TraceSourceMemory, Iteration: iteration, Payload: map[string]any{"count": len(recall.Paths)}})
					}
					a.MemoryRecallCh = nil // 只消费一次
				default:
					// prefetch 还没好，下轮再检查
				}
			}

			if exitPlanCalled {
				ch <- TurnComplete{Turn: iteration}
				ch <- LoopComplete{TotalTurns: iteration}
				finishReason = RunCompleted
				return
			}
			ch <- TurnComplete{Turn: iteration}
			a.emitHook(hooks.EventTurnEnd, "", nil)
		}
	}()

	return ch
}

// emitHook 在配置了 Engine 时触发 hook 事件。失败不致命，会通过 hook 通知队列
// 暴露出来（该队列会被排进下一轮的 system-reminder）。
func (a *Agent) emitHook(event hooks.EventName, message string, args map[string]any) {
	if a.Hooks == nil {
		return
	}
	a.Hooks.RunHooks(hooks.HookContext{
		EventName: event,
		ToolArgs:  args,
		Message:   message,
	})
}

// filterSchemasByName 只保留 "name" 能通过 allow 断言的工具 schema。
// Coordinator 模式用它把 Lead agent 限制在「只做协调」的工具上，
// 实际干活交给 teammate。
func filterSchemasByName(schemas []map[string]any, allow func(name string) bool) []map[string]any {
	out := make([]map[string]any, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		if allow(name) {
			out = append(out, s)
		}
	}
	return out
}

// handleStreamError 返回 (retry, compacted)：retry 表示让调用方重跑本轮；
// compacted 表示 ForceCompact 重写了对话，
// 调用方必须丢掉自己的用量锚点（它的 AnchorCount 已经
// 对不上新的记录了）。
func (a *Agent) handleStreamError(ctx context.Context, ch chan AgentEvent, conv *conversation.Manager, err error) (retry, compacted bool) {
	var ctxErr *llm.ContextTooLongError
	if errors.As(err, &ctxErr) {
		// 历史里的工具结果在入历史时已按预算处理为终态，直接传 nil
		// 让 ForceCompact 使用 conv 自身消息
		msg, compactErr := compact.ForceCompact(ctx, conv, a.Client, a.WorkDir, a.SessionID, a.ContextWindow, a.RecoveryState, a.currentToolSchemas())
		if compactErr == nil && msg != "" {
			ch <- CompactEvent{Message: "Auto-compacted due to context length: " + msg}
			return true, true // 重试，此时锚点已过期
		}
		return false, false
	}

	var rlErr *llm.RateLimitError
	if errors.As(err, &rlErr) {
		wait := parseRetryAfter(rlErr.RetryAfter)
		ch <- RetryEvent{Reason: "rate limited", Wait: wait}
		select {
		case <-time.After(wait):
			return true, false // 重试，不做压缩
		case <-ctx.Done():
			return false, false
		}
	}

	return false, false // 其他类型的错误让用户重试
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 5 * time.Second
	}
	if secs, err := strconv.Atoi(header); err == nil {
		return time.Duration(secs) * time.Second
	}
	return 5 * time.Second
}

type toolExecResult struct {
	toolID   string
	toolName string
	output   string
	isError  bool
	elapsed  time.Duration
	// contentBlocks 让工具把结构化 content block 透到对话历史里。只有官方端点
	// 下的 ToolSearch 会填（tool_reference），其余工具留空走纯文本。
	contentBlocks []map[string]any
}

// extractFilePath 从常见的工具参数 key 里取出一个有代表性的路径，好让 hook
// 做路径 glob 匹配（`file_path =* "**/*.go"`）。
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"file_path", "path", "pattern", "target"} {
		if v, ok := args[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// executeSingleTool 执行单个工具
func (a *Agent) executeSingleTool(ctx context.Context, eventCh chan AgentEvent, tc toolCallInfo) toolExecResult {
	tool := a.Registry.Get(tc.toolName)
	start := time.Now()

	if tool == nil {
		// 工具名不存在只回一条错误结果，让模型自己换个工具重来，不打断循环。
		return toolExecResult{
			toolID:   tc.toolID,
			toolName: tc.toolName,
			output:   fmt.Sprintf("Error: unknown tool '%s'", tc.toolName),
			isError:  true,
			elapsed:  time.Since(start),
		}
	}

	if a.Checker != nil {
		decision := a.Checker.Check(tool, tc.arguments)
		if decision.Effect == permissions.Deny {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   fmt.Sprintf("Permission denied: %s", decision.Reason),
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
		if decision.Effect == permissions.Ask {
			respCh := make(chan PermissionResponse, 1)
			desc := permissions.DescribeToolAction(tc.toolName, tc.arguments)
			eventCh <- PermissionRequestEvent{
				ToolName:   tc.toolName,
				Desc:       desc,
				ResponseCh: respCh,
			}
			resp := <-respCh
			if resp == PermDeny {
				return toolExecResult{
					toolID:   tc.toolID,
					toolName: tc.toolName,
					output:   conversation.RejectedToolResult,
					isError:  true,
					elapsed:  time.Since(start),
				}
			}
			if resp == PermAllowAlways {
				content := permissions.ExtractContent(tc.toolName, tc.arguments)
				pattern := content + "*"
				if len(content) > 60 {
					pattern = content[:60] + "*"
				}
				// 写入本地规则文件，规则引擎每次评估都现读现匹配，本轮之后即刻生效
				a.Checker.RuleEngine.AppendLocalRule(permissions.Rule{
					ToolName: tc.toolName,
					Pattern:  pattern,
					Effect:   permissions.RuleAllow,
				})
			}
		}
	}

	if a.Hooks != nil {
		hookCtx := hooks.HookContext{
			EventName: hooks.EventPreToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
		}
		if rejected, msg := a.Hooks.RunPreToolHooks(hookCtx); rejected {
			return toolExecResult{
				toolID:   tc.toolID,
				toolName: tc.toolName,
				output:   "Blocked by hook: " + msg,
				isError:  true,
				elapsed:  time.Since(start),
			}
		}
	}

	result := tool.Execute(ctx, tc.arguments)

	a.RecordRecentTool(tc.toolName)

	if !result.IsError && tc.toolName == "ReadFile" {
		if p, _ := tc.arguments["file_path"].(string); p != "" {
			if data, err := os.ReadFile(p); err == nil {
				a.RecoveryState.RecordFileRead(p, string(data))
			}
		}
	}

	if a.Hooks != nil {
		a.Hooks.RunHooks(hooks.HookContext{
			EventName: hooks.EventPostToolUse,
			ToolName:  tc.toolName,
			ToolArgs:  tc.arguments,
			FilePath:  extractFilePath(tc.arguments),
			Message:   result.Output,
		})
	}

	return toolExecResult{
		toolID:        tc.toolID,
		toolName:      tc.toolName,
		output:        result.Output,
		isError:       result.IsError,
		elapsed:       time.Since(start),
		contentBlocks: result.ContentBlocks,
	}
}

func formatToolArgs(args map[string]any) string {
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 80 {
			s = s[:80] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", k, s))
	}
	return strings.Join(parts, ", ")
}

// persistLastMessage 把刚追加进对话历史的那条消息写入会话日志。
//
// 落盘点放在主循环而不是各个前端：TUI 和 Web 共用同一条记录路径，
// 中间轮次的助手文本和完整的工具调用链都会被记下来，恢复会话时才能还原。
// WorkDir 或 SessionID 为空时（一次性调用、子 Agent）跳过，不写盘。
func (a *Agent) persistLastMessage(conv *conversation.Manager, sessionID string) {
	if a.WorkDir == "" || sessionID == "" {
		return
	}
	msgs := conv.GetMessages()
	if len(msgs) == 0 {
		return
	}
	session.SaveMessage(a.WorkDir, sessionID, session.FromConversation(msgs[len(msgs)-1]))
}
