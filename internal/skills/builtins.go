package skills

// LoadBuiltins returns embedded skills compiled into the binary.
// Currently empty — all skills are loaded from disk at runtime
// (user-level ~/.mewcode/skills/ or project-level .mewcode/skills/).
func LoadBuiltins() []*Skill {
	return nil
}
