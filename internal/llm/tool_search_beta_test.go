package llm

import "testing"

// beta header 的开关条件：只有工具真带了 defer_loading 才发。
//
// 官方端点这条路没法拿 MiniMax 之类的第三方端点真机验证，这里只能盯住请求
// 该长什么样：header 漏了，defer_loading 会被服务端直接拒；header 多发了，
// 不认识它的端点也会拒。两头都是硬失败。
func TestNeedsToolSearchBeta(t *testing.T) {
	cases := []struct {
		desc  string
		tools []map[string]any
		want  bool
	}{
		{"没有工具", nil, false},
		{"工具都不延迟", []map[string]any{{"name": "Bash"}, {"name": "ToolSearch"}}, false},
		{
			"有一个带 defer_loading",
			[]map[string]any{{"name": "Bash"}, {"name": "mcp__linear__x", "defer_loading": true}},
			true,
		},
		{
			"defer_loading 是 false 不算",
			[]map[string]any{{"name": "mcp__linear__x", "defer_loading": false}},
			false,
		},
	}
	for _, c := range cases {
		if got := needsToolSearchBeta(c.tools); got != c.want {
			t.Errorf("%s: 得到 %v，期望 %v", c.desc, got, c.want)
		}
	}
}
