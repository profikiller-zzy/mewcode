package tools

import (
	"context"
	"path/filepath"
	"testing"
)

func TestResolveToolArgumentsUsesAgentWorktree(t *testing.T) {
	workdir := t.TempDir()
	ctx := WithWorkDir(context.Background(), workdir)

	args := ResolveToolArguments(ctx, "ReadFile", map[string]any{"file_path": "src/main.go"})
	if got, want := args["file_path"], filepath.Join(workdir, "src/main.go"); got != want {
		t.Fatalf("resolved file path = %v, want %v", got, want)
	}

	glob := ResolveToolArguments(ctx, "Glob", map[string]any{"pattern": "*.go"})
	if got, want := glob["path"], workdir; got != want {
		t.Fatalf("default glob path = %v, want %v", got, want)
	}
}
