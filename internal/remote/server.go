package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"mewcode/internal/agent"
	"mewcode/internal/agents"
	"mewcode/internal/commands"
	"mewcode/internal/compact"
	"mewcode/internal/config"
	"mewcode/internal/conversation"
	"mewcode/internal/filehistory"
	"mewcode/internal/hooks"
	"mewcode/internal/llm"
	"mewcode/internal/mcp"
	"mewcode/internal/memory"
	extractor "mewcode/internal/memory/extractor"
	"mewcode/internal/permissions"
	"mewcode/internal/planfile"
	"mewcode/internal/prompt"
	"mewcode/internal/session"
	"mewcode/internal/skills"
	"mewcode/internal/teams"
	"mewcode/internal/todo"
	"mewcode/internal/tools"
	"mewcode/internal/worktree"
)

// 下行消息（Server → Web UI）
type wsMessage struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// 上行消息（Web UI → Server）
type clientMessage struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type userMessageData struct {
	Content string `json:"content"`
}

type permResponseData struct {
	ID       string `json:"id"`
	Response string `json:"response"` // "allow" / "deny" / "allowAlways" 三种取值
}

type askUserResponseData struct {
	ID      string            `json:"id"`
	Answers map[string]string `json:"answers"`
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Server 是 Remote Control 的核心，桥接 Agent 事件和 WebSocket 客户端
type Server struct {
	providers  []config.ProviderConfig
	mcpConfigs []config.MCPServerConfig
	hookCfgs   []hooks.Hook
	addr       string

	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
	// stateMu 把「会话替换 / 控制命令」和「提交 prompt」这两件事串行化。
	// 消费某个 run 的事件流期间绝不会持有它。
	stateMu sync.Mutex

	ag           *agent.Agent
	agentLoop    *agent.AgentLoop
	trace        *agent.TraceRecorder
	conv         *conversation.Manager
	registry     *tools.Registry
	defaultTools tools.DefaultTools
	client       llm.Client
	sessionID    string
	fileHistory  *filehistory.History

	askUserCh chan tools.AskUserRequest

	// 阻塞等待 Web 端回复权限/ask_user
	pendingPermMu sync.Mutex
	pendingPerms  map[string]chan<- agent.PermissionResponse

	pendingAskMu sync.Mutex
	pendingAsks  map[string]chan tools.QuestionResponse

	cmdRegistry     *commands.Registry
	skillCatalog    *skills.Catalog
	taskMgr         *agents.TaskManager
	todoList        *todo.TaskList
	memoryMgr       *memory.Manager
	memoryExtractor *extractor.Extractor
	teamMgr         *teams.TeamManager
	mcpMgr          *mcp.Manager

	instructionsContent   string
	memoryContent         string
	mcpInstructions       string
	enableCoordinatorMode bool
	forkDisabled          bool
}

func NewServer(providers []config.ProviderConfig, mcpConfigs []config.MCPServerConfig, hookCfgs []hooks.Hook, addr string, enableCoordinatorMode, forkDisabled bool) *Server {
	return &Server{
		providers:             providers,
		mcpConfigs:            mcpConfigs,
		hookCfgs:              hookCfgs,
		addr:                  addr,
		enableCoordinatorMode: enableCoordinatorMode,
		forkDisabled:          forkDisabled,
		conns:                 make(map[*websocket.Conn]struct{}),
		pendingPerms:          make(map[string]chan<- agent.PermissionResponse),
		pendingAsks:           make(map[string]chan tools.QuestionResponse),
	}
}

func (s *Server) Run() error {
	if err := s.initAgent(); err != nil {
		return fmt.Errorf("初始化 Agent 失败: %w", err)
	}

	s.initMCPServers()
	if err := s.registerLoopSession(s.sessionID); err != nil {
		return fmt.Errorf("初始化 Agent Loop 失败: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/ws", s.handleWS)

	fmt.Printf("\n  🌐 Remote UI: http://localhost%s\n\n", s.addr)
	return http.ListenAndServe(s.addr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	s.mu.Lock()
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()

	conn.SetReadLimit(4 << 20) // 4MB

	s.stateMu.Lock()
	connectedSessionID := s.sessionID
	s.stateMu.Unlock()
	s.send(wsMessage{Type: "connected", Data: map[string]string{
		"session": connectedSessionID,
		"cwd":     mustGetwd(),
	}})

	// 推送命令列表
	s.send(wsMessage{Type: "commands", Data: s.buildCommandList()})

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("WebSocket read error: %v", err)
			}
			return
		}

		var msg clientMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "user_message":
			var data userMessageData
			json.Unmarshal(msg.Data, &data)
			go s.handleUserMessage(data.Content)

		case "permission_response":
			var data permResponseData
			json.Unmarshal(msg.Data, &data)
			s.handlePermissionResponse(data)

		case "ask_user_response":
			var data askUserResponseData
			json.Unmarshal(msg.Data, &data)
			s.handleAskUserResponse(data)

		case "cancel":
			s.stateMu.Lock()
			sessionID := s.sessionID
			s.stateMu.Unlock()
			if s.agentLoop != nil {
				_ = s.agentLoop.Cancel(sessionID)
			}

		case "ping":
			// 应用层保活：客户端每 10s 发一次，服务端回 pong
			s.send(wsMessage{Type: "pong", Data: nil})
		}
	}
}

