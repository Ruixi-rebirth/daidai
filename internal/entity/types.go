package entity

import (
	"context"
	"strings"
	"time"

	"daidai/internal/core"
)

// UserRole 用户身份与权限级别
type UserRole = core.UserRole

const (
	RolePartner      = core.RolePartner      // 核心搭档/死党 (最高信任级，拥有全部权限与 Delegates 调度权)
	RoleCollaborator = core.RoleCollaborator // 协作者/好友 (允许日常工具与工程审查)
	RoleGuest        = core.RoleGuest        // 普通群友/访客 (受限权限；工具执行入口另行强制校验)
)

// SensoryEvent 经由 Channel 归一化与防抖合并后的物理感知事件 (零前缀规范)
type SensoryEvent = core.SensoryEvent

// IsSensitiveAuthCommand reports whether a command carries an OAuth authorization code.
func IsSensitiveAuthCommand(content string) bool {
	command := strings.ToLower(strings.TrimSpace(content))
	for strings.HasPrefix(command, "<@!") {
		end := strings.IndexByte(command, '>')
		if end < 0 {
			break
		}
		command = strings.TrimSpace(command[end+1:])
	}
	parts := strings.Fields(command)
	return len(parts) >= 3 && parts[0] == "/agy" && (parts[1] == "code" || parts[1] == "auth")
}

// TicketStatus 工单生命周期状态 (借鉴 DeepSeek DSH Super Agent)
type TicketStatus string

const (
	TicketPending   TicketStatus = "pending"   // 待派发
	TicketRunning   TicketStatus = "running"   // Worker 正在执行
	TicketStopping  TicketStatus = "stopping"  // 已请求取消，等待 Worker 退出
	TicketCompleted TicketStatus = "completed" // 成功完成并产出结果
	TicketFailed    TicketStatus = "failed"    // 执行失败
	TicketAborted   TicketStatus = "aborted"   // 人工打断或看门狗熔断
)

// Ticket 工单契约 (Cortex 与 Delegates 之间的唯一任务调度凭据)
type Ticket struct {
	ID             string             `json:"id"`
	Goal           string             `json:"goal"`
	RequesterID    string             `json:"requester_id"`
	ChannelID      string             `json:"channel_id"`
	Worker         string             `json:"worker,omitempty"` // 指定执行的目标工作者名称 (留空则由路由器派发至主力 Worker)
	Workspace      string             `json:"workspace"`        // Worker 执行时使用的工作目录；隔离级别由 Worker 配置决定
	Model          string             `json:"model"`            // 指定模型标识 (留空沿用当前会话模型)
	Status         TicketStatus       `json:"status"`
	CreatedAt      time.Time          `json:"created_at"`
	UpdatedAt      time.Time          `json:"updated_at"`
	Output         string             `json:"output,omitempty"`
	AllowSensitive bool               `json:"-"` // 仅核心伙伴在私聊下允许展示真实公网 IP 等敏感宿主机信息
	CancelFunc     context.CancelFunc `json:"-"`
	executionToken uint64
}

// Utterance 全息生命表达体 (由认知大脑一次性合成，零前缀规范)
type Utterance struct {
	Platform   string // 目标物理平台 (如 "qq", "telegram", "cli")
	IsGroup    bool   // 是否为群聊会话
	ChannelID  string // 纯净会话标识 (纯群号或纯私聊用户 ID)
	QuoteMsgID string
	Text       string // 纯净人话
	CardData   []byte // 美学卡片长图 (Maple Mono 排版图卷)
}

// HasMedia 是否携带富媒体图片
func (u *Utterance) HasMedia() bool {
	return len(u.CardData) > 0
}

type sensoryEventKey struct{}
type systemToolAccessKey struct{}

// WithSensoryEvent 将当前感知事件注入上下文，供下游工具执行鉴权与会话感知
func WithSensoryEvent(ctx context.Context, event *SensoryEvent) context.Context {
	return context.WithValue(ctx, sensoryEventKey{}, event)
}

// SensoryEventFromContext 从上下文中提取当前感知事件
func SensoryEventFromContext(ctx context.Context) *SensoryEvent {
	if ctx == nil {
		return nil
	}
	ev, _ := ctx.Value(sensoryEventKey{}).(*SensoryEvent)
	return ev
}

// WithSystemToolAccess marks a host initiated tool call that has no user
// identity, such as the internal model-list/status queries. Callers must not
// use it for tool calls originating from a user message.
func WithSystemToolAccess(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, systemToolAccessKey{}, true)
}

// HasSystemToolAccess reports whether the host explicitly authorized a
// user-independent tool call.
func HasSystemToolAccess(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	allowed, _ := ctx.Value(systemToolAccessKey{}).(bool)
	return allowed
}
