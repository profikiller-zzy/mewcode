package compact

import (
	"context"
	"strings"
	"testing"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/session"
)

// stubSummaryClient 实现了 llm.Client，会流式吐出一段固定的 <summary> 块，
// 这样 autoCompact 的摘要步骤在测试里是确定性的。它会记录下来被要求摘要的
// prompt，好让测试断言只有前缀部分（而不是保留的尾部）
// 被拿去摘要了。
type stubSummaryClient struct {
	summary      string
	lastPrompt   string
	allMessages  []conversation.Message
	streamCalled bool
}

func (c *stubSummaryClient) SetSystemPrompt(prompt string) {}

func (c *stubSummaryClient) Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	c.streamCalled = true
	msgs := conv.GetMessages()
	c.allMessages = msgs
	if len(msgs) > 0 {
		c.lastPrompt = msgs[len(msgs)-1].Content
	}
	ch := make(chan llm.StreamEvent, 4)
	errCh := make(chan error, 1)
	ch <- llm.TextDelta{Text: "<summary>" + c.summary + "</summary>"}
	ch <- llm.StreamEnd{StopReason: "end_turn"}
	close(ch)
	errCh <- nil
	close(errCh)
	return ch, errCh
}

// Layer 1（offload + snip）的测试已经挪到 internal/toolresult/budget_test.go，
// 实现现在在那边。compact 只负责 Layer 2（autoCompact）
// 以及 formatCompactSummary 这个辅助函数，所以本文件只覆盖这两块。

func TestFormatCompactSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "both blocks present",
			in:   "<analysis>scratch thoughts</analysis>\n<summary>final text</summary>",
			want: "final text",
		},
		{
			name: "summary block unterminated",
			in:   "<analysis>scratch</analysis>\n<summary>tail with no close tag",
			want: "tail with no close tag",
		},
		{
			name: "only analysis block — drop it",
			in:   "prefix <analysis>scratch</analysis> suffix",
			want: "prefix  suffix",
		},
		{
			name: "neither block — return raw",
			in:   "plain text response",
			want: "plain text response",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatCompactSummary(tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// EstimateTokens 覆盖所有内容来源，空输入时也不崩。
func TestEstimateTokensZeroAndPopulated(t *testing.T) {
	if got := EstimateTokens(nil); got != 0 {
		t.Errorf("empty input should be 0 tokens, got %d", got)
	}
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	got := EstimateTokens(conv.GetMessages())
	if got < 150 || got > 250 {
		t.Errorf("700-char message should estimate ~200 tokens, got %d", got)
	}
}

// BaselineFromUsage 必须把四个真实 token 计数器全部加起来，这样即使 cache 命中
// 占了大头（input 很小、cache_read 很大），锚点也能反映真实的
// prompt + output 规模。
func TestBaselineFromUsage(t *testing.T) {
	u := llm.UsageInfo{
		InputTokens:         100,
		OutputTokens:        40,
		CacheReadTokens:     5000,
		CacheCreationTokens: 200,
	}
	if got, want := BaselineFromUsage(u), 5340; got != want {
		t.Errorf("BaselineFromUsage = %d, want %d", got, want)
	}
	// usage 全为 0（什么都没上报的兼容端点）→ baseline 也是 0，
	// 好让调用方知道不能把它当锚点用。
	if got := BaselineFromUsage(llm.UsageInfo{}); got != 0 {
		t.Errorf("empty usage baseline = %d, want 0", got)
	}
}

// ComputeUsedTokens：没有锚点时（冷启动 / 第一轮），必须回退成
// 对每条消息做全量字符估算，结果与 EstimateTokens 一致。
func TestComputeUsedTokensColdStartFallback(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage(strings.Repeat("x", 700))
	conv.AddAssistantMessage(strings.Repeat("y", 700))
	msgs := conv.GetMessages()

	got := ComputeUsedTokens(msgs, UsageAnchor{}) // HasUsage == false
	want := EstimateTokens(msgs)
	if got != want {
		t.Errorf("cold-start ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// ComputeUsedTokens：有锚点时，必须返回 baseline 加上「只对
// anchorCount 之后新增的那部分消息」的估算 —— 而不是把整个对话重新估一遍。
// 这正是 cache 命中带来的收益：真实 input 远小于
// 被锚定的那段前缀的字符数。
func TestComputeUsedTokensWithAnchorIncremental(t *testing.T) {
	conv := conversation.NewManager()
	// 3 条被锚定的大消息：它们真实的 token 开销由 baseline 记录，
	// 而不是按字符数算。
	conv.AddUserMessage(strings.Repeat("x", 7000))
	conv.AddAssistantMessage(strings.Repeat("y", 7000))
	conv.AddUserMessage(strings.Repeat("z", 7000))
	anchorCount := conv.Len()
	// 锚点之后追加的一条小消息。
	conv.AddAssistantMessage(strings.Repeat("w", 350))
	msgs := conv.GetMessages()

	const baseline = 1500 // 假设真实 API 报告这段前缀花了 1500 个 token
	anchor := UsageAnchor{BaselineTokens: baseline, AnchorCount: anchorCount, HasUsage: true}

	got := ComputeUsedTokens(msgs, anchor)
	wantIncrement := EstimateTokens(msgs[anchorCount:])
	if got != baseline+wantIncrement {
		t.Errorf("anchored ComputeUsedTokens = %d, want baseline+increment %d", got, baseline+wantIncrement)
	}
	// 合理性检查：增量结果必须远低于对这份（cache 占比很高的）对话
	// 做全量字符估算的结果，以此证明我们没有把前缀重新估算一遍。
	if full := EstimateTokens(msgs); got >= full {
		t.Errorf("anchored result %d should be below full estimate %d", got, full)
	}
}

// ComputeUsedTokens：锚点过期时（AnchorCount 超过了当前消息数，
// 例如压缩把对话回退之后），不能 panic，应当
// 回退到全量估算。
func TestComputeUsedTokensStaleAnchorClamp(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("hi")
	msgs := conv.GetMessages()

	anchor := UsageAnchor{BaselineTokens: 9999, AnchorCount: 50, HasUsage: true}
	got := ComputeUsedTokens(msgs, anchor)
	if want := EstimateTokens(msgs); got != want {
		t.Errorf("stale-anchor ComputeUsedTokens = %d, want full estimate %d", got, want)
	}
}

// bigMsg 返回一条内容本身就大约能估算出 `tokens` 个 token 的消息
// （recoveryCharsPerToken ≈ 3.5 字符/token），这样测试就能确定性地驱动
// keepRecentTokens 的预算遍历。
func bigMsg(tokens int) string {
	return strings.Repeat("x", tokens*4)
}

// containsMsg 报告 msgs 里是否存在内容等于 want 的消息。
func containsMsg(msgs []conversation.Message, want string) bool {
	for _, m := range msgs {
		if m.Content == want {
			return true
		}
	}
	return false
}

// autoCompact 必须原样保留最近的尾部，而不是用摘要把它替掉。
// 我们构造一份对话，较旧的前缀大到足以让
// keepStart > 0，尾部则带着有辨识度的内容；压缩之后
// 尾部内容必须还在（不能只剩下摘要）。
func TestAutoCompactKeepsRecentVerbatim(t *testing.T) {
	conv := conversation.NewManager()
	// 较旧的前缀：几条应该被摘要掉的大消息。
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	// 最近的尾部：期望能原样存活下来的、有辨识度的小消息。
	recent := []string{"RECENT-A unique-marker-A", "RECENT-B unique-marker-B"}
	conv.AddUserMessage(recent[0])
	conv.AddAssistantMessage(recent[1])

	client := &stubSummaryClient{summary: "THE SUMMARY"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}
	out := conv.GetMessages()

	// 摘要必须存在。
	var sawSummary bool
	for _, m := range out {
		if strings.Contains(m.Content, "THE SUMMARY") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Errorf("summary not present after compaction")
	}
	// 最近的尾部必须原样保留，而不是被折叠进摘要里。
	for _, r := range recent {
		if !containsMsg(out, r) {
			t.Errorf("recent message %q not preserved verbatim after compaction; messages=%v", r, msgContents(out))
		}
	}
}

// 当 sessionID + workDir 都接上时，autoCompact 必须往 session 日志里
// 持久化一条 compact_boundary 记录：内联的摘要加上
// 保留的尾部（role+content）。这是恢复往返过程中落盘的那一半 ——
// 之后 session.FindLastCompactBoundary 会据此重建压缩后的状态。
func TestAutoCompactPersistsBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-TAIL-USER unique-marker-A")
	conv.AddAssistantMessage("RECENT-TAIL-ASSISTANT unique-marker-B")

	workDir := t.TempDir()
	sid := "compact-roundtrip"
	client := &stubSummaryClient{summary: "PERSISTED-SUMMARY"}

	msg, err := autoCompact(context.Background(), conv, client, workDir, sid, 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg == "" {
		t.Fatalf("expected a compaction message, got empty (degraded to no-op)")
	}

	// 把 session 日志读回来，断言写入的 boundary 里
	// 内联了摘要和保留的尾部。
	msgs := session.LoadSession(workDir, sid)
	boundary, after, ok := session.FindLastCompactBoundary(msgs)
	if !ok {
		t.Fatalf("expected a compact_boundary record to be persisted")
	}
	if boundary.Summary != "PERSISTED-SUMMARY" {
		t.Fatalf("persisted summary mismatch: got %q", boundary.Summary)
	}
	if len(after) != 0 {
		t.Fatalf("no messages should follow a freshly written boundary, got %d", len(after))
	}
	// 保留的尾部必须原样内联进 boundary。
	var sawTailUser, sawTailAssistant bool
	for _, k := range boundary.Keep {
		if k.Content == "RECENT-TAIL-USER unique-marker-A" {
			sawTailUser = true
		}
		if k.Content == "RECENT-TAIL-ASSISTANT unique-marker-B" {
			sawTailAssistant = true
		}
	}
	if !sawTailUser || !sawTailAssistant {
		t.Fatalf("kept tail not inlined into boundary: %+v", boundary.Keep)
	}
	// boundary 里保留的尾部必须和 autoCompact 原样保留下来的那条对话尾部
	// 完全相等（role+content 相同，顺序也相同）。磁盘上存的
	// 摘要是纯摘要文本，而内存重建之后对话变成
	// [summary user msg] + [continuation ack] + keep，所以
	// 保留的尾部位于重建后对话的末尾。
	rebuilt := conv.GetMessages()
	tail := rebuilt[len(rebuilt)-len(boundary.Keep):]
	for i, k := range boundary.Keep {
		if tail[i].Role != k.Role || tail[i].Content != k.Content {
			t.Fatalf("boundary keep[%d]=%+v does not match in-memory tail %+v", i, k, tail[i])
		}
	}
}

// 没有 sessionID/workDir 时，autoCompact 绝对不能碰任何 session 日志
// （一次性调用方、测试、sub-agent）—— 行为和以前保持一致。
func TestAutoCompactNoSessionNoBoundary(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("OLD-PREFIX " + bigMsg(3000))
		conv.AddAssistantMessage("OLD-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-A")
	conv.AddAssistantMessage("RECENT-B")

	workDir := t.TempDir()
	client := &stubSummaryClient{summary: "S"}
	// sessionID 为空 → 不做持久化。
	if _, err := autoCompact(context.Background(), conv, client, workDir, "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	// 任何 id 下都不应该生成 session 文件。
	msgs := session.LoadSession(workDir, "anything")
	if len(msgs) != 0 {
		t.Fatalf("expected no session log written when sessionID empty, got %d", len(msgs))
	}
}

// autoCompact 只能对 messages[:keepStart] 做摘要；保留的尾部
// 绝对不能出现在交给摘要器的 prompt 里。
func TestAutoCompactCacheSharingUsesOriginalMessages(t *testing.T) {
	conv := conversation.NewManager()
	for i := 0; i < 6; i++ {
		conv.AddUserMessage("PREFIX-CONTENT " + bigMsg(3000))
		conv.AddAssistantMessage("PREFIX-REPLY " + bigMsg(3000))
	}
	conv.AddUserMessage("RECENT-MARKER")
	conv.AddAssistantMessage("RECENT-REPLY")

	client := &stubSummaryClient{summary: "S"}
	if _, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil); err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if !client.streamCalled {
		t.Fatalf("summarizer was never called")
	}
	// Cache-sharing 路径下，摘要调用复用原始消息（不序列化成文本），
	// 最后一条消息是摘要指令
	lastMsg := client.allMessages[len(client.allMessages)-1]
	if !strings.Contains(lastMsg.Content, "summary") {
		t.Errorf("last message should be the summary prompt, got: %s", lastMsg.Content[:100])
	}
	// 消息列表应包含 prefix 内容（cache-sharing 的核心：原始消息不动）
	allContent := ""
	for _, m := range client.allMessages {
		allContent += m.Content + " "
	}
	if !strings.Contains(allContent, "PREFIX-CONTENT") {
		t.Errorf("summary messages must include prefix content for cache sharing")
	}
}

// computeKeepStartIndex 绝不能把 tool_use ↔ tool_result 这一对拆开：
// 如果预算边界正好落在携带 tool_results 的那条 user 消息上，它必须
// 往回退，把产生这些结果的 assistant tool_use 消息也包含进来。
func TestComputeKeepStartIndexDoesNotSplitToolPair(t *testing.T) {
	conv := conversation.NewManager()
	// 放一段大前缀，好让 keepStart > 0。
	for i := 0; i < 8; i++ {
		conv.AddUserMessage(bigMsg(3000))
		conv.AddAssistantMessage(bigMsg(3000))
	}
	// 靠近尾部的一对 tool_use / tool_result。tool_result 是一条大消息，
	// 所以预算边界很可能正好落在它上面。
	conv.AddToolUseMessage("calling tool", "tu-1", "ReadFile", map[string]any{"path": "/x"})
	conv.AddToolResultMessage("tu-1", bigMsg(9000), false)
	msgs := conv.GetMessages()

	keepStart := computeKeepStartIndex(msgs)
	if keepStart <= 0 || keepStart >= len(msgs) {
		t.Fatalf("keepStart=%d out of expected range (0, %d)", keepStart, len(msgs))
	}
	// 边界消息不能是一条孤零零的 tool_result，
	// 而它对应的 tool_use 却被留在了被摘要的前缀里。
	if hasToolResults(msgs[keepStart]) {
		t.Fatalf("keepStart landed on a tool_result message (orphaned); keepStart=%d", keepStart)
	}
	// 验证这一对在保留的尾部里是完整的：遍历一遍，确保每个
	// tool_result 在保留切片内都有一个在它之前的 tool_use。
	keep := msgs[keepStart:]
	openUses := map[string]bool{}
	for _, m := range keep {
		for _, tu := range m.ToolUses {
			openUses[tu.ToolUseID] = true
		}
		for _, tr := range m.ToolResults {
			if !openUses[tr.ToolUseID] {
				t.Errorf("tool_result %s in kept tail has no matching tool_use in tail (pair split)", tr.ToolUseID)
			}
		}
	}
}

func TestComputeKeepStartIndexDoesNotSplitAgentRun(t *testing.T) {
	var messages []conversation.Message
	appendRun := func(runID string, runMessages ...conversation.Message) {
		for _, message := range runMessages {
			message.RunID = runID
			messages = append(messages, message)
		}
	}
	appendRun("old", conversation.Message{Role: "user", Content: bigMsg(30000)}, conversation.Message{Role: "assistant", Content: "old answer"})
	appendRun("tool-run",
		conversation.Message{Role: "user", Content: "read a file"},
		conversation.Message{Role: "assistant", ToolUses: []conversation.ToolUseBlock{{ToolUseID: "t1", ToolName: "ReadFile"}}},
		conversation.Message{Role: "user", ToolResults: []conversation.ToolResultBlock{{ToolUseID: "t1", Content: bigMsg(6000)}}},
		conversation.Message{Role: "assistant", Content: "file read complete"},
	)
	appendRun("latest", conversation.Message{Role: "user", Content: "latest"}, conversation.Message{Role: "assistant", Content: "latest answer"})

	keepStart := computeKeepStartIndex(messages)
	if keepStart <= 0 || keepStart >= len(messages) {
		t.Fatalf("keepStart=%d out of expected range", keepStart)
	}
	prefixIDs := map[string]bool{}
	for _, message := range messages[:keepStart] {
		prefixIDs[message.RunID] = true
	}
	for _, message := range messages[keepStart:] {
		if prefixIDs[message.RunID] {
			t.Fatalf("run %q was split by compaction boundary %d", message.RunID, keepStart)
		}
	}
	if messages[keepStart].RunID != "tool-run" {
		t.Fatalf("expected whole tool-run in kept tail, got %q", messages[keepStart].RunID)
	}
}

// 当对话太短、没有可摘要的前缀时（keepStart
// <= 0），autoCompact 必须降级成 no-op：不做摘要，
// 对话保持原样。
func TestAutoCompactDegradesWhenTooFewMessages(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserMessage("just one")
	conv.AddAssistantMessage("two")
	before := conv.GetMessages()

	client := &stubSummaryClient{summary: "S"}
	msg, err := autoCompact(context.Background(), conv, client, "", "", 200000, nil, nil)
	if err != nil {
		t.Fatalf("autoCompact error: %v", err)
	}
	if msg != "" {
		t.Errorf("expected no-op (empty message) for too-few messages, got %q", msg)
	}
	if client.streamCalled {
		t.Errorf("summarizer should not be called when degrading to no-op")
	}
	after := conv.GetMessages()
	if len(before) != len(after) {
		t.Errorf("conversation changed during no-op: before=%d after=%d", len(before), len(after))
	}
	for i := range before {
		if before[i].Content != after[i].Content {
			t.Errorf("message %d mutated during no-op", i)
		}
	}
}

func msgContents(msgs []conversation.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		c := m.Content
		if len(c) > 40 {
			c = c[:40] + "..."
		}
		out[i] = c
	}
	return out
}
