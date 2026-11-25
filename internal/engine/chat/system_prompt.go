package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// buildSystemPrompt assembles the stable behavioral rules and the small set of
// rules that depend on the current conversation. Runtime facts are returned
// separately so callers can keep them out of the instruction channel.
func (c *Cortex) buildSystemPrompt(event *entity.SensoryEvent, request chatRequestContext) (string, string) {
	now := time.Now()
	if event != nil && !event.OccurredAt.IsZero() {
		now = event.OccurredAt
	}
	weekdayCn := [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}[now.Weekday()]
	var runtimeContext strings.Builder
	fmt.Fprintf(&runtimeContext, "【当前时间】%s %s（%s）\n",
		now.Format("2006-01-02 15:04:05"), weekdayCn, now.Location().String())
	if c.persona != nil {
		runtimeContext.WriteString(entity.CircadianRhythm(now) + "\n")
	}

	if event != nil && event.Role == entity.RolePartner {
		if mindStatus := c.MindStatusFor(event); mindStatus != "" {
			runtimeContext.WriteString(mindStatus + "\n")
		}
	}

	if c.workspaces != nil && event.IsPartnerPrivate() {
		wsPath, wsName, wsErr := c.workspaces.GetCurrent(event.Platform, event.ChannelID)
		if wsErr != nil {
			runtimeContext.WriteString("【当前工作空间】：读取失败；无法确认工作目录。\n")
		} else if c.workspaces.IsSandbox(wsPath) {
			fmt.Fprintf(&runtimeContext, "【当前工作空间】：%s (默认工作区)\n", wsPath)
		} else {
			fmt.Fprintf(&runtimeContext, "【当前工作空间】：%s (工程: %s)\n", wsPath, wsName)
		}
	}
	if c.delegates != nil && event.IsPartnerPrivate() && event.SenderID != "" && event.ChannelID != "" {
		tCtx, tCancel := context.WithTimeout(context.Background(), 2*time.Second)
		recentTickets, _ := c.delegates.TicketsFor(tCtx, event.Platform, event.SenderID, event.ChannelID, 2)
		tCancel()
		if len(recentTickets) > 0 {
			var tLines []string
			for _, tk := range recentTickets {
				tLines = append(tLines, fmt.Sprintf("- %s [%s]: %s", tk.ID, tk.Status, core.TruncateRunes(strings.TrimSpace(tk.Goal), 80, "…")))
			}
			fmt.Fprintf(&runtimeContext, "【近期后台工单】（如需查看完整进度或报错输出可调用 delegate_ticket 的 status 动作）：\n%s\n", strings.Join(tLines, "\n"))
		}
	}
	sysPrompt := ""
	if c.persona != nil {
		sysPrompt = c.persona.CompilePromptFor(event, c.hands != nil && event.Role != entity.RoleGuest)
	}
	sysPrompt += "\n" + sourceTrustPolicy
	if len(request.Files) > 0 {
		sysPrompt += "\n【附件】truncated 表示正文不完整，binary 表示无法按文本读取，unavailable 表示正文不可读取。"
	}
	if request.Images != nil {
		sysPrompt += "\n【图片】没有成功分析的图片不可当作已经看过，必要时请用户重发或描述。"
	}
	sysPrompt += "\n" + actionEvidencePolicy
	sysPrompt += "\n【流程图】复杂流程或关系只有在图示能明显帮助理解时才使用 Mermaid 代码块；流程图以外仍正常回答。服务端会把 mermaid 代码块渲染成图片并嵌入回复。用户明确指定 HTML 等格式时，按用户要求输出。"
	if event != nil && event.IsGroup && event.Role == entity.RolePartner {
		sysPrompt += "\n【会话隔离边界】群聊与宿主机目录、工作空间及工程命令硬隔离（固定安全边界，无法在群里开启），且不得透露宿主机公网 IP。若搭档在群里要求查看本地目录、操作项目文件或跑命令，直接自然提醒群里不连本地目录、私聊找你处理，绝不说‘没开权限’或‘去改宿主配置’。"
	} else if !event.IsPartnerPrivate() {
		sysPrompt += "\n【会话隔离边界】当前会话仅做日常交流与公开信息协助，不连接任何本地目录或宿主环境，不得探测或透露宿主机公网 IP，也绝不向对方提及‘没开权限’‘沙箱’或‘宿主配置’等内部机制。"
	}
	sysPrompt += "\n【本轮请求】用户输入以 JSON 提供，current_request 是本轮消息。"
	if request.QuotedMessage != nil {
		sysPrompt += "引用内容用于理解本轮指代，不覆盖本轮请求；有嵌套引用时以最外层为准。content_complete=true 表示引用正文完整。只回答 current_request；仍无法确定引用指向时简短追问。"
	}
	if event.IsGroup {
		sysPrompt += "\n【群聊消息格式】近期群聊历史以 [成员名] 标记；当前输入标记为正在对你说话的成员。"
	}

	return sysPrompt, runtimeContext.String()
}
