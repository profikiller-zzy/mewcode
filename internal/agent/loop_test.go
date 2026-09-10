package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/tools"
)

type blockingLoopClient struct {
	started chan string
	release chan struct{}

	mu       sync.Mutex
	metadata []llm.StreamMetadata
}

func newBlockingLoopClient() *blockingLoopClient {
	return &blockingLoopClient{started: make(chan string, 8), release: make(chan struct{}, 8)}
}

func (c *blockingLoopClient) SetSystemPrompt(string) {}

func (c *blockingLoopClient) Stream(ctx context.Context, conv *conversation.Manager, _ []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	events := make(chan llm.StreamEvent, 2)
	errs := make(chan error, 1)
	msgs := conv.GetMessages()
	prompt := msgs[len(msgs)-1].Content
	if metadata, ok := llm.StreamMetadataFromContext(ctx); ok {
		c.mu.Lock()
		c.metadata = append(c.metadata, metadata)
		c.mu.Unlock()
	}
	go func() {
		defer close(events)
		defer close(errs)
		c.started <- prompt
		select {
		case <-c.release:
			events <- llm.TextDelta{Text: "answer:" + prompt}
			events <- llm.StreamEnd{StopReason: "end_turn"}
		case <-ctx.Done():
			errs <- ctx.Err()
		}
	}()
	return events, errs
}

func drainSubmission(sub Submission) {
	for range sub.Events {
	}
}

func awaitString(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client call")
		return ""
	}
}

func awaitResult(t *testing.T, sub Submission) RunResult {
	t.Helper()
	select {
	case result := <-sub.Done:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for run result")
		return RunResult{}
	}
}

func TestAgentLoopSerializesPromptsWithinSession(t *testing.T) {
	client := newBlockingLoopClient()
	ag := New(client, tools.NewRegistry(), "test")
	conv := conversation.NewManager()
	loop := NewAgentLoop(nil)
	if err := loop.RegisterSession("s1", SessionOptions{Agent: ag, Conversation: conv}); err != nil {
		t.Fatal(err)
	}

	first, err := loop.Submit(context.Background(), "s1", "first")
	if err != nil {
		t.Fatal(err)
	}
	go drainSubmission(first)
	if got := awaitString(t, client.started); got != "first" {
		t.Fatalf("first call = %q", got)
	}
	second, err := loop.Submit(context.Background(), "s1", "second")
	if err != nil {
		t.Fatal(err)
	}
	go drainSubmission(second)
	if second.Position != 2 {
		t.Fatalf("queued position = %d, want 2", second.Position)
	}
	select {
	case got := <-client.started:
		t.Fatalf("second prompt started before first completed: %q", got)
	case <-time.After(40 * time.Millisecond):
	}

	client.release <- struct{}{}
	if result := awaitResult(t, first); result.Reason != RunCompleted {
		t.Fatalf("first result = %+v", result)
	}
	if got := awaitString(t, client.started); got != "second" {
		t.Fatalf("second call = %q", got)
	}
	client.release <- struct{}{}
	if result := awaitResult(t, second); result.Reason != RunCompleted {
		t.Fatalf("second result = %+v", result)
	}
	client.mu.Lock()
	metadata := append([]llm.StreamMetadata(nil), client.metadata...)
	client.mu.Unlock()
	if len(metadata) != 2 || metadata[0].SessionID != "s1" || metadata[0].RunID == "" || metadata[0].RequestID == "" || metadata[0].Iteration != 1 ||
		metadata[1].SessionID != "s1" || metadata[1].RunID == metadata[0].RunID || metadata[1].RequestID == "" {
		t.Fatalf("request metadata is not correlated to runs: %+v", metadata)
	}

	msgs := conv.GetMessages()
	if len(msgs) != 4 || msgs[0].Content != "first" || msgs[2].Content != "second" {
		t.Fatalf("unexpected committed conversation: %+v", msgs)
	}
	if msgs[0].RunID == "" || msgs[0].RunID != msgs[1].RunID || msgs[2].RunID == msgs[0].RunID || msgs[2].RunID != msgs[3].RunID {
		t.Fatalf("run boundaries not preserved: %+v", msgs)
	}
}

