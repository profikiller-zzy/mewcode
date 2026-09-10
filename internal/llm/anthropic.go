package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"mewcode/internal/config"
	"mewcode/internal/conversation"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const anthropicStreamIdleTimeout = 5 * time.Minute

// nativeToolSearchBeta 开启 defer_loading 和 tool_reference。跟
// mcp.NativeToolSearchBeta 是同一个值，这里单独定义以免 llm 反向依赖 mcp。
const nativeToolSearchBeta = "advanced-tool-use-2025-11-20"

// markToolsForCache 把缓存断点打在最后一个非延迟工具上。
//
// tool schema 在多轮之间稳定，标记尾部就能把整个工具块缓存下来，几乎是免费的。但
// 断点不能落在带 defer_loading 的工具上：一个工具同时带 defer_loading 和
// cache_control 会被官方端点直接拒掉整个请求。MCP 工具在内建工具之后注册，排序后
// 尾部往往正是延迟工具，所以必须从尾部往前找。内建工具永远不延迟，总能找到落点。
func markToolsForCache(sdkTools []anthropic.ToolUnionParam) {
	for i := len(sdkTools) - 1; i >= 0; i-- {
		t := sdkTools[i].OfTool
		if t == nil || t.DeferLoading.Valid() {
			continue
		}
		t.CacheControl = anthropic.NewCacheControlEphemeralParam()
		return
	}
}

// needsToolSearchBeta 判断这批工具里有没有带 defer_loading 的。
//
// 只在真用到时才发这个 beta header：不认识它的端点收到会直接拒请求，而
// dispatch / eager 两条路压根不需要它。
func needsToolSearchBeta(toolSchemas []map[string]any) bool {
	for _, s := range toolSchemas {
		if deferLoading, _ := s["defer_loading"].(bool); deferLoading {
			return true
		}
	}
	return false
}

// supportsAdaptiveThinking 用前缀匹配 + 版本号检查来判断模型是否支持自适应思考
func supportsAdaptiveThinking(model string) bool {
	// 例如 claude-opus-4-6、claude-opus-4-7、claude-sonnet-4-6 等。
	// 但不包括 claude-sonnet-4-5（4.5 用的是 enabled 模式）
	for _, family := range []string{"claude-opus-4-", "claude-sonnet-4-"} {
		if strings.HasPrefix(model, family) {
			rest := model[len(family):]
			if len(rest) > 0 && rest[0] >= '6' && rest[0] <= '9' {
				return true
			}
		}
	}
	return false
}

type anthropicClient struct {
	client       anthropic.Client
	model        string
	thinking     bool
	systemPrompt string
	// maxOutputTokens 用 atomic 而不是普通 int：client 实例被主 Agent 与多个并发
	// 子 Agent 共享，主循环的 max_tokens 升级会写它，每个 Stream 各自读它，
	// 并发访问是常态（同步并发化的 sub-agent 全都指向同一个 client）。
	maxOutputTokens atomic.Int64
	contextWindow   int
}

func newAnthropicClient(cfg *config.ProviderConfig, systemPrompt string) (*anthropicClient, error) {
	apiKey := cfg.ResolveAPIKey()
	if apiKey == "" {
		return nil, &AuthenticationError{
			Message: "Anthropic API key not found. Set it in .mewcode/config.yaml or via ANTHROPIC_API_KEY env var.",
		}
	}

	client := anthropic.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(cfg.BaseURL),
	)

	c := &anthropicClient{
		client:        client,
		model:         cfg.Model,
		thinking:      cfg.Thinking,
		systemPrompt:  systemPrompt,
		contextWindow: cfg.GetContextWindow(),
	}
	c.maxOutputTokens.Store(int64(cfg.GetMaxOutputTokens()))
	return c, nil
}

func (c *anthropicClient) SetSystemPrompt(prompt string) {
	c.systemPrompt = prompt
}

func (c *anthropicClient) SetMaxOutputTokens(tokens int) {
	c.maxOutputTokens.Store(int64(tokens))
}

// anthropicModelFetchTimeout 限制自动拉取模型元数据的耗时，这样缓慢
// 或不可达的端点永远不会拖慢启动。
const anthropicModelFetchTimeout = 3 * time.Second

