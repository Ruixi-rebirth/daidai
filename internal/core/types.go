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

// MaxVisionImagesPerRequest bounds image downloads and model input per turn.
const MaxVisionImagesPerRequest = 3

const (
	MaxIncomingFilesPerRequest = 3
	MaxIncomingFileBytes       = 20 << 20
	MaxAttachmentURLBytes      = 8192
)

// FormatSystemStatus 格式化主机标识与 Go 进程运行时指标（默认按非敏感模式输出）
func FormatSystemStatus() string {
	return FormatSystemStatusWithPerm(false)
}

// FormatSystemStatusWithPerm 格式化主机标识与 Go 进程运行时指标（allowSensitive 为 true 时向私聊伙伴展示真实公网 IP 与启动目录）
func FormatSystemStatusWithPerm(allowSensitive bool) string {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	uptime := time.Since(appStartTime).Truncate(time.Second)
	hostname, _ := os.Hostname()

	allocMB := float64(m.Alloc) / 1024 / 1024
	sysMB := float64(m.Sys) / 1024 / 1024
	hostResources := hostResourceSummary()
	if hostResources != "" {
		hostResources = "\n\n" + hostResources
	}

	if allowSensitive {
		pwd, _ := os.Getwd()
		ipLine := "探测中或未直连公网"
		if ips := GetSensitiveIPs(); len(ips) > 0 {
			ipLine = strings.Join(ips, ", ")
		}
		return fmt.Sprintf(`### 宿主机
- 节点：%s (%s/%s)
- 程序启动目录：%s
- 公网 IP：%s
- WARP 出站：%s

### 运行状态
- 进程运行时间：%s
- Go 版本：%s
- Goroutine：%d
- Go 堆当前分配：%.2f MiB
- Go 运行时从系统申请：%.2f MiB
- 可用逻辑 CPU：%d%s`,
			hostname, runtime.GOOS, runtime.GOARCH,
			pwd,
			ipLine,
			WarpStatus(),
			uptime,
			runtime.Version(),
			runtime.NumGoroutine(),
			allocMB, sysMB,
			runtime.NumCPU(),
			hostResources,
		)
	}

	return RedactSensitiveIPs(fmt.Sprintf(`### 宿主机
- 节点：%s (%s/%s)
- WARP 出站：%s

### 运行状态
- 进程运行时间：%s
- Go 版本：%s
- Goroutine：%d
- Go 堆当前分配：%.2f MiB
- Go 运行时从系统申请：%.2f MiB
- 可用逻辑 CPU：%d%s`,
		hostname, runtime.GOOS, runtime.GOARCH,
		WarpStatus(),
		uptime,
		runtime.Version(),
		runtime.NumGoroutine(),
		allocMB, sysMB,
		runtime.NumCPU(),
		hostResources,
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
	AttachmentFile  AttachmentKind = "file"
)

// Attachment 多模态附件抽象
type Attachment struct {
	Kind            AttachmentKind
	URL             string
	MimeType        string
	Name            string
	Path            string
	SourceMessageID string // 所属平台消息 ID，仅用于避免并发处理清除后续重发的相同附件
	Quoted          bool   // 来自用户本轮明确引用的消息，不代表当前用户本人上传
}

type GroupParticipant struct {
	ID   string
	Name string
}

// SensoryEvent 经由物理通道归一化与防抖合并后的统一感知事件 (零前缀规范)
type SensoryEvent struct {
	Platform              string        // "qq", "cli", "telegram"
	IsGroup               bool          // 是否为群聊场景
	ChannelID             string        // 纯净会话标识 (纯群号或纯私聊用户 ID)
	SenderID              string        // 纯净发送者唯一物理标识
	SenderName            string        // 发送者真实昵称（未知时留空，不伪造假名）
	Role                  UserRole      // 免疫系统评估出的权限角色
	PrivateAccessGranted  bool          // 已授权访客私聊，但仍保持访客级工具权限
	Content               string        // 纯文本内容
	HistoryAttachmentNote string        // 持久化历史中使用的附件占位说明，不包含附件地址或内容
	HistoryRecordID       int64         // 入站消息对应的持久化历史记录 ID
	Images                []*Attachment // 多模态图像感知
	OmittedImages         int           // 超出单轮视觉识别上限、未纳入分析的图片数
	PendingImages         []*Attachment // 同一发送者近期发送、尚未处理的图片元数据；仅显式请求时进行视觉分析
	OmittedPendingImages  int           // 待确认图片列表超出保留上限后丢弃的图片数
	Files                 []*Attachment // QQ 上传的普通文件；下载后由 AGY 工单按需读取
	PendingFiles          []*Attachment // 同一发送者近期发送、尚未处理的文件元数据；仅显式引用时交给 AGY
	OmittedFiles          int           // 超出单轮文件处理上限、未下载的文件数
	OmittedPendingFiles   int           // 待确认文件列表超出保留上限后丢弃的文件数
	ProcessedAttachments  []*Attachment // 本轮成功处理、仅在回复送达后从待处理列表移除的附件
	ReplyDelivered        bool          // 发声通道是否接受本轮回复（允许部分送达）；用于提交交付后生效的状态
	VisionSummary         string        // 多模态看图提炼出的视觉观察摘要（用于持久化至工作记忆）
	ReplyContext          *SensoryEvent // 引用的前置消息上下文
	ContentComplete       bool          // 引用正文已从完整出站文本缓存还原；无需把渲染图当作唯一内容来源
	RawMessageID          string        // 原始平台消息 ID (用于被动回复凭证)
	OccurredAt            time.Time     // 事件发生时间
	GroupMessageObserved  bool          // 未 @ 机器人的 QQ 群消息；写入本群历史，但默认不触发认知回复
	GroupInterjection     bool          // 当前未 @ 群消息通过低频判断，可尝试生成一条短回复
	GroupParticipants     []GroupParticipant
	OnProgress            func(string) // 认知耗时操作（如签发重型工单）时的即时进度回执回调
	StillValid            func() bool  // 可选的队列代际校验；等待会话锁后用于丢弃已被 reset 作废的事件
}

// IsPartnerPrivate 判断当前感知事件是否为核心伙伴在私聊下发起的最高信任级交互
func (e *SensoryEvent) IsPartnerPrivate() bool {
	return e != nil && !e.IsGroup && e.Role == RolePartner
}
