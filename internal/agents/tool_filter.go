package agents

import (
	"strings"

	"mewcode/internal/tools"
)

// AllAgentDisallowedTools 是任何子 Agent 都拿不到的工具，Agent 定义里的白名单也开不了它们。
// 其中 TaskOutput、ExitPlanMode、EnterPlanMode、Workflow 在当前工具集里并没有对应实现，
// 留着是为了把「子 Agent 不该有哪些能力」这份清单写全，过滤时遇到不认识的名字直接跳过，
// 将来补上同名工具就自动生效。
var AllAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// CustomAgentDisallowedTools 只是 ALL_AGENT_DISALLOWED_TOOLS 的一份拷贝。
// 单独留成一个 map，是为了将来加额外限制时不用动全局那份列表。
var CustomAgentDisallowedTools = map[string]bool{
	"TaskOutput":      true,
	"ExitPlanMode":    true,
	"EnterPlanMode":   true,
	"Agent":           true,
	"AskUserQuestion": true,
	"TaskStop":        true,
	"Workflow":        true,
}

// AsyncAgentAllowedTools 异步（后台）agent 只能用这些工具 —— 没有 Agent（不能嵌套派发）、
// 没有 TaskOutput、没有 ExitPlanMode、没有 TaskStop。本地工具命名映射为
// FILE_READ → ReadFile、FILE_EDIT → EditFile、FILE_WRITE → WriteFile。
var AsyncAgentAllowedTools = map[string]bool{
	"ReadFile":        true,
	"WebSearch":       true,
	"TodoWrite":       true,
	"Grep":            true,
	"WebFetch":        true,
	"Glob":            true,
	"Bash":            true,
	"EditFile":        true,
	"WriteFile":       true,
	"NotebookEdit":    true,
	"Skill":           true,
	"LoadSkill":       true,
	"SyntheticOutput": true,
	"ToolSearch":      true,
	// ToolSearch 只负责把 schema 读出来，真正调用要靠 mcp_call，
	// 两个得成对放行，否则子 Agent 看得见工具却调不动
	"mcp_call":        true,
	"EnterWorktree":   true,
	"ExitWorktree":    true,
}

// InProcessTeammateAllowedTools 当 sub-agent 以进程内 teammate 的形式被拉起时
// （ch15 Agent Teams），它在异步白名单之外还能拿到这些协作工具，
// 用来管理共享任务列表，以及给同伴发消息。
var InProcessTeammateAllowedTools = map[string]bool{
	"TaskCreate":  true,
	"TaskGet":     true,
	"TaskList":    true,
	"TaskUpdate":  true,
	"SendMessage": true,
	"CronCreate":  true,
	"CronDelete":  true,
	"CronList":    true,
}

// TeammateDisallowedTools 队友在协作工具之外额外被挡掉的工具。组建和解散团队
// 由 Lead 负责，队友只管干活和相互协调，不参与团队成员管理。
var TeammateDisallowedTools = []string{"TeamCreate", "TeamDelete"}

func IsMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__")
}

func FilterToolsForAgent(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync bool) *tools.Registry {
	return FilterToolsForAgentEx(reg, allowedTools, disallowedTools, isAsync, false, false)
}

// FilterToolsForAgentEx
//
// 依次应用以下几层：1. MCP 工具（mcp__*）—— 一律放行 2. ALL_AGENT_DISALLOWED_TOOLS ——
// 全局封禁（递归 / 仅主线程）3. CUSTOM_AGENT_DISALLOWED_TOOLS —— 只针对自定义
// （非内置）agent 4. ASYNC_AGENT_ALLOWED_TOOLS —— 后台 agent 走白名单；
// 如果该 agent 是进程内 teammate，再额外放行 IN_PROCESS_TEAMMATE_ALLOWED_TOOLS
// 5. Agent 定义的 disallowedTools —— 定义级黑名单 6. Agent 定义的 tools
// —— 定义级白名单求交（"*" 表示跳过这一层）
//
// isCustom：agent 来自 .mewcode/agents/，不是内置的。
// isInProcessTeammate：通过 ch15 里的 TeamCreate / SpawnTeammate 拉起的。
func FilterToolsForAgentEx(reg *tools.Registry, allowedTools, disallowedTools []string, isAsync, isCustom, isInProcessTeammate bool) *tools.Registry {
	disallowed := make(map[string]bool, len(disallowedTools))
	for _, name := range disallowedTools {
		disallowed[name] = true
	}

	allowed := make(map[string]bool, len(allowedTools))
	hasWhitelist := len(allowedTools) > 0 && !(len(allowedTools) == 1 && allowedTools[0] == "*")
	for _, name := range allowedTools {
		allowed[name] = true
	}

	filtered := tools.NewRegistry()
	for _, t := range reg.ListTools() {
		name := t.Name()

		// 第 1 层：MCP 工具一律放行。
		if IsMCPTool(name) {
			filtered.Register(t)
			continue
		}

		// 第 2 层：全局禁用（对所有 sub-agent 生效）。
		if AllAgentDisallowedTools[name] {
			continue
		}

		// 第 3 层：自定义 agent 的额外限制。
		if isCustom && CustomAgentDisallowedTools[name] {
			continue
		}

		// 第 4 层：异步 agent 白名单（含进程内 teammate 的扩展）。
		if isAsync && !AsyncAgentAllowedTools[name] {
			if isInProcessTeammate {
				// 进程内 teammate 还能用 Agent（仅限同步 subagent，在调用点校验）
				// 以及协作类工具。
				if name == "Agent" || InProcessTeammateAllowedTools[name] {
					filtered.Register(t)
					continue
				}
			}
			continue
		}

		// 第 5 层：定义级禁用。
		if disallowed[name] {
			continue
		}

		// 第 6 层：定义级放行（白名单求交）。
		if hasWhitelist && !allowed[name] {
			continue
		}

		filtered.Register(t)
	}
	return filtered
}