// FetchModelContextWindow 向兼容 Anthropic 的 /v1/models/{model}
// 端点查询模型的 max_input_tokens。它是尽力而为的：任何错误
// （非 anthropic 端点、网络失败、超时、字段缺失）都返回
// 0，既不 panic，也不会阻塞超过 anthropicModelFetchTimeout。调用方
// 把 0 当作「未知」，回退到下一层 context window 获取方式。
func (c *anthropicClient) FetchModelContextWindow(ctx context.Context) (window int) {
	// 硬性保护：这段代码在启动时执行，所以 SDK 里 panic 或响应畸形
	// 都必须静默降级，而不是把整个进程搞崩。
	defer func() {
		if recover() != nil {
			window = 0
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, anthropicModelFetchTimeout)
	defer cancel()

	// 启动时的尽力而为调用：关掉重试，这样不稳定的端点会在超时内
	// 快速失败，而不是引发重试风暴。
	info, err := c.client.Models.Get(ctx, c.model, anthropic.ModelGetParams{}, option.WithMaxRetries(0))
	if err != nil || info == nil || info.MaxInputTokens <= 0 {
		return 0
	}
	return int(info.MaxInputTokens)
}

func (c *anthropicClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan StreamEvent, <-chan error) {
	events := make(chan StreamEvent, 64)
	errs := make(chan error, 1)

	// 发请求前补齐工具调用与结果的配对：中断、恢复会话、并发交错都可能留下
	// 悬空的 tool_use，缺配对会被 API 直接拒掉。
	msgs := buildAnthropicMessages(conversation.EnsureToolPairing(conv.GetMessages()))

	var sdkTools []anthropic.ToolUnionParam
	// 带 defer_loading 的工具留在 tools[] 里但服务端不给模型看，模型要先用
	// ToolSearch 拿 tool_reference 才能调。这个字段需要 beta header 才被接受。
	sendToolSearchBeta := needsToolSearchBeta(toolSchemas)
	for _, s := range toolSchemas {
		inputSchema, _ := s["input_schema"].(map[string]any)
		props, _ := inputSchema["properties"]
		required, _ := inputSchema["required"].([]string)
		desc, _ := s["description"].(string)
		tool := &anthropic.ToolParam{
			Name:        s["name"].(string),
			Description: param.NewOpt(desc),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: props,
				Required:   required,
			},
		}
		if deferLoading, _ := s["defer_loading"].(bool); deferLoading {
			tool.DeferLoading = param.NewOpt(true)
		}
		sdkTools = append(sdkTools, anthropic.ToolUnionParam{OfTool: tool})
	}

	go func() {
		defer close(events)
		defer close(errs)

		maxTokens := c.maxOutputTokens.Load()
		// 把 prompt cache 的锚点放在最稳定的前缀上：system
		// prompt。这里标记一次，工具列表上再标记一次，下面
		// 最后一条 user 消息的尾部也标记一次 —— Anthropic 会缓存到
		// 每个断点为止，并在下一次请求时重新校验字节是否一致。
		// 断点之后的 tool_result 内容保持字节稳定，
		// 因为 toolresult 预算在消息进入时就已定型，
		// 之后不会再重写历史。
		params := anthropic.MessageNewParams{
			Model:     c.model,
			MaxTokens: maxTokens,
			System: []anthropic.TextBlockParam{{
				Text:         c.systemPrompt,
				CacheControl: anthropic.NewCacheControlEphemeralParam(),
			}},
			Messages: msgs,
		}
		markLastUserTailForCache(params.Messages)
		if c.thinking {
			if supportsAdaptiveThinking(c.model) {
				params.Thinking = anthropic.ThinkingConfigParamUnion{
					OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{},
				}
			} else {
				params.Thinking = anthropic.ThinkingConfigParamUnion{
					OfEnabled: &anthropic.ThinkingConfigEnabledParam{
						BudgetTokens: maxTokens - 1,
					},
				}
			}
		}
		if len(sdkTools) > 0 {
			markToolsForCache(sdkTools)
			params.Tools = sdkTools
		}

		var reqOpts []option.RequestOption
		if sendToolSearchBeta {
			reqOpts = append(reqOpts, option.WithHeaderAdd("anthropic-beta", nativeToolSearchBeta))
		}

		stream := c.client.Messages.NewStreaming(ctx, params, reqOpts...)
		defer stream.Close()

		var currentToolName, currentToolID, jsonAccum string
		var thinkingAccum, thinkingSignature string
		inThinking := false
		var accMessage anthropic.Message

		// 在单独的 goroutine 里读 SSE 事件，这样既能响应 ctx 取消，
		// 也能发现连接静默断开。底层连接在没发 FIN/RST 就断了的话，
		// SDK 的 stream.Next() 可能会一直阻塞。
		type sseResult struct {
			hasNext bool
		}
		nextCh := make(chan sseResult, 1)

		readNext := func() {
			nextCh <- sseResult{hasNext: stream.Next()}
		}

		idle := time.NewTimer(anthropicStreamIdleTimeout)
		defer idle.Stop()

		go readNext()
		for {
			var res sseResult
			select {
			case <-ctx.Done():
				errs <- &NetworkError{Message: fmt.Sprintf("context cancelled: %v", ctx.Err())}
				return
			case <-idle.C:
				errs <- &NetworkError{Message: fmt.Sprintf("stream idle timeout: no SSE events for %s", anthropicStreamIdleTimeout)}
				return
			case res = <-nextCh:
			}

			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(anthropicStreamIdleTimeout)

			if !res.hasNext {
				break
			}

			event := stream.Current()
			accMessage.Accumulate(event)
			// Anthropic SDK 的 Accumulate 只从 message_delta 里拷贝
			// OutputTokens，但有些 provider（MiniMax）也会在那里上报
			// InputTokens 和 cache 相关字段。这里手动补上。
			if mde, ok := event.AsAny().(anthropic.MessageDeltaEvent); ok {
				if mde.Usage.InputTokens > 0 {
					accMessage.Usage.InputTokens = mde.Usage.InputTokens
				}
				if mde.Usage.CacheReadInputTokens > 0 {
					accMessage.Usage.CacheReadInputTokens = mde.Usage.CacheReadInputTokens
				}
				if mde.Usage.CacheCreationInputTokens > 0 {
					accMessage.Usage.CacheCreationInputTokens = mde.Usage.CacheCreationInputTokens
				}
			}
			switch ev := event.AsAny().(type) {
			case anthropic.ContentBlockStartEvent:
				switch ev.ContentBlock.Type {
				case "thinking":
					inThinking = true
					thinkingAccum = ""
					thinkingSignature = ""
				case "tool_use":
					currentToolName = ev.ContentBlock.Name
					currentToolID = ev.ContentBlock.ID
					jsonAccum = ""
					events <- ToolCallStart{ToolName: currentToolName, ToolID: currentToolID}
				}
			case anthropic.ContentBlockDeltaEvent:
				switch delta := ev.Delta.AsAny().(type) {
				case anthropic.ThinkingDelta:
					thinkingAccum += delta.Thinking
					events <- ThinkingDelta{Text: delta.Thinking}
				case anthropic.SignatureDelta:
					thinkingSignature = delta.Signature
				case anthropic.TextDelta:
					events <- TextDelta{Text: delta.Text}
				case anthropic.InputJSONDelta:
					jsonAccum += delta.PartialJSON
					events <- ToolCallDelta{Text: delta.PartialJSON}
				}
			case anthropic.ContentBlockStopEvent:
				if inThinking {
					events <- ThinkingComplete{
						Thinking:  thinkingAccum,
						Signature: thinkingSignature,
					}
					inThinking = false
				}
				if currentToolName != "" {
					var args map[string]any
					if jsonAccum != "" {
						json.Unmarshal([]byte(jsonAccum), &args)
					}
					if args == nil {
						args = map[string]any{}
					}
					events <- ToolCallComplete{
						ToolID:    currentToolID,
						ToolName:  currentToolName,
						Arguments: args,
					}
					currentToolName = ""
					currentToolID = ""
					jsonAccum = ""
				}
			}

			go readNext()
		}

		if err := stream.Err(); err != nil {
			errs <- classifyAnthropicError(err)
			return
		}

		stopReason := string(accMessage.StopReason)
		if stopReason == "" {
			stopReason = "end_turn"
		}
		usage := UsageInfo{
			InputTokens:         int(accMessage.Usage.InputTokens),
			OutputTokens:        int(accMessage.Usage.OutputTokens),
			CacheReadTokens:     int(accMessage.Usage.CacheReadInputTokens),
			CacheCreationTokens: int(accMessage.Usage.CacheCreationInputTokens),
		}
		events <- StreamEnd{StopReason: stopReason, Usage: usage}
	}()

	return events, errs
}

