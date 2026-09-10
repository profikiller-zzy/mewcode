package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// defaultHookTimeout 是当 hook 配置没有自带 `timeout` 时套用的上限。
const defaultHookTimeout = 10 * time.Minute

type EventName string

const (
	EventSessionStart EventName = "session_start"
	EventSessionEnd   EventName = "session_end"
	EventTurnStart    EventName = "turn_start"
	EventTurnEnd      EventName = "turn_end"
	EventPreSend      EventName = "pre_send"
	EventPostReceive  EventName = "post_receive"
	EventPreToolUse   EventName = "pre_tool_use"
	EventPostToolUse  EventName = "post_tool_use"
	EventShutdown     EventName = "shutdown"
)

type ActionType string

const (
	ActionCommand ActionType = "command"
	ActionPrompt  ActionType = "prompt"
	ActionHTTP    ActionType = "http"
	ActionAgent   ActionType = "agent"
)

type Action struct {
	Type    ActionType        `yaml:"type"`
	Command string            `yaml:"command"`
	Message string            `yaml:"message"`
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	Timeout time.Duration     `yaml:"timeout"`
}

type Hook struct {
	ID        string    `yaml:"id"`
	Event     EventName `yaml:"event"`
	Condition string    `yaml:"if"`
	Action    Action    `yaml:"action"`
	Reject    bool      `yaml:"reject"`
	Once      bool      `yaml:"once"`
	Async     bool      `yaml:"async"`
	// OnError 控制 action 执行失败时的行为。
	//   "fail"   — 把错误继续抛出去（阻塞式 hook 的默认值）
	//   "ignore" — 记录日志后继续
	//   "reject" — 把 hook 失败当成一次拒绝（仅 pre_tool_use）
	OnError string `yaml:"on_error"`
}

type HookContext struct {
	EventName EventName
	ToolName  string
	ToolArgs  map[string]any
	FilePath  string
	Message   string
	Error     string
}

type HookResult struct {
	HookID  string
	Output  string
	Success bool
	Reject  bool
}

type Engine struct {
	mu            sync.Mutex
	hooks         []Hook
	notifications []HookResult
	fired         map[string]bool // 已经触发过的 hook ID（用于 `once`）
	// AgentRunner 执行 agent 类型的 hook。可选 —— 为 nil 时，agent hook
	// 会返回一个明确的 "no runner registered" 错误，而不是静默失败。
	AgentRunner func(prompt string, ctx HookContext) (string, error)
}

func NewEngine() *Engine {
	return &Engine{fired: make(map[string]bool)}
}

// validEventNames 是 Validate 接受的事件名白名单。
// 它直接取自上面的 EventName 常量，所以新增事件只需要在那里改一行，
// 不用改两处。
var validEventNames = map[EventName]bool{
	EventSessionStart: true,
	EventSessionEnd:   true,
	EventTurnStart:    true,
	EventTurnEnd:      true,
	EventPreSend:      true,
	EventPostReceive:  true,
	EventPreToolUse:   true,
	EventPostToolUse:  true,
	EventShutdown:     true,
}

// Validate 检查一组 hook 的配置错误，这些错误在运行时只会表现为
// 静默的异常行为。每种 action 类型都有各自必填的字段；
// URL hook 需要一个可解析的 http(s) URL；timeout
// 必须非负。
//
// 所有错误都通过 errors.Join 聚合成一个返回，这样一次调用就能把
// 全部问题报出来，而不是遇到第一个就退出。每条错误都会带上
// hook id（id 为空时用下标）和出问题的字段名作为前缀。
func Validate(hooks []Hook) error {
	var errs []error
	for i, h := range hooks {
		label := h.ID
		if label == "" {
			label = fmt.Sprintf("hook[%d]", i)
		} else {
			label = fmt.Sprintf("hook[%d] (id=%q)", i, h.ID)
		}

		if !validEventNames[h.Event] {
			errs = append(errs, fmt.Errorf("%s: unknown event %q", label, h.Event))
		}

		if h.Action.Timeout < 0 {
			errs = append(errs, fmt.Errorf("%s: action.timeout must be >= 0 (got %s)", label, h.Action.Timeout))
		}

		switch h.Action.Type {
		case ActionCommand:
			if strings.TrimSpace(h.Action.Command) == "" {
				errs = append(errs, fmt.Errorf("%s: action.command must be non-empty for type %q", label, h.Action.Type))
			}
		case ActionPrompt:
			if strings.TrimSpace(h.Action.Message) == "" {
				errs = append(errs, fmt.Errorf("%s: action.message must be non-empty for type %q", label, h.Action.Type))
			}
		case ActionHTTP:
			if strings.TrimSpace(h.Action.URL) == "" {
				errs = append(errs, fmt.Errorf("%s: action.url must be non-empty for type %q", label, h.Action.Type))
			} else {
				u, err := url.Parse(h.Action.URL)
				if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
					errs = append(errs, fmt.Errorf("%s: action.url must be a valid http(s) URL (got %q)", label, h.Action.URL))
				}
			}
		case ActionAgent:
			if strings.TrimSpace(h.Action.Message) == "" && strings.TrimSpace(h.Action.Command) == "" {
				errs = append(errs, fmt.Errorf("%s: action.message (or action.command as fallback) must be non-empty for type %q", label, h.Action.Type))
			}
		case "":
			errs = append(errs, fmt.Errorf("%s: action.type is required", label))
		default:
			errs = append(errs, fmt.Errorf("%s: unknown action.type %q (want command / prompt / http / agent)", label, h.Action.Type))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) LoadHooks(hooks []Hook) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hooks = hooks
	e.fired = make(map[string]bool)
}

