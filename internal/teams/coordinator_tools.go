package teams

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"mewcode/internal/tools"
	"mewcode/internal/worktree"
)

// TeamStatusTool 返回一个 Team 的成员和 worktree 元信息，供 Lead 调度和收尾。
type TeamStatusTool struct{ TeamMgr *TeamManager }

func (t *TeamStatusTool) Name() string                 { return "TeamStatus" }
func (t *TeamStatusTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *TeamStatusTool) Description() string {
	return "Show team members, activity state, and registered worktree paths. Read-only."
}
func (t *TeamStatusTool) Schema() map[string]any {
	return coordinatorSchema(t.Name(), t.Description(), map[string]any{
		"team_name": map[string]any{"type": "string", "description": "Team name"},
	}, []string{"team_name"})
}
func (t *TeamStatusTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	teamName, _ := args["team_name"].(string)
	team, err := coordinatorTeam(t.TeamMgr, teamName)
	if err != nil {
		return tools.ToolResult{Output: err.Error(), IsError: true}
	}

	team.mu.Lock()
	defer team.mu.Unlock()
	lines := []string{fmt.Sprintf("Team %q (mode=%s, members=%d):", team.Name, team.Mode, len(team.Members))}
	names := make([]string, 0, len(team.Members))
	for name := range team.Members {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		member := team.Members[name]
		if member == nil {
			lines = append(lines, fmt.Sprintf("  %s: unavailable", name))
			continue
		}
		state := "idle"
		if member.Active {
			state = "active"
		}
		worktreePath := "(none)"
		if member.WorktreePath != "" {
			worktreePath = member.WorktreePath
		}
		lines = append(lines, fmt.Sprintf("  %s: %s, worktree=%s", name, state, worktreePath))
	}
	return tools.ToolResult{Output: strings.Join(lines, "\n")}
}

// WorktreeInspectTool 只返回受限的 status、commit 和 diff 信息，不允许任意路径读取。
type WorktreeInspectTool struct{ TeamMgr *TeamManager }

func (t *WorktreeInspectTool) Name() string                 { return "WorktreeInspect" }
func (t *WorktreeInspectTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *WorktreeInspectTool) Description() string {
	return "Inspect a teammate's managed worktree: branch, status, commit count, and a bounded diff. Read-only."
}
func (t *WorktreeInspectTool) Schema() map[string]any {
	return coordinatorSchema(t.Name(), t.Description(), map[string]any{
		"team_name": map[string]any{"type": "string", "description": "Team name"},
		"teammate":  map[string]any{"type": "string", "description": "Teammate name"},
		"max_bytes": map[string]any{"type": "integer", "description": "Maximum diff bytes (default 20000, capped at 50000)"},
	}, []string{"team_name", "teammate"})
}
func (t *WorktreeInspectTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	_, member, err := coordinatorMember(t.TeamMgr, args)
	if err != nil {
		return tools.ToolResult{Output: err.Error(), IsError: true}
	}
	if member.WorktreePath == "" {
		return tools.ToolResult{Output: fmt.Sprintf("Teammate %q has no managed worktree", member.Name), IsError: true}
	}
	maxBytes := 20000
	if raw, ok := args["max_bytes"].(float64); ok {
		maxBytes = int(raw)
	} else if raw, ok := args["max_bytes"].(int); ok {
		maxBytes = raw
	}
	if maxBytes <= 0 {
		maxBytes = 20000
	}
	if maxBytes > 50000 {
		maxBytes = 50000
	}
	gitRoot := worktree.FindCanonicalGitRoot(member.WorktreePath)
	info, err := worktree.InspectAgentWorktree(ctx, gitRoot, member.WorktreePath, maxBytes)
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("Worktree inspect failed: %v", err), IsError: true}
	}
	clean := "dirty"
	if info.Clean {
		clean = "clean"
	}
	lines := []string{
		fmt.Sprintf("Teammate %q worktree:", member.Name),
		fmt.Sprintf("  path: %s", info.WorktreePath),
		fmt.Sprintf("  branch: %s", info.Branch),
		fmt.Sprintf("  head: %s", info.HeadCommit),
		fmt.Sprintf("  status: %s (changed_files=%d)", clean, info.ChangedFiles),
		fmt.Sprintf("  commits_since_main_head: %d", info.Commits),
	}
	if info.Status != "" {
		lines = append(lines, "  porcelain:\n"+info.Status)
	}
	if info.Diff != "" {
		lines = append(lines, "  diff:\n"+info.Diff)
	}
	return tools.ToolResult{Output: strings.Join(lines, "\n")}
}

// WorktreeMergeTool 只能把当前 Team 登记的 Worker worktree 整合到主仓库。
type WorktreeMergeTool struct{ TeamMgr *TeamManager }

func (t *WorktreeMergeTool) Name() string                 { return "WorktreeMerge" }
func (t *WorktreeMergeTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *WorktreeMergeTool) Description() string {
	return "Merge or cherry-pick a reviewed teammate worktree into the main repository. Requires clean worktrees and uses a repository lock."
}
func (t *WorktreeMergeTool) Schema() map[string]any {
	return coordinatorSchema(t.Name(), t.Description(), map[string]any{
		"team_name": map[string]any{"type": "string", "description": "Team name"},
		"teammate":  map[string]any{"type": "string", "description": "Teammate name"},
		"strategy":  map[string]any{"type": "string", "enum": []string{"merge", "cherry-pick"}, "description": "Integration strategy (default merge)"},
		"commit":    map[string]any{"type": "string", "description": "Full commit SHA, required for cherry-pick"},
	}, []string{"team_name", "teammate"})
}
func (t *WorktreeMergeTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	_, member, err := coordinatorMember(t.TeamMgr, args)
	if err != nil {
		return tools.ToolResult{Output: err.Error(), IsError: true}
	}
	if member.WorktreePath == "" {
		return tools.ToolResult{Output: fmt.Sprintf("Teammate %q has no managed worktree", member.Name), IsError: true}
	}
	strategy, _ := args["strategy"].(string)
	commit, _ := args["commit"].(string)
	gitRoot := worktree.FindCanonicalGitRoot(member.WorktreePath)
	output, err := worktree.MergeAgentWorktree(ctx, gitRoot, member.WorktreePath, strategy, commit)
	if err != nil {
		return tools.ToolResult{Output: fmt.Sprintf("Worktree integration failed: %v", err), IsError: true}
	}
	return tools.ToolResult{Output: fmt.Sprintf("Integrated teammate %q (%s):\n%s", member.Name, coordinatorStrategyName(strategy), output)}
}

