package teams

import (
	"context"
	"strings"

	"mewcode/internal/agent"
	"mewcode/internal/llm"
	"mewcode/internal/tools"
)

// StartInProcessMember 把一个 teammate 注册进团队，并在后台 goroutine 里启动
// 它长期运行的主循环。返回的 channel 会转发所有轮次中产生的每个 AgentEvent；
// 循环退出时（ctx 被取消，或收到收件箱里的关闭请求）channel 随之关闭。
//
// goroutine 的生命周期绑定在 ctx 上：调用方取消 ctx 即可停掉这个 teammate。
// 循环每转一圈就调用一次 RunInProcessTeammate，由它处理等待、
// agent 执行和空闲通知。
func StartInProcessMember(
	ctx context.Context,
	team *Team,
	memberName string,
	client llm.Client,
	registry *tools.Registry,
	protocol string,
	task string,
	addendum string,
) <-chan agent.AgentEvent {
	member := team.AddMember(memberName, client, registry, protocol)
	member.Progress = NewTeammateProgress(memberName, team.Name, randomVerb())

	memberCtx, cancel := context.WithCancel(ctx)
	member.Active = true
	member.Cancel = cancel

	eventCh := make(chan agent.AgentEvent, 32)
	go func() {
		defer close(eventCh)
		defer func() {
			// 队友退出时持久化对话记录，用于调试
			if member.Conv != nil {
				_, _ = SaveTranscript(team.Name, memberName, member.Conv)
			}
			team.mu.Lock()
			member.Active = false
			team.mu.Unlock()
		}()
		_ = RunInProcessTeammate(memberCtx, team, member, task, addendum, eventCh)
	}()
	return eventCh
}

// BuildTeammateAddendum 生成注入到每个 teammate 对话顶部的 system-reminder 文本。
// 它告诉模型自己的身份、团队里还有谁，以及怎么发消息。
func BuildTeammateAddendum(teamName, memberName string, otherMembers []string) string {
	var sb strings.Builder
	sb.WriteString("You are a member of team \"" + teamName + "\". Your name is \"" + memberName + "\".\n\n")
	sb.WriteString("The lead is reachable as \"" + LeadName + "\". Deliver your final result to the lead with SendMessage(to=\"" + LeadName + "\", content=...) — the idle notification alone only signals completion, it does not carry your output.\n")
	if len(otherMembers) > 0 {
		sb.WriteString("Other team members: " + strings.Join(otherMembers, ", ") + "\n")
	}
	sb.WriteString("\nYou can communicate with the lead and teammates using the SendMessage tool.\n")
	sb.WriteString("Messages from the team arrive as system reminders at the start of each turn.\n")
	sb.WriteString("When you finish your current task, send your final result to \"" + LeadName + "\" via SendMessage, then stop calling tools — an idle notification will be sent to the lead automatically.\n")
	return sb.String()
}

// InjectPendingMessages 把未读的邮箱消息格式化成 system-reminder 字符串返回，
// 并把它们标记为已读。RunInProcessTeammate 会在每个 teammate 轮次开始时调用它；
// 返回空字符串表示没有新消息。
func InjectPendingMessages(team *Team, memberName string) string {
	msgs, err := team.MailBox.ReadUnread(memberName)
	if err != nil || len(msgs) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("You have new messages:\n\n")
	for _, msg := range msgs {
		sb.WriteString("From " + msg.From + ": " + msg.Text + "\n\n")
	}

	_ = team.MailBox.MarkAllRead(memberName)
	return sb.String()
}