// markLastUserTailForCache 给最后一条 user 角色消息的最后一个 content
// block 加上一个 ephemeral cache_control 标记。Anthropic 会缓存到
// （并包含）这个 block 为止的前缀；后续请求只要前缀字节完全一致
// 就能命中 cache。断点之后的 tool_result 内容保持字节稳定，
// 因为 toolresult 预算在消息进入时就已定型，
// 之后不会再重写历史。
//
// 原地修改 `messages`。没有 user 消息、或最后一条 user 消息
// 没有可供标记的 content block 时不做任何事。
func markLastUserTailForCache(messages []anthropic.MessageParam) {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != anthropic.MessageParamRoleUser {
			continue
		}
		blocks := messages[i].Content
		if len(blocks) == 0 {
			return
		}
		last := &blocks[len(blocks)-1]
		switch {
		case last.OfText != nil:
			last.OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
		case last.OfToolResult != nil:
			last.OfToolResult.CacheControl = anthropic.NewCacheControlEphemeralParam()
		}
		return
	}
}

// buildAnthropicMessages 两层消息结构体设计，内层消息 conversation.Message 只表达语义信息，实际需要与对应provider交互时，
// 通过该函数将 messages 进行 Anthropic 序列化，包装成对应的消息题结构
func buildAnthropicMessages(messages []conversation.Message) []anthropic.MessageParam {
	var result []anthropic.MessageParam
	for _, m := range messages {
		if m.Role == "assistant" {
			var blocks []anthropic.ContentBlockParamUnion
			for _, tb := range m.ThinkingBlocks {
				blocks = append(blocks, anthropic.NewThinkingBlock(tb.Signature, tb.Thinking))
			}
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tu := range m.ToolUses {
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolUse: &anthropic.ToolUseBlockParam{
						ID:    tu.ToolUseID,
						Name:  tu.ToolName,
						Input: tu.Arguments,
					},
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthropic.NewTextBlock(""))
			}
			result = append(result, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: blocks,
			})
		} else if len(m.ToolResults) > 0 {
			var blocks []anthropic.ContentBlockParamUnion
			for _, tr := range m.ToolResults {
				// 带结构化 block 的走 block 数组（tool_reference 这类要求服务端
				// 解析的内容只能这么发），其余照旧发纯文本
				content := []anthropic.ToolResultBlockParamContentUnion{{
					OfText: &anthropic.TextBlockParam{Text: tr.Content},
				}}
				if structured := toolResultContentBlocks(tr.ContentBlocks); len(structured) > 0 {
					content = structured
				}
				blocks = append(blocks, anthropic.ContentBlockParamUnion{
					OfToolResult: &anthropic.ToolResultBlockParam{
						ToolUseID: tr.ToolUseID,
						IsError:   param.NewOpt(tr.IsError),
						Content:   content,
					},
				})
			}
			result = append(result, anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: blocks,
			})
		} else {
			// 合并连续的 user 文本消息，以维持角色交替。
			canMerge := false
			if n := len(result); n > 0 {
				prev := result[n-1]
				if prev.Role == anthropic.MessageParamRoleUser && len(prev.Content) > 0 && prev.Content[0].OfToolResult == nil {
					canMerge = true
				}
			}
			if canMerge {
				result[len(result)-1].Content = append(result[len(result)-1].Content, anthropic.NewTextBlock(m.Content))
			} else {
				result = append(result, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
			}
		}
	}
	return result
}

func classifyAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		if apiErr.StatusCode == 413 || strings.Contains(apiErr.Error(), "prompt is too long") {
			return &ContextTooLongError{Message: fmt.Sprintf("Context too long: %s", apiErr.Error())}
		}
		switch apiErr.Type() {
		case anthropic.ErrorTypeAuthenticationError:
			return &AuthenticationError{Message: fmt.Sprintf("Invalid API key: %s", apiErr.Error())}
		case anthropic.ErrorTypeRateLimitError:
			retry := ""
			if apiErr.Response != nil {
				retry = apiErr.Response.Header.Get("Retry-After")
			}
			msg := "Rate limited."
			if retry != "" {
				msg += fmt.Sprintf(" Retry after %ss.", retry)
			} else {
				msg += " Please wait."
			}
			return &RateLimitError{Message: msg, RetryAfter: retry}
		default:
			return &LLMError{Message: fmt.Sprintf("API error (%d): %s", apiErr.StatusCode, apiErr.Error())}
		}
	}
	return &NetworkError{Message: fmt.Sprintf("Network error: %s", err.Error())}
}

// toolResultContentBlocks 把工具产出的结构化 block 转成 SDK 的类型化联合。
// 只认识 tool_reference——目前唯一需要服务端解析的块类型，官方端点下的
// ToolSearch 用它让服务端把 MCP 工具的 schema 展开进上下文。认不出的块整个
// 放弃转换，调用方会退回纯文本，宁可少发一个块也不发一个畸形请求。
func toolResultContentBlocks(raw []map[string]any) []anthropic.ToolResultBlockParamContentUnion {
	if len(raw) == 0 {
		return nil
	}
	out := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(raw))
	for _, b := range raw {
		blockType, _ := b["type"].(string)
		if blockType != "tool_reference" {
			return nil
		}
		name, _ := b["tool_name"].(string)
		if name == "" {
			return nil
		}
		out = append(out, anthropic.ToolResultBlockParamContentUnion{
			OfToolReference: &anthropic.ToolReferenceBlockParam{ToolName: name},
		})
	}
	return out
}
