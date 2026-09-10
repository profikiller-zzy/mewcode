package skills

import (
	"strings"
)

type SkillMeta struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	WhenToUse   string   `yaml:"when_to_use"`
	Tags        []string `yaml:"tags"`
	// Mode 选择执行模式。"inline"（默认）把 skill 正文注入当前对话；
	// "fork" 则在上下文隔离的 sub-agent 里跑 skill 正文。
	Mode string `yaml:"mode"`
	// Model 覆盖本 skill 使用的 LLM。留空 = 继承主循环。
	Model string `yaml:"model"`
	// Context 只为向后兼容保留：老 skill 用 `context: fork` 表示 Mode=fork。
	// 值为 "fork" 时按 fork 模式处理。
	Context string `yaml:"context"`
	// ForkContext 控制父对话有多少内容被带进 fork 出来的 sub-agent。
	// 只有 Mode == "fork" 时才有意义。取值："full"（父对话的 LLM 摘要）、
	// "recent"（最后 5 条消息）、"none"（不带父上下文，默认）。
	ForkContext string `yaml:"fork_context"`
}

// IsFork 报告该 skill 是否应以 fork 模式运行。为向后兼容，Mode 和遗留的
// Context 字段都会检查。
func (m SkillMeta) IsFork() bool {
	return m.Mode == "fork" || m.Context == "fork"
}

type Skill struct {
	Meta       SkillMeta
	PromptBody string
	SourceDir  string
	// IsDirectory 标记 SourceDir 里还带着附加资源（references/、scripts/）的
	// skill。目录型 skill 为 true，即磁盘上 SKILL.md 旁边还有配套文件。
	// 只有内嵌 skill 为 false —— 它们运行时在磁盘上没有真实目录可访问。
	IsDirectory bool
	// BodyLoaded 标记 PromptBody 是否已经从磁盘读出。Phase-1 加载只读
	// frontmatter，正文一直为空，直到 GetFull 触发一次读取。
	BodyLoaded bool
}

// Render 返回替换了 $ARGUMENTS 的 skill 正文。如果正文里没有 $ARGUMENTS
// 占位符、而 args 非空，就把 args 追加到 "## User Request" 小节里。
//
// 执行方式由 Meta.Mode 决定，不体现在渲染结果里：inline 的正文直接进主对话，
// fork 的正文由调用方交给隔离子 Agent，两条路径拿到的都是这里渲染出的同一份文本。
func (s *Skill) Render(args string) string {
	body := s.PromptBody
	if strings.Contains(body, "$ARGUMENTS") {
		return strings.ReplaceAll(body, "$ARGUMENTS", args)
	}
	if strings.TrimSpace(args) == "" {
		return body
	}
	return body + "\n\n## User Request\n\n" + args
}

