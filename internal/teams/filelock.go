package teams

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

const (
	fileLockAcquireTimeout = 5 * time.Second
	fileLockStaleAge       = 10 * time.Second
	fileLockMaxBackoff     = 80 * time.Millisecond
)

// withFileLock implements a cooperative cross-process lock using an atomic
// O_CREATE|O_EXCL lock-file creation. The callback runs while the lock file
// exists and the lock is removed on every return path.
func withFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return err
	}
	deadline := time.Now().Add(fileLockAcquireTimeout)
	backoff := 5 * time.Millisecond
	for {
		fd, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = fd.Close()
			defer os.Remove(lockPath)
			return fn()
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > fileLockStaleAge {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待文件锁 %s 超过 %s", lockPath, fileLockAcquireTimeout)
		}
		time.Sleep(backoff + time.Duration(rand.Int63n(int64(backoff))))
		if backoff < fileLockMaxBackoff {
			backoff *= 2
		}
	}
}

// writeJSONAtomic writes a complete JSON document and atomically replaces the
// destination, so readers never observe a partially-written file.
func writeJSONAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
