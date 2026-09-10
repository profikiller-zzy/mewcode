package skills

// LoadBuiltins 返回编译进二进制的内置 skill。
// 目前是空的 —— 所有 skill 都在运行时从磁盘加载
// （用户级 ~/.mewcode/skills/ 或项目级 .mewcode/skills/）。
func LoadBuiltins() []*Skill {
	return nil
}
