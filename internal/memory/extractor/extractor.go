// extractor 包实现后台记忆提取子 Agent。
//
// 原 TS 实现使用闭包状态；Go 版本将同样的状态封装在 Extractor 结构体和 sync.Mutex 中，
// 使每个调用方拥有独立实例，也便于测试替换依赖。
//
// 触发方式：TUI 将 agent.Agent.OnLoopComplete 设置为调用 (*Extractor).Execute 的闭包。
// Agent 每次 LoopComplete 后以异步方式触发回调，真正的提取在后台执行，Execute 很快返回。
package extractor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mewcode/internal/agent"
	"mewcode/internal/agents"
	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/memory"
	"mewcode/internal/tools"
)

// Deps 保存 Extractor 所需的外部依赖。TUI 启动时创建它，再将 Execute 挂到 Agent 的 OnLoopComplete。
//
// AppendSystem 用于在提取成功后向用户显示“Memory saved: foo.md”通知。
type Deps struct {
	MemoryDir     string                  // <wd>/.mewcode/memory/ —— project/reference（末尾带分隔符）
	UserMemoryDir string                  // ~/.mewcode/memory/ —— user/feedback（末尾带分隔符）；$HOME 解析不到时可能为 ""
	ProjectRoot   string                  // 项目根目录绝对路径
	Client        llm.Client              // 派生提取 agent 用的 LLM client
	ToolRegistry  *tools.Registry         // 父工具注册表（会被过滤）
	Protocol      string                  // "anthropic" / "openai"
	Conversation  *conversation.Manager   // 父对话的引用
	AppendSystem  func(string)            // 可选：通知 TUI 已保存的记忆
	DebugLogf     func(format string, args ...any) // 可选：调试日志

	// ContextWindow / MaxOutputTokens 透传给派生 agent，让它的 Layer 2 压缩阈值
	// 按 provider 的真实窗口换算（0 表示沿用 agent.New 的默认值）。
	ContextWindow   int
	MaxOutputTokens int
}

// Extractor 是后台记忆提取器。所有状态都封装在结构体字段中，并由 mu 保护。
// 每个实例相互独立，测试可以使用模拟 Deps 构造实例而不影响全局状态。
//
// 从 TS 的 initExtractMemories 闭包移植而来，字段对应关系如下：
// inFlightExtractions Set → inFlight map[*sync.WaitGroup]struct{}
// lastMemoryMessageUuid string|undefined → lastMemoryMessageIdx int
// （MewCode 消息没有 uuid；游标是上次成功提取时父对话消息数组的索引。）
// hasLoggedGateFailure / inProgress / turnsSinceLastExtraction → bool/int
// pendingContext → *pendingExtractionCtx
type Extractor struct {
	deps Deps

	mu                       sync.Mutex
	inFlight                 map[*sync.WaitGroup]struct{}
	lastMemoryMessageIdx     int
	hasLoggedGateFailure     bool
	inProgress               bool
	turnsSinceLastExtraction int
	pendingContext           *pendingExtractionCtx
}

// pendingExtractionCtx 是尾随提取的暂存标记。Go 版本的状态都在 Extractor 内，
// 因此不需要额外载荷；非 nil 表示当前提取结束后还要再运行一次。
type pendingExtractionCtx struct{}

// InitExtractMemories 使用给定依赖构造新的 Extractor。
func InitExtractMemories(deps Deps) *Extractor {
	return &Extractor{
		deps:     deps,
		inFlight: make(map[*sync.WaitGroup]struct{}),
	}
}

