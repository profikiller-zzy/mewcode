package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RelevantMemory 表示一个被选中、准备注入主对话的记忆文件。
// MtimeMs 一并传递，调用方无需再次 stat 即可显示新鲜度。
type RelevantMemory struct {
	Path    string
	MtimeMs int64
}

// SelectorFn 抽象召回选择器使用的旁路 LLM 调用。调用方接收系统提示词和用户消息，
// 执行一次模型调用并返回原始文本。FindRelevantMemories 将错误视为“选择失败，不召回”。
// 通过回调可以创建独立的旁路客户端，避免 memory 包直接依赖 llm 包。
type SelectorFn func(ctx context.Context, systemPrompt, userMessage string) (string, error)

// SelectMemoriesSystemPrompt 是选择器 Agent 使用的系统提示词。
const SelectMemoriesSystemPrompt = `You are selecting memories that will be useful to MewCode as it processes a user's query. You will be given the user's query and a list of available memory files with their filenames and descriptions.

Return a list of filenames for the memories that will clearly be useful to MewCode as it processes the user's query (up to 5). Only include memories that you are certain will be helpful based on their name and description.
- If you are unsure if a memory will be useful in processing the user's query, then do not include it in your list. Be selective and discerning.
- If there are no memories in the list that would clearly be useful, feel free to return an empty list.
- If a list of recently-used tools is provided, do not select memories that are usage reference or API documentation for those tools (MewCode is already exercising them). DO still select memories containing warnings, gotchas, or known issues about those tools — active use is exactly when those matter.

Respond with valid JSON only, no markdown, in this exact shape: {"selected_memories": ["filename1.md", "filename2.md"]}`

// FindRelevantMemories 扫描用户级和项目级目录，让选择器为查询挑选最多 5 个相关文件，
// 返回对应的绝对路径和 mtime。MEMORY.md 已在系统提示词中加载，因此会排除它。
//
// alreadySurfaced 会在调用选择器前排除之前展示过的路径，让 5 个名额用于新的候选项。
//
// 任一目录都可以为空，只扫描非空目录。两个目录出现同名文件时，返回结果用 FilePath 区分。
//
// 选择器失败会静默处理：召回是尽力而为，不能阻塞主对话。
// 选择器或解析出错时返回空切片和 nil 错误。
func FindRelevantMemories(
	ctx context.Context,
	query string,
	userMemDir, projectMemDir string,
	recentTools []string,
	alreadySurfaced map[string]struct{},
	selector SelectorFn,
) ([]RelevantMemory, error) {
	if selector == nil {
		return nil, nil
	}
	var all []MemoryHeader
	if userMemDir != "" {
		userScan, err := ScanMemoryFiles(ctx, userMemDir, "user")
		if err != nil {
			return nil, err
		}
		all = append(all, userScan...)
	}
	if projectMemDir != "" {
		projectScan, err := ScanMemoryFiles(ctx, projectMemDir, "project")
		if err != nil {
			return nil, err
		}
		all = append(all, projectScan...)
	}
	memories := make([]MemoryHeader, 0, len(all))
	for _, m := range all {
		if _, ok := alreadySurfaced[m.FilePath]; ok {
			continue
		}
		memories = append(memories, m)
	}
	if len(memories) == 0 {
		return nil, nil
	}

	selectedFilenames, _ := selectRelevantMemories(ctx, query, memories, recentTools, selector)
	byKey := make(map[string]MemoryHeader, len(memories))
	for _, m := range memories {
		byKey[m.FilePath] = m
		// 同时按 Filename 建索引，兼容选择器只返回文件名而不返回路径的情况。
		if _, exists := byKey[m.Filename]; !exists {
			byKey[m.Filename] = m
		}
	}
	selected := make([]RelevantMemory, 0, len(selectedFilenames))
	for _, fn := range selectedFilenames {
		m, ok := byKey[fn]
		if !ok {
			continue
		}
		selected = append(selected, RelevantMemory{Path: m.FilePath, MtimeMs: m.MtimeMs})
	}
	return selected, nil
}

func selectRelevantMemories(
	ctx context.Context,
	query string,
	memories []MemoryHeader,
	recentTools []string,
	selector SelectorFn,
) ([]string, error) {
	validFilenames := make(map[string]struct{}, len(memories))
	for _, m := range memories {
		validFilenames[m.Filename] = struct{}{}
	}

	manifest := FormatMemoryManifest(memories)

	// MewCode 正在使用某个工具时，展示该工具的参考文档通常是噪声，因为对话已有实际用法。
	// 否则查询和描述中的同名关键词可能造成误选。
	toolsSection := ""
	if len(recentTools) > 0 {
		toolsSection = "\n\nRecently used tools: " + strings.Join(recentTools, ", ")
	}

	userMessage := fmt.Sprintf("Query: %s\n\nAvailable memories:\n%s%s", query, manifest, toolsSection)

	raw, err := selector(ctx, SelectMemoriesSystemPrompt, userMessage)
	if err != nil {
		return nil, nil
	}
	clean := extractJSONObject(raw)
	if clean == "" {
		return nil, nil
	}
	var parsed struct {
		SelectedMemories []string `json:"selected_memories"`
	}
	if err := json.Unmarshal([]byte(clean), &parsed); err != nil {
		return nil, nil
	}
	out := make([]string, 0, len(parsed.SelectedMemories))
	for _, f := range parsed.SelectedMemories {
		if _, ok := validFilenames[f]; ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// extractJSONObject 返回原始文本中的第一个 JSON 对象；如果文本以 { 开头则直接返回裁剪后的文本。
// 即使模型违反要求添加 Markdown 围栏或说明文字，也能尽量容错。
func extractJSONObject(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	start := strings.Index(trimmed, "{")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(trimmed, "}")
	if end < start {
		return ""
	}
	return trimmed[start : end+1]
}

