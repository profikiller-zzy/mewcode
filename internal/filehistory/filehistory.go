package filehistory

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxSnapshots = 100

type Backup struct {
	BackupPath string    `json:"backup_path"`
	Version    int       `json:"version"`
	Time       time.Time `json:"time"`
}

type Snapshot struct {
	MessageIndex int               `json:"message_index"`
	UserText     string            `json:"user_text"`
	Backups      map[string]Backup `json:"backups"`
	Timestamp    time.Time         `json:"timestamp"`
}

type History struct {
	mu           sync.Mutex
	sessionDir   string
	trackedFiles map[string]int // filepath → 当前版本
	snapshots    []Snapshot
}

func New(baseDir, sessionID string) *History {
	dir := filepath.Join(baseDir, ".mewcode", "file-history", sessionID)
	_ = os.MkdirAll(dir, 0o755)
	return &History{
		sessionDir:   dir,
		trackedFiles: make(map[string]int),
	}
}

func backupName(filePath string, version int) string {
	h := sha256.Sum256([]byte(filePath))
	return fmt.Sprintf("%x@v%d", h[:8], version)
}

// TrackEdit 在 path 指向的文件被修改之前先做备份，任何 write/edit 操作前都要调用。
// 文件还不存在时（新文件）不生成备份，但仍然记录这个路径，
// 这样 Rewind 才知道要把它删掉。
func (h *History) TrackEdit(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}

	ver := h.trackedFiles[absPath]
	newVer := ver + 1

	data, err := os.ReadFile(absPath)
	if err == nil {
		bp := filepath.Join(h.sessionDir, backupName(absPath, newVer))
		_ = os.WriteFile(bp, data, 0o644)
	}
	// 文件不存在时照样把版本号 +1，这样 Rewind 知道在这个版本上
	// 文件并不存在（磁盘上没有备份文件 → 回滚时删掉）。

	h.trackedFiles[absPath] = newVer
}

// MakeSnapshot 创建一个检查点，绑定到给定的对话消息索引。
// userText 是给 UI 看的简短标签。
func (h *History) MakeSnapshot(msgIndex int, userText string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	backups := make(map[string]Backup, len(h.trackedFiles))
	for path, ver := range h.trackedFiles {
		bp := filepath.Join(h.sessionDir, backupName(path, ver))
		if _, err := os.Stat(bp); err != nil {
			if data, readErr := os.ReadFile(path); readErr == nil {
				_ = os.WriteFile(bp, data, 0o644)
			}
		}
		backups[path] = Backup{BackupPath: bp, Version: ver, Time: time.Now()}
	}

	snap := Snapshot{
		MessageIndex: msgIndex,
		UserText:     userText,
		Backups:      backups,
		Timestamp:    time.Now(),
	}

	h.snapshots = append(h.snapshots, snap)
	if len(h.snapshots) > maxSnapshots {
		h.snapshots = h.snapshots[len(h.snapshots)-maxSnapshots:]
	}
}

// GetSnapshots 返回所有快照的副本，供 UI 展示。
func (h *History) GetSnapshots() []Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Snapshot, len(h.snapshots))
	copy(out, h.snapshots)
	return out
}

// Rewind 把文件恢复到指定索引处快照记录的状态，
// 返回实际发生改动的文件列表。
func (h *History) Rewind(snapshotIndex int) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if snapshotIndex < 0 || snapshotIndex >= len(h.snapshots) {
		return nil, fmt.Errorf("invalid snapshot index %d", snapshotIndex)
	}

	target := h.snapshots[snapshotIndex]
	var changed []string

	for path, backup := range target.Backups {
		backupData, err := os.ReadFile(backup.BackupPath)
		if err != nil {
			// 备份文件缺失 → 那个时间点文件并不存在；删掉它
			if _, statErr := os.Stat(path); statErr == nil {
				_ = os.Remove(path)
				changed = append(changed, path)
			}
			continue
		}

		currentData, _ := os.ReadFile(path)
		if string(currentData) != string(backupData) {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			if writeErr := os.WriteFile(path, backupData, 0o644); writeErr == nil {
				changed = append(changed, path)
			}
		}
	}

	// target 之后才第一次被追踪的文件，target.Backups 里没有它们的记录，
	// 上面这段循环碰不到：在 target 那个时间点它们还不存在，回滚到那个点
	// 就该删掉，不能留在磁盘上。
	for path := range h.trackedFiles {
		if _, ok := target.Backups[path]; ok {
			continue
		}
		if _, statErr := os.Stat(path); statErr == nil {
			if rmErr := os.Remove(path); rmErr == nil {
				changed = append(changed, path)
			}
		}
		delete(h.trackedFiles, path)
	}

	// 截断快照列表：删掉 target 之后的所有记录
	h.snapshots = h.snapshots[:snapshotIndex+1]

	// 把已追踪文件的版本号重置为快照里的版本
	for path, backup := range target.Backups {
		h.trackedFiles[path] = backup.Version
	}

	return changed, nil
}

// HasSnapshots 在至少有一个可回滚的快照时返回 true。
func (h *History) HasSnapshots() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.snapshots) > 0
}

// Save 把快照元数据持久化到磁盘。
func (h *History) Save() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := json.MarshalIndent(h.snapshots, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(h.sessionDir, "snapshots.json"), data, 0o644)
}
