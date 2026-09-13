package worktree

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

const (
	worktreeLockTimeout = 30 * time.Second
	worktreeLockStale   = 2 * time.Minute
	worktreeLockBackoff = 80 * time.Millisecond
)

// withRepositoryLock serializes operations that mutate the repository's
// shared git metadata (.git/config, refs and .git/worktrees). Git's own
// lockfiles protect individual writes, but two concurrent `git worktree add`
// operations can still race while updating the shared config.
func withRepositoryLock(ctx context.Context, repoRoot string, fn func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	gitDir, err := ResolveGitDir(repoRoot)
	if err != nil {
		return err
	}
	if gitDir == "" {
		return fmt.Errorf("cannot lock worktree metadata: not a git repository")
	}
	commonDir, err := GetCommonDir(gitDir)
	if err != nil {
		return err
	}
	if commonDir == "" {
		commonDir = gitDir
	}
	lockPath := filepath.Join(commonDir, "mewcode-worktree.lock")
	if err := acquireRepositoryLock(ctx, lockPath); err != nil {
		return err
	}
	defer os.Remove(lockPath)
	return fn()
}

func acquireRepositoryLock(ctx context.Context, lockPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(worktreeLockTimeout)
	backoff := 5 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fd, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = fd.Close()
			return nil
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > worktreeLockStale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 worktree 仓库锁 %s 超过 %s", lockPath, worktreeLockTimeout)
		}
		wait := backoff + time.Duration(rand.Int63n(int64(backoff)))
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		}
		if backoff < worktreeLockBackoff {
			backoff *= 2
		}
	}
}
