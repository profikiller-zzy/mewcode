package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireRepositoryLockHonorsCancellation(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "repo.lock")
	if err := os.WriteFile(lockPath, []byte("held"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := acquireRepositoryLock(ctx, lockPath)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquireRepositoryLock error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled lock acquisition took %s", elapsed)
	}
}
