package conversation

import (
	"strings"
	"time"
)

type ToolUseBlock struct {
	ToolUseID string
	ToolName  string
	Arguments map[string]any
}

type ToolResultBlock struct {
	ToolUseID string
	Content   string
	IsError   bool
	// ContentBlocks 用于把工具结果发成结构化 content block 而不是纯文本。
	// 目前只有官方端点下的 ToolSearch 会用：它回 tool_reference 块，由服务端
	// 把 schema 展开进上下文。填了它时 Content 仍保留等价文本，token 估算和
	// TUI 展示都走 Content。
	ContentBlocks []map[string]any
}

type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type Message struct {
	Role           string
	Content        string
	ThinkingBlocks []ThinkingBlock
	ToolUses       []ToolUseBlock
	ToolResults    []ToolResultBlock
	// RunID identifies the independent user request that produced this message.
	// Empty is retained for legacy sessions and session-level reminders.
	RunID string
}

type Manager struct {
	history        []Message // 对话历史
	ltmInjected    bool
	baselineTokens int //上次请求LLM API返回的精确token数
	anchorCount    int
	hasUsage       bool
	activeRunID    string
}

func NewManager() *Manager {
	return &Manager{}
}

func (m *Manager) AddUserMessage(content string) {
	m.history = append(m.history, Message{Role: "user", Content: content, RunID: m.activeRunID})
}

func (m *Manager) AddAssistantMessage(content string) {
	m.history = append(m.history, Message{Role: "assistant", Content: content, RunID: m.activeRunID})
}

func (m *Manager) AddToolUseMessage(text, toolUseID, toolName string, arguments map[string]any) {
	m.history = append(m.history, Message{
		Role:    "assistant",
		Content: text,
		RunID:   m.activeRunID,
		ToolUses: []ToolUseBlock{{
			ToolUseID: toolUseID,
			ToolName:  toolName,
			Arguments: arguments,
		}},
	})
}

func (m *Manager) AddAssistantMessageWithTools(text string, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:     "assistant",
		Content:  text,
		ToolUses: toolUses,
		RunID:    m.activeRunID,
	})
}

func (m *Manager) AddAssistantFull(text string, thinking []ThinkingBlock, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:           "assistant",
		Content:        text,
		ThinkingBlocks: thinking,
		ToolUses:       toolUses,
		RunID:          m.activeRunID,
	})
}

func (m *Manager) AddToolResultMessage(toolUseID, content string, isError bool) {
	m.history = append(m.history, Message{
		Role:  "user",
		RunID: m.activeRunID,
		ToolResults: []ToolResultBlock{{
			ToolUseID: toolUseID,
			Content:   content,
			IsError:   isError,
		}},
	})
}

func (m *Manager) AddToolResultsMessage(results []ToolResultBlock) {
	m.history = append(m.history, Message{
		Role:        "user",
		ToolResults: results,
		RunID:       m.activeRunID,
	})
}

func (m *Manager) AddSystemReminder(content string) {
	m.history = append(m.history, Message{
		Role:    "user",
		Content: "<system-reminder>\n" + content + "\n</system-reminder>",
		RunID:   m.activeRunID,
	})
}

// BeginRun establishes the boundary for an independent user request. Messages
// appended until EndRun inherit runID. For backward-compatible callers that
// add the prompt before starting the run, the most recent unassigned user
// message is claimed by this run.
func (m *Manager) BeginRun(runID string) {
	m.activeRunID = runID
	for i := len(m.history) - 1; i >= 0; i-- {
		msg := &m.history[i]
		if msg.RunID != "" {
			break
		}
		if msg.Role == "user" && len(msg.ToolResults) == 0 && !strings.HasPrefix(msg.Content, "<system-reminder>") {
			msg.RunID = runID
			break
		}
	}
}

// EndRun clears the append-time run marker without changing recorded messages.
func (m *Manager) EndRun() { m.activeRunID = "" }

// ActiveRunID returns the run currently appending to this conversation.
func (m *Manager) ActiveRunID() string { return m.activeRunID }

// Clone creates an isolated snapshot suitable for one AgentRun. Message slices
// and nested tool payloads are copied so a failed or concurrent run cannot
// mutate the session's committed transcript.
func (m *Manager) Clone() *Manager {
	clone := &Manager{
		ltmInjected:    m.ltmInjected,
		baselineTokens: m.baselineTokens,
		anchorCount:    m.anchorCount,
		hasUsage:       m.hasUsage,
		activeRunID:    m.activeRunID,
	}
	clone.history = cloneMessages(m.history)
	return clone
}

// ReplaceWith commits a completed run snapshot back to its owning session.
func (m *Manager) ReplaceWith(other *Manager) {
	if other == nil {
		return
	}
	*m = *other.Clone()
}

