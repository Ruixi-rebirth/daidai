package entity

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"daidai/internal/db"
)

// ChannelRegistry 全双工通道管理器
type ChannelRegistry struct {
	mu       sync.RWMutex
	channels map[string]Channel
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
	r.mu.RLock()
	defer r.mu.RUnlock()

	for name, ch := range r.channels {
		slog.Info("channel: 正在启动物理通道", "name", name)
		if err := ch.Start(ctx, onSensory); err != nil {
			return fmt.Errorf("failed to start channel %s: %w", name, err)
		}
	}
	return nil
}

func (r *ChannelRegistry) StopAll(ctx context.Context) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var firstErr error
	for _, ch := range r.channels {
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
	sessionLocks   sync.Map // key: sessionKey -> *sync.Mutex，保证同会话认知严格有序
	sessionCancels sync.Map // key: sessionKey -> context.CancelFunc，支持 /stop 即时打断在途思考
}

func sessionKey(event *SensoryEvent) string {
	return fmt.Sprintf("%s:%v:%s", event.Platform, event.IsGroup, event.ChannelID)
}

func (a *SuperAgent) lockSession(event *SensoryEvent) func() {
	key := sessionKey(event)
	val, _ := a.sessionLocks.LoadOrStore(key, &sync.Mutex{})
	mu := val.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
	slog.Info("Daidai SuperAgent 正在苏醒...", "name", a.Persona.Name)

	return a.Channels.StartAll(ctx, func(event *SensoryEvent) {
		procCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()
		a.Process(procCtx, event)
	})
}

// Stop 优雅休眠生命体
func (a *SuperAgent) Stop(ctx context.Context) error {
	slog.Info("Daidai SuperAgent 正在休眠...")
	return a.Channels.StopAll(ctx)
}

func (a *SuperAgent) replyText(ctx context.Context, event *SensoryEvent, text string) {
	if !event.IsPartnerPrivate() {
		text = core.RedactSensitiveIPs(text)
	}
	_ = a.Channels.Transmit(ctx, &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       text,
	})
}

