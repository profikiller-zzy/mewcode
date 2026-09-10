package commands

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CommandMeta 是基于文件的 prompt command 的 frontmatter。只保留为旧版
// /commands/ 文件读取的那几个字段：description、argument-hint、aliases。
type CommandMeta struct {
	Description  string   `yaml:"description"`
	ArgumentHint string   `yaml:"argument-hint"`
	Aliases      []string `yaml:"aliases"`
}

// LoadDir 递归扫描 dir 下的 *.md 文件，每个文件返回一个 Command。命令名由
// 文件相对于 dir 的路径推导而来，子目录按最初的命名空间规则
// 用 ':' 连接（sub/dir/foo.md → "sub:dir:foo"）。解析失败的文件会被静默跳过
// （一个写坏的用户命令不应该让启动失败）。
//
// 返回的每个命令都带 Type=TypePrompt，以及一个把 markdown 正文中的
// $ARGUMENTS 替换掉的 Handler。如果正文里没有 $ARGUMENTS 占位符而 args 非空，
// args 会被追加到一个 "## User Request" 小节里。
func LoadDir(dir string) []*Command {
	if dir == "" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}

	var cmds []*Command
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".md") {
			return nil
		}
		cmd := parseCommandFile(dir, path)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
		return nil
	})
	return cmds
}

// LoadUserCommands 合并两个搜索路径下基于文件的命令：
// 1. ~/.mewcode/commands/（用户全局）2. $workDir/.mewcode/commands/（项目级）。
//
// 名字冲突时，后加载的覆盖先加载的。
func LoadUserCommands(workDir string) []*Command {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".mewcode", "commands"))
	}
	dirs = append(dirs,
		filepath.Join(workDir, ".mewcode", "commands"),
	)

	merged := map[string]*Command{}
	var order []string
	for _, d := range dirs {
		for _, cmd := range LoadDir(d) {
			if _, seen := merged[cmd.Name]; !seen {
				order = append(order, cmd.Name)
			}
			merged[cmd.Name] = cmd
		}
	}

	out := make([]*Command, 0, len(order))
	for _, name := range order {
		out = append(out, merged[name])
	}
	return out
}

// parseCommandFile 读取单个 .md 文件并返回对应的 Command，读取或解析失败时
// 返回 nil。名字由相对路径算出：baseDir 下的 "git/log.md" → "git:log"。
// 名字会转成小写，以匹配 Parse 中 /<name> 的查找约定。
func parseCommandFile(baseDir, path string) *Command {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(baseDir, path)
	if err != nil {
		return nil
	}
	rel = strings.TrimSuffix(rel, ".md")
	parts := strings.Split(rel, string(filepath.Separator))
	for i, p := range parts {
		parts[i] = strings.ToLower(strings.ReplaceAll(p, " ", "-"))
	}
	name := strings.Join(parts, ":")
	if name == "" {
		return nil
	}

	meta, body := splitFrontmatter(string(data))
	body = strings.TrimSpace(body)
	if meta.Description == "" {
		meta.Description = firstNonHeaderLine(body)
	}

	return &Command{
		Name:        name,
		Description: meta.Description,
		Aliases:     meta.Aliases,
		Type:        TypePrompt,
		ArgPrompt:   meta.ArgumentHint,
		Handler:     promptHandler(body),
	}
}

// splitFrontmatter 把 YAML frontmatter 和 markdown 正文分开。
// 如果没有 frontmatter 或解析失败，就返回空的 meta 和原始内容 ——
// 一个写坏的命令文件不应该让启动失败。
func splitFrontmatter(content string) (CommandMeta, string) {
	var meta CommandMeta
	if !strings.HasPrefix(strings.TrimSpace(content), "---") {
		return meta, content
	}
	parts := strings.SplitN(content, "---", 3)
	if len(parts) < 3 {
		return meta, content
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		return CommandMeta{}, content
	}
	return meta, parts[2]
}

// firstNonHeaderLine 返回第一行非空、非标题的行 —— 当 frontmatter
// 没有提供 description 时用作兜底。
func firstNonHeaderLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// promptHandler 返回一个 Handler，渲染命令正文并替换其中的 $ARGUMENTS。
// 没有占位符的正文会把 args 追加到一个 "## User Request" 小节里。
func promptHandler(body string) Handler {
	return func(ctx *Context) string {
		if strings.Contains(body, "$ARGUMENTS") {
			return strings.ReplaceAll(body, "$ARGUMENTS", ctx.Args)
		}
		if strings.TrimSpace(ctx.Args) == "" {
			return body
		}
		return body + "\n\n## User Request\n\n" + ctx.Args
	}
}
