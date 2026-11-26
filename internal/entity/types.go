package entity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
)

var ErrMessageTooLong = errors.New("message exceeds the platform's single-response limit")

// ErrReplyDeadlineWithDraft reports a timed-out reply with a saved checkpoint.
var ErrReplyDeadlineWithDraft = fmt.Errorf("reply deadline with saved draft: %w", context.DeadlineExceeded)

type ReplyDeadlineWithDraft struct {
	DraftID  string
	Progress string
}

func (e *ReplyDeadlineWithDraft) Error() string { return ErrReplyDeadlineWithDraft.Error() }
func (e *ReplyDeadlineWithDraft) Unwrap() error { return ErrReplyDeadlineWithDraft }

// UserRole 用户身份与权限级别
type UserRole = core.UserRole

const (
	RolePartner      = core.RolePartner      // 核心搭档/机主 (最高信任级，拥有全部运维、账号、模型与工程权)
	RoleCollaborator = core.RoleCollaborator // 协作者 (私聊许可、动态表情与群画图免限额；无系统运维或工程权)
	RoleFriend       = core.RoleFriend       // 已授权私聊好友 (允许日常私聊对话与公开工具)
	RoleGuest        = core.RoleGuest        // 普通群友/访客 (受限权限；工具执行入口统一强制校验)
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

// SanitizeQuotedMessageContent redacts OAuth code commands and bounds quoted
// text before it reaches a model or a persistent memory task.
func SanitizeQuotedMessageContent(content string) string {
	content = strings.TrimSpace(content)
	if IsSensitiveAuthCommand(content) {
		content = "[OAuth authorization code redacted]"
	}
	return core.TruncateRunes(content, core.MaxQuotedMessageRunes, "\n[引用消息过长，后续内容已截断]")
}

// TicketStatus 工单生命周期状态 (借鉴 DeepSeek DSH Super Agent)
type TicketStatus string

const (
	TicketPending     TicketStatus = "pending"     // 待派发
	TicketRunning     TicketStatus = "running"     // Worker 正在执行
	TicketStopping    TicketStatus = "stopping"    // 已请求取消，等待 Worker 退出
	TicketCompleted   TicketStatus = "completed"   // 成功完成并产出结果
	TicketFailed      TicketStatus = "failed"      // 执行失败
	TicketAborted     TicketStatus = "aborted"     // 人工打断或看门狗熔断
	TicketInterrupted TicketStatus = "interrupted" // 进程重启时未完成，未自动重跑
)

// Ticket 工单契约 (Cortex 与 Delegates 之间的唯一任务调度凭据)
type Ticket struct {
	ID                 string             `json:"id"`
	Goal               string             `json:"goal"`
	RequesterID        string             `json:"requester_id"`
	Platform           string             `json:"-"`
	ChannelID          string             `json:"channel_id"`
	IsGroup            bool               `json:"-"`                // runtime-only destination type for completion notifications
	Worker             string             `json:"worker,omitempty"` // 指定执行的目标工作者名称 (留空则由路由器派发至主力 Worker)
	Workspace          string             `json:"workspace"`        // Worker 执行时使用的工作目录；隔离级别由 Worker 配置决定
	Model              string             `json:"model"`            // 指定模型标识 (留空沿用当前会话模型)
	Status             TicketStatus       `json:"status"`
	CreatedAt          time.Time                            `json:"created_at"`
	UpdatedAt          time.Time                            `json:"updated_at"`
	Output             string                               `json:"output,omitempty"`
	Summary            string                               `json:"-"` // 终局精炼结论摘要（剥离流式中间进度后供主动通知预览）
	AllowSensitive     bool                                 `json:"-"` // 仅核心伙伴在私聊下允许展示真实公网 IP 等敏感宿主机信息
	ExecutionTimeout   time.Duration                        `json:"-"` // 当前工单的调度器硬超时；0 表示无额外时限
	AdditionalDirs     []string                             `json:"-"` // 本工单显式授权给 AGY 沙箱访问的附件输入目录
	OnLiveUpdate       func(model, liveOut, summary string) `json:"-"`
	CancelFunc         context.CancelFunc                   `json:"-"`
	executionToken     uint64
	persistenceWarning string
}

// Utterance 全息生命表达体 (由认知大脑一次性合成，零前缀规范)
type Utterance struct {
	Platform                string // 目标物理平台 (如 "qq", "telegram", "cli")
	IsGroup                 bool   // 是否为群聊会话
	ChannelID               string // 纯净会话标识 (纯群号或纯私聊用户 ID)
	QuoteMsgID              string
	DraftID                 string // 将未送达的续写正文关联到指定草稿
	ContinuationQueuedAt    time.Time
	HasMoreText             bool               // 本次发送之后仍有正文等待续发
	TargetUserID            string             // 目标用户 ID (群聊回复或插话的交互对象)
	Proactive               bool               // 主动发送；平台可能限制适用场景或频率
	GroupInterjection       bool               // 未 @ 群消息的有限插话；必须关联原群消息作为被动回复
	MentionUserIDs          []string           // QQ 群聊消息要点名的成员 OpenID；主动插话由模型决定是否填写
	Text                    string             // 纯净人话
	QuoteText               string             // 内嵌媒体的可引用语义正文，不含临时平台素材 URL
	MediaHistory            string             // 媒体确认送达后写入聊天历史，不随消息发送
	HistoryText             string             // 写入后续上下文的安全替代文本，不影响实际交付内容
	FallbackText            string             // 媒体发送失败时替代图片的文字内容
	CardData                []byte             // 美学卡片长图 (Maple Mono 排版图卷)
	Images                  [][]byte           // 一条表达携带的多张图片；CardData 保留用于兼容单图调用
	InlineAssets            []core.InlineAsset // 正文中按 Markdown 标记定位的程序生成图片
	ImageSources            []string           // 与 Images 对齐；文字渲染图保留正文，真实图片留空
	InlineImages            bool               // 将多张图片作为 Markdown 图片引用放进同一条消息
	InlineNonTextImageCount int                // InlineImages 中非正文分页渲染图的数量
	ReplyCardPages          int                // Images 末尾的长回复分页图数量
	SuppressAutoCard        bool               // 用户要求纯文字时，跳过自动长图排版
	OnDelivered             func(context.Context)
	FixedPresentation       bool // 已由结构化视图完成排版，不再次把索引或文字回退转成图
	OnDeliveryFailed        func(context.Context, error)
}

// DeliveryRejection is only used for an explicit platform rejection. Transport
// errors and missing receipts remain ambiguous and must not be blindly retried.
type DeliveryRejection struct {
	Err   error
	After time.Duration
}

// UnsentError means media preparation failed before a user-facing message was
// attempted, so a text fallback cannot duplicate a delivered message.
type UnsentError struct{ Err error }

func (e *UnsentError) Error() string { return e.Err.Error() }
func (e *UnsentError) Unwrap() error { return e.Err }

func (e *DeliveryRejection) Error() string { return e.Err.Error() }
func (e *DeliveryRejection) Unwrap() error { return e.Err }

// PartialDeliveryError marks a channel failure after part of an utterance was
// already delivered, so callers must not resend the full text fallback.
type PartialDeliveryError struct {
	Err             error
	DeliveredText   string
	UndeliveredText string
}

func (e *PartialDeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "message was partially delivered"
	}
	return fmt.Sprintf("message was partially delivered: %v", e.Err)
}

