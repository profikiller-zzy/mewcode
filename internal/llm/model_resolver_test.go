package llm

import (
	"slices"
	"strings"
	"testing"

	"mewcode/internal/config"
)

func TestResolveModelAliasPrefersUserConfiguration(t *testing.T) {
	cfg := config.ProviderConfig{
		Name:         "deepseek",
		Model:        "deepseek-chat",
		ModelAliases: map[string]string{"haiku": "deepseek-chat", "sonnet": "deepseek-reasoner"},
	}
	got, err := resolveModelAlias(cfg, "haiku")
	if err != nil || got != "deepseek-chat" {
		t.Fatalf("user-configured alias should win: got %q, err %v", got, err)
	}
}

func TestResolveModelAliasUsesClaudeTableOnlyForClaudeProviders(t *testing.T) {
	claude := config.ProviderConfig{Name: "anthropic", Model: "claude-sonnet-4-6"}
	got, err := resolveModelAlias(claude, "haiku")
	if err != nil || got != claudeModelAliases["haiku"] {
		t.Fatalf("claude provider should resolve the built-in alias: got %q, err %v", got, err)
	}

	// 非 Claude provider 拿到内置档位名时必须报错，而不是把 claude-* 的模型 ID
	// 发到不认识的端点上 —— 那个错误要等真正发请求才暴露，更难排查。
	other := config.ProviderConfig{Name: "deepseek", Model: "deepseek-chat"}
	if _, err := resolveModelAlias(other, "haiku"); err == nil {
		t.Fatal("non-claude provider must refuse a built-in claude alias")
	} else if !strings.Contains(err.Error(), "model_aliases") {
		t.Errorf("error should point at the model_aliases config field, got: %v", err)
	}
}

func TestResolveModelAliasPassesUnknownNamesVerbatim(t *testing.T) {
	cfg := config.ProviderConfig{Name: "openai", Model: "gpt-4o"}
	got, err := resolveModelAlias(cfg, "gpt-4.1-mini")
	if err != nil || got != "gpt-4.1-mini" {
		t.Fatalf("literal model names should pass through: got %q, err %v", got, err)
	}
}

func TestNewModelResolverRejectsClaudeAliasOnForeignProvider(t *testing.T) {
	// 端到端确认解析失败发生在构造 client 之前：调用方 selectClient 拿到
	// error 就会回退到主 Agent 的 client。
	resolve := NewModelResolver(config.ProviderConfig{Name: "deepseek", Model: "deepseek-chat", APIKey: "k"})
	if _, err := resolve("haiku"); err == nil {
		t.Fatal("expected an error for a claude alias on a non-claude provider")
	}
}

func TestAvailableModelAliases(t *testing.T) {
	// 非 Claude：只列用户配置的档位名，按字典序。
	other := config.ProviderConfig{
		Model:        "deepseek-chat",
		ModelAliases: map[string]string{"sonnet": "r", "haiku": "c"},
	}
	if got, want := AvailableModelAliases(other), []string{"haiku", "sonnet"}; !slices.Equal(got, want) {
		t.Errorf("AvailableModelAliases = %v, want %v", got, want)
	}

	// Claude：用户配置优先，再补上没被覆盖的内置档位，顺序固定。
	claude := config.ProviderConfig{
		Model:        "claude-sonnet-4-6",
		ModelAliases: map[string]string{"haiku": "my-cheap-model"},
	}
	if got, want := AvailableModelAliases(claude), []string{"haiku", "opus", "sonnet"}; !slices.Equal(got, want) {
		t.Errorf("AvailableModelAliases = %v, want %v", got, want)
	}

	// 都没有：空切片，schema 就不给 enum。
	if got := AvailableModelAliases(config.ProviderConfig{Model: "gpt-4o"}); len(got) != 0 {
		t.Errorf("expected no enumerable aliases, got %v", got)
	}
}
