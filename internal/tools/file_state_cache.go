package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileStateCache tracks which files have been read and their modification
// times, enforcing a "read-before-edit" discipline to prevent blind overwrites.
type FileStateCache struct {
	mu      sync.Mutex
	entries map[string]int64 // path → mtime (UnixMilli)
}

func NewFileStateCache() *FileStateCache {
	return &FileStateCache{
		entries: make(map[string]int64),
	}
}

// Record stores the file mtime after a successful read.
func (c *FileStateCache) Record(filePath string, mtime int64) {
	abs := normalizePath(filePath)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[abs] = mtime
}

// Check verifies that a file has been read and hasn't been modified since.
// Returns (true, "") if OK, or (false, errorMessage) if the edit should be
// blocked.
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
		// File might have been deleted — let the caller handle that.
		return true, ""
	}
	currentMtime := info.ModTime().UnixMilli()
	if currentMtime > cachedMtime {
		return false, fmt.Sprintf("Error: file has been modified since last read. Read it again before editing.")
	}

	return true, ""
}

// Update refreshes the cache entry after a successful edit or write.
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