// initAgent 复刻 TUI 中 initSingleProviderMsg 的初始化流程
func (s *Server) initAgent() error {
	p := &s.providers[0]
	wd, _ := os.Getwd()

	s.askUserCh = make(chan tools.AskUserRequest, 1)
	s.defaultTools = tools.CreateDefaultToolsWithWorkDir(wd)
	s.registry = s.defaultTools.Registry
	s.registry.Register(&tools.AskUserQuestionTool{RequestCh: s.askUserCh})

	s.cmdRegistry = commands.CreateDefaultRegistry()
	s.skillCatalog = skills.LoadCatalog(wd)
	s.instructionsContent = loadCustomInstructions(wd)
	s.memoryContent = memory.LoadAutoMemoryPrompt(wd)

	skillSection := buildSkillSection(s.skillCatalog, wd)

	env := prompt.DetectEnvironment(wd)
	env.Model = p.Model
	// 系统提示词只放跟项目无关的产品定义，这样它全局一份、缓存能一直命中。
	// 指令、自动记忆和 Skill 清单都跟着项目走，由
	// conversation.InjectLongTermMemory 以 system-reminder 注入首条消息。
	systemPrompt := prompt.BuildSystemPrompt(env, prompt.BuildOptions{})

	client, err := llm.NewClient(p, systemPrompt)
	if err != nil {
		return err
	}
	s.client = client
	s.conv = conversation.NewManager()
	s.sessionID = session.NewID()
	s.fileHistory = filehistory.New(wd, s.sessionID)
	s.defaultTools.EditFile.FileHistory = s.fileHistory
	s.defaultTools.WriteFile.FileHistory = s.fileHistory

	llm.ResolveContextWindow(context.Background(), p)
	s.registerTools(client, p, wd)

	ag := agent.New(client, s.registry, p.Protocol)
	ag.ContextWindow = p.GetContextWindow()
	ag.MaxOutputTokens = p.GetMaxOutputTokens()
	ag.Instructions = s.instructionsContent
	ag.MemoryContent = s.memoryContent
	ag.SkillSection = skillSection
	ag.FileHistory = s.fileHistory
	ag.SetSessionID(s.sessionID)

	sandboxAllow := []string{memory.GetAutoMemPath(wd)}
	if userMem := memory.GetUserAutoMemPath(); userMem != "" {
		sandboxAllow = append(sandboxAllow, userMem)
	}
	ag.Checker = permissions.NewChecker(
		permissions.NewPathSandbox(wd, sandboxAllow...),
		permissions.NewRuleEngine(wd),
		permissions.ModeDefault,
	)

	if len(s.hookCfgs) > 0 {
		eng := hooks.NewEngine()
		eng.LoadHooks(s.hookCfgs)
		eng.AgentRunner = newAgentHookRunner(client)
		ag.Hooks = eng
	}

	// 队员干完活的回传落在 lead 信箱里，每轮排空成 system-reminder 交给 Lead。
	// coordinator 模式下 Lead 只能靠这条通道知道队员的进展，断了就等于派出去石沉大海。
	ag.NotificationFn = func() []string {
		var messages []string
		if s.taskMgr != nil {
			for _, n := range s.taskMgr.DrainNotifications() {
				messages = append(messages, fmt.Sprintf(
					"<task-notification>\n<task_id>%s</task_id>\n<status>%s</status>\n<summary>Agent \"%s\" %s</summary>\n<result>%s</result>\n</task-notification>",
					n.TaskID, n.Status, n.Name, n.Status, n.Output))
			}
		}
		return append(messages, teams.DrainLeadMailbox(s.teamMgr)...)
	}
	ag.ToolNameFilter = teams.CoordinatorToolFilter(s.enableCoordinatorMode)
	ag.CoordinatorActiveFn = teams.CoordinatorActiveFn(s.enableCoordinatorMode)

	s.ag = ag
	s.trace = agent.NewTraceRecorder(agent.NewJSONLTraceStore(wd))
	ag.Trace = s.trace
	s.agentLoop = agent.NewAgentLoop(s.trace)

	if at, ok := s.registry.Get("Agent").(*agents.AgentTool); ok {
		at.ParentChecker = ag.Checker
	}

	s.wireSkillsToAgent(wd)
	s.memoryExtractor = installMemExtractor(ag, wd, p, client, s.registry, s.conv)

	gitRoot := worktree.FindCanonicalGitRoot(wd)
	s.registry.Register(&tools.EnterWorktreeTool{SessionID: s.sessionID, RepoRoot: gitRoot})
	s.registry.Register(&tools.ExitWorktreeTool{RepoRoot: gitRoot})
	worktree.StartCleanupLoop(context.Background())

	return nil
}

