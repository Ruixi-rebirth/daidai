package entity

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

// ChannelRegistry 全双工通道管理器
type ChannelRegistry struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex
	channels    map[string]Channel
}

func NewChannelRegistry() *ChannelRegistry {
	return &ChannelRegistry{
		channels: make(map[string]Channel),
	}
}

func (r *ChannelRegistry) Register(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[ch.Name()] = ch
}

func (r *ChannelRegistry) Get(name string) (Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ch, ok := r.channels[name]
	return ch, ok
}

func (r *ChannelRegistry) StartAll(ctx context.Context, onSensory func(event *SensoryEvent)) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.RLock()
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Strings(names)
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()

	started := make([]Channel, 0, len(names))
	for i, name := range names {
		ch := channels[i]
		slog.Info("channel: 正在启动物理通道", "name", name)
		started = append(started, ch)
		if err := ch.Start(ctx, onSensory); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			for i := len(started) - 1; i >= 0; i-- {
				if stopErr := started[i].Stop(cleanupCtx); stopErr != nil {
					slog.Warn("channel: 启动失败后的回滚停止未完全成功", "name", started[i].Name(), "err", stopErr)
				}
			}
			cancel()
			return fmt.Errorf("failed to start channel %s: %w", name, err)
		}
	}
	return nil
}

