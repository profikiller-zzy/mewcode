package llm

import (
	"context"
	"testing"

	"mewcode/internal/conversation"
)

type historyCaptureClient struct {
	history []conversation.Message
}

func (c *historyCaptureClient) Stream(_ context.Context, conv *conversation.Manager, _ []map[string]any) (<-chan StreamEvent, <-chan error) {
	c.history = conv.GetMessages()
	events := make(chan StreamEvent, 1)
	errs := make(chan error)
	events <- StreamEnd{StopReason: "end_turn"}
	close(events)
	close(errs)
	return events, errs
}

func (c *historyCaptureClient) SetSystemPrompt(string) {}

func TestStreamOnceSendsNormalizedConversationSnapshot(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddToolUseMessage("", "call-1", "Bash", map[string]any{"command": "true"})
	conv.AddToolResultMessage("call-1", "ok", false)
	conv.AddToolResultMessage("call-1", "duplicate", false)

	client := &historyCaptureClient{}
	events, errs := StreamOnce(context.Background(), client, StreamRequest{Conversation: conv})
	for range events {
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("StreamOnce returned error: %v", err)
		}
	}

	if got := len(client.history); got != 2 {
		t.Fatalf("provider received %d messages, want normalized 2", got)
	}
	if got := len(client.history[1].ToolResults); got != 1 {
		t.Fatalf("provider received %d tool results, want 1", got)
	}
	if got := len(conv.GetMessages()); got != 3 {
		t.Fatalf("original conversation was mutated: got %d messages, want 3", got)
	}
}
