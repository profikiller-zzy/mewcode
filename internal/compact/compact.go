// compact 包实现 MewCode 的第二层上下文管理：由 LLM 生成完整对话摘要，
// 根据 token 阈值触发（旧默认值为上下文窗口的 80%），用摘要消息替换对话，
// 并可通过 ForceCompact（/compact 命令）主动触发。
//
// 第一层（工具结果预算）位于 toolresult 包：单条超限和单消息聚合
// 超限都在结果进入对话历史那一刻处理完，消息一进历史就是终态，这里
// 拿到的消息大小即最终大小。
package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/session"
)

const (
	// maxPTLRetries 限制摘要请求因 prompt 过长失败后，通过删除最早 API 轮次分组进行重试的次数。
	maxPTLRetries = 3
	// 删除旧分组后，在开头加入 ptlRetryMarker，确保摘要请求仍以 user 角色消息开始。
	ptlRetryMarker = "[earlier conversation truncated for compaction retry]"

	// autoCompactThreshold 是旧的比例阈值，仅保留作参考。
	// 当前实际判断使用下面的绝对 token 公式：当用量接近上下文窗口上限时触发压缩，并为下一轮预留空间。
	autoCompactThreshold = 0.80

	// summaryOutputReserve 为摘要响应本身预留空间，因此有效窗口为
	// contextWindow − min(model maxOutput, summaryOutputReserve)。
	summaryOutputReserve = 20000
	// autoCompactSafetyMargin 设置低于有效窗口的软自动压缩触发线。
	autoCompactSafetyMargin = 13000
	// manualCompactSafetyMargin 设置硬阻断线：用量超过 effectiveWindow − manualCompactSafetyMargin 后，
	// 强制执行压缩。
	manualCompactSafetyMargin = 3000
)

// 压缩时的近期消息保留预算：保留尾部近期消息原文，只摘要较早的前缀。
const (
	// keepRecentTokens 是 token 保留下限：从尾部向前累加消息 token，直到至少达到该值。
	keepRecentTokens = 10000
	// minKeepMessages 是不考虑 token 数时也必须保留的近期消息数。
	// keepRecentTokens 或 minKeepMessages 任一条件先满足，就停止向前遍历。
	minKeepMessages = 5
	// keepMaxTokens 限制保留尾部的最大 token 数；加入下一条会超过上限时停止，
	// 避免保留过多而使摘要无法节省空间。
	keepMaxTokens = 40000
)

// computeCompactThreshold 返回第二层应触发的绝对 token 用量线。
// effectiveWindow = contextWindow − min(maxOutput, summaryOutputReserve)；
// 阈值为有效窗口减去安全余量（硬阻断线使用手动余量，软触发线使用自动余量）。
func computeCompactThreshold(contextWindow, maxOutput int, manual bool) int {
	reserve := summaryOutputReserve
	if maxOutput > 0 && maxOutput < reserve {
		reserve = maxOutput
	}
	effectiveWindow := contextWindow - reserve
	margin := autoCompactSafetyMargin
	if manual {
		margin = manualCompactSafetyMargin
	}
	return effectiveWindow - margin
}

// MaxConsecutiveAutoCompactFailures 用于上下文无法恢复地超限时（例如 prompt_too_long）停止自动压缩重试，
// 避免 Agent 每轮都向 API 发起注定失败的请求。
const MaxConsecutiveAutoCompactFailures = 3

// AutoCompactTrackingState 在 Agent 循环各轮之间传递熔断器状态。
// 该结构由调用方持有，ManageContext 会原地修改它。
type AutoCompactTrackingState struct {
	// ConsecutiveFailures 统计上次成功后返回错误的连续自动压缩次数，成功后清零。
	ConsecutiveFailures int
}

