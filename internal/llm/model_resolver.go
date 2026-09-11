package llm

import (
	"fmt"
	"sort"
	"strings"

	"mewcode/internal/config"
)

// claudeModelAliases 是内置的 Claude 档位映射。它只在 provider 的主模型
// 本身就是 Claude 系列时才生效：把 claude-* 的模型 ID 发给别家端点，服务端
// 只会回 model not found，而且这个错误要等真正发出请求才暴露 —— 与其等
// 运行期炸掉，不如在解析阶段就拒绝。
var claudeModelAliases = map[string]string{
	"haiku":  "claude-haiku-4-5-20251001",
	"sonnet": "claude-sonnet-4-6-20250514",
	"opus":   "claude-opus-4-6-20250514",
}

// claudeAliasOrder 固定内置档位名在 schema enum 里的出现顺序，保证工具定义
// 稳定 —— 顺序一变就是一次 tools[] 变动，会打断 prompt 缓存。
var claudeAliasOrder = []string{"haiku", "opus", "sonnet"}

func NewModelResolver(baseCfg config.ProviderConfig) func(string) (Client, error) {
	return func(shortName string) (Client, error) {
		modelID, err := resolveModelAlias(baseCfg, shortName)
		if err != nil {
			return nil, err
		}
		cfg := baseCfg
		cfg.Model = modelID
		return NewClient(&cfg, "")
	}
}

// resolveModelAlias 把子 Agent 传入的模型名 / 档位名解析成实际发给端点的模型 ID：
//
//  1. provider 配置里的 model_aliases —— 用户显式指定，最可靠；
//  2. 内置 Claude 档位 → 仅当主模型本身是 Claude 系列；
//  3. 都不命中 → 当作字面模型名原样透传（用户可以直接写具体模型 ID）。
//
// 第 2 条不成立时故意返回错误而不是硬套 claude-*：调用方
// （AgentTool.selectClient）会捕获错误并回退到主 Agent 的 client，于是非
// Claude provider 下 explore 这类角色的档位偏好自然降级为主模型。
func resolveModelAlias(cfg config.ProviderConfig, name string) (string, error) {
	if m, ok := cfg.ModelAliases[name]; ok && m != "" {
		return m, nil
	}
	if claudeID, ok := claudeModelAliases[name]; ok {
		if isClaudeModel(cfg.Model) {
			return claudeID, nil
		}
		return "", fmt.Errorf(
			"model alias %q maps to %q, but provider %q runs %q — add model_aliases to the provider config",
			name, claudeID, cfg.Name, cfg.Model)
	}
	return name, nil
}

func isClaudeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

// AvailableModelAliases 返回当前 provider 下可用的档位名，顺序固定：先用户
// 配置的（按字典序），再补适用的内置 Claude 档位。子 Agent 工具 schema 的
// model enum 用它生成，所以顺序必须稳定；返回空切片表示这个 provider 没有
// 可枚举的档位，schema 就干脆不给 enum。
func AvailableModelAliases(cfg config.ProviderConfig) []string {
	out := make([]string, 0, len(cfg.ModelAliases)+len(claudeAliasOrder))
	seen := make(map[string]bool, len(cfg.ModelAliases))

	keys := make([]string, 0, len(cfg.ModelAliases))
	for name, target := range cfg.ModelAliases {
		if name == "" || target == "" {
			continue
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		out = append(out, name)
		seen[name] = true
	}

	if isClaudeModel(cfg.Model) {
		for _, name := range claudeAliasOrder {
			if !seen[name] {
				out = append(out, name)
			}
		}
	}
	return out
}
