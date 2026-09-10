package agents

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// AgentMemoryScope 持久化记忆的存放位置：按用户、按项目，或按 checkout
// （不纳入版本控制）。
type AgentMemoryScope string

const (
	AgentMemoryScopeUser    AgentMemoryScope = "user"
	AgentMemoryScopeProject AgentMemoryScope = "project"
	AgentMemoryScopeLocal   AgentMemoryScope = "local"
)

// IsolationMode 对应 `isolation` frontmatter 字段。
type IsolationMode string

const (
	IsolationWorktree IsolationMode = "worktree"
	IsolationRemote   IsolationMode = "remote"
)

// AgentDefinition 中尚未被运行时使用的字段（Effort、Skills、McpServers、Hooks、
// Memory、InitialPrompt、OmitMewcodeMd、RequiredMcpServers）仍然会被解析，这样用户的
// 定义在往返读写时不会丢数据，未来的通道也可以直接取用，不必再迁一次 schema。
type AgentDefinition struct {
	AgentType       string   `yaml:"name"`
	WhenToUse       string   `yaml:"description"`
	Tools           []string `yaml:"tools"`
	DisallowedTools []string `yaml:"disallowedTools"`
	Model           string   `yaml:"model"`
	MaxTurns        int      `yaml:"maxTurns"`

	// permissionMode 覆盖父 Agent 的权限模式，只对当前 sub-agent 生效。合法取值与
	// internal/permissions.PermissionMode 一致。
	PermissionMode string `yaml:"permissionMode"`

	// Effort 是给模型的任务复杂度提示（"low" | "medium" | "high" | int）。
	// 目前只存储，尚未消费。
	Effort any `yaml:"effort"`

	// Skills 是 sub-agent 启动时要预加载的 skill 名。
	Skills []string `yaml:"skills"`

	// McpServers 是作用范围限定在当前 Agent 的 MCP server 名或内联配置。以 raw any
	// 存储，这样将来的加载逻辑既能解释字符串引用，也能解释内联配置。
	McpServers []any `yaml:"mcpServers"`

	// RequiredMcpServers 是 Agent 的准入门槛：如果列出的 server 在加载时不可用，
	// 该 Agent 会被 hasRequiredMcpServers 过滤掉。
	RequiredMcpServers []string `yaml:"requiredMcpServers"`

	// Hooks 是 Agent 启动时注册的、作用范围为 session 的 hook。以原始 YAML 存储；
	// hooks 包会在消费时做类型检查。
	Hooks any `yaml:"hooks"`

	// Memory 在三种 scope 之一中启用持久化记忆。
	Memory AgentMemoryScope `yaml:"memory"`

	// Background 已废弃：异步 sub-agent 路径已整体移除（一律主 Agent 同步等待 +
	// 子 Agent 并发运行）。字段保留解析只为让存量定义文件不报错，运行期不再消费。
	Background bool `yaml:"background"`

	// Isolation 为派生选择文件系统隔离模式。
	Isolation IsolationMode `yaml:"isolation"`

	// InitialPrompt 会被前置到第一轮 user turn（slash command 也能用）。
	InitialPrompt string `yaml:"initialPrompt"`

	// OmitMewcodeMd 从该 Agent 的 user context 中去掉 MEWCODE.md 层级。只读 Agent
	// （Explore、Plan）跳过它可以省 token。
	OmitMewcodeMd bool `yaml:"omitMewcodeMd"`

	// SystemPrompt 是定义文件的 Markdown 正文。
	SystemPrompt string `yaml:"-"`

	// FilePath / Source / Filename 在加载时填充。
	FilePath string `yaml:"-"`
	Source   string `yaml:"-"`
	Filename string `yaml:"-"`
}

// validPermissionModes 是 Agent 定义里 permission_mode 字段的合法取值，空串表示不覆盖、
// 沿用父 Agent 的模式。
var validPermissionModes = map[string]bool{
	"":                  true,
	"acceptEdits":       true,
	"bypassPermissions": true,
	"default":           true,
	"plan":              true,
}

var validMemoryScopes = map[AgentMemoryScope]bool{
	"":                      true,
	AgentMemoryScopeUser:    true,
	AgentMemoryScopeProject: true,
	AgentMemoryScopeLocal:   true,
}

var validIsolationModes = map[IsolationMode]bool{
	"":                true,
	IsolationWorktree: true,
	IsolationRemote:   true,
}

func ParseAgentFile(path string) (*AgentDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var def AgentDefinition
	def.FilePath = path

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &def); err != nil {
				return nil, fmt.Errorf("parse frontmatter in %s: %w", path, err)
			}
			def.SystemPrompt = strings.TrimSpace(parts[2])
		}
	} else {
		def.SystemPrompt = strings.TrimSpace(content)
	}

	if def.AgentType == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'name'", path)
	}
	if def.WhenToUse == "" {
		return nil, fmt.Errorf("agent definition %s: missing required field 'description'", path)
	}

	// 规范化并校验 `model`。与 AgentJsonSchema 保持一致：只要求「必须是非空字符串」——
	// 实际是否可用交给宿主的 ModelResolver / LLM router 判断。第三方模型名
	// （例如 "glm-5.1"）必须能原样往返。小写的 "inherit" 归一成 "inherit"
	// （表示「使用父级 client」的哨兵值）；其他值原样保留，便于 router 匹配。
	def.Model = strings.TrimSpace(def.Model)
	if strings.EqualFold(def.Model, "inherit") {
		def.Model = "inherit"
	}

	if !validPermissionModes[def.PermissionMode] {
		return nil, fmt.Errorf("agent definition %s: invalid permissionMode '%s'", path, def.PermissionMode)
	}

	if !validMemoryScopes[def.Memory] {
		return nil, fmt.Errorf("agent definition %s: invalid memory scope '%s'", path, def.Memory)
	}

	if !validIsolationModes[def.Isolation] {
		return nil, fmt.Errorf("agent definition %s: invalid isolation mode '%s'", path, def.Isolation)
	}

	return &def, nil
}

func (d *AgentDefinition) ToSpec() SubAgentSpec {
	return SubAgentSpec{
		Name:                 d.AgentType,
		Description:          d.WhenToUse,
		Tools:                d.Tools,
		DisallowedTools:      d.DisallowedTools,
		SystemPromptOverride: d.SystemPrompt,
		MaxTurns:             d.MaxTurns,
		Model:                d.Model,
		PermissionMode:       d.PermissionMode,
		Isolation:            d.Isolation,
		InitialPrompt:        d.InitialPrompt,
		OmitMewcodeMd:         d.OmitMewcodeMd,
		Skills:               d.Skills,
		Memory:               d.Memory,
		McpServers:           d.McpServers,
		RequiredMcpServers:   d.RequiredMcpServers,
		Hooks:                d.Hooks,
		Effort:               d.Effort,
	}
}

// HasRequiredMcpServers 当该 Agent 没有 MCP 要求，或者每个 required pattern
// 都能匹配到某个可用的 server 名（大小写不敏感的子串匹配）时返回 true。
func (d *AgentDefinition) HasRequiredMcpServers(availableServers []string) bool {
	if len(d.RequiredMcpServers) == 0 {
		return true
	}
	for _, pattern := range d.RequiredMcpServers {
		patLower := strings.ToLower(pattern)
		matched := false
		for _, server := range availableServers {
			if strings.Contains(strings.ToLower(server), patLower) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