func (r *ChannelRegistry) StopAll(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()

	r.mu.RLock()
	var firstErr error
	names := make([]string, 0, len(r.channels))
	for name := range r.channels {
		names = append(names, name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	channels := make([]Channel, len(names))
	for i, name := range names {
		channels[i] = r.channels[name]
	}
	r.mu.RUnlock()
	for i := range names {
		ch := channels[i]
		if err := ch.Stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *ChannelRegistry) Transmit(ctx context.Context, u *Utterance) error {
	if u == nil {
		return nil
	}
	if u.Platform == "" {
		return fmt.Errorf("empty platform in utterance")
	}

	if u.IsGroup {
		u.Text = core.RedactSensitiveIPs(u.Text)
	}

	ch, ok := r.Get(u.Platform)
	if !ok {
		return fmt.Errorf("channel %q not mounted", u.Platform)
	}

	return ch.Transmit(ctx, u)
}

// SuperAgent 呆呆超级数字生命体核心聚合根 (仅持有实际调度的核心职责)
type SuperAgent struct {
	Persona        *Persona
	Immune         *ImmuneSystem
	Cognition      Cognition
	Delegates      *Delegates
	Channels       *ChannelRegistry
	Workspaces     *WorkspaceManager
	AGYAuth        AGYAuthenticator
	OnAuthSuccess  func(ctx context.Context)
	sessionLocksMu sync.Mutex
	sessionLocks   map[string]*sessionLockEntry // 引用计数覆盖持锁者和等待者，空闲后删除会话锁
	sessionCancels sync.Map                     // key: sessionKey -> context.CancelFunc，支持 /stop 即时打断在途思考
}

type sessionLockEntry struct {
	mu   sync.Mutex
	refs int
}

func sessionKey(event *SensoryEvent) string {
	return fmt.Sprintf("%s:%v:%s", event.Platform, event.IsGroup, event.ChannelID)
}

func (a *SuperAgent) lockSession(event *SensoryEvent) func() {
	key := sessionKey(event)
	a.sessionLocksMu.Lock()
	if a.sessionLocks == nil {
		a.sessionLocks = make(map[string]*sessionLockEntry)
	}
	entry := a.sessionLocks[key]
	if entry == nil {
		entry = &sessionLockEntry{}
		a.sessionLocks[key] = entry
	}
	entry.refs++
	a.sessionLocksMu.Unlock()

	entry.mu.Lock()
	return func() {
		a.sessionLocksMu.Lock()
		entry.mu.Unlock()
		entry.refs--
		if entry.refs == 0 {
			delete(a.sessionLocks, key)
		}
		a.sessionLocksMu.Unlock()
	}
}

// Config 生命体初始化依赖
type Config struct {
	PartnerID     string
	Persona       *Persona
	Immune        *ImmuneSystem
	Cognition     Cognition
	Delegates     *Delegates
	Channels      *ChannelRegistry
	Workspaces    *WorkspaceManager
	AGYAuth       AGYAuthenticator
	OnAuthSuccess func(ctx context.Context)
}

// NewSuperAgent 组装生命体核心聚合根
func NewSuperAgent(cfg Config) *SuperAgent {
	persona := cfg.Persona
	if persona == nil {
		persona = NewDefaultPersona()
	}
	immune := cfg.Immune
	if immune == nil {
		immune = NewImmuneSystem(cfg.PartnerID)
	}
	channels := cfg.Channels
	if channels == nil {
		channels = NewChannelRegistry()
	}

	return &SuperAgent{
		Persona:       persona,
		Immune:        immune,
		Cognition:     cfg.Cognition,
		Delegates:     cfg.Delegates,
		Channels:      channels,
		Workspaces:    cfg.Workspaces,
		AGYAuth:       cfg.AGYAuth,
		OnAuthSuccess: cfg.OnAuthSuccess,
	}
}

// Start 唤醒生命体并启动所有物理通信通道
func (a *SuperAgent) Start(ctx context.Context) error {
	if a.Channels == nil {
		return fmt.Errorf("channel registry is not configured")
	}
	name := ""
	if a.Persona != nil {
		name = a.Persona.Name
	}
	slog.Info("Daidai SuperAgent 正在苏醒...", "name", name)

	return a.Channels.StartAll(ctx, func(event *SensoryEvent) {
		procCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()
		a.Process(procCtx, event)
	})
}

// Stop 优雅休眠生命体
func (a *SuperAgent) Stop(ctx context.Context) error {
	slog.Info("Daidai SuperAgent 正在休眠...")
	if a.AGYAuth != nil {
		a.AGYAuth.Cancel()
	}
	if a.Channels == nil {
		return nil
	}
	return a.Channels.StopAll(ctx)
}

func (a *SuperAgent) transmitReplyText(ctx context.Context, event *SensoryEvent, text string) error {
	if a.Channels == nil {
		err := fmt.Errorf("channel registry is not configured")
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
		return err
	}
	if !event.IsPartnerPrivate() {
		text = core.RedactSensitiveIPs(text)
	}
	err := a.Channels.Transmit(ctx, &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       text,
	})
	if err != nil {
		slog.Warn("superagent: 文本回复发送失败", "platform", event.Platform, "channel", event.ChannelID, "err", err)
	}
	return err
}

func (a *SuperAgent) replyText(ctx context.Context, event *SensoryEvent, text string) {
	_ = a.transmitReplyText(ctx, event, text)
}

func (a *SuperAgent) delegationPermissionError(role UserRole) error {
	if a.Immune == nil {
		return fmt.Errorf("权限校验未配置，拒绝执行此操作")
	}
	return a.Immune.VerifyDelegationPerm(role)
}

func parseAGYAuthCodeCommand(content string) (string, bool) {
	parts := strings.Fields(strings.TrimSpace(content))
	if len(parts) < 3 || !strings.EqualFold(parts[0], "/agy") ||
		(!strings.EqualFold(parts[1], "code") && !strings.EqualFold(parts[1], "auth")) {
		return "", false
	}
	return strings.Trim(strings.Join(parts[2:], " "), "`'\""), true
}

// Process 核心认知信息流：物理感知 -> 控制面快筛 -> 认知大脑 -> 物理通道发声
func (a *SuperAgent) Process(ctx context.Context, event *SensoryEvent) {
	if event == nil {
		return
	}

	content := strings.TrimSpace(event.Content)
	slog.Info("superagent: 感知事件流入大脑", "channel", event.ChannelID, "isGroup", event.IsGroup, "sender", event.SenderName, "role", event.Role, "contentBytes", len(content))

	// 极速控制面 (0 Token 拦截通道)
	if strings.HasPrefix(content, "/") {
		switch content {
		case "/status", "/sysinfo":
			var sections []string
			sections = append(sections, "### 呆呆生命体当前机能状态")
			if a.Cognition != nil {
				if mindStatus := strings.TrimSpace(a.Cognition.MindStatus()); mindStatus != "" {
					sections = append(sections, mindStatus)
				}
			}
			sections = append(sections, core.FormatSystemStatusWithPerm(event.IsPartnerPrivate()))
			if event.Role != RoleGuest {
				if a.Workspaces != nil {
					sections = append(sections, a.Workspaces.FormatCurrent(event.ChannelID))
				}
				if a.Delegates != nil {
					active := a.Delegates.ActiveTickets()
					if len(active) == 0 {
						sections = append(sections, "后台工程工单：当前空闲 (0 个活跃)")
					} else {
						var tLines []string
						for _, t := range active {
							tLines = append(tLines, fmt.Sprintf("- %s [%s · %s]: %s (已持续 %s)", t.ID, t.Worker, t.Status, t.Goal, time.Since(t.CreatedAt).Truncate(time.Second)))
						}
						sections = append(sections, fmt.Sprintf("后台工程工单 (%d 个活跃):\n%s", len(active), strings.Join(tLines, "\n")))
					}
				}
			}
			a.replyText(ctx, event, strings.Join(sections, "\n\n"))
			return

		case "/usage", "/model", "/models":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.Cognition == nil {
				return
			}
			if content == "/usage" {
				a.replyText(ctx, event, a.Cognition.ModelUsage())
			} else {
				a.replyText(ctx, event, a.Cognition.ModelList())
			}
			return

		case "/agy login", "/agy auth", "/agy login force", "/agy login --force":
			if event.IsGroup {
				a.replyText(ctx, event, "AGY 认证链接包含账号授权信息，请在与呆呆的私聊中发起登录。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			isForce := strings.HasSuffix(content, "force")
			if !isForce {
				if st := a.AGYAuth.GetStatus(); st.IsLoggedIn {
					a.replyText(ctx, event, fmt.Sprintf("当前已登录 Google 账号 (%s)。如需重新登录或切换账号，请发送 `/agy logout` 注销后再登录，或发送 `/agy login force` 强制重新认证。", st.Email))
					return
				}
			} else {
				if err := a.AGYAuth.Logout(); err != nil {
					a.replyText(ctx, event, fmt.Sprintf("清理现有 AGY 登录状态失败，已取消强制重新认证：%v", err))
					return
				}
			}
			a.replyText(ctx, event, "正在发起 Google 账号认证，请稍候获取授权链接...")
			authURL, err := a.AGYAuth.StartLogin(ctx)
			if err != nil {
				a.replyText(ctx, event, fmt.Sprintf("发起登录失败: %v", err))
				return
			}
			reply := fmt.Sprintf("【Google Antigravity 登录授权】\n请在浏览器或手机中打开以下链接，登录 Google 账号并完成授权：\n\n```\n%s\n```\n\n授权成功后，页面会显示一串授权码 (Authorization Code)。\n请直接回复：\n`/agy code <授权码>`\n（授权会话 5 分钟内有效）", authURL)
			a.replyText(ctx, event, reply)
			return

		case "/agy status":
			if event.IsGroup {
				a.replyText(ctx, event, "AGY 账号状态包含私人信息，请在与呆呆的私聊中查询。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			st := a.AGYAuth.GetStatus()
			if !st.IsLoggedIn {
				a.replyText(ctx, event, "【AGY 认证状态】\n当前尚未登录 Google 账号或凭证已失效。可发送 `/agy login` 发起登录。")
				return
			}
			expiryStr := "长期有效"
			if !st.Expiry.IsZero() {
				expiryStr = st.Expiry.Local().Format("2006-01-02 15:04:05")
			}
			reply := fmt.Sprintf("【AGY 认证状态】\n- 状态：已登录在线\n- 账号：%s\n- Token 有效期至：%s", st.Email, expiryStr)
			a.replyText(ctx, event, reply)
			return

		case "/agy logout":
			if event.IsGroup {
				a.replyText(ctx, event, "账号注销操作请在与呆呆的私聊中进行。")
				return
			}
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			st := a.AGYAuth.GetStatus()
			if err := a.AGYAuth.Logout(); err != nil {
				a.replyText(ctx, event, fmt.Sprintf("注销失败: %v", err))
				return
			}
			if !st.IsLoggedIn {
				a.replyText(ctx, event, "【AGY 登录已取消】\n当前没有已登录账号；等待中的登录会话已清理。如需使用可发送 `/agy login` 重新认证。")
				return
			}
			emailHint := ""
			if st.Email != "" {
				emailHint = fmt.Sprintf(" (%s)", st.Email)
			}
			a.replyText(ctx, event, fmt.Sprintf("【AGY 账号已注销】\n已安全清除 Google 账号凭据%s。如需再次使用可发送 `/agy login` 重新认证。", emailHint))
			return

		case "/stop":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}
			aborted := 0
			if a.Delegates != nil {
				aborted = a.Delegates.AbortByChannel(event.ChannelID)
			}
			stoppedThinking := false
			if cancelVal, ok := a.sessionCancels.LoadAndDelete(sessionKey(event)); ok {
				if cancelFn, ok := cancelVal.(context.CancelFunc); ok && cancelFn != nil {
					cancelFn()
					stoppedThinking = true
				}
			}
			msg := "当前没有正在运行中的思考任务或后台工单。"
			if aborted > 0 && stoppedThinking {
				msg = fmt.Sprintf("已紧急中断当前思考进程，并向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
			} else if aborted > 0 {
				msg = fmt.Sprintf("已向 %d 个后台工单发出停止请求（尚未启动的任务已取消；运行中任务退出后释放执行容量）。", aborted)
			} else if stoppedThinking {
				msg = "已紧急中断当前正在进行的思考与工具调用。"
			}
			a.replyText(ctx, event, msg)
			return

		case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
			if err := a.delegationPermissionError(event.Role); err != nil {
				a.replyText(ctx, event, err.Error())
				return
			}

			// 先中断在途工单与正在持有会话锁的思考协程，防止 lockSession 陷入死等
			if a.Delegates != nil {
				_ = a.Delegates.AbortByChannel(event.ChannelID)
			}
			if cancelVal, ok := a.sessionCancels.LoadAndDelete(sessionKey(event)); ok {
				if cancelFn, ok := cancelVal.(context.CancelFunc); ok && cancelFn != nil {
					cancelFn()
				}
			}

			unlock := a.lockSession(event)
			defer unlock()

			currWs := ""
			wsName := ""
			if a.Workspaces != nil {
				var workspaceErr error
				currWs, wsName, workspaceErr = a.Workspaces.GetCurrentChecked(event.ChannelID)
				if workspaceErr != nil {
					a.replyText(ctx, event, fmt.Sprintf("【会话重开失败】\n无法确认当前工作区，近期历史和 AGY 会话均未重置：%v", workspaceErr))
					return
				}
			}

			chatType := "private"
			if event.IsGroup {
				chatType = "group"
			}
			if err := db.DefaultManager().ResetSessionHistoryInChat(event.Platform, chatType, event.ChannelID); err != nil {
				slog.Error("superagent: 重开会话时清理近期历史失败", "platform", event.Platform, "channelID", event.ChannelID, "err", err)
				a.replyText(ctx, event, "【会话重开失败】\n近期对话历史未能清理，AGY 会话记录未清空；已中断的任务不会自动恢复。请稍后重试。")
				return
			}
			if a.Delegates != nil {
				a.Delegates.ResetSession(event.ChannelID, currWs)
			}

			feedback := fmt.Sprintf("【当前会话已完成重开】\n已清空工作空间 `%s` (`%s`) 的 agy 会话与近期对话记忆，长期画像与项目文件完好保留。", wsName, currWs)
			a.replyText(ctx, event, feedback)
			return

		case "/pwd":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "current", "", a.Delegates))
			return

		case "/workspaces", "/ws":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "list", "", a.Delegates))
			return

		case "/ws exit", "/workspaces exit":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "exit", "", a.Delegates))
			return

		case "/ws clean", "/clean":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "clean", "", a.Delegates))
			return

		case "/help":
			helpText := fmt.Sprintf(`【%s · 控制指令指引】
- /status   : 查看生命体运行指标与活跃工单
- /models   : 查看实时心智模型池、各通道状态与用量说明
- /agy login : 发起 Google 账号认证并获取授权网页链接
- /agy code  : 提交授权码完成登录 (例: /agy code 4/0A...)
- /agy status: 查看当前 AGY 的 Google 账号认证状态
- /agy logout: 注销当前 Google 账号并清除本地凭据
- /stop     : 紧急中断正在运行的思考与后台工单
- /reset    : 重开当前会话 (清空当前项目 agy 会话与近期上下文)
- /pwd      : 查看当前所在工作空间与运行模式
- /cd <目标> : 切入指定生产工程
- /ws       : 查看已纳管工作空间书签列表 (支持 /ws rm <项目名> 注销)
- /ws exit  : 退出当前工程，退回默认工作区
- /ws clean : 清理默认工作区里的临时文件`, a.Persona.Name)
			a.replyText(ctx, event, helpText)
			return

		default:
			if code, isAuthCodeCommand := parseAGYAuthCodeCommand(content); isAuthCodeCommand {
				if event.IsGroup {
					a.replyText(ctx, event, "授权码属于私人凭据，请勿在群聊中发送。请在与呆呆的私聊中提交授权码。")
					return
				}
				if err := a.delegationPermissionError(event.Role); err != nil {
					a.replyText(ctx, event, err.Error())
					return
				}
				if a.AGYAuth == nil {
					a.replyText(ctx, event, "AGY 认证管理器未挂载。")
					return
				}
				a.replyText(ctx, event, "正在验证并换取凭证，请稍候...")
				email, err := a.AGYAuth.SubmitCode(ctx, code)
				if err != nil {
					a.replyText(ctx, event, fmt.Sprintf("认证失败: %v", err))
					return
				}
				if a.OnAuthSuccess != nil {
					a.OnAuthSuccess(ctx)
				}
				a.replyText(ctx, event, fmt.Sprintf("【AGY 登录成功】\n已成功绑定 Google 账号：%s！\n心智模型池已自动刷新就绪。", email))
				return
			}
			if strings.HasPrefix(content, "/ws rm ") || strings.HasPrefix(content, "/workspaces rm ") {
				unlock := a.lockSession(event)
				defer unlock()
				if a.Workspaces == nil {
					a.replyText(ctx, event, "工作空间管理器未配置。")
					return
				}
				target := strings.TrimSpace(strings.TrimPrefix(content, "/workspaces rm "))
				if strings.HasPrefix(content, "/ws rm ") {
					target = strings.TrimSpace(strings.TrimPrefix(content, "/ws rm "))
				}
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "remove", target, a.Delegates))
				return
			}

			if strings.HasPrefix(content, "/cd ") || content == "/cd" {
				unlock := a.lockSession(event)
				defer unlock()
				if a.Workspaces == nil {
					a.replyText(ctx, event, "工作空间管理器未配置。")
					return
				}
				target := strings.TrimSpace(strings.TrimPrefix(content, "/cd"))
				if target == "" {
					target = "~"
				}
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "switch", target, a.Delegates))
				return
			}
		}
	}

	// 同一会话内认知严格串行化，防止并发消息上下文读写乱序
	unlock := a.lockSession(event)
	defer unlock()

	if event.OnProgress == nil {
		event.OnProgress = func(progressText string) {
			if strings.TrimSpace(progressText) == "" || ctx.Err() != nil {
				return
			}
			pCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			a.replyText(pCtx, event, progressText)
		}
	}

	if a.Cognition != nil {
		sKey := sessionKey(event)
		thinkCtx, thinkCancel := context.WithCancel(ctx)
		a.sessionCancels.Store(sKey, thinkCancel)
		defer func() {
			a.sessionCancels.Delete(sKey)
			thinkCancel()
		}()

		utterance, err := a.Cognition.Think(thinkCtx, event)
		if err != nil {
			if thinkCtx.Err() != nil {
				slog.Info("superagent: 当前会话思考上下文已结束", "channel", event.ChannelID, "err", thinkCtx.Err())
				return
			}
			slog.Error("superagent: 思考过程发生异常", "err", err)
			errorReply := fmt.Sprintf("思考时神经回路有些紊乱：%v", err)
			if !event.IsPartnerPrivate() {
				errorReply = core.RedactSensitiveIPs(errorReply)
			}
			sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if sendErr := a.transmitReplyText(sendCtx, event, errorReply); sendErr != nil {
				return
			}
			if committer, ok := a.Cognition.(interface {
				CommitDeliveredReply(context.Context, *SensoryEvent, string)
			}); ok {
				commitCtx, commitCancel := context.WithTimeout(ctx, 15*time.Second)
				committer.CommitDeliveredReply(commitCtx, event, errorReply)
				commitCancel()
			}
			return
		}

		if utterance != nil {
			if thinkCtx.Err() != nil {
				return
			}
			if !event.IsPartnerPrivate() {
				utterance.Text = core.RedactSensitiveIPs(utterance.Text)
			}
			slog.Info("superagent: 认知合成完毕准备发声", "channel", utterance.ChannelID, "textBytes", len(utterance.Text), "hasMedia", utterance.HasMedia())
			sendCtx, cancel := context.WithTimeout(thinkCtx, 30*time.Second)
			sendErr := a.Channels.Transmit(sendCtx, utterance)
			cancel()

			replyText := utterance.Text
			if sendErr != nil {
				slog.Error("superagent: 回复发送失败", "channel", utterance.ChannelID, "err", sendErr)
				replyText = ""
			}
			if committer, ok := a.Cognition.(interface {
				CommitDeliveredReply(context.Context, *SensoryEvent, string)
			}); ok {
				commitCtx, commitCancel := context.WithTimeout(ctx, 15*time.Second)
				committer.CommitDeliveredReply(commitCtx, event, replyText)
				commitCancel()
			}
		}
	}
}
