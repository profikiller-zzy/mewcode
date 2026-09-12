package tools

import (
	"context"
	"path/filepath"
)

// workDirContextKey carries the agent's effective working directory through a
// tool invocation. Tool instances are intentionally shareable between agents,
// so per-agent state must travel with the call rather than being stored on the
// Tool value itself.
type workDirContextKey struct{}

// WithWorkDir returns a context whose relative filesystem operations should be
// resolved against workDir. An empty workDir leaves the caller's existing
// process-working-directory semantics unchanged.
func WithWorkDir(ctx context.Context, workDir string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if workDir == "" {
		return ctx
	}
	return context.WithValue(ctx, workDirContextKey{}, workDir)
}

// WorkDir returns the effective per-agent working directory, if one was set.
func WorkDir(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	workDir, _ := ctx.Value(workDirContextKey{}).(string)
	return workDir
}

// ResolvePath resolves a relative tool path against the effective agent
// worktree. Absolute paths remain absolute so callers can intentionally access
// paths outside the worktree when the permission layer allows them.
func ResolvePath(ctx context.Context, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if workDir := WorkDir(ctx); workDir != "" {
		return filepath.Join(workDir, path)
	}
	return path
}

// ResolvePathArgument copies args and resolves the path-bearing argument used
// by the built-in filesystem tools. Keeping the original map untouched means
// the conversation still records exactly what the model asked for.
func ResolvePathArgument(ctx context.Context, args map[string]any, key string) map[string]any {
	if args == nil {
		return nil
	}
	value, ok := args[key].(string)
	if !ok || value == "" || WorkDir(ctx) == "" || filepath.IsAbs(value) {
		return args
	}
	resolved := make(map[string]any, len(args))
	for k, v := range args {
		resolved[k] = v
	}
	resolved[key] = ResolvePath(ctx, value)
	return resolved
}

// ResolveToolArguments resolves the path-bearing argument used by built-in
// filesystem tools. Unknown tools receive the original map unchanged because
// their argument semantics are tool-specific.
func ResolveToolArguments(ctx context.Context, toolName string, args map[string]any) map[string]any {
	switch toolName {
	case "ReadFile", "WriteFile", "EditFile":
		return ResolvePathArgument(ctx, args, "file_path")
	case "Glob", "Grep":
		_, hadPath := args["path"]
		resolved := ResolvePathArgument(ctx, args, "path")
		if WorkDir(ctx) != "" {
			path, _ := resolved["path"].(string)
			if path == "" {
				if !hadPath {
					resolved = make(map[string]any, len(args)+1)
					for k, v := range args {
						resolved[k] = v
					}
				}
				resolved["path"] = WorkDir(ctx)
			}
		}
		return resolved
	default:
		return args
	}
}
