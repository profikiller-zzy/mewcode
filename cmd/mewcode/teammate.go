package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mewcode/internal/agent"
	"mewcode/internal/config"
	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/mcp"
	"mewcode/internal/prompt"
	"mewcode/internal/session"
	"mewcode/internal/skills"
	"mewcode/internal/teams"
	"mewcode/internal/tools"
	"mewcode/internal/worktree"
)

// teammateArgs 存放本进程以 teammate worker 身份启动时
// 从命令行解析出来的参数（也就是由 tmux/iTerm 经
// teams.BuildTeammateCLI 拉起的那种场景）。
type teammateArgs struct {
	teamName   string
	memberName string
}

// parseTeammateFlags 在 os.Args 带 --teammate 标志时返回 (args, true)，
// 表示进入 teammate-worker 模式；其他情况返回 ok=false，
// 调用方应当照常启动 TUI。
//
// teams.BuildTeammateCLI 生成的命令行格式：
//
//	mewcode --teammate --team-name <t> --agent-name <n>
//
// 解析逻辑刻意做得极简：只认这个 worker 需要的三个标志，
// 而且它们必须作为独立的 token 出现。
func parseTeammateFlags(args []string) (teammateArgs, bool) {
	var out teammateArgs
	if len(args) == 0 || args[0] != "--teammate" {
		return out, false
	}
	i := 1
	for i < len(args) {
		switch args[i] {
		case "--team-name":
			if i+1 < len(args) {
				out.teamName = args[i+1]
				i += 2
				continue
			}
		case "--agent-name":
			if i+1 < len(args) {
				out.memberName = args[i+1]
				i += 2
				continue
			}
		}
		i++
	}
	return out, true
}

// runTeammate 把本进程作为已有团队的一个 worker 启动。它
// 加载与 TUI 运行时相同的配置，组装工具注册表，然后
// 进入 teams.RunInProcessTeammate。初始任务从邮箱里读取 ——
// Lead 在调用 tmux/iTerm spawn 之前已经把它写在那里了
// （见 teams.SpawnTeammate）。
func runTeammate(args teammateArgs) error {
	if args.teamName == "" || args.memberName == "" {
		return fmt.Errorf("--teammate requires --team-name and --agent-name")
	}

	cfg, err := config.LoadConfig("")
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	provider := cfg.Providers[0]

	wd, _ := os.Getwd()
	sessionID := session.NewID()

	// worker 进程会收到转发过来的 SIGINT/SIGTERM，这样关掉面板
	// 或在标签页里按 Ctrl-C 时，能干净地取消 loop，
	// 让 defer 里的清理逻辑跑完。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Skill 清单随首条 system-reminder 注入对话（见下面的 ag.SkillSection），
	// 同时挂到 LoadSkill 工具上供模型按名字加载。
	skillCatalog := skills.LoadCatalog(wd)
	env := prompt.DetectEnvironment(wd)
	env.Model = provider.Model
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{})

	client, err := llm.NewClient(&provider, systemPrompt)
	if err != nil {
		return fmt.Errorf("create LLM client: %w", err)
	}

	// 队员进程直接从磁盘上的 config.json 把团队捞回来，这样它看到的队友名单
	// 和 Lead 那边是同一份。团队目录在用户主目录下，不受 worktree 换工作目录影响。
	teamMgr := teams.NewTeamManager()
	team := teamMgr.GetTeam(args.teamName)
	if team == nil {
		// 配置还没落盘（例如 Lead 刚建完团队就 spawn），退化成本地构造一份，
		// 邮箱目录按同样的约定拼出来，投递仍然对得上。
		team = teams.NewTeam(args.teamName, teams.ModeInProcess)
		teamMgr.CreateTeamWith(team)
	}

	registry := buildTeammateRegistry(ctx, teammateToolOptions{
		WorkDir:    wd,
		Protocol:   provider.Protocol,
		SessionID:  sessionID,
		TeamMgr:    teamMgr,
		TeamName:   args.teamName,
		MemberName: args.memberName,
		MCPServers: cfg.MCPServers,

		BaseURL:       provider.BaseURL,
		ContextWindow: provider.GetContextWindow(),
	})

	member := team.AddMember(args.memberName, client, registry, provider.Protocol)
	member.AgentRef.SkillSection = buildPrintSkillSection(skillCatalog)

	// Skill 工具的宿主由队友自己的 Agent 担任，所以要等 AddMember 建好 Agent
	// 之后再接入。没有 ForkHost，声明 fork 模式的 skill 会退回 inline 执行。
	registry.Register(&skills.LoadSkillTool{Catalog: skillCatalog, Host: member.AgentRef})
	registry.Register(&skills.InstallSkillTool{Catalog: skillCatalog})

	addendum := teams.BuildTeammateAddendum(args.teamName, args.memberName, nil)

	// 这里不传初始 prompt：Lead 在 spawn 之前已经把第一条消息
	// 写进邮箱，loop 的第一次空闲轮询就会取到它。传 ""
	// 是为了不让 RunInProcessTeammate 再注入
	// 一条重复的用户消息。
	fmt.Fprintf(os.Stderr, "[teammate %s/%s] booted, awaiting tasks\n", args.teamName, args.memberName)
	return teams.RunInProcessTeammate(ctx, team, member, "", addendum, streamEventsToStderr())
}