func (e *PartialDeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// HasMedia 是否携带富媒体图片
func (u *Utterance) HasMedia() bool {
	if len(u.InlineAssets) > 0 {
		return true
	}
	if len(u.CardData) > 0 {
		return true
	}
	for _, image := range u.Images {
		if len(image) > 0 {
			return true
		}
	}
	return false
}

type sensoryEventKey struct{}
type systemToolAccessKey struct{}
type channelStatusKey struct{}

// WithChannelStatus adds the current physical messaging-channel status to an
// interaction context so system-status tools can report both host and gateway
// health without depending on a concrete channel implementation.
func WithChannelStatus(ctx context.Context, status string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, channelStatusKey{}, status)
}

func ChannelStatusFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	status, _ := ctx.Value(channelStatusKey{}).(string)
	return status
}

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

// WithSystemToolAccess grants host code access only to the named tools. It is
// for internal calls that have no user initiated tool request; callers must
// never use it to bypass permissions for a model generated tool call.
func WithSystemToolAccess(ctx context.Context, toolNames ...string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	allowed := make(map[string]struct{}, len(toolNames))
	for _, name := range toolNames {
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	return context.WithValue(ctx, systemToolAccessKey{}, allowed)
}

// HasSystemToolAccess reports whether host code explicitly authorized this
// specific tool for a user-independent call.
func HasSystemToolAccess(ctx context.Context, toolName string) bool {
	if ctx == nil {
		return false
	}
	allowed, _ := ctx.Value(systemToolAccessKey{}).(map[string]struct{})
	_, ok := allowed[toolName]
	return ok
}
