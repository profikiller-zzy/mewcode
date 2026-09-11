package agents

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"mewcode/internal/agent"
)

type TaskStatus string

const (
	TaskPending   TaskStatus = "pending"
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskFailed    TaskStatus = "failed"
	TaskCancelled TaskStatus = "cancelled"
)

type Task struct {
	ID        string
	Name      string
	Status    TaskStatus
	Output    string
	Error     string
	CreatedAt time.Time
	DoneAt    time.Time
	Cancel    context.CancelFunc
}

type TaskManager struct {
	mu            sync.Mutex
	tasks         map[string]*Task
	nextID        int
	notifications []TaskNotification
}

type TaskNotification struct {
	TaskID string
	Name   string
	Status TaskStatus
	Output string
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		tasks: make(map[string]*Task),
	}
}

func (tm *TaskManager) CreateTask(name string) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.nextID++
	id := fmt.Sprintf("task_%d", tm.nextID)
	tm.tasks[id] = &Task{
		ID:        id,
		Name:      name,
		Status:    TaskPending,
		CreatedAt: time.Now(),
	}
	return id
}

func (tm *TaskManager) GetTask(id string) *Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.tasks[id]
}

func (tm *TaskManager) ListTasks() []*Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var result []*Task
	for _, t := range tm.tasks {
		result = append(result, t)
	}
	return result
}

func (tm *TaskManager) SetRunning(id string, cancel context.CancelFunc) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskRunning
		t.Cancel = cancel
	}
}

func (tm *TaskManager) SetCompleted(id, output string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskCompleted
		t.Output = output
		t.DoneAt = time.Now()
		tm.notifications = append(tm.notifications, TaskNotification{
			TaskID: id,
			Name:   t.Name,
			Status: TaskCompleted,
			Output: output,
		})
	}
}

func (tm *TaskManager) SetFailed(id, errMsg string) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok {
		t.Status = TaskFailed
		t.Error = errMsg
		t.DoneAt = time.Now()
		tm.notifications = append(tm.notifications, TaskNotification{
			TaskID: id,
			Name:   t.Name,
			Status: TaskFailed,
			Output: errMsg,
		})
	}
}

func (tm *TaskManager) DrainNotifications() []TaskNotification {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	n := tm.notifications
	tm.notifications = nil
	return n
}

func (tm *TaskManager) AdoptRunning(name string, eventCh <-chan agent.AgentEvent, cancel context.CancelFunc) string {
	taskID := tm.CreateTask("adopted: " + truncate(name, 40))
	tm.SetRunning(taskID, cancel)

	go func() {
		var output string
		for ev := range eventCh {
			switch e := ev.(type) {
			case agent.StreamText:
				output += e.Text
			case agent.ErrorEvent:
				tm.SetFailed(taskID, e.Message)
				return
			}
		}
		tm.SetCompleted(taskID, output)
	}()

	return taskID
}

func (tm *TaskManager) FindByName(name string) *Task {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for _, t := range tm.tasks {
		if t.Name == name || strings.HasPrefix(t.Name, name+":") {
			return t
		}
	}
	return nil
}

func (tm *TaskManager) CancelTask(id string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if t, ok := tm.tasks[id]; ok && t.Cancel != nil {
		t.Cancel()
		t.Status = TaskCancelled
		t.DoneAt = time.Now()
		return true
	}
	return false
}

// SubAgentSpec 摘取了 BaseAgentDefinition 中与运行时相关的那部分。它是加载层
// （AgentDefinition）和执行层（runSync / runFork）之间的桥梁。
// 内置 agent 跳过文件解析，直接通过 BuiltinSpecs 实例化这个结构。
type SubAgentSpec struct {
	Name            string
	Description     string
	Tools           []string
	DisallowedTools []string
	// SystemPromptOverride 这个字段封装在system-reminder标签中，放在user message里
	SystemPromptOverride string
	MaxTurns             int
	Model                string
	// PermissionMode 在 sub-agent 运行期间覆盖父 agent 的权限模式。
	// 空字符串表示继承自父 agent。
	PermissionMode string
	// Isolation 选择文件系统隔离模式；"worktree" 会创建一个临时的 git worktree。
	Isolation IsolationMode
	// InitialPrompt 会被前置到第一轮 user 对话。
	InitialPrompt string
	// OmitMewcodeMd 把这个 agent 的 userContext 里的 MEWCODE.md 层级去掉。
	OmitMewcodeMd bool
	// Skills 是 sub-agent 启动时要预加载的 skill 名。
	Skills []string
	// Memory 在三种作用域之一里开启持久化记忆。
	Memory AgentMemoryScope
	// McpServers / RequiredMcpServers / Hooks / Effort 把 frontmatter 数据往后传，
	// 这样以后新增通道时可以直接消费，不用再做一次 schema 迁移。
	McpServers         []any
	RequiredMcpServers []string
	Hooks              any
	Effort             any
}

const planAgentSystemPrompt = `You are a software architect and planning specialist.

=== CRITICAL: READ-ONLY MODE - NO FILE MODIFICATIONS ===
You are STRICTLY PROHIBITED from creating, modifying, or deleting any files.
Your role is EXCLUSIVELY to explore code and design implementation plans.

## Your Process

1. **Understand Requirements**: Analyze the user's request carefully.

2. **Explore Thoroughly**:
   - Read files with ReadFile to understand current architecture
   - Use Grep to find patterns, function definitions, and references
   - Use Glob to discover file structure
   - Use Bash ONLY for read-only operations (ls, find, grep, cat, head, tail)
   - NEVER use Bash for: mkdir, touch, rm, cp, mv, git add/commit, npm install

3. **Design Solution**:
   - Create a concrete implementation approach
   - Consider trade-offs and explain your reasoning
   - Follow existing patterns in the codebase

4. **Detail the Plan**:
   - Provide step-by-step implementation strategy
   - Identify file dependencies and sequencing
   - Anticipate potential challenges

## Required Output
End your response with:

### Critical Files for Implementation
List the most critical files for implementing this change:
- path/to/file1 — reason
- path/to/file2 — reason`

// BuiltinSpecs 内置的自定义subagent
var BuiltinSpecs = map[string]SubAgentSpec{
	"general-purpose": {
		Name:        "general-purpose",
		Description: "General-purpose agent for research and multi-step tasks",
		MaxTurns:    200,
	},
	"plan": {
		Name:                 "plan",
		Description:          "Software architect for designing implementation plans. Returns step-by-step plans, identifies critical files, and considers architectural trade-offs.",
		DisallowedTools:      []string{"EditFile", "WriteFile"},
		SystemPromptOverride: planAgentSystemPrompt,
		MaxTurns:             15,
	},
	"explore": {
		Name:            "explore",
		Description:     "Fast read-only search agent for locating code",
		DisallowedTools: []string{"EditFile", "WriteFile"},
		// 不写 MaxTurns → 默认 200（和 general-purpose 的兜底值一样）。之前的 30 轮
		// 上限会踩坑：当 LLM 需要发很多次 ToolSearch/Glob/Grep 来摸清一个陌生 repo 时，
		// spawn 会在还没报出任何有用信息前
		// 就以 "reached maximum iterations" 失败。
		//
		// "haiku" 是一个档位偏好，不是字面模型名：Claude provider 下解析成
		// claude-haiku；其他 provider 在 config 里配 model_aliases 映射到自己的
		// 轻量模型；都没配则回退主模型（解析失败由 selectClient 兜底）。
		Model: "haiku",
	},
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
