package memory

import (
	"fmt"
	"time"
)

// MemoryAgeDays 返回自 mtime 起经过的天数（向下取整）：今天为 0，昨天为 1，更早为 2+。
// 未来时间戳（例如时钟偏差）会被限制为 0。
func MemoryAgeDays(mtimeMs int64) int {
	d := (time.Now().UnixMilli() - mtimeMs) / 86_400_000
	if d < 0 {
		return 0
	}
	return int(d)
}

// MemoryAge 返回便于人阅读的时间描述。模型不擅长日期计算，
// “47 days ago”比原始 ISO 时间戳更容易触发过期判断。
func MemoryAge(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d == 0 {
		return "today"
	}
	if d == 1 {
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", d)
}

// MemoryFreshnessText 为超过一天的记忆返回纯文本过期提示。
// 今天或昨天的记忆返回空字符串，避免产生噪声。
//
// 当调用方已经自行添加包装（例如 relevant_memories → wrapMessagesInSystemReminder）时使用。
//
// 起因是用户反馈：过期的代码状态记忆（指向早已改动过的代码的
// file:line 引用）被当成事实来断言 ——
// 因为代码引用会让过期结论听起来更像事实。
func MemoryFreshnessText(mtimeMs int64) string {
	d := MemoryAgeDays(mtimeMs)
	if d <= 1 {
		return ""
	}
	return fmt.Sprintf(
		"This memory is %d days old. "+
			"Memories are point-in-time observations, not live state — "+
			"claims about code behavior or file:line citations may be outdated. "+
			"Verify against current code before asserting as fact.",
		d,
	)
}

// MemoryFreshnessNote 返回包在 <system-reminder> 标签中的单条记忆过期提示。
// 一天以内的记忆返回空字符串，供没有自行添加 system-reminder 包装的调用方使用。
func MemoryFreshnessNote(mtimeMs int64) string {
	text := MemoryFreshnessText(mtimeMs)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<system-reminder>%s</system-reminder>\n", text)
}
