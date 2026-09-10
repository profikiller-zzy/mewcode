package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Catalog 是所有已加载 skill 的内存注册表。Phase-1 条目
// 只包含 frontmatter（PromptBody 为空，BodyLoaded 为 false）；
// GetFull 会在每次调用时触发 phase-2 的正文读取（热重载）。
type Catalog struct {
	skills      map[string]*Skill
	sources     map[string]string // skill 名称 → "builtin" | "user" | "project" | 绝对路径
	workDir     string            // 记住它，好让 Reload 能重新扫描同样的三层
	hasReload   bool
	dirModTimes map[string]time.Time // skill 目录路径 → 上次记录的 modtime
}

func NewCatalog() *Catalog {
	return &Catalog{
		skills:      make(map[string]*Skill),
		sources:     make(map[string]string),
		dirModTimes: make(map[string]time.Time),
	}
}

// Register 往 catalog 中添加（或覆盖）一个 skill。来源标签
// 会由 /skills 展示出来。
func (c *Catalog) Register(s *Skill, source string) {
	c.skills[s.Meta.Name] = s
	c.sources[s.Meta.Name] = source
}

// Get 返回 phase-1 的 skill（只有 frontmatter）。如果 catalog
// 是以 phase-1 模式加载的，PromptBody 可能为空。
func (c *Catalog) Get(name string) *Skill {
	return c.skills[name]
}

// GetFull 返回正文已加载的 skill。对磁盘上的 skill，正文会在
// 每次调用时重新读取（热重载）。对内嵌的 builtins，正文
// 已经在内存里，这就是一次缓存命中。读取失败时
// 保留之前缓存的正文，并返回错误。
func (c *Catalog) GetFull(name string) (*Skill, error) {
	skill, ok := c.skills[name]
	if !ok {
		return nil, fmt.Errorf("unknown skill: %s", name)
	}
	if skill.SourceDir == "" {
		// 内嵌 skill —— 正文在启动时就已加载，无需刷新。
		return skill, nil
	}
	if err := loadSkillBody(skill); err != nil {
		// 有之前缓存的正文就先留着；由调用方决定是暴露这个错误
		// 还是继续往下走。
		if skill.PromptBody == "" {
			return nil, err
		}
		return skill, err
	}
	return skill, nil
}

// List 返回每个已加载 skill 的元数据。顺序是 map 的迭代顺序
// （未排序）—— 需要稳定顺序的调用方应按 Name 排序。
func (c *Catalog) List() []SkillMeta {
	result := make([]SkillMeta, 0, len(c.skills))
	for _, s := range c.skills {
		result = append(result, s.Meta)
	}
	return result
}

// Source 返回 skill 的来源标签（"builtin"、"user"、"project"
// 或某个路径）。skill 未加载时返回 ""。
func (c *Catalog) Source(name string) string {
	return c.sources[name]
}

// Reload 重新扫描全部三层（builtin + user + project）并原地
// 重建 catalog。供 `/skills reload` 和测试使用。
func (c *Catalog) Reload(workDir string) {
	fresh := LoadCatalog(workDir)
	c.skills = fresh.skills
	c.sources = fresh.sources
	c.workDir = fresh.workDir
	c.dirModTimes = fresh.dirModTimes
}

// NeedsReload 检查 skill 目录的 modtime 自上次加载 catalog
// 之后是否变过。modtime 变化说明有 skill 被添加或删除（已有
// skill 内部的文件改动已经由 GetFull 每次调用时的
// 重新读取处理了）。
func (c *Catalog) NeedsReload() bool {
	for dir, recorded := range c.dirModTimes {
		info, err := os.Stat(dir)
		if err != nil {
			if recorded.IsZero() {
				continue
			}
			return true // 目录消失了
		}
		if !info.ModTime().Equal(recorded) {
			return true
		}
	}
	// 检查自上次加载以来是否有新目录被创建
	dirs := skillDirPaths(c.workDir)
	for _, dir := range dirs {
		if _, tracked := c.dirModTimes[dir]; !tracked {
			if info, err := os.Stat(dir); err == nil && !info.ModTime().IsZero() {
				return true
			}
		}
	}
	return false
}

// snapshotDirModTimes 记录所有 skill 目录当前的 modtime。
func (c *Catalog) snapshotDirModTimes() {
	c.dirModTimes = make(map[string]time.Time)
	for _, dir := range skillDirPaths(c.workDir) {
		info, err := os.Stat(dir)
		if err != nil {
			c.dirModTimes[dir] = time.Time{}
			continue
		}
		c.dirModTimes[dir] = info.ModTime()
	}
}

// skillDirPaths 返回用户级全局和项目级 skill 目录的路径。
func skillDirPaths(workDir string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".mewcode", "skills"))
	}
	if workDir != "" {
		dirs = append(dirs, filepath.Join(workDir, ".mewcode", "skills"))
	}
	return dirs
}

// LoadCatalog 合并三层构建出一个 phase-1 catalog，后面的来源按
// 名称覆盖前面的（project 胜于 user，user 胜于 builtin）：
//
//  1. internal/skills/builtins/*（通过 go:embed 内嵌，优先级最低）
//  2. ~/.mewcode/skills/         （用户级全局）
//  3. $workDir/.mewcode/skills/  （项目级，优先级最高）
//
// 这个阶段只读 frontmatter；在调用 GetFull 之前 PromptBody 一直为空。
// 单个 skill 解析失败会被静默跳过 ——
// 一个坏文件不能拖垮整个 catalog。
func LoadCatalog(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir

	// 第 1 层：内嵌的 builtins
	for _, s := range LoadBuiltins() {
		c.Register(s, "builtin")
	}

	// 第 2 层：用户级全局
	if home, err := os.UserHomeDir(); err == nil {
		loadTierInto(c, filepath.Join(home, ".mewcode", "skills"), "user")
	}

	// 第 3 层：项目级
	loadTierInto(c, filepath.Join(workDir, ".mewcode", "skills"), "project")

	c.snapshotDirModTimes()
	return c
}

// LoadFromDirectory 把 dir 的每个子目录都当作一个 skill 加载。
// 供测试和只想加载单层的临时调用方使用。正文会被提前读取
// （不分成两个阶段），这样已有的、会访问 skill.PromptBody 的
// 测试代码还能继续工作。
func LoadFromDirectory(dir string) (*Catalog, error) {
	c := NewCatalog()
	loadTierEager(c, dir, dir)
	return c, nil
}

// LoadSkills 是保留下来的旧版两层加载器，用来兼容那些仍然提前
// 预加载正文的代码。新的调用方应该用
// LoadCatalog + GetFull。顺序：用户级全局 → 项目级。
func LoadSkills(workDir string) *Catalog {
	c := NewCatalog()
	c.workDir = workDir
	if home, err := os.UserHomeDir(); err == nil {
		loadTierEager(c, filepath.Join(home, ".mewcode", "skills"), "user")
	}
	loadTierEager(c, filepath.Join(workDir, ".mewcode", "skills"), "project")
	return c
}

// loadTierInto 遍历单层目录，把每个子目录注册为 phase-1 的
// skill。单个条目出错会被吞掉。
func loadTierInto(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skill, err := parseFrontmatterOnly(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		c.Register(skill, source)
	}
}

// loadTierEager 和 loadTierInto 类似，但会连正文一起读。供旧版的
// LoadSkills / LoadFromDirectory 用来保持原有行为。
func loadTierEager(c *Catalog, dir, source string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		skill, err := parseFrontmatterOnly(path)
		if err != nil {
			continue
		}
		_ = loadSkillBody(skill)
		c.Register(skill, source)
	}
}
