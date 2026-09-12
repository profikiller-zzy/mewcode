package teams

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"mewcode/internal/agent"
	"mewcode/internal/conversation"
	"mewcode/internal/permissions"
	"mewcode/internal/planfile"
)

// LeadName 是协调方一侧惯用的收发标识。teammate 把空闲通知发到这里，
// 并从 From == LeadName
// 的消息里读取 Lead 分派的任务。
const LeadName = "lead"

// ShutdownPrefix 把一条邮箱消息标记为「终止该 teammate」的请求。Lead 写入
// 这样一条消息，用来干净地收掉一个成员；runner 在空闲轮询时发现它，
// 就从循环里返回。
const ShutdownPrefix = "[shutdown]"

// IdlePollInterval 是空闲 teammate 扫描收件箱、找新活的频率。
const IdlePollInterval = 500 * time.Millisecond

// IsShutdownRequest 通过匹配 shutdown 前缀，判断一条邮箱消息
// 是不是在要求该 teammate 退出。

// CreateIdleNotification 构造 teammate 跑完一轮之后发给 Lead 的消息。
// Lead 靠读这些消息来分派工作。
func CreateIdleNotification(memberName, reason string) FileMailMessage {
	return NewFileMailMessage(memberName, fmt.Sprintf("[idle] %s (reason: %s)", memberName, reason))
}

// RunInProcessTeammate 在当前进程里驱动一个 teammate 的主循环。它一直阻塞，
// 直到 ctx 被取消，或者收件箱里来了 shutdown 请求。每轮迭代：
//
// 1. waitForNextPromptOrShutdown —— 把待处理的邮箱消息折叠成一条 user prompt
// （遇到 shutdown / 取消就直接返回）。2. runAgent —— 在共享对话上调用 agent.Run，
// 并通过 eventOut 转发事件，channel 关闭即表示本轮结束。3. sendIdleNotification
// —— 往 Lead 的收件箱里丢一个空闲标记，好让 Lead 派发下一个任务。
//
// 这条初始 prompt 用来启动第一轮迭代；之后每轮的 prompt 都从收件箱里取。
func RunInProcessTeammate(
	ctx context.Context,
	team *Team,
	member *Member,
	initialPrompt string,
	addendum string,
	eventOut chan<- agent.AgentEvent,
) error {
	if addendum != "" {
		member.Conv.AddSystemReminder(addendum)
	}

	nextPrompt := initialPrompt
	idleReason := "available"

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// 把本轮开始前落进收件箱的消息折叠成 system-reminder 塞进对话，
		// 这样模型会把它们当成收到的通知，而不是用户的指令。
		if reminder := InjectPendingMessages(team, member.Name); reminder != "" {
			member.Conv.AddSystemReminder(reminder)
		}

		if nextPrompt != "" {
			member.Conv.AddUserMessage(nextPrompt)
		}
		nextPrompt = ""

		ch := member.AgentRef.Run(ctx, member.Conv)
		for ev := range ch {
			// 更新进度追踪
			if member.Progress != nil {
				switch e := ev.(type) {
				case agent.ToolUseEvent:
					member.Progress.RecordToolUse(e.ToolName, e.Args)
				case agent.UsageEvent:
					member.Progress.RecordTokens(int64(e.InputTokens), int64(e.OutputTokens))
				}
			}
			if eventOut != nil {
				select {
				case eventOut <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if e, ok := ev.(agent.ErrorEvent); ok && e.Message != "" {
				idleReason = "failed"
			}
		}

		if member.Progress != nil {
			if idleReason == "failed" {
				member.Progress.SetStatus("failed")
			} else {
				member.Progress.SetStatus("idle")
			}
		}

		// 计划模式的队友：一轮跑完意味着它调了 ExitPlanMode，计划已经落到磁盘。
		// 把计划交给 Lead 审批，通过了才解除只读限制开始动手。
		if planModeActive(member) {
			approved, feedback, err := requestPlanApproval(ctx, team, member)
			if err != nil {
				return err
			}
			if approved {
				// 批准后切回正常权限，队友可以改文件了
				member.AgentRef.Checker.Mode = permissions.ModeDefault
				nextPrompt = "Lead 已批准你的计划，现在按计划开始执行。"
			} else {
				// 驳回时留在计划模式，带着修改意见重写计划
				nextPrompt = "Lead 驳回了你的计划，修改意见：" + feedback + "\n请据此修订计划后再次提交。"
			}
			continue
		}

		// 通知 Lead 这个 teammate 跑完了一轮，
		// 好让 Lead 决定要不要再给它派活。
		_ = team.MailBox.Send(LeadName, CreateIdleNotification(member.Name, idleReason))
		idleReason = "available"

		// 空闲轮询。先睡 IdlePollInterval，再清空收件箱。
		// 遇到 shutdown 消息就停；否则构造下一条 prompt 继续循环。
		prompt, shutdown, err := waitForNextPromptOrShutdown(ctx, team, member.Name)
		if err != nil {
			return err
		}
		if shutdown != nil {
			// 收工前先给 Lead 一个明确答复，让它知道可以回收窗格了。
			// 队友这里一律同意：它已经处在空闲轮询里，手上没有干到一半的活。
			// 真正需要拒绝的场景是干活干到一半被打断，那种情况下队友根本轮询不到这条消息。
			if shutdown.Type == MsgShutdownRequest {
				_ = team.MailBox.Send(LeadName,
					NewShutdownResponse(member.Name, shutdown.RequestID, true, "acknowledged, shutting down"))
			}
			return nil
		}
		nextPrompt = prompt
	}
}

// waitForNextPromptOrShutdown 阻塞到收件箱里至少有一条消息，然后把这批未读
// 消息变成下一条 user prompt。如果其中有 shutdown 请求，就直接返回
// shutdown=true，不构造 prompt。
// planModeActive 判断队友是否处在计划模式。只有被 Lead 标了 planModeRequired
// 的队友才会进这个模式，普通队友直接干活。
func planModeActive(member *Member) bool {
	return member.AgentRef != nil &&
		member.AgentRef.Checker != nil &&
		member.AgentRef.Checker.Mode == permissions.ModePlan
}

// requestPlanApproval 把队友写好的计划发给 Lead，然后阻塞等待批复。
//
// 队友这时候手上是只读权限，等多久都不会造成破坏，所以这里不设超时：
// 与其超时后自作主张开始改文件，不如一直等着，由用户从 Lead 那边推进。
func requestPlanApproval(ctx context.Context, team *Team, member *Member) (bool, string, error) {
	plan := readPlanForReview(member)
	req := NewPlanApprovalRequest(member.Name, plan)
	if err := team.MailBox.Send(LeadName, req); err != nil {
		return false, "", err
	}
	if member.Progress != nil {
		member.Progress.SetStatus("awaiting plan approval")
	}

	for {
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(member.Name)
		if err != nil {
			return false, "", err
		}
		for _, m := range msgs {
			// 只认对应这次请求的批复，别的消息留到下一轮再处理
			if m.Type == MsgPlanApprovalResponse && m.RequestID == req.RequestID {
				_ = team.MailBox.MarkAllRead(member.Name)
				return m.Approved(), m.Text, nil
			}
		}
	}
}

// readPlanForReview 读出队友写好的计划全文，交给 Lead 审阅。
func readPlanForReview(member *Member) string {
	workDir := ""
	if member.AgentRef != nil {
		workDir = member.AgentRef.WorkDir
	}
	path := planfile.GetOrCreatePlanPath(workDir)
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return "（计划文件为空，队友可能未按要求写入计划）"
	}
	return string(data)
}