// summarySystemPrompt 要求模型分两阶段响应：先输出 <analysis> 草稿块，再输出 <summary> 块。
// 写回对话前由 formatCompactSummary 删除 analysis，只保留结构化摘要。
const summarySystemPrompt = `Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests and your previous actions.
This summary should be thorough in capturing technical details, code patterns, and architectural decisions that would be essential for continuing development work without losing context.

Before providing your final summary, wrap your analysis in <analysis> tags to organize your thoughts and ensure you've covered all necessary points. In your analysis process:

1. Chronologically analyze each message and section of the conversation. For each section thoroughly identify:
   - The user's explicit requests and intents
   - Your approach to addressing the user's requests
   - Key decisions, technical concepts and code patterns
   - Specific details like:
     - file names
     - full code snippets
     - function signatures
     - file edits
   - Errors that you ran into and how you fixed them
   - Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
2. Double-check for technical accuracy and completeness, addressing each required element thoroughly.

After your analysis, output your final summary wrapped in <summary> tags. Your summary should include the following sections:

1. Primary Request and Intent: Capture all of the user's explicit requests and intents in detail
2. Key Technical Concepts: List all important technical concepts, technologies, and frameworks discussed.
3. Files and Code Sections: Enumerate specific files and code sections examined, modified, or created. Pay special attention to the most recent messages and include full code snippets where applicable and include a summary of why this file read or edit is important.
4. Errors and fixes: List all errors that you ran into, and how you fixed them. Pay special attention to specific user feedback that you received, especially if the user told you to do something differently.
5. Problem Solving: Document problems solved and any ongoing troubleshooting efforts.
6. All user messages: List ALL user messages that are not tool results. These are critical for understanding the users' feedback and changing intent.
7. Pending Tasks: Outline any pending tasks that you have explicitly been asked to work on.
8. Current Work: Describe in detail precisely what was being worked on immediately before this summary request, paying special attention to the most recent messages from both user and assistant. Include file names and code snippets where applicable.
9. Optional Next Step: List the next step that you will take that is related to the most recent work you were doing. IMPORTANT: ensure that this step is DIRECTLY in line with the user's most recent explicit requests, and the task you were working on immediately before this summary request. If your last task was concluded, then only list next steps if they are explicitly in line with the users request.
   If there is a next step, include direct quotes from the most recent conversation showing exactly what task you were working on and where you left off. This should be verbatim to ensure there's no drift in task interpretation.

Output structure:

<analysis>
[Your thought process, ensuring all points are covered thoroughly and accurately]
</analysis>

<summary>
1. Primary Request and Intent:
   [Detailed description]

2. Key Technical Concepts:
   - [Concept 1]
   - [Concept 2]

3. Files and Code Sections:
   - [File Name 1]
      - [Summary and important code snippet]

4. Errors and fixes:
   - [Error and fix description]

5. Problem Solving:
   [Description]

6. All user messages:
   - [User message 1]
   - [User message 2]

7. Pending Tasks:
   - [Task 1]

8. Current Work:
   [Precise description]

9. Optional Next Step:
   [Next step if applicable]
</summary>`

// EstimateTokens 使用 3.5 字符/token 的近似值统计正文、工具参数、工具结果和思考块。
func EstimateTokens(messages []conversation.Message) int {
	total := 0
	for _, m := range messages {
		total += int(float64(len(m.Content))/3.5) + 4
		for _, tu := range m.ToolUses {
			argsJSON, _ := json.Marshal(tu.Arguments)
			total += 50 + int(float64(len(argsJSON))/3.5)
		}
		for _, tr := range m.ToolResults {
			total += int(float64(len(tr.Content))/3.5) + 10
		}
		for _, tb := range m.ThinkingBlocks {
			total += int(float64(len(tb.Thinking)) / 3.5)
		}
	}
	return total
}

// UsageAnchor 记录最近一次真实 API 用量，以及报告该用量时的对话长度。
// baselineTokens 是该轮真实 prompt+output 总量（input + cache_read + cache_creation + output）；
// anchorCount 是 assistant 轮次稳定后 conv.Len() 的值。追加在其后的内容尚无真实用量，
// 因此只在基线上做增量估算。
// 零值锚点（HasUsage 为 false）表示尚未观察到真实用量，调用方会估算全部消息。
type UsageAnchor struct {
	BaselineTokens int
	AnchorCount    int
	HasUsage       bool
}

