package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseFrontmatterOnly 做第一阶段加载：只读 skill 文件里够解析出
// SkillMeta 的部分，PromptBody 留空。开销足够小，
// 启动时对上百个 skill 跑一遍也没问题。
//
// 两种目录布局都支持：
//   - <dir>/skill.yaml（+ 可选的 prompt.md，第一阶段忽略）
//   - <dir>/SKILL.md，带 `---` YAML frontmatter
func parseFrontmatterOnly(dir string) (*Skill, error) {
	yamlPath := filepath.Join(dir, "skill.yaml")
	if data, err := os.ReadFile(yamlPath); err == nil {
		var meta SkillMeta
		if err := yaml.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("parse skill.yaml: %w", err)
		}
		applyMetaDefaults(&meta, dir, "")
		return &Skill{
			Meta:        meta,
			SourceDir:   dir,
			IsDirectory: true,
			BodyLoaded:  false,
		}, nil
	}

	mdPath := filepath.Join(dir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return nil, fmt.Errorf("no skill.yaml or SKILL.md: %w", err)
	}
	meta, _ := splitFrontmatter(string(data))
	applyMetaDefaults(&meta, dir, string(data))
	return &Skill{
		Meta:        meta,
		SourceDir:   dir,
		IsDirectory: true,
		BodyLoaded:  false,
	}, nil
}

// loadSkillBody 读取已经解析过 frontmatter 的 skill 的正文。
// 每次 skill 被调用时由 Catalog.GetFull 调用（热加载）。
// 出现任何读取/解析错误时，保持已有的 PromptBody 不变并把错误返回，
// 让调用方可以退回缓存版本。
func loadSkillBody(skill *Skill) error {
	yamlPath := filepath.Join(skill.SourceDir, "skill.yaml")
	if _, err := os.Stat(yamlPath); err == nil {
		promptPath := filepath.Join(skill.SourceDir, "prompt.md")
		body, err := os.ReadFile(promptPath)
		if err != nil {
			return fmt.Errorf("read prompt.md: %w", err)
		}
		skill.PromptBody = string(body)
		skill.BodyLoaded = true
		return nil
	}

	mdPath := filepath.Join(skill.SourceDir, "SKILL.md")
	data, err := os.ReadFile(mdPath)
	if err != nil {
		return fmt.Errorf("read SKILL.md: %w", err)
	}
	_, body := splitFrontmatter(string(data))
	skill.PromptBody = body
	skill.BodyLoaded = true
	return nil
}

// loadSkillFromBytes 从内存中的字节解析 skill（用于 go:embed 的内置 skill，
// 它们没有可以重新读取的磁盘目录）。
func loadSkillFromBytes(name string, mdBytes []byte) (*Skill, error) {
	meta, body := splitFrontmatter(string(mdBytes))
	if meta.Name == "" {
		meta.Name = name
	}
	applyMetaDefaults(&meta, name, string(mdBytes))
	return &Skill{
		Meta:        meta,
		PromptBody:  body,
		SourceDir:   "", // 内嵌 —— 没有源目录
		IsDirectory: false,
		BodyLoaded:  true,
	}, nil
}

// splitFrontmatter 把 YAML frontmatter 和 markdown 正文分开。
// 没有 `---` frontmatter 时返回零值 meta。
func splitFrontmatter(content string) (SkillMeta, string) {
	var meta SkillMeta
	body := content

	if strings.HasPrefix(strings.TrimSpace(content), "---") {
		parts := strings.SplitN(content, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &meta); err == nil {
				body = strings.TrimSpace(parts[2])
			}
		}
	}
	return meta, body
}

// applyMetaDefaults 按旧版 parseSkillMD 的方式
// 补齐 name/description 的兜底值：
//   - 缺 name → 从目录 basename 推导（转小写 + kebab）
//   - 缺 description → 取正文里第一条非空、非标题的行
//   - 缺 Mode → "inline"
//   - 缺 ForkContext → "none"（仅当 Mode == "fork" 时才有意义）
func applyMetaDefaults(meta *SkillMeta, dirOrName, body string) {
	if meta.Name == "" {
		base := filepath.Base(dirOrName)
		meta.Name = strings.ToLower(strings.ReplaceAll(base, " ", "-"))
	}
	if meta.Description == "" && body != "" {
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "---") {
				meta.Description = line
				break
			}
		}
	}
	if meta.Mode == "" {
		if meta.Context == "fork" {
			meta.Mode = "fork"
		} else {
			meta.Mode = "inline"
		}
	}
	if meta.IsFork() && meta.ForkContext == "" {
		meta.ForkContext = "none"
	}
}