func cloneMessages(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, msg := range messages {
		out[i] = msg
		out[i].ThinkingBlocks = append([]ThinkingBlock(nil), msg.ThinkingBlocks...)
		out[i].ToolUses = append([]ToolUseBlock(nil), msg.ToolUses...)
		for j := range out[i].ToolUses {
			out[i].ToolUses[j].Arguments = cloneMap(msg.ToolUses[j].Arguments)
		}
		out[i].ToolResults = append([]ToolResultBlock(nil), msg.ToolResults...)
		for j := range out[i].ToolResults {
			blocks := msg.ToolResults[j].ContentBlocks
			out[i].ToolResults[j].ContentBlocks = make([]map[string]any, len(blocks))
			for k := range blocks {
				out[i].ToolResults[j].ContentBlocks[k] = cloneMap(blocks[k])
			}
		}
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneValue(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(typed))
		for i, item := range typed {
			out[i] = cloneMap(item)
		}
		return out
	default:
		return value
	}
}

// HasReminderContaining 报告历史里还有没有包含 marker 的提醒。
//
// 用来判断一条「只需要说一次」的提醒是否已经在上下文里。compact 会把历史压成
// 摘要，原来那条提醒随之消失，这时候必须重发，否则模型再也看不到。调用方拿这个
// 结果决定重发，就不用在 compact 那边额外挂钩子。
func (m *Manager) HasReminderContaining(marker string) bool {
	for _, msg := range m.history {
		if msg.Role == "user" && strings.Contains(msg.Content, marker) {
			return true
		}
	}
	return false
}

func (m *Manager) InjectLongTermMemory(instructions, memories, skills string) {
	if m.ltmInjected {
		return
	}
	var sections []string
	if instructions != "" {
		sections = append(sections, "# mewcodeMd\nCodebase and user instructions are shown below. Be sure to adhere to these instructions. IMPORTANT: These instructions OVERRIDE any default behavior and you MUST follow them exactly as written.\n\n"+instructions)
	}
	if memories != "" {
		sections = append(sections, "# autoMemory\n"+memories)
	}
	// Skill 清单跟着项目走，放系统提示词会让每个项目各有一份、跨项目缓存全失效，
	// 所以和指令、记忆一样放在这条消息里。
	if skills != "" {
		sections = append(sections, "# availableSkills\n"+skills)
	}
	if len(sections) == 0 {
		return
	}
	sections = append(sections, "# currentDate\nToday's date is "+time.Now().Format("2006-01-02")+".")
	body := strings.Join(sections, "\n\n")
	wrapped := "<system-reminder>\nAs you answer the user's questions, you can use the following context:\n" +
		body +
		"\n\n      IMPORTANT: this context may or may not be relevant to your tasks. You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>"
	m.history = append([]Message{{Role: "user", Content: wrapped}}, m.history...)
	m.ltmInjected = true
}

// AppendMessages copies the given messages onto the end of the history. Used by
// compaction to replay the recent-tail messages verbatim after the summary.
func (m *Manager) AppendMessages(messages []Message) {
	m.history = append(m.history, messages...)
}

func (m *Manager) Len() int {
	return len(m.history)
}

func (m *Manager) TruncateTo(index int) {
	if index < 0 {
		index = 0
	}
	if index > len(m.history) {
		return
	}
	m.history = m.history[:index]
}

func (m *Manager) GetMessages() []Message {
	result := make([]Message, len(m.history))
	copy(result, m.history)
	return result
}

// ReplaceToolResults 就地替换指定消息的 ToolResults 列表。
// 用于 Layer 1 tool-result budget 的 Design A 实现：直接修改原始
// 对话历史，而非生成新的副本。msgIndex 越界时静默忽略。
func (m *Manager) ReplaceToolResults(msgIndex int, newResults []ToolResultBlock) {
	if msgIndex < 0 || msgIndex >= len(m.history) {
		return
	}
	m.history[msgIndex].ToolResults = newResults
}

// RecordUsageAnchor 锚定本轮 API 返回的真实 token 用量。
// 调用时机：assistant 消息已追加到 history 之后。
func (m *Manager) RecordUsageAnchor(input, output, cacheRead, cacheCreation int) {
	baseline := input + cacheRead + cacheCreation + output
	if baseline <= 0 {
		return
	}
	m.baselineTokens = baseline
	m.anchorCount = len(m.history)
	m.hasUsage = true
}

// ClearUsageAnchor 压缩后清零锚点，下次估算回退到全量字符估算。
func (m *Manager) ClearUsageAnchor() {
	m.baselineTokens = 0
	m.anchorCount = 0
	m.hasUsage = false
}

// UsageAnchorState 返回当前锚点状态，供 compact 层读取。
func (m *Manager) UsageAnchorState() (baselineTokens, anchorCount int, hasUsage bool) {
	return m.baselineTokens, m.anchorCount, m.hasUsage
}