// BaselineFromUsage 将 API 用量报告折叠为锚点基线使用的单个“已传输真实 token”数。
// Anthropic 将 cache_read / cache_creation 与 input_tokens 分开报告，因此真实 prompt 大小是四项之和。
func BaselineFromUsage(u llm.UsageInfo) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens + u.OutputTokens
}

// ComputeUsedTokens 返回当前“已用 token”数，并与压缩阈值比较。
// 有真实用量锚点时，返回 baselineTokens 加上锚点后新增消息的估算值。
// 没有锚点时（冷启动、第一轮）估算全部消息，以便首个 usage 报告到达前 Agent 也能工作。
func ComputeUsedTokens(messages []conversation.Message, anchor UsageAnchor) int {
	if !anchor.HasUsage {
		return EstimateTokens(messages)
	}
	// 防御性边界检查：如果对话被回退到锚点之前（例如压缩后），锚点已过期，
	// 应估算全部消息，避免切片越界。
	if anchor.AnchorCount < 0 || anchor.AnchorCount > len(messages) {
		return EstimateTokens(messages)
	}
	return anchor.BaselineTokens + EstimateTokens(messages[anchor.AnchorCount:])
}

// ComputeUsedTokensFromConv 从 ConversationManager 读取锚点状态来计算
// 当前 token 用量。这是 ComputeUsedTokens 的便捷封装，避免调用方
// 手动传递 UsageAnchor 参数。
func ComputeUsedTokensFromConv(conv *conversation.Manager) int {
	baseline, count, has := conv.UsageAnchorState()
	return ComputeUsedTokens(conv.GetMessages(), UsageAnchor{
		BaselineTokens: baseline,
		AnchorCount:    count,
		HasUsage:       has,
	})
}

// ManageContext 在已用 token 达到自动压缩线时执行第二层；超过硬阻断线时强制压缩。
// 第一层在工具结果进入历史时完成，因此这里直接使用已定型的消息估算，无需再次裁剪。
// tracking 在循环各轮之间传递熔断器状态；传入 nil 时禁用熔断器。
// anchor 携带最近一次真实 API 用量和当时的对话长度；存在时只估算锚点后新增的消息，
// 不存在时估算全部消息。它只改变当前用量的计算方式，不改变阈值公式。
// workDir 与 sessionID 用于定位会话日志；两者非空且压缩成功时追加 compact_boundary，
// 让后续恢复重建压缩状态，而不是重放完整的压缩前记录。任一为空时跳过持久化。
func ManageContext(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	maxOutput int,
	tracking *AutoCompactTrackingState,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	// 历史里的工具结果在入历史时已按预算处理为终态，conv 自身消息
	// 就是实际发送量，直接用它估算。
	baseline, count, has := conv.UsageAnchorState()
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: count, HasUsage: has}
	tokens := ComputeUsedTokens(conv.GetMessages(), anchor)
	// 软自动压缩触发：used tokens >= effectiveWindow − auto margin。
	if tokens < computeCompactThreshold(contextWindow, maxOutput, false) {
		return "", nil
	}

	// 硬阻断线：用量超过 effectiveWindow − manual margin 后，绕过熔断器直接强制压缩，
	// 因为上下文过于接近上限，不能跳过处理。
	if tokens >= computeCompactThreshold(contextWindow, maxOutput, true) {
		return ForceCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	}

	// 熔断器：连续失败 N 次后停止重试；否则无法恢复的超限会导致每轮都向 API 发起失败的压缩请求。
	if tracking != nil && tracking.ConsecutiveFailures >= MaxConsecutiveAutoCompactFailures {
		return "", nil
	}

	msg, err := autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
	if err != nil {
		if tracking != nil {
			tracking.ConsecutiveFailures++
		}
		return "", err
	}
	if tracking != nil {
		tracking.ConsecutiveFailures = 0
	}
	return msg, nil
}

