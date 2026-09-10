package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mewcode/internal/config"
)

// TestResolveContextWindow_FetchSuccess 覆盖第 2 层正常工作的情况：
// 健康的 /v1/models/{model} 接口返回 max_input_tokens，它会被缓存到
// provider 配置上，并由 GetContextWindow 暴露出来。
func TestResolveContextWindow_FetchSuccess(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"claude-sonnet-4-6","type":"model","display_name":"x","max_input_tokens":555000,"max_tokens":8192}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-sonnet-4-6",
	}
	ResolveContextWindow(context.Background(), cfg)

	if !strings.Contains(gotPath, "/v1/models/claude-sonnet-4-6") {
		t.Errorf("expected fetch to hit /v1/models/{model}, got path %q", gotPath)
	}
	if got := cfg.GetContextWindow(); got != 555000 {
		t.Fatalf("fetched window should be used: got %d, want 555000", got)
	}
}

// TestResolveContextWindow_FetchErrorDegrades 覆盖关键路径：接口报错时
// （这里返回 500），拉取必须静默失败 —— 不 panic、不阻塞 ——
// 然后 GetContextWindow 回退到映射表。
func TestResolveContextWindow_FetchErrorDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-sonnet-4-6",
	}
	ResolveContextWindow(context.Background(), cfg) // 不能 panic

	// 映射表给 claude 的是 200000；拉取失败不能把它拉低。
	if got := cfg.GetContextWindow(); got != 200000 {
		t.Fatalf("on fetch error should fall back to mapping table: got %d, want 200000", got)
	}
}

// TestResolveContextWindow_UnreachableDegrades 模拟一个死掉的接口
// （服务已关闭）。带超时上限的拉取必须回退到映射表，
// 既不卡住也不崩。
func TestResolveContextWindow_UnreachableDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关闭，让连接被拒绝

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: url, APIKey: "k", Model: "gpt-4o",
	}
	ResolveContextWindow(context.Background(), cfg)

	if got := cfg.GetContextWindow(); got != 128000 {
		t.Fatalf("unreachable endpoint should fall back: got %d, want 128000", got)
	}
}

// TestResolveContextWindow_NonAnthropicSkipped 确认第 2 层只对
// Anthropic 协议的 provider 生效：非 anthropic 的 provider 不会发起拉取，
// 而是按映射表解析。
func TestResolveContextWindow_NonAnthropicSkipped(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"max_input_tokens":999999}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "openai-compat", BaseURL: srv.URL, APIKey: "k", Model: "gpt-4o",
	}
	ResolveContextWindow(context.Background(), cfg)

	if called {
		t.Error("non-anthropic provider must not trigger a /v1/models fetch")
	}
	if got := cfg.GetContextWindow(); got != 128000 {
		t.Fatalf("non-anthropic should use mapping table: got %d, want 128000", got)
	}
}

// TestResolveContextWindow_ConfigOverrideSkipsFetch 确认显式配置的窗口
// 会完全短路掉拉取（一次网络请求都不发）。
func TestResolveContextWindow_ConfigOverrideSkipsFetch(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"max_input_tokens":999999}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k",
		Model: "claude-sonnet-4-6", ContextWindow: 4096,
	}
	ResolveContextWindow(context.Background(), cfg)

	if called {
		t.Error("explicit config window must skip the fetch")
	}
	if got := cfg.GetContextWindow(); got != 4096 {
		t.Fatalf("config window should win: got %d, want 4096", got)
	}
}