func (s *Server) registerTools(client llm.Client, p *config.ProviderConfig, wd string) {
	s.taskMgr = agents.NewTaskManager()
	store := todo.NewStore(wd, s.sessionID)
	s.todoList = todo.NewTaskList(store)
	s.memoryMgr = memory.NewManager(wd)
	loader := agents.NewAgentLoader(wd)
	loader.LoadAll()
	s.teamMgr = teams.NewTeamManager()

	s.registry.Register(&tools.ExitPlanModeTool{
		IsPlanMode: func() bool {
			return s.ag != nil && s.ag.Checker != nil && s.ag.Checker.Mode == permissions.ModePlan
		},
		PlanExists: func() bool { return false },
	})
	s.registry.Register(&todo.TaskCreateTool{List: s.todoList})
	s.registry.Register(&todo.TaskGetTool{List: s.todoList})
	s.registry.Register(&todo.TaskListTool{List: s.todoList})
	s.registry.Register(&todo.TaskUpdateTool{List: s.todoList})
	s.registry.Register(&tools.ToolSearchTool{Registry: s.registry, Protocol: p.Protocol})
	s.registry.Register(&tools.McpCallTool{Registry: s.registry})
	s.registry.Register(&teams.TeamCreateTool{TeamMgr: s.teamMgr})
	s.registry.Register(&teams.TeamDeleteTool{TeamMgr: s.teamMgr})
	s.registry.Register(&teams.SendMessageTool{TeamMgr: s.teamMgr, SenderName: "lead"})
	s.registry.Register(&teams.TaskStopTool{TeamMgr: s.teamMgr})
	s.registry.Register(&tools.SyntheticOutputTool{})
	subProgressCh := make(chan agents.SubAgentProgress, 32)
	s.registry.Register(&agents.AgentTool{
		Client:        client,
		ModelResolver: llm.NewModelResolver(*p),
		ModelAliases:  llm.AvailableModelAliases(*p),
		// 子 Agent 的压缩阈值也按 provider 的真实窗口换算。
		ContextWindow:   p.GetContextWindow(),
		MaxOutputTokens: p.GetMaxOutputTokens(),
		Registry:        s.registry,
		Protocol:        p.Protocol,
		ProgressCh:    subProgressCh,
		Loader:        loader,
		Conversation:  s.conv,
		TeamMgr:       s.teamMgr,
		ForkDisabled:  s.forkDisabled,
	})
}

func (s *Server) initMCPServers() {
	if len(s.mcpConfigs) == 0 {
		return
	}
	mgr := mcp.NewManager()
	var serverConfigs []mcp.ServerConfig
	for _, c := range s.mcpConfigs {
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
	result := mgr.ConnectAll(context.Background())
	s.mcpMgr = result.Mgr
	for _, t := range result.Tools {
		s.registry.Register(t)
	}
	for _, errMsg := range result.Errors {
		log.Printf("MCP error: %s", errMsg)
	}
	// 工具都在位了才算得准 schema 总量跟上下文窗口的比例
	if len(s.providers) > 0 {
		p := &s.providers[0]
		mcp.DecideAndApply(s.registry, p.BaseURL, p.GetContextWindow())
	}
	if len(result.Servers) > 0 {
		toolsByServer := make(map[string][]string)
		for _, t := range result.Tools {
			toolName := t.Name()
			for _, srv := range result.Servers {
				if strings.HasPrefix(toolName, mcp.MCPToolNamePrefix(srv.Name)) {
					toolsByServer[srv.Name] = append(toolsByServer[srv.Name], toolName)
					break
				}
			}
		}
		var mcpParts []string
		for _, srv := range result.Servers {
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("## %s\n", srv.Name))
			if srv.Instructions != "" {
				sb.WriteString(srv.Instructions + "\n")
			}
			if toolNames, ok := toolsByServer[srv.Name]; ok && len(toolNames) > 0 {
				sb.WriteString("\nAvailable tools: " + strings.Join(toolNames, ", "))
			}
			mcpParts = append(mcpParts, sb.String())
		}
		s.mcpInstructions = "# MCP Server Instructions\n\nThe following MCP servers are connected. Use their tools when the user asks.\n\n" + strings.Join(mcpParts, "\n\n")
	}
}