func waitForNextPromptOrShutdown(ctx context.Context, team *Team, memberName string) (string, *FileMailMessage, error) {
	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(IdlePollInterval):
		}

		msgs, err := team.MailBox.ReadUnread(memberName)
		if err != nil {
			return "", nil, err
		}
		if len(msgs) == 0 {
			continue
		}

		var shutdown *FileMailMessage
		var keep []FileMailMessage
		for i, m := range msgs {
			if IsShutdownRequest(m) {
				shutdown = &msgs[i]
				continue
			}
			keep = append(keep, m)
		}
		_ = team.MailBox.MarkAllRead(memberName)

		if shutdown != nil {
			return "", shutdown, nil
		}
		return formatInboundAsPrompt(keep), nil, nil
	}
}

// DrainLeadMailbox 读取每个团队 Lead 收件箱里的全部未读通知，并以
// system-reminder 字符串的形式返回（每个团队一条）。Lead 的主循环把它装到
// Agent.NotificationFn 上，这样 teammate 的空闲通知就能在每轮开头浮现给模型。
func DrainLeadMailbox(mgr *TeamManager) []string {
	if mgr == nil {
		return nil
	}
	var notes []string
	for _, name := range mgr.ListTeams() {
		team := mgr.GetTeam(name)
		if team == nil {
			continue
		}
		msgs, err := team.MailBox.ReadUnread(LeadName)
		if err != nil || len(msgs) == 0 {
			continue
		}
		var sb strings.Builder
		sb.WriteString("<team-notification team=\"")
		sb.WriteString(name)
		sb.WriteString("\">\n")
		for _, m := range msgs {
			sb.WriteString("from=")
			sb.WriteString(m.From)
			sb.WriteString(": ")
			sb.WriteString(m.Text)
			sb.WriteString("\n")
		}
		sb.WriteString("</team-notification>")
		notes = append(notes, sb.String())
		_ = team.MailBox.MarkAllRead(LeadName)
	}
	return notes
}

// formatInboundAsPrompt 把一批未读消息合成一条 user prompt。每条消息都带上
// 发送者，方便 teammate 决定回复给谁。与 formatAsTeammateMessage 对应，
// 这里简化成了纯文本，不用 XML。
func formatInboundAsPrompt(msgs []FileMailMessage) string {
	if len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("You have new messages from your team:\n\n")
	for _, m := range msgs {
		sb.WriteString(fmt.Sprintf("From %s: %s\n\n", m.From, m.Text))
	}
	return sb.String()
}

// 当 conversation 只通过 Member.Conv 的方法被引用到时，
// 用 _ 消掉 unused-import 警告。
var _ = conversation.NewManager
