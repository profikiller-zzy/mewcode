package llm

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

// 缓存断点的落点。
//
// 该长什么样：落在最后一个非延迟工具上。一个工具同时带 defer_loading 和
// cache_control 会被官方端点直接拒掉整个请求（400），而 MCP 工具在内建工具之后
// 注册，排序后尾部往往正是延迟工具，所以不能简单地标记最后一个。
func TestMarkToolsForCache(t *testing.T) {
	plain := func(name string) anthropic.ToolUnionParam {
		return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: name}}
	}
	deferred := func(name string) anthropic.ToolUnionParam {
		return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:         name,
			DeferLoading: param.NewOpt(true),
		}}
	}
	// CacheControl 是值类型，零值代表没打标记，用 TTL/Type 之外的方式不好判，
	// 直接比对零值最稳
	var unmarked anthropic.CacheControlEphemeralParam
	marked := func(tools []anthropic.ToolUnionParam) []string {
		var out []string
		for _, u := range tools {
			if u.OfTool == nil || u.OfTool.CacheControl == unmarked {
				continue
			}
			out = append(out, u.OfTool.Name)
		}
		return out
	}

	t.Run("尾部是延迟工具时往前找", func(t *testing.T) {
		tools := []anthropic.ToolUnionParam{
			plain("ReadFile"), plain("WriteFile"), plain("ToolSearch"),
			deferred("mcp__linear__create_issue"), deferred("mcp__sentry__resolve"),
		}
		markToolsForCache(tools)
		got := marked(tools)
		if len(got) != 1 || got[0] != "ToolSearch" {
			t.Fatalf("该标记 ToolSearch，实际标记了 %v", got)
		}
	})

	t.Run("全是非延迟工具时标记最后一个", func(t *testing.T) {
		tools := []anthropic.ToolUnionParam{plain("ReadFile"), plain("Bash")}
		markToolsForCache(tools)
		if got := marked(tools); len(got) != 1 || got[0] != "Bash" {
			t.Fatalf("该标记 Bash，实际标记了 %v", got)
		}
	})

	t.Run("延迟工具夹在中间也不会被选中", func(t *testing.T) {
		tools := []anthropic.ToolUnionParam{
			plain("Bash"), deferred("mcp__a__x"), plain("Grep"), deferred("mcp__z__y"),
		}
		markToolsForCache(tools)
		if got := marked(tools); len(got) != 1 || got[0] != "Grep" {
			t.Fatalf("该标记 Grep，实际标记了 %v", got)
		}
	})

	t.Run("全是延迟工具时一个都不标记", func(t *testing.T) {
		// 官方要求至少有一个非延迟工具，真实注册表里内建工具永远非延迟，
		// 所以这是防御分支：宁可不缓存，也不能发出会被 400 的请求
		tools := []anthropic.ToolUnionParam{deferred("mcp__a__x"), deferred("mcp__b__y")}
		markToolsForCache(tools)
		if got := marked(tools); len(got) != 0 {
			t.Fatalf("不该标记任何工具，实际标记了 %v", got)
		}
	})
}