// Execute 是异步触发入口，由 TUI 挂到 agent.Agent.OnLoopComplete。
// 它会快速返回；提取工作在后台进行。错误按尽力而为处理，调用方会忽略返回值。
func (e *Extractor) Execute(ctx context.Context) error {
	if e == nil {
		return nil
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	e.mu.Lock()
	e.inFlight[wg] = struct{}{}
	e.mu.Unlock()
	defer func() {
		wg.Done()
		e.mu.Lock()
		delete(e.inFlight, wg)
		e.mu.Unlock()
	}()

	return e.executeImpl(ctx)
}

func (e *Extractor) executeImpl(ctx context.Context) error {
	// 并发合并：如果已有提取正在运行，则暂存本次调用，当前任务结束后再执行一次尾随提取。
	e.mu.Lock()
	if e.inProgress {
		e.deps.debugf("[extractMemories] extraction in progress — stashing for trailing run")
		e.pendingContext = &pendingExtractionCtx{}
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	return e.runExtraction(ctx, false)
}

func (e *Extractor) runExtraction(ctx context.Context, isTrailingRun bool) error {
	messages := e.deps.Conversation.GetMessages()
	newMessageCount := countModelVisibleMessagesSince(messages, e.lastMemoryMessageIdx)

	// 互斥：主 Agent 已自行写入记忆时，派生提取器没有必要运行，推进游标后返回。
	if hasMemoryWritesSince(messages, e.lastMemoryMessageIdx, e.deps.ProjectRoot) {
		e.deps.debugf("[extractMemories] skipping — conversation already wrote to memory files")
		e.advanceCursor(len(messages))
		return nil
	}

	// 节流：默认值为 1，即每轮运行；尾随任务处理已提交的工作，因此跳过节流。
	if !isTrailingRun {
		e.turnsSinceLastExtraction++
		if e.turnsSinceLastExtraction < 1 {
			return nil
		}
	}
	e.turnsSinceLastExtraction = 0

	e.mu.Lock()
	e.inProgress = true
	e.mu.Unlock()
	startTime := time.Now()

	defer func() {
		e.mu.Lock()
		e.inProgress = false
		trailing := e.pendingContext
		e.pendingContext = nil
		e.mu.Unlock()
		if trailing != nil {
			e.deps.debugf("[extractMemories] running trailing extraction for stashed context")
			_ = e.runExtraction(ctx, true)
		}
	}()

	e.deps.debugf("[extractMemories] starting — %d new messages, memoryDir=%s, userMemoryDir=%s",
		newMessageCount, e.deps.MemoryDir, e.deps.UserMemoryDir)

	// 预先注入记忆目录清单，避免提取 Agent 浪费一轮执行 ls；两个目录会合并为一份清单。
	var combinedScan []memory.MemoryHeader
	if e.deps.UserMemoryDir != "" {
		userScan, _ := memory.ScanMemoryFiles(ctx, e.deps.UserMemoryDir, "user")
		combinedScan = append(combinedScan, userScan...)
	}
	projectScan, _ := memory.ScanMemoryFiles(ctx, e.deps.MemoryDir, "project")
	combinedScan = append(combinedScan, projectScan...)
	manifest := memory.FormatMemoryManifest(combinedScan)
	extractionPrompt := BuildExtractAutoOnlyPrompt(newMessageCount, manifest, false, e.deps.UserMemoryDir, e.deps.MemoryDir)

	// 构建派生对话：复制父对话消息，再追加提取 Prompt 作为新的用户消息。
// 不添加 agents.runFork 的额外引导，因为提取器需要保持主对话的完整副本。
	forkedConv := buildExtractorConversation(e.deps.Conversation, extractionPrompt)

	// 工具白名单包括 ReadFile、WriteFile、EditFile、Glob、Grep、Bash、ToolSearch；
// Agent 和 AskUserQuestion 会自动排除。
	subRegistry := agents.FilterToolsForAgent(e.deps.ToolRegistry, nil, nil, true)

	// 严格的路径沙箱：文件工具只允许访问 memoryDir。这比原来的
	// createAutoMemCanUseTool 更严格（后者放任 Read/Grep/Glob 到处跑），
	// 但与 prompt 里明确警告不要 grep 源码一致，因此行为差异很小，
	// 安全性收益却很实在。
	//
	// 使用 ModeBypass，避免文件或命令工具进入需要人工确认的 Ask 状态；后台没有 TUI 可以回答。
	subChecker := memory.NewSubAgentChecker(e.deps.ProjectRoot, e.deps.UserMemoryDir)

	subAgent := agent.New(e.deps.Client, subRegistry, e.deps.Protocol)
	subAgent.MaxIterations = 5
	subAgent.Checker = subChecker
	subAgent.WorkDir = e.deps.ProjectRoot
	// 压缩阈值跟随 provider 的真实窗口，和通过 Agent 工具派发的子 Agent 一致。
	if e.deps.ContextWindow > 0 {
		subAgent.ContextWindow = e.deps.ContextWindow
	}
	if e.deps.MaxOutputTokens > 0 {
		subAgent.MaxOutputTokens = e.deps.MaxOutputTokens
	}

	// 驱动派生 Agent 执行到结束并排空事件通道；不展示流式文本，只关心文件写入。
	ch := subAgent.Run(ctx, forkedConv)
	for range ch {
		// 排空事件，不将子 Agent 事件转发到 UI。
	}

	// 只有任务完成后才推进游标；即使本轮没有选出可保存内容，也不应重复处理。
	e.advanceCursor(len(messages))

	writtenPaths := extractWrittenPaths(forkedConv.GetMessages())
	e.deps.debugf("[extractMemories] finished in %s, %d files written: %v",
		time.Since(startTime), len(writtenPaths), writtenPaths)

	// 索引文件 MEMORY.md 属于机械维护；用户看到的记忆是主题文件，而不是索引更新。
	var memoryPaths []string
	for _, p := range writtenPaths {
		if filepath.Base(p) == memory.AutoMemEntrypointName {
			continue
		}
		memoryPaths = append(memoryPaths, p)
	}

	if len(memoryPaths) > 0 && e.deps.AppendSystem != nil {
		var names []string
		for _, p := range memoryPaths {
			names = append(names, filepath.Base(p))
		}
		e.deps.AppendSystem(fmt.Sprintf("Memory saved: %s", strings.Join(names, ", ")))
	}

	return nil
}

// Drain 等待所有进行中的提取（包括待执行的尾随任务）完成，并提供软超时。
// TUI 关闭时调用它，避免派生提取 Agent 在写入中途被终止。
//
// timeoutMs 为 0 时，如果仍有任务进行则立即返回；负值按 60000 毫秒（默认 60 秒）处理。
func (e *Extractor) Drain(timeoutMs int) error {
	if e == nil {
		return nil
	}
	if timeoutMs < 0 {
		timeoutMs = 60000
	}

	e.mu.Lock()
	wgs := make([]*sync.WaitGroup, 0, len(e.inFlight))
	for wg := range e.inFlight {
		wgs = append(wgs, wg)
	}
	e.mu.Unlock()

	if len(wgs) == 0 {
		return nil
	}

	done := make(chan struct{})
	go func() {
		for _, wg := range wgs {
			wg.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
		return nil
	}
}

func (e *Extractor) advanceCursor(to int) {
	e.mu.Lock()
	if to > e.lastMemoryMessageIdx {
		e.lastMemoryMessageIdx = to
	}
	e.mu.Unlock()
}

// countModelVisibleMessagesSince 统计 sinceIdx 之后新增的 user/assistant 消息。回退路径是：
// sinceIdx 越界时会统计全部模型可见消息（例如对话压缩后原游标已不再对应当前位置）。
// 这是未找到起点时的恢复路径。
func countModelVisibleMessagesSince(messages []conversation.Message, sinceIdx int) int {
	if sinceIdx < 0 || sinceIdx > len(messages) {
		return countModelVisible(messages)
	}
	n := 0
	for _, m := range messages[sinceIdx:] {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func countModelVisible(messages []conversation.Message) int {
	n := 0
	for _, m := range messages {
		if isModelVisibleMessage(m) {
			n++
		}
	}
	return n
}

func isModelVisibleMessage(m conversation.Message) bool {
	return m.Role == "user" || m.Role == "assistant"
}

// hasMemoryWritesSince 检查 sinceIdx 之后的 Assistant 消息是否包含针对自动记忆路径的 Write/Edit 调用。
// 返回 true 时，runExtraction 会跳过派生 Agent。
func hasMemoryWritesSince(messages []conversation.Message, sinceIdx int, projectRoot string) bool {
	if sinceIdx < 0 {
		sinceIdx = 0
	}
	if sinceIdx >= len(messages) {
		return false
	}
	for _, m := range messages[sinceIdx:] {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if memory.IsAutoMemPath(fp, projectRoot) {
				return true
			}
		}
	}
	return false
}

// getWrittenFilePath 从 Write/Edit 工具调用块中提取 file_path 参数；如果不是这类调用则返回空字符串。
func getWrittenFilePath(tu conversation.ToolUseBlock) string {
	if tu.ToolName != "WriteFile" && tu.ToolName != "EditFile" {
		return ""
	}
	fp, ok := tu.Arguments["file_path"].(string)
	if !ok {
		return ""
	}
	return fp
}

// extractWrittenPaths 收集派生 Agent Assistant 消息中所有 Write/Edit 调用的唯一 file_path。
// 同一路径只保留第一次出现的位置。
func extractWrittenPaths(messages []conversation.Message) []string {
	var paths []string
	seen := make(map[string]struct{})
	for _, m := range messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tu := range m.ToolUses {
			fp := getWrittenFilePath(tu)
			if fp == "" {
				continue
			}
			if _, ok := seen[fp]; ok {
				continue
			}
			seen[fp] = struct{}{}
			paths = append(paths, fp)
		}
	}
	return paths
}

// buildExtractorConversation 将父对话消息复制到新的 Manager，并在末尾追加提取 Prompt 用户消息。
// 它不像 agents.buildForkedConversation 那样注入 ForkBoilerplateTag，因为这里需要的是主对话的完整副本。
func buildExtractorConversation(parent *conversation.Manager, prompt string) *conversation.Manager {
	forked := conversation.NewManager()
	for _, msg := range parent.GetMessages() {
		switch msg.Role {
		case "assistant":
			if len(msg.ToolUses) > 0 {
				forked.AddAssistantMessageWithTools(msg.Content, msg.ToolUses)
			} else {
				forked.AddAssistantMessage(msg.Content)
			}
		default:
			if len(msg.ToolResults) > 0 {
				forked.AddToolResultsMessage(msg.ToolResults)
			} else {
				forked.AddUserMessage(msg.Content)
			}
		}
	}
	forked.AddUserMessage(prompt)
	return forked
}

func (d Deps) debugf(format string, args ...any) {
	if d.DebugLogf != nil {
		d.DebugLogf(format, args...)
	}
}
