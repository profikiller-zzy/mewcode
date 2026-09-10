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

// RunOptions 只装同一次独立用户请求相关的状态。
// session 调度和队列状态是故意不放这里的。
type RunOptions struct {
	ID           string
	SessionID    string
	Prompt       string
	Conversation *conversation.Manager
	Trace        *TraceRecorder
	Context      ContextCoordinator
	SessionHooks bool
}

// AgentRun 掌管一整个完整的 ReAct 循环：一条 prompt、一次或多次模型
// 请求 / 迭代、零个或多个工具批次，以及一个终止原因。
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

// traceToolArguments 保留可重放 / 调试的形状，但不持久化常见的凭证字段，
// 也不持久化任意大的参数体。
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