func TestAgentLoopRunsDifferentSessionsConcurrently(t *testing.T) {
	client := newBlockingLoopClient()
	loop := NewAgentLoop(nil)
	for _, id := range []string{"s1", "s2"} {
		ag := New(client, tools.NewRegistry(), "test")
		if err := loop.RegisterSession(id, SessionOptions{Agent: ag, Conversation: conversation.NewManager()}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := loop.Submit(context.Background(), "s1", "one")
	second, _ := loop.Submit(context.Background(), "s2", "two")
	go drainSubmission(first)
	go drainSubmission(second)
	seen := map[string]bool{awaitString(t, client.started): true, awaitString(t, client.started): true}
	if !seen["one"] || !seen["two"] {
		t.Fatalf("both sessions did not start concurrently: %v", seen)
	}
	client.release <- struct{}{}
	client.release <- struct{}{}
	if awaitResult(t, first).Reason != RunCompleted || awaitResult(t, second).Reason != RunCompleted {
		t.Fatal("concurrent sessions did not complete")
	}
}

func TestAgentLoopCancellationAdvancesQueue(t *testing.T) {
	client := newBlockingLoopClient()
	loop := NewAgentLoop(nil)
	ag := New(client, tools.NewRegistry(), "test")
	if err := loop.RegisterSession("s1", SessionOptions{Agent: ag, Conversation: conversation.NewManager()}); err != nil {
		t.Fatal(err)
	}
	first, _ := loop.Submit(context.Background(), "s1", "cancel-me")
	go drainSubmission(first)
	if got := awaitString(t, client.started); got != "cancel-me" {
		t.Fatalf("first call = %q", got)
	}
	second, _ := loop.Submit(context.Background(), "s1", "run-next")
	go drainSubmission(second)
	if err := loop.Cancel("s1"); err != nil {
		t.Fatal(err)
	}
	if result := awaitResult(t, first); result.Reason != RunCancelled || result.Err == nil {
		t.Fatalf("cancelled result = %+v", result)
	}
	if got := awaitString(t, client.started); got != "run-next" {
		t.Fatalf("queued call after cancellation = %q", got)
	}
	client.release <- struct{}{}
	if result := awaitResult(t, second); result.Reason != RunCompleted {
		t.Fatalf("second result = %+v", result)
	}
}

func TestAgentRunTraceAggregatesIterationsAndRequestMetadata(t *testing.T) {
	client := &mockClient{responses: [][]llm.StreamEvent{
		{
			llm.ToolCallStart{ToolName: "ReadFile", ToolID: "tool-1"},
			llm.ToolCallComplete{ToolID: "tool-1", ToolName: "ReadFile", Arguments: map[string]any{"path": "/tmp/x", "token": "secret"}},
			llm.StreamEnd{StopReason: "tool_use"},
		},
		{llm.TextDelta{Text: "done"}, llm.StreamEnd{StopReason: "end_turn"}},
	}}
	registry := tools.NewRegistry()
	registry.Register(&mockTool{name: "ReadFile", result: "contents"})
	store := &MemoryTraceStore{}
	recorder := NewTraceRecorder(store)
	ag := New(client, registry, "test")
	conv := conversation.NewManager()
	run := ag.NewRun(RunOptions{ID: "r1", SessionID: "s1", Prompt: "read", Conversation: conv, Trace: recorder})
	collectEvents(run.Start(context.Background()))

	events := store.Events("s1", "r1")
	var starts, finishes, requests int
	requestIDs := map[string]bool{}
	for i, event := range events {
		if event.Sequence != uint64(i+1) {
			t.Fatalf("sequence[%d] = %d", i, event.Sequence)
		}
		switch event.Type {
		case TraceRunStarted:
			starts++
		case TraceRunFinished:
			finishes++
			if event.Reason != RunCompleted {
				t.Fatalf("finish reason = %s", event.Reason)
			}
		case TraceModelRequestStarted:
			requests++
			requestIDs[event.RequestID] = true
		case TraceToolCallArguments:
			arguments := event.Payload["arguments"].(map[string]any)
			if arguments["token"] != "[REDACTED]" {
				t.Fatalf("sensitive argument was not redacted: %+v", arguments)
			}
		}
	}
	if starts != 1 || finishes != 1 || requests != 2 || len(requestIDs) != 2 {
		t.Fatalf("trace did not aggregate the complete run: starts=%d finishes=%d requests=%d ids=%d", starts, finishes, requests, len(requestIDs))
	}
}
