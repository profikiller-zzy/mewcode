package agents

import (
	"context"
	"errors"
	"strings"
	"testing"

	"mewcode/internal/agent"
	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/permissions"
	"mewcode/internal/teams"
	"mewcode/internal/tools"
)

func TestBuildForkedConversationPreservesThinkingBlocks(t *testing.T) {
	// 逐字节重放：带 thinking blocks 的 assistant 消息，在 fork 出来的对话里
	// 必须带着同样的 thinking blocks 复现，否则 API 请求的前缀就会分叉，
	// prompt cache 也会失效。
	parent := conversation.NewManager()
	thinking := []conversation.ThinkingBlock{{Thinking: "secret plan", Signature: "sig-1"}}
	parent.AddAssistantFull("hello", thinking, []conversation.ToolUseBlock{
		{ToolUseID: "tool_1", ToolName: "Bash", Arguments: map[string]any{"command": "ls"}},
	})

	forked := buildForkedConversation(parent, "do work")
	msgs := forked.GetMessages()
	var found *conversation.Message
	for i := range msgs {
		if len(msgs[i].ThinkingBlocks) > 0 {
			found = &msgs[i]
			break
		}
	}
	if found == nil {
		t.Fatal("forked conversation lost thinking blocks")
	}
	if found.ThinkingBlocks[0].Thinking != "secret plan" || found.ThinkingBlocks[0].Signature != "sig-1" {
		t.Errorf("thinking blocks not preserved verbatim: %+v", found.ThinkingBlocks)
	}
}

func TestDeriveSubAgentCheckerOverrideMode(t *testing.T) {
	// spec.PermissionMode 必须产出一个 Checker：沿用父级的 Sandbox / RuleEngine，
	// 但把 Mode 换掉。override 为空 → 原样返回。
	sb := permissions.NewPathSandbox("/tmp", "")
	eng := &permissions.RuleEngine{}
	parent := permissions.NewChecker(sb, eng, permissions.ModeDefault)

	if derived := deriveSubAgentChecker(parent, ""); derived != parent {
		t.Error("empty override should return parent unchanged")
	}
	derived := deriveSubAgentChecker(parent, "plan")
	if derived == parent {
		t.Fatal("plan override should produce a new checker instance")
	}
	if derived.Mode != permissions.ModePlan {
		t.Errorf("derived.Mode = %q, want plan", derived.Mode)
	}
	if derived.Sandbox != sb || derived.RuleEngine != eng {
		t.Error("derived checker should share parent's Sandbox + RuleEngine")
	}
	if deriveSubAgentChecker(nil, "plan") != nil {
		t.Error("nil parent should propagate nil")
	}
}

func TestRunForkRejectedWhenQuerySourceIsFork(t *testing.T) {
	// 嵌套 fork 的首道防线：直接看 QuerySource 标记。
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conversation.NewManager(),
		QuerySource:  ForkQuerySource,
	}
	result := tool.runFork(context.Background(), "desc", "do work", "")
	if !result.IsError {
		t.Fatal("runFork should reject when QuerySource is fork")
	}
	if !strings.Contains(result.Output, "cannot fork from a forked agent") {
		t.Errorf("unexpected error message: %s", result.Output)
	}
}

func TestRunForkRejectedWhenBoilerplateInHistory(t *testing.T) {
	// 嵌套 fork 的兜底防线：QuerySource 没传到时，扫对话历史里的 fork 样板标记。
	conv := conversation.NewManager()
	conv.AddUserMessage(ForkBoilerplateTag + " stale message from a prior fork")
	tool := &AgentTool{
		Registry:     tools.NewRegistry(),
		Conversation: conv,
	}
	result := tool.runFork(context.Background(), "desc", "do work", "")
	if !result.IsError {
		t.Fatal("runFork should reject when conversation history contains ForkBoilerplateTag")
	}
}

func TestCloneRegistryForForkSetsQuerySource(t *testing.T) {
	// fork 必须原样继承父工具池，只把其中的 Agent 工具换成带 QuerySource=ForkQuerySource
	// 的拷贝，这样再往下 fork 会在调用那一刻被拦住。
	reg := tools.NewRegistry()
	reg.Register(&AgentTool{}) // 模拟父级的 Agent 工具
	reg.Register(&dummyTool{name: "Bash", category: tools.CategoryCommand})

	forked := cloneRegistryForFork(reg)
	if forked.Get("Bash") == nil {
		t.Error("Bash should still be present in forked registry")
	}
	at, ok := forked.Get("Agent").(*AgentTool)
	if !ok {
		t.Fatal("Agent tool should still be present (tool pool inherited verbatim)")
	}
	if at.QuerySource != ForkQuerySource {
		t.Errorf("cloned Agent tool QuerySource = %q, want %q", at.QuerySource, ForkQuerySource)
	}
}

func TestExecuteRejectsUnknownAgentType(t *testing.T) {
	// 未知 subagent_type 必须报错并列出可用角色，而不是静默回退。
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
		Protocol: "anthropic",
	}
	result := tool.Execute(context.Background(), map[string]any{
		"description":   "verify run",
		"prompt":        "do it",
		"subagent_type": "background-only",
	})
	if !result.IsError || !strings.Contains(result.Output, "unknown agent type") {
		t.Errorf("expected unknown-agent-type error, got %q", result.Output)
	}
}