func (e *Engine) RunHooks(ctx HookContext) []HookResult {
	var results []HookResult
	for _, h := range e.snapshotHooks() {
		if h.Event != ctx.EventName {
			continue
		}
		if !e.shouldFire(h, ctx) {
			continue
		}
		if h.Async {
			go func(h Hook) {
				res := e.executeAction(h, ctx)
				e.recordNotification(res)
			}(h)
			results = append(results, HookResult{HookID: h.ID, Output: "(async)", Success: true})
			continue
		}
		result := e.executeAction(h, ctx)
		results = append(results, result)
		e.recordNotification(result)
	}
	return results
}

// RunPreToolHooks 执行 pre-tool-use hook。返回 (rejected, message)。
// 不拒绝的 hook 也会执行，因为它们可能带来副作用（通知/HTTP 等）。
func (e *Engine) RunPreToolHooks(ctx HookContext) (bool, string) {
	for _, h := range e.snapshotHooks() {
		if h.Event != EventPreToolUse {
			continue
		}
		if !e.shouldFire(h, ctx) {
			continue
		}
		result := e.executeAction(h, ctx)
		e.recordNotification(result)
		// hook 可能是配置成拒绝，也可能是以 on_error=reject 的方式失败。
		if h.Reject || (!result.Success && h.OnError == "reject") {
			msg := result.Output
			if msg == "" {
				msg = "blocked by hook " + h.ID
			}
			return true, msg
		}
	}
	return false, ""
}

func (e *Engine) shouldFire(h Hook, ctx HookContext) bool {
	if h.Condition != "" && !evaluateCondition(h.Condition, ctx) {
		return false
	}
	if h.Once {
		e.mu.Lock()
		defer e.mu.Unlock()
		if h.ID != "" && e.fired[h.ID] {
			return false
		}
		if h.ID != "" {
			e.fired[h.ID] = true
		}
	}
	return true
}

func (e *Engine) snapshotHooks() []Hook {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Hook, len(e.hooks))
	copy(out, e.hooks)
	return out
}

func (e *Engine) recordNotification(r HookResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.notifications = append(e.notifications, r)
}

func (e *Engine) DrainNotifications() []HookResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := e.notifications
	e.notifications = nil
	return n
}

// evaluateCondition 支持：
//   - 叶子条件：`var == "value"`、`var =~ /regex/`、`var =* "glob"`、`var != "value"`
//   - 组合条件：`cond1 && cond2`、`cond1 || cond2`
//   - 取反：`!cond`（必须写在叶子条件之前，不能跨操作符拆分）
//
// 不支持括号；组合严格从左到右，`&&` 和 `||`
// 优先级相同（与简单 shell 风格一致）。
func evaluateCondition(condition string, ctx HookContext) bool {
	condition = strings.TrimSpace(condition)
	if condition == "" {
		return true
	}
	// 组合条件：遍历字符串，按顶层的 && 和 || 拆分。
	// 我们不支持包含这两个操作符的引号，因为 hook 条件是用户自己配的 ——
	// 解析逻辑就保持简单直白。
	if tokens := splitComposite(condition); len(tokens) > 1 {
		result := evaluateCondition(tokens[0].expr, ctx)
		for i := 1; i < len(tokens); i++ {
			rhs := evaluateCondition(tokens[i].expr, ctx)
			if tokens[i].op == "&&" {
				result = result && rhs
			} else {
				result = result || rhs
			}
		}
		return result
	}

	if strings.HasPrefix(condition, "!") {
		return !evaluateCondition(strings.TrimSpace(condition[1:]), ctx)
	}

	return evaluateLeaf(condition, ctx)
}

type compToken struct {
	op   string // 第一个 token 为 ""，之后是 "&&" 或 "||"
	expr string
}

func splitComposite(s string) []compToken {
	var out []compToken
	start := 0
	cur := ""
	for i := 0; i < len(s)-1; i++ {
		pair := s[i : i+2]
		if pair == "&&" || pair == "||" {
			out = append(out, compToken{op: cur, expr: strings.TrimSpace(s[start:i])})
			cur = pair
			start = i + 2
			i++
		}
	}
	out = append(out, compToken{op: cur, expr: strings.TrimSpace(s[start:])})
	if len(out) == 1 {
		// 没有找到可拆分的位置。
		return nil
	}
	return out
}

