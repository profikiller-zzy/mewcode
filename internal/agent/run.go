package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"mewcode/internal/conversation"
)

// RunOptions contains only state belonging to one independent user request.
// Session scheduling and queue state intentionally do not live here.
type RunOptions struct {
	ID           string
	SessionID    string
	Prompt       string
	Conversation *conversation.Manager
	Trace        *TraceRecorder
	Context      ContextCoordinator
	SessionHooks bool
}

// AgentRun owns one complete ReAct loop: one prompt, one or more model
// requests/iterations, zero or more tool batches, and one terminal reason.
type AgentRun struct {
	ID           string
	SessionID    string
	Prompt       string
	Agent        *Agent
	Conversation *conversation.Manager
	Trace        *TraceRecorder
	Context      ContextCoordinator
	sessionHooks bool

	mu           sync.RWMutex
	finishReason RunFinishReason
}

func (r *AgentRun) FinishReason() RunFinishReason {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.finishReason
}

func (r *AgentRun) setFinishReason(reason RunFinishReason) {
	r.mu.Lock()
	r.finishReason = reason
	r.mu.Unlock()
}

func (a *Agent) NewRun(options RunOptions) *AgentRun {
	id := options.ID
	if id == "" {
		id = newRuntimeID("run")
	}
	sessionID := options.SessionID
	if sessionID == "" {
		sessionID = a.SessionID
	}
	conv := options.Conversation
	if conv == nil {
		conv = conversation.NewManager()
	}
	recorder := options.Trace
	if recorder == nil {
		recorder = a.Trace
	}
	coordinator := options.Context
	if coordinator == nil {
		coordinator = &contextCoordinator{tracking: &a.compactTracking}
	}
	return &AgentRun{
		ID:           id,
		SessionID:    sessionID,
		Prompt:       options.Prompt,
		Agent:        a,
		Conversation: conv,
		Trace:        recorder,
		Context:      coordinator,
		sessionHooks: options.SessionHooks,
	}
}

func (r *AgentRun) record(ctx context.Context, event TraceEvent) {
	if r == nil || r.Trace == nil {
		return
	}
	event.SessionID = r.SessionID
	event.RunID = r.ID
	r.Trace.Record(ctx, event)
}

func newRuntimeID(prefix string) string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err == nil {
		return prefix + "_" + hex.EncodeToString(bytes[:])
	}
	return prefix + "_" + time.Now().UTC().Format("20060102T150405.000000000")
}

var sensitiveTraceKeys = []string{
	"authorization", "cookie", "password", "passwd", "secret", "token", "api_key", "apikey", "private_key",
}

// traceToolArguments retains replay/debug shape without persisting common
// credential fields or arbitrarily large argument bodies.
func traceToolArguments(arguments map[string]any) map[string]any {
	redacted := make(map[string]any, len(arguments))
	for key, value := range arguments {
		lower := strings.ToLower(key)
		sensitive := false
		for _, marker := range sensitiveTraceKeys {
			if strings.Contains(lower, marker) {
				sensitive = true
				break
			}
		}
		if sensitive {
			redacted[key] = "[REDACTED]"
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			redacted[key] = "[UNSERIALIZABLE]"
			continue
		}
		preview, truncated := tracePreview(string(encoded))
		if truncated {
			redacted[key] = map[string]any{"preview": preview, "truncated": true}
		} else {
			redacted[key] = value
		}
	}
	return map[string]any{"arguments": redacted}
}
