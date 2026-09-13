package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type FileMailBox struct {
	baseDir string
	// 同进程内的并发直接用内存锁串行化，文件锁只负责隔离独立进程的 teammate。
	// 省掉一轮文件系统争抢，也避免同进程的 goroutine 互相把重试预算耗光。
	mu sync.Mutex
}

// FileMailMessage 信箱通行的抽象
type FileMailMessage struct {
	From      string `json:"from"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	Read      bool   `json:"read"`
	Color     string `json:"color,omitempty"`

	// 结构化消息用的三个字段，普通文本消息留空。
	// Type 见 protocol.go 里的 Msg* 常量；RequestID 让应答能对上请求；
	// Approve 用指针是为了区分「没表态」和「明确拒绝」。
	Type      string `json:"type,omitempty"`
	RequestID string `json:"requestId,omitempty"`
	Approve   *bool  `json:"approve,omitempty"`
}

// NewFileMailMessage 构造一条普通文本消息，时间戳按 RFC3339Nano 记。
func NewFileMailMessage(from, text string) FileMailMessage {
	return FileMailMessage{
		From:      from,
		Text:      text,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func NewFileMailBox(baseDir string) *FileMailBox {
	os.MkdirAll(baseDir, 0755)
	return &FileMailBox{baseDir: baseDir}
}

func (mb *FileMailBox) inboxPath(agentID string) string {
	return filepath.Join(mb.baseDir, agentID+".json")
}

func (mb *FileMailBox) lockPath(agentID string) string {
	return filepath.Join(mb.baseDir, agentID+".json.lock")
}

func (mb *FileMailBox) Send(recipient string, msg FileMailMessage) error {
	return mb.withLock(recipient, func(messages []FileMailMessage) ([]FileMailMessage, error) {
		msg.Read = false
		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
		}
		return append(messages, msg), nil
	})
}

func (mb *FileMailBox) ReadUnread(agentID string) ([]FileMailMessage, error) {
	messages, err := mb.readInbox(agentID)
	if err != nil {
		return nil, err
	}
	var unread []FileMailMessage
	for _, m := range messages {
		if !m.Read {
			unread = append(unread, m)
		}
	}
	return unread, nil
}

func (mb *FileMailBox) MarkAllRead(agentID string) error {
	return mb.withLock(agentID, func(messages []FileMailMessage) ([]FileMailMessage, error) {
		for i := range messages {
			messages[i].Read = true
		}
		return messages, nil
	})
}

// withLock 获取文件锁，读取收件箱，应用修改，然后写回。
func (mb *FileMailBox) withLock(agentID string, fn func([]FileMailMessage) ([]FileMailMessage, error)) error {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	return withFileLock(mb.lockPath(agentID), func() error {
		messages, err := mb.readInbox(agentID)
		if err != nil {
			return err
		}
		messages, err = fn(messages)
		if err != nil {
			return err
		}
		return mb.writeInbox(agentID, messages)
	})
}

func (mb *FileMailBox) readInbox(agentID string) ([]FileMailMessage, error) {
	path := mb.inboxPath(agentID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var messages []FileMailMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func (mb *FileMailBox) writeInbox(agentID string, messages []FileMailMessage) error {
	path := mb.inboxPath(agentID)
	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return err
	}
	return writeJSONAtomic(path, data, 0644)
}