// teammateToolOptions 汇总组装队友工具集需要的外部依赖。
type teammateToolOptions struct {
	WorkDir    string
	Protocol   string
	SessionID  string
	TeamMgr    *teams.TeamManager
	TeamName   string
	MemberName string
	MCPServers []config.MCPServerConfig
	// BaseURL 和 ContextWindow 供 MCP 加载模式分流用：判官方端点看前者，
	// 判 schema 体量占比看后者。
	BaseURL       string
	ContextWindow int
}

// buildTeammateRegistry 组装队友工具集：文件与命令工具、工具检索、Worktree
// 切换、MCP 扩展，再加上团队协作工具（按自己的名字发消息，以及读写团队共享
// 任务板）。任务板按团队名解析到同一份 tasks.json，所以队友之间看到的是同一
// 张表。
//
// Agent 不在其中，调用树到队友这一层为止，队友不再往下派子 Agent。
// TeamCreate 与 TeamDelete 也不在其中，组建和解散团队是 Lead 的职责。
//
// Skill 工具需要 Agent 实例充当宿主，由调用方在 Agent 建好之后单独接入。
func buildTeammateRegistry(ctx context.Context, opts teammateToolOptions) *tools.Registry {
	registry := tools.CreateDefaultToolsWithWorkDir(opts.WorkDir).Registry

	registry.Register(&tools.ToolSearchTool{Registry: registry, Protocol: opts.Protocol})
	registry.Register(&tools.McpCallTool{Registry: registry})
	registry.Register(&tools.SyntheticOutputTool{})

	gitRoot := worktree.FindCanonicalGitRoot(opts.WorkDir)
	registry.Register(&tools.EnterWorktreeTool{SessionID: opts.SessionID, RepoRoot: gitRoot})
	registry.Register(&tools.ExitWorktreeTool{RepoRoot: gitRoot})

	registry.Register(&teams.SendMessageTool{TeamMgr: opts.TeamMgr, SenderName: opts.MemberName})
	registry.Register(&teams.TaskCreateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName, AgentName: opts.MemberName})
	registry.Register(&teams.TaskGetTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskListTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})
	registry.Register(&teams.TaskUpdateTool{TeamMgr: opts.TeamMgr, TeamName: opts.TeamName})

	if len(opts.MCPServers) > 0 {
		mgr := mcp.NewManager()
		serverConfigs := make([]mcp.ServerConfig, 0, len(opts.MCPServers))
		for _, c := range opts.MCPServers {
			serverConfigs = append(serverConfigs, mcp.ServerConfig{
				Name:      c.Name,
				Command:   c.Command,
				Args:      c.Args,
				URL:       c.URL,
				Transport: c.Transport,
				Headers:   c.Headers,
				Env:       c.Env,
			})
		}
		mgr.LoadConfigs(serverConfigs)
		mgr.RegisterAllTools(ctx, registry)
		// 工具都在位了才算得准 schema 总量跟上下文窗口的比例
		mcp.DecideAndApply(registry, opts.BaseURL, opts.ContextWindow)
	}

	return registry
}

// streamEventsToStderr 返回一个 channel，把每个 agent 事件以可读的形式
// 转发到 stderr。worker 进程没有 TUI，
// 这样 tmux/iTerm 面板里至少有东西可看。
func streamEventsToStderr() chan<- agent.AgentEvent {
	ch := make(chan agent.AgentEvent, 32)
	go func() {
		for ev := range ch {
			switch e := ev.(type) {
			case agent.StreamText:
				fmt.Fprint(os.Stderr, e.Text)
			case agent.ToolUseEvent:
				fmt.Fprintf(os.Stderr, "\n[tool %s]\n", e.ToolName)
			case agent.ToolResultEvent:
				summary := strings.TrimSpace(e.Output)
				if len(summary) > 200 {
					summary = summary[:200] + "..."
				}
				fmt.Fprintf(os.Stderr, "[result] %s\n", summary)
			case agent.ErrorEvent:
				fmt.Fprintf(os.Stderr, "[error] %s\n", e.Message)
			case agent.LoopComplete:
				fmt.Fprintf(os.Stderr, "[turn done after %d steps]\n", e.TotalTurns)
			}
		}
	}()
	return ch
}

// 当 conversation 只通过 Member.Conv 被间接引用时，
// 用这个 _ 消掉未使用导入的告警。
var _ = conversation.NewManager
