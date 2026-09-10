package compact

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// 追加到摘要消息后面的附件块的大小上限。Compact 会清空当前对话；
// 没有这些快照，模型就会忘记自己刚读过哪些文件、
// 当时在按哪些
// skill SOP 干活。
const (
	RecoveryFileLimit      = 5
	RecoveryTokensPerFile  = 5_000
	RecoverySkillsBudget   = 25_000
	RecoveryTokensPerSkill = 5_000
	recoveryCharsPerToken  = 3.5
)

// FileReadRecord 记录一次 ReadFile 调用返回给模型的字节快照。
// compact 之后会重新注入，好让模型在触发阈值那一刻
// 正在推理的内容还在手边。
type FileReadRecord struct {
	Path      string
	Content   string
	Timestamp time.Time
}

// SkillInvocationRecord 保存调用 skill 时附带的 SOP 正文。
// compact 之后同一份定义会被重新拼回去，
// 使跨边界的行为保持一致。
type SkillInvocationRecord struct {
	Name      string
	Body      string
	Timestamp time.Time
}

// RecoveryState 记录需要在 compaction 之后存活的 per-agent 数据。
// 这个结构体的写入是并发安全的 —— 流式执行器里
// tool 回调可能来自流式 executor
// 里并行的 goroutine。
type RecoveryState struct {
	mu     sync.Mutex
	files  map[string]FileReadRecord
	skills map[string]SkillInvocationRecord
}

// NewRecoveryState 返回一个可以直接开始记录的空状态。
func NewRecoveryState() *RecoveryState {
	return &RecoveryState{
		files:  map[string]FileReadRecord{},
		skills: map[string]SkillInvocationRecord{},
	}
}

// RecordFileRead 会覆盖同一路径上的旧记录，让最新快照生效。
// 在 nil receiver 上调用也是安全的。
func (s *RecoveryState) RecordFileRead(path, content string) {
	if s == nil || path == "" {
		return
	}
	s.mu.Lock()
	s.files[path] = FileReadRecord{Path: path, Content: content, Timestamp: time.Now()}
	s.mu.Unlock()
}

// RecordSkillInvocation 会覆盖同一 skill 名下的旧记录。
// 在 nil receiver 上调用也是安全的。
func (s *RecoveryState) RecordSkillInvocation(name, body string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	s.skills[name] = SkillInvocationRecord{Name: name, Body: body, Timestamp: time.Now()}
	s.mu.Unlock()
}

// snapshotFiles 最多返回 `limit` 条记录，按时间由新到旧。
func (s *RecoveryState) snapshotFiles(limit int) []FileReadRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FileReadRecord, 0, len(s.files))
	for _, r := range s.files {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// snapshotSkills 返回所有已记录的 skill，按时间由新到旧。
func (s *RecoveryState) snapshotSkills() []SkillInvocationRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkillInvocationRecord, 0, len(s.skills))
	for _, r := range s.skills {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out
}

// BuildRecoveryAttachment 把 compact 之后用于恢复的各个小节
// （最近读过的文件、skill 定义、工具清单，以及一段
// 「别凭摘要瞎猜」的收尾提示）渲染成一整块文本。
// 没什么可输出时返回 ""，好让调用方
// 保持摘要消息干净。
func BuildRecoveryAttachment(state *RecoveryState, toolSchemas []map[string]any) string {
	var sb strings.Builder

	if files := state.snapshotFiles(RecoveryFileLimit); len(files) > 0 {
		sb.WriteString("## Recently read files\n\n")
		sb.WriteString("These snapshots are what the file-reading tool last returned. Re-open with the tool if you need the current bytes.\n\n")
		for _, f := range files {
			content := truncateByTokens(f.Content, RecoveryTokensPerFile)
			ts := f.Timestamp.UTC().Format("2006-01-02T15:04:05Z")
			fmt.Fprintf(&sb, "### %s  (read %s)\n\n", f.Path, ts)
			sb.WriteString("```\n")
			sb.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				sb.WriteByte('\n')
			}
			sb.WriteString("```\n\n")
		}
	}

	if skills := state.snapshotSkills(); len(skills) > 0 {
		var section strings.Builder
		section.WriteString("## Active skills\n\n")
		section.WriteString("These skills were invoked earlier in the session. Continue to follow each SOP when its triggering condition applies.\n\n")
		used := 0
		emitted := false
		for _, sk := range skills {
			body := truncateByTokens(sk.Body, RecoveryTokensPerSkill)
			tokens := approxTokens(body) + approxTokens(sk.Name) + 8
			if used+tokens > RecoverySkillsBudget {
				break
			}
			used += tokens
			fmt.Fprintf(&section, "### %s\n\n%s\n\n", sk.Name, body)
			emitted = true
		}
		if emitted {
			sb.WriteString(section.String())
		}
	}

	if len(toolSchemas) > 0 {
		sb.WriteString("## Available tools\n\nYou still have access to the following tools — call them directly when the task needs one:\n\n")
		for _, t := range toolSchemas {
			name, _ := t["name"].(string)
			if name == "" {
				continue
			}
			desc, _ := t["description"].(string)
			desc = firstLine(desc)
			if desc != "" {
				fmt.Fprintf(&sb, "- %s — %s\n", name, desc)
			} else {
				fmt.Fprintf(&sb, "- %s\n", name)
			}
		}
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		return ""
	}

	sb.WriteString("## Note\n\nEverything above the divider is reconstructed context. For exact code, error strings, or user-typed text, re-read the source rather than guess from the summary.\n")
	return sb.String()
}

// approxTokens 用与 EstimateTokens 相同的「字符数/token」估算口径，
// 这样整个包的预算计算保持一致。
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	return int(float64(len(s)) / recoveryCharsPerToken)
}

// truncateByTokens 在刚好低于 token 预算的字节偏移处截断 s，
// 并追加一个标记，
// 让模型能看出内容被裁剪过。
func truncateByTokens(s string, tokenBudget int) string {
	if tokenBudget <= 0 || s == "" {
		return s
	}
	if approxTokens(s) <= tokenBudget {
		return s
	}
	maxChars := int(float64(tokenBudget) * recoveryCharsPerToken)
	if maxChars <= 0 || maxChars >= len(s) {
		return s
	}
	return s[:maxChars] + "\n… (content truncated)"
}

// firstLine 返回 s 的第一行非空内容，并做 trim。用于在描述是多段文本时
// 让工具清单保持紧凑。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