// Process 核心认知信息流：物理感知 -> 控制面快筛 -> 认知大脑 -> 物理通道发声
func (a *SuperAgent) Process(ctx context.Context, event *SensoryEvent) {
	if event == nil {
		return
	}

	content := strings.TrimSpace(event.Content)
	slog.Info("superagent: 感知事件流入大脑", "channel", event.ChannelID, "isGroup", event.IsGroup, "sender", event.SenderName, "role", event.Role, "content", content)

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
						sections = append(sections, "后台沙箱工单: 当前空闲 (0 个运行中)")
					} else {
						var tLines []string
						for _, t := range active {
							tLines = append(tLines, fmt.Sprintf("- %s [%s]: %s (已运行 %s)", t.ID, t.Worker, t.Goal, time.Since(t.CreatedAt).Truncate(time.Second)))
						}
						sections = append(sections, fmt.Sprintf("后台沙箱工单 (%d 个运行中):\n%s", len(active), strings.Join(tLines, "\n")))
					}
				}
			}
			a.replyText(ctx, event, strings.Join(sections, "\n\n"))
			return

		case "/usage":
			if a.Cognition != nil {
				a.replyText(ctx, event, a.Cognition.ModelUsage())
				return
			}

		case "/model", "/models":
			if a.Cognition != nil {
				a.replyText(ctx, event, a.Cognition.ModelList())
				return
			}

		case "/agy login", "/agy auth":
			if a.Immune != nil {
				if err := a.Immune.VerifyDelegationPerm(event.Role); err != nil {
					a.replyText(ctx, event, err.Error())
					return
				}
			}
			if a.AGYAuth == nil {
				a.replyText(ctx, event, "AGY 认证管理器未挂载。")
				return
			}
			a.replyText(ctx, event, "正在发起 Google 账号认证，请稍候获取授权链接...")
			authURL, err := a.AGYAuth.StartLogin(ctx)
			if err != nil {
				a.replyText(ctx, event, fmt.Sprintf("发起登录失败: %v", err))
				return
			}
			reply := fmt.Sprintf("【Google Antigravity 登录授权】\n请在浏览器或手机中打开以下链接，登录 Google 账号并完成授权：\n\n%s\n\n授权成功后，页面会显示一串授权码 (Authorization Code)。\n请直接回复：\n`/agy code <授权码>`\n（授权会话 5 分钟内有效）", authURL)
			a.replyText(ctx, event, reply)
			return

		case "/agy status":
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

		case "/stop":
			if a.Immune != nil {
				if err := a.Immune.VerifyDelegationPerm(event.Role); err != nil {
					a.replyText(ctx, event, err.Error())
					return
				}
			}
			aborted := 0
			if a.Delegates != nil {
				aborted = a.Delegates.AbortByChannel(event.ChannelID)
				if aborted == 0 && event.Role == RolePartner {
					aborted = a.Delegates.AbortAll()
				}
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
				msg = fmt.Sprintf("已紧急中断当前思考进程及 %d 个正在执行的后台工单。", aborted)
			} else if aborted > 0 {
				msg = fmt.Sprintf("已紧急中断 %d 个正在执行的后台工单。", aborted)
			} else if stoppedThinking {
				msg = "已紧急中断当前正在进行的思考与工具调用。"
			}
			a.replyText(ctx, event, msg)
			return

		case "/reset", "/new", "/clear", "/ws clear", "/ws reset":
			if a.Immune != nil {
				if err := a.Immune.VerifyDelegationPerm(event.Role); err != nil {
					a.replyText(ctx, event, err.Error())
					return
				}
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
				currWs, wsName = a.Workspaces.GetCurrent(event.ChannelID)
			}

			if a.Delegates != nil {
				a.Delegates.ResetSession(event.ChannelID, currWs)
			}
			_ = db.DefaultManager().ResetSessionHistory(event.Platform, event.ChannelID)

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
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "current", ""))
			return

		case "/workspaces", "/ws":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "list", ""))
			return

		case "/ws exit", "/workspaces exit":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "exit", ""))
			return

		case "/ws clean", "/clean":
			unlock := a.lockSession(event)
			defer unlock()
			if a.Workspaces == nil {
				a.replyText(ctx, event, "工作空间管理器未配置。")
				return
			}
			a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "clean", ""))
			return

		case "/help":
			helpText := fmt.Sprintf(`【%s · 控制指令指引】
- /status   : 查看生命体运行指标与活跃工单
- /models   : 查看实时心智模型池、各通道状态与用量说明
- /agy login: 发起 Google 账号认证并获取授权网页链接
- /agy code : 提交授权码完成登录 (例: /agy code 4/0A...)
- /agy status: 查看当前 AGY 的 Google 账号认证状态
- /stop     : 紧急中断正在运行的思考与后台工单
- /reset    : 重开当前会话 (清空当前项目 agy 会话与近期上下文)
- /pwd      : 查看当前所在工作空间与运行模式
- /cd <目标> : 切入指定生产工程
- /ws       : 查看已纳管工作空间书签列表 (支持 /ws rm <项目名> 注销)
- /ws exit  : 退出当前工程，退回隔离沙盒演练区
- /ws clean : 清空沙盒里的临时草稿与测试文件`, a.Persona.Name)
			a.replyText(ctx, event, helpText)
			return

		default:
			if strings.HasPrefix(content, "/agy code ") || strings.HasPrefix(content, "/agy auth ") {
				if a.Immune != nil {
					if err := a.Immune.VerifyDelegationPerm(event.Role); err != nil {
						a.replyText(ctx, event, err.Error())
						return
					}
				}
				if a.AGYAuth == nil {
					a.replyText(ctx, event, "AGY 认证管理器未挂载。")
					return
				}
				code := strings.TrimSpace(strings.TrimPrefix(content, "/agy code "))
				if strings.HasPrefix(content, "/agy auth ") {
					code = strings.TrimSpace(strings.TrimPrefix(content, "/agy auth "))
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
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "remove", target))
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
				a.replyText(ctx, event, a.Workspaces.ExecuteAction(event.ChannelID, event.Role, a.Immune, "switch", target))
				return
			}
		}
	}

	// 同一会话内认知严格串行化，防止并发消息上下文读写乱序
	unlock := a.lockSession(event)
	defer unlock()

	if event.OnProgress == nil {
		event.OnProgress = func(progressText string) {
			if strings.TrimSpace(progressText) == "" {
				return
			}
			pCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
			if thinkCtx.Err() == context.Canceled {
				slog.Info("superagent: 当前会话思考已被主动中断", "channel", event.ChannelID)
				return
			}
			slog.Error("superagent: 思考过程发生异常", "err", err)
			sendCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a.replyText(sendCtx, event, fmt.Sprintf("思考时神经回路有些紊乱：%v", err))
			return
		}

		if utterance != nil {
			if !event.IsPartnerPrivate() {
				utterance.Text = core.RedactSensitiveIPs(utterance.Text)
			}
			slog.Info("superagent: 认知合成完毕准备发声", "channel", utterance.ChannelID, "text", utterance.Text, "hasMedia", utterance.HasMedia())
			sendCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = a.Channels.Transmit(sendCtx, utterance)
		}
	}
}