func evaluateLeaf(condition string, ctx HookContext) bool {
	for _, op := range []string{"!=", "=~", "=*", "=="} {
		if idx := strings.Index(condition, op); idx >= 0 {
			left := strings.TrimSpace(condition[:idx])
			right := strings.Trim(strings.TrimSpace(condition[idx+len(op):]), `"'`)
			val := resolveVar(left, ctx)
			switch op {
			case "==":
				return val == right
			case "!=":
				return val != right
			case "=~":
				pattern := strings.Trim(right, "/")
				matched, _ := regexp.MatchString(pattern, val)
				return matched
			case "=*":
				matched, _ := filepath.Match(right, val)
				return matched
			}
		}
	}
	// 没有操作符 → 当成变量是否为真的判断。
	return resolveVar(condition, ctx) != ""
}

func resolveVar(name string, ctx HookContext) string {
	switch name {
	case "tool":
		return ctx.ToolName
	case "event":
		return string(ctx.EventName)
	case "file_path":
		return ctx.FilePath
	case "message":
		return ctx.Message
	}
	if strings.HasPrefix(name, "args.") {
		key := strings.TrimPrefix(name, "args.")
		if ctx.ToolArgs != nil {
			if v, ok := ctx.ToolArgs[key]; ok {
				return fmt.Sprintf("%v", v)
			}
		}
	}
	return ""
}

func (e *Engine) executeAction(h Hook, ctx HookContext) HookResult {
	switch h.Action.Type {
	case ActionCommand:
		return runCommand(h, ctx)
	case ActionPrompt:
		return HookResult{
			HookID:  h.ID,
			Output:  h.Action.Message,
			Success: true,
			Reject:  h.Reject,
		}
	case ActionHTTP:
		return runHTTP(h, ctx)
	case ActionAgent:
		return e.runAgent(h, ctx)
	default:
		return HookResult{
			HookID:  h.ID,
			Output:  fmt.Sprintf("Unknown action type: %s", h.Action.Type),
			Success: false,
		}
	}
}

// runAgent 把 hook 的 Message 当作一次性 prompt 调用可选的 AgentRunner。
// 没有配置 runner 时返回明确的错误，让用户知道需要在 main 入口里
// 把 agent 类型的 hook 接起来。
func (e *Engine) runAgent(h Hook, ctx HookContext) HookResult {
	if e.AgentRunner == nil {
		return HookResult{
			HookID:  h.ID,
			Output:  "agent-type hook configured but no AgentRunner registered",
			Success: false,
			Reject:  h.Reject,
		}
	}
	prompt := h.Action.Message
	if prompt == "" {
		prompt = h.Action.Command
	}
	output, err := e.AgentRunner(prompt, ctx)
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	return HookResult{HookID: h.ID, Output: output, Success: true, Reject: h.Reject}
}

func runCommand(h Hook, ctx HookContext) HookResult {
	timeout := h.Action.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	execCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "bash", "-c", h.Action.Command)
	cmd.Env = append(cmd.Environ(),
		"MEWCODE_EVENT="+string(ctx.EventName),
		"MEWCODE_TOOL="+ctx.ToolName,
		"MEWCODE_FILE_PATH="+ctx.FilePath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\n" + stderr.String()
	}
	output = strings.TrimSpace(output)

	// 把超时和普通的 exec 失败区分开，这样用户（以及下游的通知）
	// 能知道 hook 为什么退出。CommandContext 在到期时会用
	// SIGKILL 杀掉子进程；而 Run 返回的错误本身是含糊的
	// *exec.ExitError，所以我们改为检查 context。
	if execCtx.Err() == context.DeadlineExceeded {
		msg := fmt.Sprintf("command timed out after %s", timeout)
		if output != "" {
			msg = msg + ": " + output
		}
		return HookResult{
			HookID:  h.ID,
			Output:  msg,
			Success: false,
			Reject:  h.Reject,
		}
	}
	return HookResult{
		HookID:  h.ID,
		Output:  output,
		Success: err == nil,
		Reject:  h.Reject,
	}
}

func runHTTP(h Hook, ctx HookContext) HookResult {
	method := strings.ToUpper(h.Action.Method)
	if method == "" {
		method = "POST"
	}
	timeout := h.Action.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	body := h.Action.Body
	if body == "" {
		payload := map[string]any{
			"event":     string(ctx.EventName),
			"tool":      ctx.ToolName,
			"tool_args": ctx.ToolArgs,
			"file_path": ctx.FilePath,
			"message":   ctx.Message,
			"error":     ctx.Error,
		}
		if b, err := json.Marshal(payload); err == nil {
			body = string(b)
		}
	}

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, method, h.Action.URL, strings.NewReader(body))
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	for k, v := range h.Action.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Content-Type") == "" && body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HookResult{HookID: h.ID, Output: err.Error(), Success: false, Reject: h.Reject}
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	return HookResult{
		HookID:  h.ID,
		Output:  fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes))),
		Success: ok,
		Reject:  h.Reject,
	}
}
