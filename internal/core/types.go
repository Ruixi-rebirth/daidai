package core

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

var appStartTime = time.Now()

// MaxImageBytes is the shared image payload limit for rendered, generated, and uploaded media.
const MaxImageBytes = 10 << 20

// FormatSystemStatus 格式化宿主机实时硬件与运行时指标（默认按非敏感脱敏模式输出）
func FormatSystemStatus() string {
	return FormatSystemStatusWithPerm(false)
}

// FormatSystemStatusWithPerm 格式化宿主机实时硬件与运行时指标（allowSensitive 为 true 时向私聊伙伴展示真实公网 IP 与启动目录）
func FormatSystemStatusWithPerm(allowSensitive bool) string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	uptime := time.Since(appStartTime).Truncate(time.Second)
	hostname, _ := os.Hostname()

	allocMB := float64(m.Alloc) / 1024 / 1024
	sysMB := float64(m.Sys) / 1024 / 1024

	if allowSensitive {
		pwd, _ := os.Getwd()
		ipLine := "探测中或未直连公网"
		if ips := GetSensitiveIPs(); len(ips) > 0 {
			ipLine = strings.Join(ips, ", ")
		}
		return fmt.Sprintf(`程序启动目录: %s
宿主节点: %s (%s/%s)
本机公网 IP: %s
WARP 出站隧道: %s
运行时间: %s
Go 运行时: %s
活跃协程: %d
进程内存占用: %.2f MB (系统保留: %.2f MB)
CPU 核心数: %d`,
			pwd,
			hostname, runtime.GOOS, runtime.GOARCH,
			ipLine,
			WarpStatus(),
			uptime,
			runtime.Version(),
			runtime.NumGoroutine(),
			allocMB, sysMB,
			runtime.NumCPU(),
		)
	}

	return RedactSensitiveIPs(fmt.Sprintf(`宿主节点: %s (%s/%s)
WARP 出站隧道: %s
运行时间: %s
Go 运行时: %s
活跃协程: %d
进程内存占用: %.2f MB (系统保留: %.2f MB)
CPU 核心数: %d`,
		hostname, runtime.GOOS, runtime.GOARCH,
		WarpStatus(),
		uptime,
		runtime.Version(),
		runtime.NumGoroutine(),
		allocMB, sysMB,
		runtime.NumCPU(),
	))
}

// UserRole 用户身份与权限级别
type UserRole string

const (
	RolePartner      UserRole = "partner"      // 核心搭档/死党 (最高信任级，拥有全部权限与 Delegates 调度权)
	RoleCollaborator UserRole = "collaborator" // 协作者/好友 (允许日常工具与工程审查)
	RoleGuest        UserRole = "guest"        // 普通群友/访客 (受限权限，由执行层强制校验可用工具)
)

// AttachmentKind 表示附件类型
type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
)

// Attachment 多模态附件抽象
type Attachment struct {
	Kind     AttachmentKind
	URL      string
	MimeType string
}

// SensoryEvent 经由物理通道归一化与防抖合并后的统一感知事件 (零前缀规范)
type SensoryEvent struct {
	Platform      string        // "qq", "cli", "telegram"
	IsGroup       bool          // 是否为群聊场景
	ChannelID     string        // 纯净会话标识 (纯群号或纯私聊用户 ID)
	SenderID      string        // 纯净发送者唯一物理标识
	SenderName    string        // 发送者真实昵称（未知时留空，不伪造假名）
	Role          UserRole      // 免疫系统评估出的权限角色
	Content       string        // 纯文本内容
	Images        []*Attachment // 多模态图像感知
	VisionSummary string        // 多模态看图提炼出的视觉观察摘要（用于持久化至工作记忆）
	ReplyContext  *SensoryEvent // 引用的前置消息上下文
	RawMessageID  string        // 原始平台消息 ID (用于被动回复凭证)
	OccurredAt    time.Time     // 事件发生时间
	OnProgress    func(string)  // 认知耗时操作（如签发重型工单）时的即时进度回执回调
}

// IsPartnerPrivate 判断当前感知事件是否为核心伙伴在私聊下发起的最高信任级交互
func (e *SensoryEvent) IsPartnerPrivate() bool {
	return e != nil && !e.IsGroup && e.Role == RolePartner
}
