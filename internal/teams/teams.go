package teams

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mewcode/internal/agent"
	"mewcode/internal/conversation"
	"mewcode/internal/llm"
	"mewcode/internal/tools"
)

type TeamMode string

const (
	ModeInProcess TeamMode = "in-process"
)

// teamsBaseDir 是所有团队目录的根。放在用户主目录而不是项目目录下，
// 因为窗格队员是独立进程、工作目录可能被 worktree 换掉，用主目录才能保证
// 队员进程和 Lead 找到的是同一份团队配置。
func teamsBaseDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		wd, _ := os.Getwd()
		home = wd
	}
	return filepath.Join(home, ".mewcode", "teams")
}

// Member agent team 中的一个成员定义
type Member struct {
	Name     string
	AgentRef *agent.Agent
	// member 拥有自己的上下文管理，是一个独立的 agent
	Conv   *conversation.Manager
	Active bool
	// Lead 停止它的入口
	Cancel   context.CancelFunc
	Progress *TeammateProgress

	// 以下几个是要落盘的元信息，运行时不参与调度，只在写 config.json
	// 和从磁盘恢复团队时用到。
	AgentID   string
	AgentType string
	Model     string
	// Worker 独立工作目录
	WorktreePath string
	JoinedAt     int64
}

type Team struct {
	Name    string
	Mode    TeamMode
	Members map[string]*Member
	MailBox *FileMailBox
	mu      sync.Mutex
	// ctx/cancel 的生命周期属于整个 Team，而不是某一次 Lead run。
	// in-process teammate 使用它运行，只有 StopMember/DeleteTeam/CloseAll
	// 才会结束队友循环。
	ctx    context.Context
	cancel context.CancelFunc

	// 落盘用的团队级元信息
	LeadAgentID string
	Description string
	CreatedAt   int64
}

func NewTeam(name string, mode TeamMode) *Team {
	inboxDir := filepath.Join(teamDir(name), "inboxes")
	ctx, cancel := context.WithCancel(context.Background())
	return &Team{
		Name:      name,
		Mode:      mode,
		Members:   make(map[string]*Member),
		MailBox:   NewFileMailBox(inboxDir),
		CreatedAt: time.Now().Unix(),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// workerContext 返回 Team 级别的上下文。它不会随着 Lead 的单次请求结束，
// 只有显式关闭 Team 才会取消。
func (t *Team) workerContext() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ctx == nil {
		t.ctx, t.cancel = context.WithCancel(context.Background())
	}
	return t.ctx
}

func (t *Team) AddMember(name string, client llm.Client, registry *tools.Registry, protocol string) *Member {
	t.mu.Lock()
	defer t.mu.Unlock()

	ag := agent.New(client, registry, protocol)
	member := &Member{
		Name:     name,
		AgentRef: ag,
		Conv:     conversation.NewManager(),
		Active:   false,
		AgentID:  name,
		JoinedAt: time.Now().Unix(),
	}
	t.Members[name] = member
	t.persist()
	return member
}

// SetMemberMeta 补齐成员的元信息（agent 类型、模型、worktree 路径）并落盘。
// spawn 流程拿到这些信息的时机晚于 AddMember，所以分成两步写。
func (t *Team) SetMemberMeta(name, agentType, model, worktreePath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	member, ok := t.Members[name]
	if !ok {
		return
	}
	member.AgentType = agentType
	member.Model = model
	member.WorktreePath = worktreePath
	t.persist()
}

func (t *Team) StartMember(ctx context.Context, name string, task string) (<-chan agent.AgentEvent, error) {
	t.mu.Lock()
	member, ok := t.Members[name]
	t.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("member not found: %s", name)
	}

	memberCtx, cancel := context.WithCancel(ctx)
	member.Active = true
	member.Cancel = cancel

	t.mu.Lock()
	t.persist()
	t.mu.Unlock()

	member.Conv.AddUserMessage(task)
	ch := member.AgentRef.Run(memberCtx, member.Conv)
	return ch, nil
}

func (t *Team) StopMember(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	member, ok := t.Members[name]
	if !ok {
		return
	}
	// teammate 始终是当前进程里的 goroutine，只需要取消它的 context。
	if member.Cancel != nil {
		member.Cancel()
	}
	member.Active = false
	t.persist()
}

func (t *Team) GetTeammateProgress() []*TeammateProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	var result []*TeammateProgress
	for _, m := range t.Members {
		if m.Progress != nil {
			result = append(result, m.Progress)
		}
	}
	return result
}

