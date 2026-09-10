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

// StreamMetadata correlates one provider request with the AgentRun and ReAct
// iteration that issued it. It is carried in context so existing provider and
// third-party Client implementations remain source compatible.
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

// StreamRequest makes the single-request boundary explicit. StreamOnce is the
// adapter used by AgentRun; it deliberately performs exactly one Client.Stream
// call and does not own retries, tools, memory, or the ReAct loop.
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

// WithStreamObserver installs a run-local observer. Because it is carried by
// context, nested compaction requests are correlated without importing the
// agent package or changing provider implementations.
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

// contextWindowFetcher is implemented by clients that can pull the model's
// context window from their provider. Only the Anthropic client does so.
type contextWindowFetcher interface {
	FetchModelContextWindow(ctx context.Context) int
}

// ResolveContextWindow performs layer 2 of context-window resolution: for
// Anthropic-protocol providers it pulls the model's max_input_tokens from
// {base_url}/v1/models/{model} once and caches it on cfg via
// SetFetchedContextWindow, so later cfg.GetContextWindow() calls use it
// without hitting the network again.
//
// It is fully best-effort and never returns an error: a non-Anthropic
// provider, a client-construction failure, or a failed/timed-out fetch all
// leave the cache untouched, letting GetContextWindow fall back to the
// built-in mapping table / default. Safe to call at startup — it will not
// block beyond the fetch's own timeout and will not panic.
func ResolveContextWindow(ctx context.Context, cfg *config.ProviderConfig) {
	// An explicit config value already wins in GetContextWindow, so there's
	// nothing to fetch. Likewise skip if we've already cached a value.
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
