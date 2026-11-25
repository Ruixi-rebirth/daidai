package core

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var appStartTime = time.Now()

// MaxImageBytes is the shared image payload limit for rendered, generated, and uploaded media.
const MaxImageBytes = 10 << 20

// MaxVideoBytes is the shared video payload limit for generated and uploaded MP4 media.
const MaxVideoBytes = 10 << 20

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
	RolePartner      UserRole = "partner"      // 核心搭档/机主 (最高信任级，拥有全部运维、账号、模型与工程权)
	RoleCollaborator UserRole = "collaborator" // 协作者 (私聊许可、动态表情与群画图免限额；无系统运维或工程权)
	RoleFriend       UserRole = "friend"       // 已授权私聊好友 (允许日常私聊对话与公开工具；无动态表情与群管理)
	RoleGuest        UserRole = "guest"        // 普通群友/未授权访客 (受限权限，由统一策略与配额约束)
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
	Content               string        // 纯文本内容
	HistoryAttachmentNote string        // 持久化历史中使用的附件占位说明，不包含附件地址或内容
	HistoryRecordIDs      []int64       // 本轮原始入站消息的记录 ID，按合并顺序保留
	Images                []*Attachment // 多模态图像感知
	OmittedImages         int           // 超出单轮视觉识别上限、未纳入分析的图片数
	PendingImages         []*Attachment // 同一发送者近期发送、尚未处理的图片元数据；仅显式请求时进行视觉分析
	OmittedPendingImages  int           // 待确认图片列表超出保留上限后丢弃的图片数
	Files                 []*Attachment // QQ 上传的普通文件；下载后由 AGY 工单按需读取
	PendingFiles          []*Attachment // 同一发送者近期发送、尚未处理的文件元数据；仅显式引用时交给 AGY
	OmittedFiles          int           // 超出单轮文件处理上限、未下载的文件数
	OmittedPendingFiles   int           // 待确认文件列表超出保留上限后丢弃的文件数
	ProcessedAttachments  []*Attachment // 本轮成功处理、仅在回复送达后从待处理列表移除的附件
	VisionSummary         string        // 多模态看图提炼出的视觉观察摘要（用于持久化至工作记忆）
	ReplyContext          *SensoryEvent // 引用的前置消息上下文
	ContentComplete       bool          // 引用的文字正文已完整保存
	ContentKind           string        // 引用内容类型：text、text_card、mixed_media、image、file、unknown
	RawMessageID          string        // 原始平台消息 ID (用于被动回复凭证)
	OccurredAt            time.Time     // 事件发生时间
	GroupMessageObserved  bool          // 未 @ 机器人的 QQ 群消息；写入本群历史，但默认不触发认知回复
	GroupInterjection     bool          // 当前未 @ 群消息通过低频判断，可尝试生成一条短回复
	GroupContinuation     bool          // 免 @ 续聊候选，须先确认当前发言对象
	GroupExpression       string        // 已核验的群内表达类型：chatter/dream；仅用于交付后的记忆处理
	SkipFactExtraction    bool          // 本轮操作不参与自动提炼个人事实与熟悉度计数
	SkipVectorIndex       bool          // 本轮消息不生成稠密语义检索向量（避免运维/管理工具输出污染语义回溯）
	EphemeralControlTurn  bool          // 纯控制/状态开关轮次，完成后清理预写入入站记录且不落盘至对话历史表
	GroupParticipants     []GroupParticipant
	MentionedUsers        []GroupParticipant // 本条消息明确 @ 的群成员；供需要精确目标身份的功能校验
	OnProgress            func(string)       // 认知耗时操作（如签发重型工单）时的即时进度回执回调
	StillValid            func() bool        // 可选的队列代际校验；等待会话锁后用于丢弃已被 reset 作废的事件
}

// IsPartnerPrivate 判断当前感知事件是否为核心伙伴在私聊下发起的最高信任级交互
func (e *SensoryEvent) IsPartnerPrivate() bool {
	return e != nil && !e.IsGroup && e.Role == RolePartner
}

// CanPrivateChat reports whether the sender is permitted to hold a private chat.
func (e *SensoryEvent) CanPrivateChat() bool {
	if e == nil {
		return false
	}
	return e.Role == RolePartner || e.Role == RoleCollaborator || e.Role == RoleFriend
}

// HistoryBoundaryID is the earliest original record, before which prior
// conversation history may be read. It also works for already merged events.
func (e *SensoryEvent) HistoryBoundaryID() int64 {
	if e == nil {
		return 0
	}
	var first int64
	for _, id := range e.HistoryRecordIDs {
		if id > 0 && (first == 0 || id < first) {
			first = id
		}
	}
	return first
}

// LastHistoryRecordID associates this logical reply with its last input message.
func (e *SensoryEvent) LastHistoryRecordID() int64 {
	if e != nil {
		for i := len(e.HistoryRecordIDs) - 1; i >= 0; i-- {
			if e.HistoryRecordIDs[i] > 0 {
				return e.HistoryRecordIDs[i]
			}
		}
	}
	return 0
}

// IsNixOS 判断当前运行宿主机是否为 NixOS 或具备可用 Nix 工具链
func IsNixOS() bool {
	if _, err := os.Stat("/etc/NIXOS"); err == nil {
		return true
	}
	if data, err := os.ReadFile("/etc/os-release"); err == nil && strings.Contains(string(data), "ID=nixos") {
		return true
	}
	_, err := exec.LookPath("nix")
	return err == nil
}
