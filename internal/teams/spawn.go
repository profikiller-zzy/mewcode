package teams

import (
	"context"
	"fmt"
	"os"
	"strings"

	"mewcode/internal/agent"
	"mewcode/internal/llm"
	"mewcode/internal/permissions"
	"mewcode/internal/tools"
)

// TeammateSpawnConfig 收集 SpawnTeammate 需要的全部参数。
// 各字段在不同 backend 下的生效情况：
// Team / MemberName / Task / Addendum：总是用到。
// Client / Registry / Protocol：只有 in-process 用到；外部 backend
// 会由新起的进程自己从它自己的 config 里加载。
// Workdir：可选的工作目录覆盖。非空时，in-process 成员的
// Agent.WorkDir 会指向它，
// tmux/iTerm 派生时会 cd 进这个路径。用于
// worktree 隔离，免得并发的 teammate 抢同一批文件。
type TeammateSpawnConfig struct {
	Team       *Team
	MemberName string
	Task       string
	Addendum   string

	Client   llm.Client
	Registry *tools.Registry
	Protocol string

	Workdir string

	// Checker 是队友的权限检查器。Lead 派人时标了 plan_mode_required，
	// 这里就是一个 ModePlan 的 checker，队友只能读不能改，直到计划获批。
	Checker *permissions.Checker
}

// SpawnResult 承载 SpawnTeammate 按 backend 返回的句柄。in-process 派生
// 拿到的是 event channel；tmux/iTerm 派生拿到的 pane 句柄也会存到
// Member.PaneID 上，供后续拆除使用。
type SpawnResult struct {
	Mode    TeamMode
	EventCh <-chan agent.AgentEvent // 仅 in-process
	PaneID  string                  // 仅 tmux/iTerm
}

// SpawnTeammate 创建一个新团队成员，并在团队当前选定的 backend
// （Team.Mode）下启动它。它是 Agent 工具 team_name 代码路径使用的唯一入口，
// 具体分派见下面。
//
// 对外部 backend，teammate 的初始任务会在新进程启动前通过 mailbox 投递，
// 这样 teammate 在第一次 idle 轮询时就能看到任务。
func SpawnTeammate(ctx context.Context, cfg TeammateSpawnConfig) (*SpawnResult, error) {
	if cfg.Team == nil {
		return nil, fmt.Errorf("SpawnTeammate: team is required")
	}
	if cfg.MemberName == "" {
		return nil, fmt.Errorf("SpawnTeammate: member name is required")
	}

	// 把成员名字登记到全局名称注册表，供 SendMessage 按名字解析投递
	GetNameRegistry().Register(cfg.MemberName, cfg.MemberName)

	switch cfg.Team.Mode {
	case ModeInProcess:
		ch := StartInProcessMember(
			ctx,
			cfg.Team,
			cfg.MemberName,
			cfg.Client,
			cfg.Registry,
			cfg.Protocol,
			cfg.Task,
			cfg.Addendum,
		)
		// Workdir 作用到刚注册的成员的 Agent 上，这样每个 file/Bash 工具的
		// 相对路径都解析到隔离目录里。
		if m, ok := cfg.Team.Members[cfg.MemberName]; ok && m.AgentRef != nil {
			if cfg.Workdir != "" {
				m.AgentRef.WorkDir = cfg.Workdir
			}
			m.AgentRef.Checker = cfg.Checker
		}
		return &SpawnResult{Mode: ModeInProcess, EventCh: ch}, nil

	case ModeTmux:
		// 外部进程通过 mailbox 领取任务。在派生动它之前先把初始任务投进去，
		// 这样新进程第一次轮询就能看到活。
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		paneID, err := spawnTmuxTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, paneID)
		return &SpawnResult{Mode: ModeTmux, PaneID: paneID}, nil

	case ModeITerm:
		if cfg.Task != "" {
			_ = cfg.Team.MailBox.Send(cfg.MemberName, FileMailMessage{
				From: LeadName,
				Text: cfg.Task,
			})
		}
		cliCommand, err := BuildTeammateCLI(cfg.Team.Name, cfg.MemberName, cfg.Workdir)
		if err != nil {
			return nil, err
		}
		tabID, err := spawnITermTeammate(cfg.Team.Name, cfg.MemberName, cliCommand)
		if err != nil {
			return nil, err
		}
		cfg.Team.recordExternalMember(cfg.MemberName, tabID)
		return &SpawnResult{Mode: ModeITerm, PaneID: tabID}, nil
	}

	return nil, fmt.Errorf("unknown team mode: %s", cfg.Team.Mode)
}

// recordExternalMember 把 tmux/iTerm 派生出来的 teammate 登记到内存里的
// Members map，好让 StopMember 之后能定位到它做拆除。外部 teammate 在这一侧
// 没有 AgentRef —— 它们的 LLM 在新起的进程里 —— 所以这条记录只是一个
// name+handle 的占位，供 Lead 的协调工具使用。
func (t *Team) recordExternalMember(name, paneID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Members[name] = &Member{
		Name:   name,
		Active: true,
		PaneID: paneID,
	}
}

// BuildTeammateCLI 返回这样一条 shell 命令：在新的终端 pane/tab 里执行后，
// 会以 teammate 模式启动本 mewcode 二进制，加入给定的 team/member。workdir
// 参数决定新进程在哪跑；传 "" 会回退到 Lead 的当前目录，
// 这样 mailbox 路径解析出来一致。worktree 隔离是非空 workdir 的典型用法。
//
// 输出格式与 cmd/mewcode/main.go 里 teammate-mode 分支解析的命令行一致。
func BuildTeammateCLI(teamName, memberName, workdir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate mewcode binary: %w", err)
	}
	if workdir == "" {
		workdir, _ = os.Getwd()
	}
	return fmt.Sprintf(
		"cd %s && %s --teammate --team-name %s --agent-name %s",
		shellQuote(workdir),
		shellQuote(exe),
		shellQuote(teamName),
		shellQuote(memberName),
	), nil
}

// shellQuote 把值包一层，好安全地放进 /bin/sh -c 的参数里。用单引号转义
// 是因为 tmux send-keys 和 osascript `write text` 都会把字符串交给 shell 解释。
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