func (s *Server) handleUserMessage(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}

	// 斜杠命令处理
	if strings.HasPrefix(content, "/") {
		s.handleSlashCommand(content)
		return
	}

	s.stateMu.Lock()
	if s.agentLoop == nil {
		s.stateMu.Unlock()
		s.send(wsMessage{Type: "error", Data: map[string]string{"message": "Agent loop is not initialized."}})
		return
	}
	submission, err := s.agentLoop.Submit(context.Background(), s.sessionID, content)
	s.stateMu.Unlock()
	if err != nil {
		s.send(wsMessage{Type: "error", Data: map[string]string{"message": err.Error()}})
		return
	}
	askDone := make(chan struct{})
	go s.listenForAskUser(askDone)
	s.consumeAgentEvents(submission.Events)
	close(askDone)
}

func (s *Server) registerLoopSession(sessionID string) error {
	if s.agentLoop == nil || s.ag == nil || s.conv == nil {
		return fmt.Errorf("agent loop dependencies are not initialized")
	}
	wd, _ := os.Getwd()
	return s.agentLoop.RegisterSession(sessionID, agent.SessionOptions{
		Agent:        s.ag,
		Conversation: s.conv,
		QueueStore:   agent.NewJSONQueueStore(wd),
		BeforeRun: func(_ context.Context, _, _ string, _ *agent.Agent, conv *conversation.Manager) {
			s.mu.Lock()
			instructions := s.mcpInstructions
			s.mcpInstructions = ""
			s.mu.Unlock()
			if instructions != "" {
				conv.AddSystemReminder(instructions)
			}
		},
	})
}

func (s *Server) buildCommandList() []map[string]string {
	var list []map[string]string
	for _, cmd := range s.cmdRegistry.ListCommands() {
		list = append(list, map[string]string{
			"name":        cmd.Name,
			"description": cmd.Description,
		})
	}
	return list
}