// WorktreeFinalizeTool 在 Lead 明确决定后保留或删除 Worker worktree。
type WorktreeFinalizeTool struct{ TeamMgr *TeamManager }

func (t *WorktreeFinalizeTool) Name() string                 { return "WorktreeFinalize" }
func (t *WorktreeFinalizeTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *WorktreeFinalizeTool) Description() string {
	return "Keep or remove a teammate's managed worktree after review. Removal is conservative unless force=true."
}
func (t *WorktreeFinalizeTool) Schema() map[string]any {
	return coordinatorSchema(t.Name(), t.Description(), map[string]any{
		"team_name": map[string]any{"type": "string", "description": "Team name"},
		"teammate":  map[string]any{"type": "string", "description": "Teammate name"},
		"action":    map[string]any{"type": "string", "enum": []string{"keep", "remove"}, "description": "Keep or remove the worktree"},
		"force":     map[string]any{"type": "boolean", "description": "Allow removal when commits or uncommitted changes remain"},
	}, []string{"team_name", "teammate", "action"})
}
func (t *WorktreeFinalizeTool) Execute(ctx context.Context, args map[string]any) tools.ToolResult {
	_, member, err := coordinatorMember(t.TeamMgr, args)
	if err != nil {
		return tools.ToolResult{Output: err.Error(), IsError: true}
	}
	if member.WorktreePath == "" {
		return tools.ToolResult{Output: fmt.Sprintf("Teammate %q has no managed worktree", member.Name), IsError: true}
	}
	action, _ := args["action"].(string)
	if action == "keep" {
		return tools.ToolResult{Output: fmt.Sprintf("Kept worktree for %q at %s", member.Name, member.WorktreePath)}
	}
	if action != "remove" {
		return tools.ToolResult{Output: "Error: action must be 'keep' or 'remove'", IsError: true}
	}
	force, _ := args["force"].(bool)
	gitRoot := worktree.FindCanonicalGitRoot(member.WorktreePath)
	info, inspectErr := worktree.InspectAgentWorktree(ctx, gitRoot, member.WorktreePath, 1)
	if inspectErr != nil {
		return tools.ToolResult{Output: fmt.Sprintf("Worktree inspect failed: %v", inspectErr), IsError: true}
	}
	if !force && (!info.Clean || info.Commits > 0) {
		return tools.ToolResult{Output: "Refusing to remove worktree with changes or commits; review/merge it first or pass force=true", IsError: true}
	}
	branch := info.Branch
	if !worktree.RemoveAgentWorktree(ctx, member.WorktreePath, branch, gitRoot) {
		return tools.ToolResult{Output: fmt.Sprintf("Failed to remove worktree %s", member.WorktreePath), IsError: true}
	}
	return tools.ToolResult{Output: fmt.Sprintf("Removed worktree for %q: %s", member.Name, member.WorktreePath)}
}

func coordinatorSchema(name, description string, properties map[string]any, required []string) map[string]any {
	return map[string]any{
		"name": name, "description": description,
		"input_schema": map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func coordinatorStrategyName(strategy string) string {
	if strategy == "" {
		return "merge"
	}
	return strategy
}

func coordinatorTeam(mgr *TeamManager, name string) (*Team, error) {
	if mgr == nil || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("Error: 'team_name' is required")
	}
	team := mgr.GetTeam(name)
	if team == nil {
		return nil, fmt.Errorf("Error: team %q not found", name)
	}
	return team, nil
}

func coordinatorMember(mgr *TeamManager, args map[string]any) (*Team, Member, error) {
	teamName, _ := args["team_name"].(string)
	memberName, _ := args["teammate"].(string)
	team, err := coordinatorTeam(mgr, teamName)
	if err != nil {
		return nil, Member{}, err
	}
	if strings.TrimSpace(memberName) == "" {
		return nil, Member{}, fmt.Errorf("Error: 'teammate' is required")
	}
	team.mu.Lock()
	member, ok := team.Members[memberName]
	if !ok || member == nil {
		team.mu.Unlock()
		return nil, Member{}, fmt.Errorf("Error: teammate %q not found in team %q", memberName, teamName)
	}
	snapshot := *member
	team.mu.Unlock()
	return team, snapshot, nil
}

// RegisterCoordinatorTools 把 Lead 专用的观察/整合工具注册到根 registry。
// Worker 继承 registry 时会由 TeammateDisallowedTools 过滤掉这些工具。
func RegisterCoordinatorTools(reg *tools.Registry, mgr *TeamManager) {
	if reg == nil {
		return
	}
	reg.Register(&TeamStatusTool{TeamMgr: mgr})
	reg.Register(&WorktreeInspectTool{TeamMgr: mgr})
	reg.Register(&WorktreeMergeTool{TeamMgr: mgr})
	reg.Register(&WorktreeFinalizeTool{TeamMgr: mgr})
}
