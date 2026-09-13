package teams

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
)

// SharedTask 是团队共享任务板上的一条任务，带依赖关系（Blocks / BlockedBy）
// 和归属（Assignee）。
type SharedTask struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // pending | in_progress | completed | blocked
	Assignee    string   `json:"assignee"`
	Blocks      []string `json:"blocks"`
	BlockedBy   []string `json:"blocked_by"`
	CreatedBy   string   `json:"created_by"`
}

// storeData 是 tasks.json 的整体结构：下一个可用 id + 任务列表。
type storeData struct {
	NextID int          `json:"next_id"`
	Tasks  []SharedTask `json:"tasks"`
}

// SharedTaskStore 以 JSON 文件（tasks.json）落盘，供同一团队的所有成员读写。每次读操作前先重新加载文件，保证跨进程的队友能拿到最新数据。
// 一个team对应一份 SharedTaskStore。对应的文件是 ~/.mewcode/teams/<team>/tasks.json
type SharedTaskStore struct {
	mu     sync.Mutex
	path   string
	nextID int
	tasks  []SharedTask
}

// NewSharedTaskStore 打开（或初始化）指定路径的共享任务库。
func NewSharedTaskStore(path string) *SharedTaskStore {
	s := &SharedTaskStore{path: path, nextID: 1}
	_ = s.load()
	return s
}

// load 从磁盘重新读取任务列表；文件不存在时保持空。调用方需持有锁。
func (s *SharedTaskStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.tasks = nil
			s.nextID = 1
			return nil
		}
		return err
	}
	var sd storeData
	if err := json.Unmarshal(data, &sd); err != nil {
		return fmt.Errorf("解析任务板 %s 失败: %w", s.path, err)
	}
	s.tasks = sd.Tasks
	if sd.NextID > 0 {
		s.nextID = sd.NextID
	} else {
		s.nextID = 1
	}
	return nil
}

// save 把当前任务列表写回磁盘。调用方需持有锁。
func (s *SharedTaskStore) save() error {
	sd := storeData{NextID: s.nextID, Tasks: s.tasks}
	if s.tasks == nil {
		sd.Tasks = []SharedTask{}
	}
	out, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return err
	}
	return writeJSONAtomic(s.path, out, 0o644)
}

func (s *SharedTaskStore) lockPath() string { return s.path + ".lock" }

// withWriteLock 串行化同进程和跨进程写操作，并确保在锁内重新加载最新状态。
func (s *SharedTaskStore) withWriteLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return withFileLock(s.lockPath(), func() error {
		if err := s.load(); err != nil {
			return err
		}
		return fn()
	})
}

// Create 创建一条共享任务，返回新任务。
func (s *SharedTaskStore) Create(title, description, assignee string, blocks, blockedBy []string, createdBy string) SharedTask {
	task, _ := s.CreateWithError(title, description, assignee, blocks, blockedBy, createdBy)
	return task
}

// CreateWithError 在跨进程锁内 reload、创建并原子写回任务。
func (s *SharedTaskStore) CreateWithError(title, description, assignee string, blocks, blockedBy []string, createdBy string) (SharedTask, error) {
	if blocks == nil {
		blocks = []string{}
	}
	if blockedBy == nil {
		blockedBy = []string{}
	}
	var task SharedTask
	err := s.withWriteLock(func() error {
		task = SharedTask{
			ID:          strconv.Itoa(s.nextID),
			Title:       title,
			Description: description,
			Status:      "pending",
			Assignee:    assignee,
			Blocks:      blocks,
			BlockedBy:   blockedBy,
			CreatedBy:   createdBy,
		}
		s.nextID++
		s.tasks = append(s.tasks, task)
		return s.save()
	})
	if err != nil {
		return SharedTask{}, err
	}
	return task, nil
}

// Get 按 id 获取任务；读前先 reload 拿最新。找不到返回 nil。
func (s *SharedTaskStore) Get(id string) *SharedTask {
	task, _ := s.GetWithError(id)
	return task
}

func (s *SharedTaskStore) GetWithError(id string) (*SharedTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			t := s.tasks[i]
			return &t, nil
		}
	}
	return nil, nil
}

// ListTasks 列出任务，可选按状态、归属人过滤。
func (s *SharedTaskStore) ListTasks(status, assignee string) []SharedTask {
	tasks, _ := s.ListTasksWithError(status, assignee)
	return tasks
}

func (s *SharedTaskStore) ListTasksWithError(status, assignee string) ([]SharedTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	var result []SharedTask
	for _, t := range s.tasks {
		if status != "" && t.Status != status {
			continue
		}
		if assignee != "" && t.Assignee != assignee {
			continue
		}
		result = append(result, t)
	}
	return result, nil
}

// TaskUpdate 描述一次更新；nil 指针表示对应字段不改。
type TaskUpdate struct {
	Status       *string
	Assignee     *string
	Description  *string
	AddBlocks    []string
	AddBlockedBy []string
}

// Update 按 TaskUpdate 修改任务；AddBlocks / AddBlockedBy 追加依赖（去重）。
// 任务不存在返回 nil。
func (s *SharedTaskStore) Update(id string, upd TaskUpdate) *SharedTask {
	updated, _ := s.UpdateWithError(id, upd)
	return updated
}

// UpdateWithError 在跨进程锁内 reload、修改并原子写回任务。
func (s *SharedTaskStore) UpdateWithError(id string, upd TaskUpdate) (*SharedTask, error) {
	var updated *SharedTask
	err := s.withWriteLock(func() error {
		for i := range s.tasks {
			if s.tasks[i].ID != id {
				continue
			}
			t := &s.tasks[i]
			if upd.Status != nil {
				t.Status = *upd.Status
			}
			if upd.Assignee != nil {
				t.Assignee = *upd.Assignee
			}
			if upd.Description != nil {
				t.Description = *upd.Description
			}
			t.Blocks = appendUnique(t.Blocks, upd.AddBlocks)
			t.BlockedBy = appendUnique(t.BlockedBy, upd.AddBlockedBy)
			if err := s.save(); err != nil {
				return err
			}
			copy := *t
			updated = &copy
			return nil
		}
		return nil
	})
	return updated, err
}

// InitEmpty 清空任务库并落盘，用于新建团队时初始化。
func (s *SharedTaskStore) InitEmpty() {
	_ = s.InitEmptyWithError()
}

func (s *SharedTaskStore) InitEmptyWithError() error {
	return s.withWriteLock(func() error {
		s.tasks = []SharedTask{}
		s.nextID = 1
		return s.save()
	})
}

// appendUnique 把 add 里尚不存在的元素追加到 base，返回新切片。
func appendUnique(base, add []string) []string {
	for _, v := range add {
		found := false
		for _, e := range base {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			base = append(base, v)
		}
	}
	if base == nil {
		base = []string{}
	}
	return base
}