func (s *Server) handleSlashCommand(input string) {
	defer func() {
		if r := recover(); r != nil {
			s.send(wsMessage{Type: "error", Data: map[string]string{
				"message": fmt.Sprintf("Command panic: %v", r),
			}})
		}
	}()

	name, args := commands.Parse(input)
	if name == "" {
		return
	}

	cmd := s.cmdRegistry.Find(name)
	if cmd == nil {
		s.send(wsMessage{Type: "error", Data: map[string]string{
			"message": fmt.Sprintf("Unknown command: /%s — type /help to see available commands", name),
		}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}
	promptStateLocked := false
	if cmd.Type != commands.TypePrompt {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
	} else {
		s.stateMu.Lock()
		promptStateLocked = true
		defer func() {
			if promptStateLocked {
				s.stateMu.Unlock()
			}
		}()
	}
	if cmd.Type != commands.TypePrompt && s.agentLoop != nil && s.agentLoop.IsBusy(s.sessionID) {
		s.send(wsMessage{Type: "error", Data: map[string]string{
			"message": fmt.Sprintf("/%s cannot run while this session is busy; wait for queued runs or cancel the active run first", name),
		}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}

	if args == "" && cmd.ArgPrompt != "" {
		s.send(wsMessage{Type: "system", Data: map[string]string{"message": cmd.ArgPrompt}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}

	ctx := s.buildCommandContext(args)

	switch cmd.Type {
	case commands.TypeLocal:
		if cmd.Handler != nil {
			result := cmd.Handler(ctx)
			s.send(wsMessage{Type: "system", Data: map[string]string{"message": result}})
		}
		s.send(wsMessage{Type: "command_done", Data: nil})

	case commands.TypeLocalUI:
		switch name {
		case "clear":
			s.conv.ReplaceWith(conversation.NewManager())
			if s.ag != nil {
				s.ag.ClearActiveSkills()
				// Skill 的工具收窄随对话一起清掉，但 coordinator 的约束不清：
				// 它跟着 Team 走，Team 还在就该继续管着 Lead。
				s.ag.SetToolFilter(teams.CoordinatorToolFilter(s.enableCoordinatorMode))
			}
			s.send(wsMessage{Type: "clear", Data: nil})

		case "compact":
			s.handleCompact()
			return // compact 自己管 streaming 状态

		case "plan":
			s.handlePlan(args)
			if args != "" {
				return // 带参数的 plan 走 agent 流程
			}

		case "resume":
			s.handleResume(args)
			return // resume 需要交互或直接恢复

		case "rewind":
			s.send(wsMessage{Type: "system", Data: map[string]string{
				"message": "Rewind is not yet supported in remote mode.",
			}})
		}
		s.send(wsMessage{Type: "command_done", Data: nil})

	case commands.TypePrompt:
		if cmd.Handler == nil {
			return
		}
		prompt := cmd.Handler(ctx)
		// Prompt 命令和普通输入共用 session FIFO，不绕过 AgentLoop
		// 去并发修改 conversation。
		submission, err := s.agentLoop.Submit(context.Background(), s.sessionID, prompt)
		s.stateMu.Unlock()
		promptStateLocked = false
		if err != nil {
			s.send(wsMessage{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
		askDone := make(chan struct{})
		go s.listenForAskUser(askDone)
		s.consumeAgentEvents(submission.Events)
		close(askDone)
	}
}

func (s *Server) buildCommandContext(args string) *commands.Context {
	wd, _ := os.Getwd()
	return &commands.Context{
		Args:       args,
		TokenCount: func() (int, int) { return 0, 0 },
		PermissionMode: func() string {
			if s.ag != nil && s.ag.Checker != nil {
				return string(s.ag.Checker.Mode)
			}
			return "default"
		},
		ToolCount: func() int { return len(s.registry.ListTools()) },
		SessionInfo: func() string {
			return fmt.Sprintf("Session: %s\nCWD: %s", s.sessionID, wd)
		},
		SkillList: func() []commands.SkillInfo {
			if s.skillCatalog == nil {
				return nil
			}
			var list []commands.SkillInfo
			for _, meta := range s.skillCatalog.List() {
				list = append(list, commands.SkillInfo{
					Name:        meta.Name,
					Description: meta.Description,
				})
			}
			return list
		},
		MCPInfo: func() string {
			if s.mcpMgr == nil {
				return ""
			}
			return "MCP connected"
		},
		WorkDir: wd,
		Model:   s.providers[0].Model,
	}
}

func (s *Server) handleCompact() {
	if s.client == nil || s.conv == nil {
		s.send(wsMessage{Type: "error", Data: map[string]string{"message": "Compact requires an active provider."}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}
	s.send(wsMessage{Type: "system", Data: map[string]string{"message": "Compacting conversation…"}})
	wd, _ := os.Getwd()
	window := s.providers[0].GetContextWindow()
	var recovery *compact.RecoveryState
	var schemas []map[string]any
	if s.ag != nil {
		recovery = s.ag.RecoveryState
		schemas = s.ag.Registry.GetAllSchemas(s.ag.Protocol)
	}
	msg, err := compact.ForceCompact(context.Background(), s.conv, s.client, wd, s.sessionID, window, recovery, schemas)
	if err != nil {
		s.send(wsMessage{Type: "error", Data: map[string]string{"message": err.Error()}})
	} else {
		s.send(wsMessage{Type: "system", Data: map[string]string{"message": "⟳ " + msg}})
	}
	s.send(wsMessage{Type: "command_done", Data: nil})
}

func (s *Server) handlePlan(args string) {
	wd, _ := os.Getwd()
	if s.ag == nil || s.ag.Checker == nil {
		s.send(wsMessage{Type: "error", Data: map[string]string{"message": "Agent not initialized."}})
		return
	}
	s.ag.Checker.Mode = permissions.ModePlan
	planPath := planfile.GetOrCreatePlanPath(wd)
	s.ag.Checker.PlanFilePath = planPath
	s.send(wsMessage{Type: "system", Data: map[string]string{
		"message": fmt.Sprintf("Entered Plan mode. Plan file: %s\nExplore the codebase and design your approach.", planPath),
	}})

	if args != "" {
		// 带参数的 plan 也是一个独立 run，与其他 prompt 严格 FIFO。
		submission, err := s.agentLoop.Submit(context.Background(), s.sessionID, args)
		if err != nil {
			s.send(wsMessage{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
		askDone := make(chan struct{})
		go s.listenForAskUser(askDone)
		s.consumeAgentEvents(submission.Events)
		close(askDone)
	}
}

func (s *Server) handleResume(args string) {
	wd, _ := os.Getwd()
	sessions := session.ListSessions(wd)

	if args == "" {
		// 没有参数，列出可选会话
		if len(sessions) == 0 {
			s.send(wsMessage{Type: "system", Data: map[string]string{"message": "No previous sessions found."}})
			s.send(wsMessage{Type: "command_done", Data: nil})
			return
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Available sessions (%d):\n\n", len(sessions)))
		for i, sess := range sessions {
			if i >= 20 {
				sb.WriteString(fmt.Sprintf("  … and %d more\n", len(sessions)-20))
				break
			}
			first := sess.FirstMessage
			if len(first) > 60 {
				first = first[:60] + "…"
			}
			sb.WriteString(fmt.Sprintf("  %d. [%s] %s (%d msgs)\n", i+1, sess.ID, first, sess.MessageCount))
		}
		sb.WriteString("\nUsage: /resume <number> or /resume <session-id>")
		s.send(wsMessage{Type: "system", Data: map[string]string{"message": sb.String()}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}

	// 有参数，直接恢复指定会话
	targetID := strings.TrimSpace(args)
	var idx int
	if n, _ := fmt.Sscanf(targetID, "%d", &idx); n == 1 && idx >= 1 && idx <= len(sessions) {
		targetID = sessions[idx-1].ID
	}

	msgs := session.LoadSession(wd, targetID)
	if len(msgs) == 0 {
		s.send(wsMessage{Type: "error", Data: map[string]string{
			"message": fmt.Sprintf("Session '%s' not found or empty.", targetID),
		}})
		s.send(wsMessage{Type: "command_done", Data: nil})
		return
	}

	// 重建会话；保留 Manager 指针，避免 session worker/记忆提取器
	// 继续持有已经失效的 conversation。
	oldSessionID := s.sessionID
	if s.agentLoop != nil {
		_ = s.agentLoop.UnregisterSession(oldSessionID)
	}
	s.conv.ReplaceWith(conversation.NewManager())
	s.sessionID = targetID
	if s.ag != nil {
		s.ag.SetSessionID(s.sessionID)
	}

	boundary, after, compacted := session.FindLastCompactBoundary(msgs)
	var replay []session.Message
	if compacted {
		resumeSummary := "本次会话延续自之前的对话，因上下文空间不足进行了压缩。以下是早期对话的摘要：\n\n" + boundary.Summary
		if len(boundary.Keep) > 0 {
			resumeSummary += "\n\n近期消息已原样保留。"
		}
		replay = append(replay, session.Message{Role: "user", Content: resumeSummary})
		for _, k := range boundary.Keep {
			replay = append(replay, session.Message{
				Role:        k.Role,
				RunID:       k.RunID,
				Content:     k.Content,
				ToolUses:    k.ToolUses,
				ToolResults: k.ToolResults,
			})
		}
		replay = append(replay, after...)
	} else {
		replay = msgs
	}

	// 清除旧 UI，重放消息
	s.send(wsMessage{Type: "clear", Data: nil})
	for _, msg := range replay {
		// 只带工具结果的消息没有文本，不推给前端，但要进对话历史保住调用链
		s.conv.AppendMessages([]conversation.Message{msg.ToConversation()})
		if msg.Content == "" {
			continue
		}
		switch msg.Role {
		case "user":
			s.send(wsMessage{Type: "replay_user", Data: map[string]string{"content": msg.Content}})
		case "assistant":
			s.send(wsMessage{Type: "replay_assistant", Data: map[string]string{"content": msg.Content}})
		}
	}
	// 先完整重建上下文，再启动 worker；若有持久化的排队 prompt，
	// 它们只会在恢复后的 conversation 上继续。
	if s.agentLoop != nil {
		if err := s.registerLoopSession(s.sessionID); err != nil {
			s.send(wsMessage{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
	}

	restored := fmt.Sprintf("Session %s restored (%d messages).", targetID, len(replay))
	if compacted {
		restored = fmt.Sprintf("Session %s restored from compacted state (summary + %d kept + %d newer).",
			targetID, len(boundary.Keep), len(after))
	}
	s.send(wsMessage{Type: "system", Data: map[string]string{"message": restored}})
	s.send(wsMessage{Type: "command_done", Data: nil})
}

func (s *Server) consumeAgentEvents(events <-chan agent.AgentEvent) {
	streamBuf := ""
	startTime := time.Now()

	for ev := range events {
		switch e := ev.(type) {
		case agent.StreamText:
			streamBuf += e.Text
			s.send(wsMessage{Type: "stream_text", Data: map[string]string{"text": e.Text}})

		case agent.ThinkingText:
			s.send(wsMessage{Type: "thinking_text", Data: map[string]string{"text": e.Text}})

		case agent.ToolUseEvent:
			s.send(wsMessage{Type: "tool_use", Data: map[string]any{
				"toolId":   e.ToolID,
				"toolName": e.ToolName,
				"args":     e.Args,
			}})

		case agent.ToolResultEvent:
			if streamBuf != "" {
				s.send(wsMessage{Type: "stream_end", Data: map[string]string{"text": streamBuf}})
				streamBuf = ""
			}
			s.send(wsMessage{Type: "tool_result", Data: map[string]any{
				"toolId":   e.ToolID,
				"toolName": e.ToolName,
				"output":   e.Output,
				"isError":  e.IsError,
				"elapsed":  e.Elapsed.Seconds(),
			}})

		case agent.PermissionRequestEvent:
			id := fmt.Sprintf("perm_%d", time.Now().UnixNano())
			s.pendingPermMu.Lock()
			s.pendingPerms[id] = e.ResponseCh
			s.pendingPermMu.Unlock()
			s.send(wsMessage{Type: "permission_request", Data: map[string]string{
				"id":          id,
				"toolName":    e.ToolName,
				"description": e.Desc,
			}})

		case agent.AskUserQuestionEvent:
			id := fmt.Sprintf("ask_%d", time.Now().UnixNano())
			respCh := make(chan tools.QuestionResponse, 1)
			s.pendingAskMu.Lock()
			s.pendingAsks[id] = respCh
			s.pendingAskMu.Unlock()
			s.send(wsMessage{Type: "ask_user", Data: map[string]any{
				"id":        id,
				"questions": e.Questions,
			}})
			go func() {
				resp := <-respCh
				e.ResponseCh <- resp.Answers
			}()

		case agent.TurnComplete:
			if streamBuf != "" {
				s.send(wsMessage{Type: "stream_end", Data: map[string]string{"text": streamBuf}})
				streamBuf = ""
			}
			s.send(wsMessage{Type: "turn_complete", Data: map[string]int{"turn": e.Turn}})

		case agent.LoopComplete:
			if streamBuf != "" {
				// 助手消息由主循环在进入对话历史时落盘，这里只负责推送给前端
				s.send(wsMessage{Type: "stream_end", Data: map[string]string{"text": streamBuf}})
				streamBuf = ""
			}
			elapsed := time.Since(startTime).Seconds()
			s.send(wsMessage{Type: "loop_complete", Data: map[string]any{
				"totalTurns": e.TotalTurns,
				"elapsed":    elapsed,
			}})

		case agent.UsageEvent:
			s.send(wsMessage{Type: "usage", Data: map[string]int{
				"inputTokens":  e.InputTokens,
				"outputTokens": e.OutputTokens,
			}})

		case agent.ErrorEvent:
			s.send(wsMessage{Type: "error", Data: map[string]string{"message": e.Message}})

		case agent.CompactEvent:
			s.send(wsMessage{Type: "compact", Data: map[string]string{"message": e.Message}})

		case agent.RetryEvent:
			s.send(wsMessage{Type: "retry", Data: map[string]any{
				"reason": e.Reason,
				"waitMs": e.Wait.Milliseconds(),
			}})

		case agent.QueueStatusEvent:
			s.send(wsMessage{Type: "queue_status", Data: map[string]any{
				"sessionId": e.SessionID,
				"runId":     e.RunID,
				"position":  e.Position,
				"status":    e.Status,
			}})
		}
	}
}

func (s *Server) listenForAskUser(done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case req, ok := <-s.askUserCh:
			if !ok {
				return
			}
			id := fmt.Sprintf("ask_%d", time.Now().UnixNano())
			respCh := make(chan tools.QuestionResponse, 1)
			s.pendingAskMu.Lock()
			s.pendingAsks[id] = respCh
			s.pendingAskMu.Unlock()
			s.send(wsMessage{Type: "ask_user", Data: map[string]any{
				"id":        id,
				"questions": req.Questions,
			}})
			resp := <-respCh
			req.ResponseCh <- resp
		}
	}
}

func (s *Server) handlePermissionResponse(data permResponseData) {
	s.pendingPermMu.Lock()
	ch, ok := s.pendingPerms[data.ID]
	if ok {
		delete(s.pendingPerms, data.ID)
	}
	s.pendingPermMu.Unlock()

	if !ok {
		return
	}

	var resp agent.PermissionResponse
	switch data.Response {
	case "allow":
		resp = agent.PermAllow
	case "deny":
		resp = agent.PermDeny
	case "allowAlways":
		resp = agent.PermAllowAlways
	default:
		resp = agent.PermDeny
	}
	ch <- resp
}

func (s *Server) handleAskUserResponse(data askUserResponseData) {
	s.pendingAskMu.Lock()
	ch, ok := s.pendingAsks[data.ID]
	if ok {
		delete(s.pendingAsks, data.ID)
	}
	s.pendingAskMu.Unlock()

	if !ok {
		return
	}

	ch <- tools.QuestionResponse{Answers: data.Answers}
}

func (s *Server) send(msg wsMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) == 0 {
		return
	}
	data, _ := json.Marshal(msg)
	for conn := range s.conns {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("[ws] write error: type=%s err=%v", msg.Type, err)
		}
	}
}

func (s *Server) wireSkillsToAgent(wd string) {
	if s.skillCatalog == nil || s.ag == nil {
		return
	}
	s.registry.Register(&skills.LoadSkillTool{
		Catalog: s.skillCatalog,
		Host:    s,
	})
}

// SkillHost 接口实现

func (s *Server) ActivateSkill(name, body string) {
	if s.ag != nil {
		s.ag.ActivateSkill(name, body)
	}
}

func (s *Server) SetToolFilter(allow func(name string) bool) {
	if s.ag != nil {
		s.ag.SetToolFilter(allow)
	}
}

func (s *Server) ToolRegistry() *tools.Registry {
	return s.registry
}

// 辅助函数

func loadCustomInstructions(wd string) string {
	paths := []string{
		filepath.Join(wd, ".mewcode", "instructions.md"),
		filepath.Join(wd, "CLAUDE.md"),
	}
	var parts []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil {
			parts = append(parts, string(data))
		}
	}
	return strings.Join(parts, "\n\n")
}

func buildSkillSection(catalog *skills.Catalog, wd string) string {
	if catalog == nil {
		return ""
	}
	metas := catalog.List()
	if len(metas) == 0 {
		return ""
	}
	skillsDir := filepath.Join(wd, ".mewcode", "skills")
	var sb strings.Builder
	sb.WriteString("## Available Skills\n\n")
	sb.WriteString(fmt.Sprintf("Skills are installed at: %s\n", skillsDir))
	sb.WriteString("When creating new skills, always place them under this directory as <skill-name>/SKILL.md.\n\n")
	for _, meta := range metas {
		desc := meta.Description
		if len(desc) > 200 {
			desc = desc[:200] + "…"
		}
		sb.WriteString(fmt.Sprintf("- /%s: %s\n", meta.Name, desc))
	}
	return sb.String()
}

func installMemExtractor(ag *agent.Agent, wd string, p *config.ProviderConfig, client llm.Client, registry *tools.Registry, conv *conversation.Manager) *extractor.Extractor {
	extr := extractor.InitExtractMemories(extractor.Deps{
		MemoryDir:       memory.GetAutoMemPath(wd),
		UserMemoryDir:   memory.GetUserAutoMemPath(),
		ProjectRoot:     wd,
		Client:          client,
		ToolRegistry:    registry,
		Protocol:        p.Protocol,
		Conversation:    conv,
		AppendSystem:    func(s string) { conv.AddSystemReminder(s) },
		ContextWindow:   p.GetContextWindow(),
		MaxOutputTokens: p.GetMaxOutputTokens(),
	})
	ag.OnLoopComplete = func(_ *conversation.Manager) {
		_ = extr.Execute(context.Background())
	}
	return extr
}

func newAgentHookRunner(client llm.Client) func(prompt string, ctx hooks.HookContext) (string, error) {
	return func(p string, _ hooks.HookContext) (string, error) {
		c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		conv := conversation.NewManager()
		conv.AddUserMessage(p)
		events, errs := client.Stream(c, conv, nil)
		var text string
		for ev := range events {
			if td, ok := ev.(llm.TextDelta); ok {
				text += td.Text
			}
		}
		select {
		case err := <-errs:
			if err != nil {
				return "", err
			}
		default:
		}
		return text, nil
	}
}

func mustGetwd() string {
	wd, _ := os.Getwd()
	return wd
}
