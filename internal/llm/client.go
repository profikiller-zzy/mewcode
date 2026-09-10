package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"mewcode/internal/config"
	"mewcode/internal/conversation"
)

type Client interface {
	// Stream 传入对话历史和工具列表，返回事件流和错误流
	Stream(ctx context.Context, conv *conversation.Manager, tools []map[string]any) (<-chan StreamEvent, <-chan error)
	SetSystemPrompt(prompt string)
}

// StreamMetadata 把一次 provider 请求与发起它的 AgentRun、ReAct
// 轮次关联起来。它通过 context 传递，这样已有的 provider 和
// 第三方 Client 实现保持源码兼容。
type StreamMetadata struct {
	SessionID string
	RunID     string
	RequestID string
	Iteration int
}

type streamMetadataKey struct{}

func WithStreamMetadata(ctx context.Context, metadata StreamMetadata) context.Context {
	return context.WithValue(ctx, streamMetadataKey{}, metadata)
}

func StreamMetadataFromContext(ctx context.Context) (StreamMetadata, bool) {
	metadata, ok := ctx.Value(streamMetadataKey{}).(StreamMetadata)
	return metadata, ok
}

// StreamRequest 把「单次请求」的边界显性化。StreamOnce 是 AgentRun 用的
// 适配器；它有意只执行一次 Client.Stream
// 调用，不负责重试、工具、memory 或 ReAct 循环。
type StreamRequest struct {
	Metadata     StreamMetadata
	Conversation *conversation.Manager
	Tools        []map[string]any
}

type RequestPhase string

const (
	RequestStarted  RequestPhase = "started"
	RequestFinished RequestPhase = "finished"
)

type RequestLifecycle struct {
	Phase      RequestPhase
	Metadata   StreamMetadata
	StopReason string
	Usage      UsageInfo
	Err        error
}

type streamObserverKey struct{}

// WithStreamObserver 装上一个 run 级别的 observer。因为它挂在 context 上，
// 嵌套的 compaction 请求也能被关联起来，无需引入
// agent 包或改动 provider 实现。
func WithStreamObserver(ctx context.Context, observer func(RequestLifecycle)) context.Context {
	return context.WithValue(ctx, streamObserverKey{}, observer)
}

func StreamOnce(ctx context.Context, client Client, request StreamRequest) (<-chan StreamEvent, <-chan error) {
	metadata, _ := StreamMetadataFromContext(ctx)
	if request.Metadata.SessionID != "" {
		metadata.SessionID = request.Metadata.SessionID
	}
	if request.Metadata.RunID != "" {
		metadata.RunID = request.Metadata.RunID
	}
	if request.Metadata.Iteration != 0 {
		metadata.Iteration = request.Metadata.Iteration
	}
	if request.Metadata.RequestID != "" {
		metadata.RequestID = request.Metadata.RequestID
	}
	if metadata.RequestID == "" {
		metadata.RequestID = newRequestID()
	}
	ctx = WithStreamMetadata(ctx, metadata)
	observer, _ := ctx.Value(streamObserverKey{}).(func(RequestLifecycle))
	if observer != nil {
		observer(RequestLifecycle{Phase: RequestStarted, Metadata: metadata})
	}
	providerEvents, providerErrors := client.Stream(ctx, request.Conversation, request.Tools)
	if observer == nil {
		return providerEvents, providerErrors
	}
	events := make(chan StreamEvent)
	errs := make(chan error, 1)
	type streamSummary struct {
		stopReason string
		usage      UsageInfo
	}
	eventDone := make(chan streamSummary, 1)
	errorDone := make(chan error, 1)
	go func() {
		var summary streamSummary
		for event := range providerEvents {
			if end, ok := event.(StreamEnd); ok {
				summary.stopReason = end.StopReason
				summary.usage = end.Usage
			}
			events <- event
		}
		eventDone <- summary
	}()
	go func() {
		var firstErr error
		for err := range providerErrors {
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err != nil {
				select {
				case errs <- err:
				default:
				}
			}
		}
		errorDone <- firstErr
	}()
	go func() {
		summary := <-eventDone
		streamErr := <-errorDone
		observer(RequestLifecycle{
			Phase:      RequestFinished,
			Metadata:   metadata,
			StopReason: summary.stopReason,
			Usage:      summary.usage,
			Err:        streamErr,
		})
		close(events)
		close(errs)
	}()
	return events, errs
}

func newRequestID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return "req_" + hex.EncodeToString(bytes[:])
	}
	return "req_" + time.Now().UTC().Format("20060102T150405.000000000")
}

type MaxTokensSetter interface {
	SetMaxOutputTokens(tokens int)
}

func NewClient(cfg *config.ProviderConfig, systemPrompt string) (Client, error) {
	switch cfg.Protocol {
	case "anthropic":
		return newAnthropicClient(cfg, systemPrompt)
	case "openai":
		return newOpenAIClient(cfg, systemPrompt)
	case "openai-compat":
		return newOpenAICompatClient(cfg, systemPrompt)
	default:
		return nil, fmt.Errorf("unknown protocol: %s", cfg.Protocol)
	}
}

// contextWindowFetcher 由那些能向自己的 provider 拉取模型 context window 的
// client 实现。目前只有 Anthropic client 这么做。
type contextWindowFetcher interface {
	FetchModelContextWindow(ctx context.Context) int
}

// ResolveContextWindow 执行 context window 解析的第 2 层：对于
// Anthropic 协议的 provider，它从 {base_url}/v1/models/{model}
// 拉取一次模型的 max_input_tokens，并通过
// SetFetchedContextWindow 缓存到 cfg 上，这样后续
// cfg.GetContextWindow() 直接用缓存，不再走网络。
//
// 它完全是尽力而为的，永不返回错误：非 Anthropic 的 provider、
// client 构造失败、或拉取失败/超时，都不会动缓存，
// 让 GetContextWindow 回退到内置的
// 映射表 / 默认值。启动时调用是安全的 —— 除拉取本身的超时外不会阻塞，
// 也不会 panic。
func ResolveContextWindow(ctx context.Context, cfg *config.ProviderConfig) {
	// 显式配置值在 GetContextWindow 里本来就优先，所以没什么可拉的。
	// 已经缓存过值的情况同样跳过。
	if cfg == nil || cfg.ContextWindow > 0 {
		return
	}
	if cfg.Protocol != "anthropic" {
		return
	}

	client, err := NewClient(cfg, "")
	if err != nil {
		return
	}
	fetcher, ok := client.(contextWindowFetcher)
	if !ok {
		return
	}
	if window := fetcher.FetchModelContextWindow(ctx); window > 0 {
		cfg.SetFetchedContextWindow(window)
	}
}
