package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStateCache 记录哪些文件被读过以及它们的修改时间，
// 以此强制「先读后改」的规矩，避免盲写覆盖。
type FileStateCache struct {
	mu      sync.Mutex
	entries map[string]int64 // 路径 → mtime（UnixMilli）
}

func NewFileStateCache() *FileStateCache {
	return &FileStateCache{
		entries: make(map[string]int64),
	}
}

// Record 在成功读取后记录文件的 mtime。
func (c *FileStateCache) Record(filePath string, mtime int64) {
	abs := normalizePath(filePath)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[abs] = mtime
}

// Check 校验文件是否被读过、之后有没有被改动过。
// 通过时返回 (true, "")，需要拦截编辑时
// 返回 (false, errorMessage)。
func (c *FileStateCache) Check(filePath string) (bool, string) {
	abs := normalizePath(filePath)
	c.mu.Lock()
	cachedMtime, exists := c.entries[abs]
	c.mu.Unlock()

	if !exists {
		return false, fmt.Sprintf("Error: file has not been read yet. Read it first before editing.")
	}

	info, err := os.Stat(abs)
	if err != nil {
		// 文件可能已被删除 —— 交给调用方处理。
		return true, ""
	}
	currentMtime := info.ModTime().UnixMilli()
	if currentMtime > cachedMtime {
		return false, fmt.Sprintf("Error: file has been modified since last read. Read it again before editing.")
	}

	return true, ""
}

// Update 在编辑或写入成功后刷新缓存条目。
func (c *FileStateCache) Update(filePath string) {
	abs := normalizePath(filePath)
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[abs] = info.ModTime().UnixMilli()
}

func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