// IsConcurrencySafe 是 fan-out 的关键开关：普通子 Agent 必须判为并发安全，
// 同一轮里连续派出的多个调用才会被归入同一并发批次；teammate 有注册副作用，
// 必须保持串行。
func TestAgentToolIsConcurrencySafe(t *testing.T) {
	plain := &AgentTool{}
	if !plain.IsConcurrencySafe(map[string]any{"description": "x", "prompt": "y"}) {
		t.Error("plain sub-agent spawn should be concurrency-safe")
	}

	withTeams := &AgentTool{TeamMgr: teams.NewTeamManager()}
	if withTeams.IsConcurrencySafe(map[string]any{"team_name": "squad"}) {
		t.Error("teammate spawn must stay serial")
	}
	// 没有 team_name 时不会走 teammate 路径，仍按同步子 Agent 处理。
	if !withTeams.IsConcurrencySafe(map[string]any{"description": "x"}) {
		t.Error("spawn without team_name should be concurrency-safe")
	}
}

func TestAgentToolSchemaDescribesDispatchModes(t *testing.T) {
	tool := &AgentTool{}
	description := tool.Description()
	for _, want := range []string{
		"Omit \"subagent_type\" to fork a snapshot",
		"Set \"subagent_type\" to start a fresh-context agent",
		"Ordinary sub-agent calls are synchronous",
		"long-running team teammate",
	} {
		if !strings.Contains(description, want) {
			t.Errorf("Agent description missing %q: %s", want, description)
		}
	}

	schema := tool.Schema()["input_schema"].(map[string]any)["properties"].(map[string]any)
	propertyDescription := func(name string) string {
		return schema[name].(map[string]any)["description"].(string)
	}
	if got := propertyDescription("description"); !strings.Contains(got, "not the task instructions") {
		t.Errorf("description parameter is misleading: %q", got)
	}
	if got := propertyDescription("prompt"); !strings.Contains(got, "fork") || !strings.Contains(got, "fresh-context") {
		t.Errorf("prompt parameter does not explain both context modes: %q", got)
	}
	if got := propertyDescription("team_name"); !strings.Contains(got, "created automatically") {
		t.Errorf("team_name parameter does not describe automatic team creation: %q", got)
	}
}

// 子 Agent 的 Layer 2 压缩阈值按 provider 的真实窗口换算。不透传时 agent.New
// 写死的 200000 会让小窗口模型压缩来不及触发、大窗口模型被过早压缩。
func TestApplyProviderLimits(t *testing.T) {
	sub := agent.New(&fakeLLMClient{}, tools.NewRegistry(), "anthropic")
	if sub.ContextWindow != 200000 {
		t.Fatalf("baseline should be the agent.New default, got %d", sub.ContextWindow)
	}

	tool := &AgentTool{ContextWindow: 128000, MaxOutputTokens: 8192}
	tool.applyProviderLimits(sub)
	if sub.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", sub.ContextWindow)
	}
	if sub.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d, want 8192", sub.MaxOutputTokens)
	}

	// 未配置（0）时不覆盖 agent.New 的默认值。
	fresh := agent.New(&fakeLLMClient{}, tools.NewRegistry(), "anthropic")
	(&AgentTool{}).applyProviderLimits(fresh)
	if fresh.ContextWindow != 200000 {
		t.Errorf("zero limits must not clobber the default window, got %d", fresh.ContextWindow)
	}
	if fresh.MaxOutputTokens != 0 {
		t.Errorf("zero limits must not clobber the default output budget, got %d", fresh.MaxOutputTokens)
	}
}

func TestSelectClientFallsBackWhenAliasUnresolvable(t *testing.T) {
	// 档位名解析失败（例如非 Claude provider 拿到 haiku）时必须回退到主 Agent
	// 的 client：宁可让子 Agent 用主模型跑，也不能把请求发给一个端点不存在的
	// 模型 —— 那要等发请求才报错，排查成本高得多。
	parent := &fakeLLMClient{reply: "x"}
	tool := &AgentTool{
		Client: parent,
		ModelResolver: func(string) (llm.Client, error) {
			return nil, errors.New("alias not resolvable for this provider")
		},
	}
	if got := tool.selectClient("haiku", ""); got != parent {
		t.Error("unresolvable alias must fall back to the parent client")
	}

	alt := &fakeLLMClient{reply: "y"}
	tool.ModelResolver = func(string) (llm.Client, error) { return alt, nil }
	if got := tool.selectClient("haiku", ""); got != alt {
		t.Error("resolvable alias should use the resolved client")
	}

	// 模型为空 / inherit 直接走父 client，不咨询 resolver。
	tool.ModelResolver = func(string) (llm.Client, error) {
		t.Error("resolver must not be consulted for empty/inherit")
		return nil, nil
	}
	if got := tool.selectClient("", ""); got != parent {
		t.Error("empty model should return the parent client")
	}
	if got := tool.selectClient("inherit", ""); got != parent {
		t.Error("inherit should return the parent client")
	}
}

func TestExecuteValidatesMode(t *testing.T) {
	tool := &AgentTool{
		Registry: tools.NewRegistry(),
	}

	badMode := tool.Execute(context.Background(), map[string]any{
		"description": "x", "prompt": "y",
		"mode": "not-a-real-mode",
	})
	if !badMode.IsError || !strings.Contains(badMode.Output, "invalid mode") {
		t.Errorf("invalid mode must be rejected, got %q", badMode.Output)
	}
}