// ForceCompact 是手动 /compact 的入口，无论当前 token 比例如何都会执行第二层。
// 第一层无需单独执行，因为完整摘要会覆盖工具结果预算的作用。
func ForceCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	return autoCompact(ctx, conv, client, workDir, sessionID, contextWindow, recovery, toolSchemas)
}

// hasToolResults 判断消息是否包含 tool_result 块（工具执行器生成的 user 消息）。
// 这类消息不能与其对应的 assistant tool_use 分开，否则 API 会拒绝包含孤儿结果的对话。
func hasToolResults(m conversation.Message) bool {
	return len(m.ToolResults) > 0
}

// computeKeepStartIndex 选择摘要前缀（messages[:keepStart]）与原样保留尾部
// （messages[keepStart:]）之间的边界。预算以完整的用户 run 为单位从尾部向前
// 累加，所以边界不会落在一个多 iteration 工具链中间。旧会话没有
// RunID 时，以普通 user prompt 作为 run 起点兼容分组。
// 返回边界索引；keepStart <= 0 表示可摘要内容太少，调用方不执行压缩。
func computeKeepStartIndex(messages []conversation.Message) int {
	groups := groupMessagesByRun(messages)
	if len(groups) == 0 {
		return 0
	}

	keptTokens := 0
	keptCount := 0
	keepStart := len(messages)
	for i := len(groups) - 1; i >= 0; i-- {
		groupTokens := EstimateTokens(groups[i])
		// 最近的一个 run 即使自身超预算也必须整个保留。
		if keptCount > 0 && keptTokens+groupTokens > keepMaxTokens {
			break
		}
		keptTokens += groupTokens
		keptCount += len(groups[i])
		keepStart -= len(groups[i])
		if keptTokens >= keepRecentTokens || keptCount >= minKeepMessages {
			break
		}
	}
	return keepStart
}