func (t *Team) SendMessage(from, to, content string) {
	t.MailBox.Send(to, FileMailMessage{
		From:      from,
		Text:      content,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

type TeamManager struct {
	mu         sync.Mutex
	teams      map[string]*Team
	taskStores map[string]*SharedTaskStore // 每团队一份共享任务库
}

func NewTeamManager() *TeamManager {
	return &TeamManager{
		teams:      make(map[string]*Team),
		taskStores: make(map[string]*SharedTaskStore),
	}
}

func teamDir(name string) string {
	return filepath.Join(teamsBaseDir(), sanitizeTeamName(name))
}

func (tm *TeamManager) CreateTeam(name string, mode TeamMode) *Team {
	return tm.CreateTeamFull(name, mode, "", "")
}

// CreateTeamFull 建团队并记下 lead 和描述，随后把配置写进 config.json。
// 落盘之后队员进程和下一次会话都能靠 GetTeam 把这个团队捞回来。
func (tm *TeamManager) CreateTeamFull(name string, mode TeamMode, leadAgentID, description string) *Team {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	team := NewTeam(name, mode)
	team.LeadAgentID = leadAgentID
	team.Description = description
	tm.teams[name] = team
	// 新建团队时初始化一份空的共享任务库
	store := NewSharedTaskStore(filepath.Join(teamDir(name), "tasks.json"))
	store.InitEmpty()
	tm.taskStores[name] = store
	team.persist()
	return team
}

// GetTaskStore 获取团队的共享任务库；内存无缓存时（例如队友进程）从磁盘 tasks.json 加载。
func (tm *TeamManager) GetTaskStore(teamName string) *SharedTaskStore {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if store, ok := tm.taskStores[teamName]; ok {
		return store
	}
	store := NewSharedTaskStore(filepath.Join(teamDir(teamName), "tasks.json"))
	tm.taskStores[teamName] = store
	return store
}

// CreateTeamWith 注册一个外部构造好的 Team。保留这个入口用于恢复磁盘上的
// Team 元信息；实际 teammate 始终由当前进程内的 SpawnTeammate 启动。
func (tm *TeamManager) CreateTeamWith(team *Team) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.teams[team.Name] = team
}

// GetTeam 先查内存，未命中再看磁盘上有没有 config.json。
// 从磁盘重建出来的 Team 只带元信息，成员的 agent 实例和 conversation 都是空的，
// 够 SendMessage 按名字投递和 UI 展示用；要真正让某个成员跑起来还得重新 spawn。
func (tm *TeamManager) GetTeam(name string) *Team {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if team, ok := tm.teams[name]; ok {
		return team
	}
	tf, err := ReadTeamFile(name)
	if err != nil || tf == nil {
		return nil
	}
	team := NewTeam(tf.Name, ModeInProcess)
	team.LeadAgentID = tf.LeadAgentID
	team.Description = tf.Description
	team.CreatedAt = tf.CreatedAt
	for _, m := range tf.Members {
		// 旧版本 config.json 可能保存过 tmux/iTerm；当前实现只支持
		// in-process，故忽略历史 backendType。
		team.Mode = ModeInProcess
		active := false
		if m.IsActive != nil {
			active = *m.IsActive
		}
		team.Members[m.Name] = &Member{
			Name:         m.Name,
			AgentID:      m.AgentID,
			AgentType:    m.AgentType,
			Model:        m.Model,
			WorktreePath: m.WorktreePath,
			JoinedAt:     m.JoinedAt,
			Active:       active,
		}
	}
	tm.teams[name] = team
	return team
}

func (tm *TeamManager) DeleteTeam(name string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if team, ok := tm.teams[name]; ok {
		registry := GetNameRegistry()
		for memberName := range team.Members {
			team.StopMember(memberName)
			// 解绑该成员在全局名称注册表里的映射
			registry.Unregister(memberName)
		}
		if team.cancel != nil {
			team.cancel()
		}
		delete(tm.teams, name)
	}
	delete(tm.taskStores, name)
	// 团队目录里是 config.json、tasks.json 和收件箱，团队没了就一起清掉，
	// 免得下次同名团队捞到上一次的残留
	_ = os.RemoveAll(teamDir(name))
}

func (tm *TeamManager) ListTeams() []string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var names []string
	for name := range tm.teams {
		names = append(names, name)
	}
	return names
}

func (tm *TeamManager) GetAllTeammateProgress() []*TeammateProgress {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var result []*TeammateProgress
	for _, team := range tm.teams {
		result = append(result, team.GetTeammateProgress()...)
	}
	return result
}

func (tm *TeamManager) CloseAll() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for name, team := range tm.teams {
		for memberName := range team.Members {
			team.StopMember(memberName)
		}
		if team.cancel != nil {
			team.cancel()
		}
		delete(tm.teams, name)
	}
}
