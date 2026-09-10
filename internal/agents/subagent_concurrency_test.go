package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/tools"
)

// fakeLLMClient 是测试用的最小 llm.Client：不发任何网络请求，直接按脚本回事件。
// barrier 非 nil 时，每次 Stream 会先阻塞在它上面 —— 用来把并发窗口撑开，
// 验证 n 个子 Agent 真的同时在跑，而不是被某个全局锁串行化。
type fakeLLMClient struct {
	mu        sync.Mutex
	streams   int
	barrier   chan struct{}
	reply     string
	streamErr error
}

func (f *fakeLLMClient) SetSystemPrompt(string) {}

func (f *fakeLLMClient) Stream(ctx context.Context, _ *conversation.Manager, _ []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	f.mu.Lock()
	f.streams++
	f.mu.Unlock()

	events := make(chan llm.StreamEvent, 4)
	errs := make(chan error, 1)

	go func() {
		defer close(events)
		defer close(errs)
		if f.barrier != nil {
			select {
			case <-f.barrier:
			case <-ctx.Done():
				return
			}
		}
		if f.reply != "" {
			events <- llm.TextDelta{Text: f.reply}
		}
		// 不带 tool call 的收尾：子 Agent 一轮就 LoopComplete。
		events <- llm.StreamEnd{StopReason: "end_turn", Usage: llm.UsageInfo{}}
		if f.streamErr != nil {
			errs <- f.streamErr
		}
	}()

	return events, errs
}

func (f *fakeLLMClient) streamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", msg)
}

// 并发是 fan-out 的核心承诺：n 个同步子 Agent 必须能同时推进。barrier 保证
// 所有请求都进入 Stream 之后才放行 —— 只要有一条被串行挡住，streamCount
// 就永远到不了 n，测试超时失败。
func TestRunSyncExecutesConcurrently(t *testing.T) {
	const n = 4
	client := &fakeLLMClient{reply: "done", barrier: make(chan struct{})}
	tool := &AgentTool{
		Client:     client,
		Registry:   tools.NewRegistry(),
		Protocol:   "anthropic",
		ProgressCh: make(chan SubAgentProgress, 128),
	}
	spec := SubAgentSpec{Name: "general-purpose", MaxTurns: 5}

	results := make([]tools.ToolResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = tool.runSync(context.Background(), spec, fmt.Sprintf("task-%d", i), "do work", "", "")
		}(i)
	}

	waitFor(t, 5*time.Second, func() bool { return client.streamCount() == n },
		"all sub-agents to enter Stream concurrently")
	close(client.barrier)
	wg.Wait()

	for i, r := range results {
		if r.IsError {
			t.Fatalf("result %d errored: %s", i, r.Output)
		}
		if !strings.Contains(r.Output, "done") {
			t.Errorf("result %d missing sub-agent output: %q", i, r.Output)
		}
	}
}

// 失败路径必须把失败前已产出的部分结果一并带回，否则子 Agent 跑过的所有工作
// 都被一句错误信息抹掉。
func TestRunSyncFailureKeepsPartialOutput(t *testing.T) {
	client := &fakeLLMClient{reply: "partial findings", streamErr: fmt.Errorf("upstream exploded")}
	tool := &AgentTool{
		Client:   client,
		Registry: tools.NewRegistry(),
		Protocol: "anthropic",
	}

	result := tool.runSync(context.Background(), SubAgentSpec{Name: "general-purpose", MaxTurns: 5}, "probe", "do work", "", "")

	if !result.IsError {
		t.Fatal("expected IsError=true when the sub-agent fails")
	}
	if !strings.Contains(result.Output, "upstream exploded") {
		t.Errorf("failure message missing root cause: %q", result.Output)
	}
	if !strings.Contains(result.Output, "partial findings") {
		t.Errorf("failure result dropped partial output: %q", result.Output)
	}
}