// groupMessagesByRun 优先使用显式 RunID；对历史记录则把每条普通
// user prompt 到下一条 prompt 之前的整条工具链视为一个 run。
func groupMessagesByRun(messages []conversation.Message) [][]conversation.Message {
	var groups [][]conversation.Message
	var current []conversation.Message
	currentRunID := ""

	for _, m := range messages {
		startsExplicitRun := m.RunID != "" && m.RunID != currentRunID
		startsLegacyRun := m.RunID == "" && m.Role == "user" && len(m.ToolResults) == 0 &&
			!strings.HasPrefix(m.Content, "<system-reminder>") && len(current) > 0
		if (startsExplicitRun || startsLegacyRun) && len(current) > 0 {
			groups = append(groups, current)
			current = nil
		}
		current = append(current, m)
		if m.RunID != "" {
			currentRunID = m.RunID
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// truncateHeadForPTL 从前缀删除最早的完整 agent run，直到估算 token 至少减少 tokenGap。
// 没有可供摘要的有效内容时返回 nil。
func truncateHeadForPTL(prefix []conversation.Message, tokenGap int) []conversation.Message {
	groups := groupMessagesByRun(prefix)
	if len(groups) < 2 {
		return nil
	}

	dropCount := 0
	if tokenGap > 0 {
		acc := 0
		for _, g := range groups {
			acc += EstimateTokens(g)
			dropCount++
			if acc >= tokenGap {
				break
			}
		}
	} else {
		dropCount = max(1, len(groups)/5)
	}

	dropCount = min(dropCount, len(groups)-1)
	if dropCount < 1 {
		return nil
	}

	var result []conversation.Message
	for _, g := range groups[dropCount:] {
		result = append(result, g...)
	}
	if len(result) > 0 && result[0].Role != "user" {
		marker := conversation.Message{Role: "user", Content: ptlRetryMarker}
		result = append([]conversation.Message{marker}, result...)
	}
	return result
}

// buildPrefixText 将前缀消息序列化为摘要 LLM 调用使用的文本块。
func buildPrefixText(prefix []conversation.Message) string {
	var sb strings.Builder
	for _, m := range prefix {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		for _, tu := range m.ToolUses {
			sb.WriteString(fmt.Sprintf("[tool_use %s]: %s\n", tu.ToolName, tu.ToolUseID))
		}
		for _, tr := range m.ToolResults {
			content := tr.Content
			if len(content) > 500 {
				content = content[:500] + "..."
			}
			sb.WriteString(fmt.Sprintf("[tool_result]: %s\n", content))
		}
	}
	return sb.String()
}

// autoCompact 是第二层：让 LLM 摘要较早的前缀，仅用一条摘要消息替换 messages[:keepStart]，
// 同时原样保留近期尾部 messages[keepStart:]。摘要生成后，在摘要消息后附加恢复块，
// 使模型仍能看到最近读取的文件快照、已调用 Skill 的 SOP 和当前工具清单。
// 前缀过短时不执行压缩。
func autoCompact(
	ctx context.Context,
	conv *conversation.Manager,
	client llm.Client,
	workDir string,
	sessionID string,
	contextWindow int,
	recovery *RecoveryState,
	toolSchemas []map[string]any,
) (string, error) {
	messages := conv.GetMessages()
	beforeTokens := EstimateTokens(messages)

	// 选择原样保留的近期尾部，仅摘要 keepStart 之前的前缀。
	// 如果边界前没有（或几乎没有）可摘要内容，就保持对话不变。
	keepStart := computeKeepStartIndex(messages)
	if keepStart <= 0 {
		// 所有内容都在保留尾部中（对话太短），因此不做无效压缩。
		return "", nil
	}
	prefix := messages[:keepStart]
	keep := messages[keepStart:]

	// Cache-sharing 摘要：保留原始消息不动，在末尾追加摘要指令。
	// API 调用的消息前缀和主对话上一次调用一致，命中 Prompt Cache，
	// 只有末尾那条摘要指令按全价处理。PTL 时降级到文本序列化 + 截断重试。
	finalSummary, err := callSummaryWithCacheSharing(ctx, client, messages, toolSchemas)
	if err != nil {
		var ptlErr *llm.ContextTooLongError
		if !errors.As(err, &ptlErr) {
			return "", err
		}
		finalSummary, err = callSummaryWithPTLRetry(ctx, client, prefix, toolSchemas)
		if err != nil {
			return "", err
		}
	}

	// 持久化 compact_boundary，使后续恢复可以重建“摘要 + 保留尾部”，无需重放完整旧记录。
	// 采用追加写入：原始前缀仍留在会话文件中，但恢复时不会重放边界之前的内容。
	// 保留尾部连同工具块一起内嵌，从而保留近期轮次完整的工具调用链。
	// 边界只保存纯摘要文本，不保存恢复附件；恢复快照是仅存在于内存中的辅助信息。
	// sessionID 或 workDir 为空时跳过（测试和一次性调用）。
	if sessionID != "" && workDir != "" {
		keepRecords := make([]session.KeepMessage, 0, len(keep))
		for _, m := range keep {
			keepRecords = append(keepRecords, session.FromConversationKeep(m))
		}
		session.SaveCompactBoundary(workDir, sessionID, finalSummary, keepRecords)
	}

	content := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + finalSummary
	if len(keep) > 0 {
		content += "\n\n近期消息已原样保留。"
	}
	if sessionID != "" && workDir != "" {
		content += fmt.Sprintf("\n\n如果你需要压缩前的具体细节（代码片段、报错信息等），请用 ReadFile 读取完整会话记录：%s", session.SessionFilePath(workDir, sessionID))
	}
	if attachment := BuildRecoveryAttachment(recovery, toolSchemas); attachment != "" {
		content += "\n\n---\n\n" + attachment
	}

	compacted := conversation.NewManager()
	compacted.AddUserMessage(content)
	compacted.AppendMessages(keep)

	*conv = *compacted
	afterTokens := EstimateTokens(conv.GetMessages())
	return fmt.Sprintf("Compacted: %d → %d estimated tokens", beforeTokens, afterTokens), nil
}

// callSummaryWithCacheSharing 保留原始消息列表不做序列化，在末尾追加摘要
// 指令作为一条 user message 发给 LLM。消息前缀和主对话上一次 API 调用一致，
// 能命中 Prompt Cache（Anthropic 90% 折扣、OpenAI 50% 折扣、DeepSeek ~90% 折扣）。
func callSummaryWithCacheSharing(
	ctx context.Context,
	client llm.Client,
	messages []conversation.Message,
	toolSchemas []map[string]any,
) (string, error) {
	// 找到最后一条 assistant 消息，确保追加 user message 后消息序列合法
	lastAssistant := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAssistant = i
			break
		}
	}
	if lastAssistant < 0 {
		return "", fmt.Errorf("no assistant message found for cache-sharing compact")
	}

	summaryConv := conversation.NewManager()
	summaryConv.AppendMessages(messages[:lastAssistant+1])
	summaryConv.AddUserMessage(summarySystemPrompt)

	events, errs := llm.StreamOnce(ctx, client, llm.StreamRequest{Conversation: summaryConv, Tools: toolSchemas})
	var summary strings.Builder
	for ev := range events {
		if td, ok := ev.(llm.TextDelta); ok {
			summary.WriteString(td.Text)
		}
	}
	var streamErr error
	select {
	case streamErr = <-errs:
	default:
	}
	if streamErr != nil {
		return "", streamErr
	}
	return formatCompactSummary(summary.String()), nil
}

// callSummaryWithPTLRetry 将前缀发送给 LLM 生成摘要。
// 如果请求返回 ContextTooLongError，就从前缀删除最早的 API 轮次分组并重试，最多 maxPTLRetries 次。
func callSummaryWithPTLRetry(
	ctx context.Context,
	client llm.Client,
	prefix []conversation.Message,
	toolSchemas []map[string]any,
) (string, error) {
	currentPrefix := prefix
	for attempt := 0; ; attempt++ {
		text := buildPrefixText(currentPrefix)
		summaryConv := conversation.NewManager()
		summaryConv.AddUserMessage(summarySystemPrompt + "\n\n" + text)

		events, errs := llm.StreamOnce(ctx, client, llm.StreamRequest{Conversation: summaryConv, Tools: toolSchemas})
		var summary strings.Builder
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				summary.WriteString(td.Text)
			}
		}
		var streamErr error
		select {
		case streamErr = <-errs:
		default:
		}

		if streamErr == nil {
			return formatCompactSummary(summary.String()), nil
		}

		var ptlErr *llm.ContextTooLongError
		if !errors.As(streamErr, &ptlErr) || attempt >= maxPTLRetries {
			return "", streamErr
		}

		tokenGap := EstimateTokens(currentPrefix) / 5
		truncated := truncateHeadForPTL(currentPrefix, tokenGap)
		if truncated == nil {
			return "", streamErr
		}
		currentPrefix = truncated
	}
}

// formatCompactSummary 删除模型两阶段响应中的 <analysis> 草稿块，只返回 <summary> 块内容。
// 如果两个标签都不存在（模型未遵循格式），则返回原始文本，避免摘要完全丢失。
func formatCompactSummary(raw string) string {
	if start := strings.Index(raw, "<summary>"); start >= 0 {
		body := raw[start+len("<summary>"):]
		if end := strings.Index(body, "</summary>"); end >= 0 {
			return strings.TrimSpace(body[:end])
		}
		return strings.TrimSpace(body)
	}
	// 没有 <summary> 块：如果存在 <analysis>...</analysis> 块则删除它，返回剩余内容。
	if start := strings.Index(raw, "<analysis>"); start >= 0 {
		if end := strings.Index(raw, "</analysis>"); end > start {
			return strings.TrimSpace(raw[:start] + raw[end+len("</analysis>"):])
		}
	}
	return strings.TrimSpace(raw)
}
