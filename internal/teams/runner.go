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

// LeadName is the conventional sender/recipient identifier used by the coordinator side. Teammates
// send idle notifications here and read the lead's task assignments from messages with From ==
// LeadName.
const LeadName = "lead"

// ShutdownPrefix marks a mailbox message as a request to terminate the teammate. The lead writes
// one of these to wind down a member cleanly; the runner sees it during idle polling and returns
// from the loop.
const ShutdownPrefix = "[shutdown]"

// IdlePollInterval is how often an idle teammate scans its inbox for new work.
const IdlePollInterval = 500 * time.Millisecond

// IsShutdownRequest reports whether a mailbox message asks the teammate to exit by matching the
// shutdown prefix.

// CreateIdleNotification builds the message a teammate sends to the lead after finishing a turn.
// The lead routes work by reading these.
func CreateIdleNotification(memberName, reason string) FileMailMessage {
	return NewFileMailMessage(memberName, fmt.Sprintf("[idle] %s (reason: %s)", memberName, reason))
}

// RunInProcessTeammate drives a teammate's main loop in the current process. It blocks until ctx is
// cancelled or a shutdown request lands in the inbox. Each iteration:
//
// 1. waitForNextPromptOrShutdown — fold any pending mailbox messages into a user prompt (or return
// on shutdown / cancellation). 2. runAgent — call agent.Run on the shared conversation; forward
// events through eventOut. The channel closing signals turn-end. 3. sendIdleNotification — drop an
// idle marker into the lead's inbox so it can dispatch the next task.
//
// This The initial prompt jump-starts the first iteration; subsequent iterations get their prompt
// from the mailbox.
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

		// Fold any messages that landed in the inbox before this turn into the conversation as a system
		// reminder so the model sees them as inbound notifications, not user instructions.
		if reminder := InjectPendingMessages(team, member.Name); reminder != "" {
			member.Conv.AddSystemReminder(reminder)
		}

		if nextPrompt != "" {
			member.Conv.AddUserMessage(nextPrompt)
		}
		nextPrompt = ""

		ch := member.AgentRef.Run(ctx, member.Conv)
		for ev := range ch {
			// Update progress tracking
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

		// Notify the lead that this teammate finished its turn so the lead can decide whether to feed it
		// more work.
		_ = team.MailBox.Send(LeadName, CreateIdleNotification(member.Name, idleReason))
		idleReason = "available"

		// Idle poll. Sleep IdlePollInterval, then drain the inbox. Stop on shutdown messages; otherwise
		// build the next prompt and loop back.
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

// waitForNextPromptOrShutdown blocks until the inbox has at least one message, then turns the
// unread batch into the next user prompt. If any message is a shutdown request, the function
// returns shutdown=true without building a prompt.
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

// DrainLeadMailbox reads every unread notification in every team's lead inbox and returns them as
// system-reminder strings (one per team). The lead's main loop installs this in
// Agent.NotificationFn so teammate idle notifications surface to the model at the top of each turn.
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

// formatInboundAsPrompt turns an unread batch into a single user prompt. Each message is tagged
// with its sender so the teammate can route a reply. Matches formatAsTeammateMessage in ,
// simplified to plain text instead of XML.
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

// _ silences the unused-import warning when conversation is referenced only via Member.Conv
// methods.
var _ = conversation.NewManager
