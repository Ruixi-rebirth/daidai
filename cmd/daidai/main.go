package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"daidai/internal/adapter/qq"
	"daidai/internal/adapter/telegram"
	"daidai/internal/briefing"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/agent"
	"daidai/internal/engine/chat"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/groupchat"
	"daidai/internal/media"
	"daidai/internal/oci"
	"daidai/internal/provider/llm"
	"daidai/internal/runtime"
	"daidai/internal/tool"
)

const gracefulShutdownTimeout = 4 * time.Minute

func main() {
	os.Exit(run())
}

func run() int {
	slog.Info("正在装载呆呆超级数字生命体...")

	var cfgFlag string
	var resetFlag bool
	flag.StringVar(&cfgFlag, "config", "", "指定配置文件路径")
	flag.BoolVar(&resetFlag, "reset", false, "启动前重置搭档私聊的短期对话历史")
	flag.Parse()

	cfgPath := cfgFlag
	explicitConfig := cfgPath != ""
	if cfgPath == "" {
		cfgPath = os.Getenv("DAIDAI_CONFIG")
		explicitConfig = cfgPath != ""
	}
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		if !explicitConfig && errors.Is(err, os.ErrNotExist) {
			slog.Warn("默认配置文件不存在，将使用不启用 QQ 的默认配置运行", "path", cfgPath)
			cfg = config.DefaultConfig()
			config.SetGlobalBotName(cfg.GetBotName())
		} else {
			slog.Error("配置文件无效，停止启动", "path", cfgPath, "err", err)
			return 1
		}
	}

	// 提前抢占端口监听，防止重复启动多个进程导致 QQ WebSocket 脑裂互踩
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.Port))
	if err != nil {
		slog.Error(fmt.Sprintf("端口已被占用，系统中已有另一个%s生命体实例在运行，请勿重复启动以免导致状态冲突", config.BotName()), "port", cfg.Server.Port, "err", err)
		return 1
	}
	defer func() { _ = listener.Close() }()

	var ociClient *oci.OCIClient
	if cfg.OCI.Enabled {
		ociCtx, ociCancel := context.WithTimeout(context.Background(), 3*time.Second)
		ociClient, err = oci.NewOCIClient(ociCtx, cfg.OCI)
		ociCancel()
		if err != nil {
			slog.Error("OCI 已启用但客户端初始化失败，停止启动", "err", err)
			return 1
		}
	}

	database := db.NewManager("data/db")
	defer func() {
		if err := database.Close(); err != nil {
			slog.Warn("关闭 SQLite 数据库连接异常", "err", err)
		} else {
			slog.Info("SQLite 数据库连接池已关闭")
		}
	}()

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// 启动本机公网 IP 自动探测与全局出站脱敏护栏
	core.InitPublicIPGuard(rootCtx)

	// 初始化底层大语言模型驱动 (API 通道与 AGY 通道)
	apiProvider := llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
	})

	agyCommand, agyDesc, defaultWorkDir, err := resolveAGYRuntime(cfg)
	if err != nil {
		slog.Error("初始化 AGY 运行配置失败", "err", err)
		return 1
	}

	agyMind := llm.NewAGYMindProvider(llm.AGYMindConfig{
		Command:    agyCommand,
		WorkingDir: defaultWorkDir,
	})

	// 组装统一异构多心智开放路由器 (支持运行时动态探测各通道可用模型列表)
	mindRouter := llm.NewMindRouter(llm.MindRouterConfig{
		StatePath:       "data/state/active_mind.json",
		FallbackChannel: "API",
	})

	agyWorker := agent.NewAGYWorkerWithConfig(agent.AGYWorkerConfig{
		Command:     agyCommand,
		Description: agyDesc,
		DefaultDir:  defaultWorkDir,
		Provider:    agyMind,
		Router:      mindRouter,
		SessionDB:   database,
	})

	// 挂载 API 通道 (自动调用 OpenAI 兼容 GET /models 探测可用模型池)
	mindRouter.RegisterChannel(llm.ChannelRegistration{
		Channel:  "API",
		Provider: apiProvider,
	})

	// 挂载 AGY 自主智能体通道 (自动调用 `agy models` 实时探测可用模型与思考强度)
	mindRouter.RegisterChannel(llm.ChannelRegistration{
		Channel:  "AGY",
		Provider: agyMind,
	})

	memEngine := newMemoryEngine(cfg, database, apiProvider)

	mediaSources := &media.Sources{}
	visEngine := vision.NewEngine(vision.Config{
		Sources:        mediaSources,
		CLICommand:     agyCommand,
		RequireAGYAuth: true,
	})

	// 初始化认知、记忆、工具与执行单元
	ownerOpenID := cfg.QQ.OwnerOpenID
	if resetFlag {
		if ownerOpenID == "" {
			slog.Error("--reset 需要配置 qq.owner_openid，无法确定要重置的私聊会话")
			return 1
		}
		if err := database.ResetSessionHistoryInChat("qq", "private", ownerOpenID); err != nil {
			slog.Error("重置会话历史失败，停止启动以避免误报成功", "ownerOpenID", ownerOpenID, "err", err)
			return 1
		}
		slog.Info("已按 --reset 参数重置会话短期上下文游标", "ownerOpenID", ownerOpenID)
	}
	roleCtx, roleCancel := context.WithTimeout(rootCtx, 5*time.Second)
	if err := database.BootstrapConfiguredCollaborators(roleCtx, "qq", cfg.QQ.Collaborators); err != nil {
		slog.Warn("迁移配置中的 QQ 协作者失败", "err", err)
	}
	storedRoles, roleErr := database.ListUserRoles(roleCtx, "qq")
	roleCancel()
	immune := entity.NewImmuneSystem(ownerOpenID)
	if roleErr != nil {
		slog.Warn("读取数据库角色失败，协作者权限暂不可用", "err", roleErr)
	} else {
		for _, role := range storedRoles {
			switch role.Role {
			case "collaborator":
				immune.AddCollaborator(role.UserID)
			case "friend":
				immune.AddPrivateUser(role.UserID)
			}
		}
	}
	persona := entity.NewDefaultPersona()
	persona.Name = cfg.GetBotName()

	// 按配置挂载多沙箱工作者与工单调度中枢 (全局最大并发限制)
	delegates, err := newDelegates(cfg, database, agyWorker)
	if err != nil {
		slog.Error("初始化工单调度失败", "err", err)
		return 1
	}

	// 初始化通道级工作空间中枢 (SQLite 状态持久化 + 显式激活纳管)
	workspaceMgr := entity.NewWorkspaceManager(defaultWorkDir, database)
	attachmentInbox, inboxErr := media.NewInbox("data/inbox")
	if inboxErr != nil {
		slog.Warn("初始化 QQ 文件附件暂存目录失败，文件处理能力不可用", "err", inboxErr)
	} else {
		attachmentInbox.Sources = mediaSources
		attachmentInbox.RunCleanup(rootCtx)
	}

	channelRegistry := entity.NewChannelRegistry()
	var telegramChannel *telegram.Channel
	var briefingService *briefing.Service
	briefingOwners := map[string]string{"qq": ownerOpenID, "telegram": strings.ToLower(strings.TrimPrefix(strings.TrimSpace(cfg.Telegram.OwnerUsername), "@"))}
	if cfg.Briefing.Enabled {
		briefingStore, openErr := briefing.Open("data/db/briefing.db")
		if openErr != nil {
			slog.Error("初始化早报归档失败", "err", openErr)
			return 1
		}
		defer func() {
			if closeErr := briefingStore.Close(); closeErr != nil {
				slog.Warn("关闭早报归档失败", "err", closeErr)
			}
		}()
		briefingService, err = briefing.New(rootCtx, briefingStore, cfg.Briefing, apiProvider, channelRegistry)
		if err != nil {
			slog.Error("初始化早报服务失败", "err", err)
			return 1
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
			defer cancel()
			if err := briefingService.Shutdown(cleanup); err != nil {
				slog.Warn("早报服务清理未完成", "err", err)
			}
		}()
		briefingService.Verify = func(ctx context.Context, sub briefing.Subscription) (bool, error) {
			if sub.Owner == "" || sub.Owner != briefingOwners[sub.Platform] {
				return false, nil
			}
			switch sub.Platform {
			case "qq":
				return cfg.QQ.Enabled && (sub.IsGroup || sub.Recipient == ownerOpenID), nil
			case "telegram":
				if telegramChannel == nil {
					return false, nil
				}
				if sub.IsGroup {
					return telegramChannel.VerifyGroup(ctx, sub.Recipient)
				}
				return telegramChannel.VerifyOwnerPrivate(ctx, sub.Recipient)
			}
			return false, nil
		}
		briefingService.Record = func(ctx context.Context, sub briefing.Subscription, text string) {
			kind := "private"
			if sub.IsGroup {
				kind = "group"
			}
			if _, err := database.LogBotReply(ctx, sub.Platform, kind, sub.Recipient, text, "", 0); err != nil {
				slog.Error("briefing: 保存已送达历史失败", "err", err)
			}
		}
	}
	groupExpressions := &groupchat.Store{Database: database}
	hands := tool.NewHandsWithConfig(tool.HandsConfig{
		AGYCommand:       agyCommand,
		Router:           mindRouter,
		Delegates:        delegates,
		Immune:           immune,
		Workspaces:       workspaceMgr,
		AttachmentInbox:  attachmentInbox,
		Database:         database,
		OCIConfig:        &cfg.OCI,
		OCIClient:        ociClient,
		CloudflareConfig: &cfg.Cloudflare,
		Briefing:         briefingService,
		BriefingOwners:   briefingOwners,
		GroupExpressions: groupExpressions,
		Vision:           visEngine,
	})

	// 具体认知大脑 (实现 entity.Cognition)
	cortex := chat.NewCortex(chat.CortexConfig{
		LLM:              mindRouter,
		Persona:          persona,
		Memory:           memEngine,
		Hands:            hands,
		Vision:           visEngine,
		Workspaces:       workspaceMgr,
		Inbox:            attachmentInbox,
		GroupExpressions: groupExpressions,
		Drafts:           database,
	})

	// 全双工通信管道 (实现 entity.Channel)
	if cfg.Telegram.Enabled {
		telegramChannel = telegram.New(telegram.Config{
			Token:         cfg.Telegram.Token,
			OwnerUsername: cfg.Telegram.OwnerUsername,
			BotName:       cfg.TelegramBotName(),
		}, database, cortex.RecordInbound, immune)
		telegramChannel.Inbox = attachmentInbox
		mediaSources.Register("telegram-file", telegramChannel.FetchFile)
		channelRegistry.Register(telegramChannel)
	}
	var qqChannel *qq.QQChannel
	if cfg.QQ.Enabled && cfg.QQ.AppID != "" && cfg.QQ.Secret != "" {
		qqChannel = qq.NewQQChannel(qq.Config{
			AppID:       cfg.QQ.AppID,
			Secret:      cfg.QQ.Secret,
			OwnerOpenID: ownerOpenID,
			BotName:     cfg.QQBotName(),
			AGYCommand:  agyCommand,
		}, database, immune)
		qqChannel.SetAttachmentInbox(attachmentInbox)
		qqChannel.SetHistoryRecorder(cortex.RecordInbound)
		channelRegistry.Register(qqChannel)
	}

	// 依赖注入组装生命体核心聚合根
	agyAuth := llm.NewAGYAuthManager(agyCommand, "")
	superAgent := runtime.NewSuperAgent(runtime.Config{
		Database:   database,
		PartnerID:  ownerOpenID,
		Persona:    persona,
		Immune:     immune,
		Cognition:  cortex,
		Delegates:  delegates,
		Channels:   channelRegistry,
		Workspaces: workspaceMgr,
		AGYAuth:    agyAuth,
		OnAuthSuccess: func(ctx context.Context) {
			mindRouter.RefreshModels(ctx)
		},
	})
	if briefingService != nil {
		superAgent.OnStop = func(ctx context.Context, event *entity.SensoryEvent) bool {
			if event.Role != entity.RolePartner {
				return false
			}
			if !briefingService.Cancel(event.Platform, event.ChannelID, event.IsGroup) {
				return false
			}
			if err := briefingService.Store.Pause(ctx, event.Platform, event.ChannelID, event.IsGroup, briefingService.Now().Format("2006-01-02")); err != nil {
				slog.Warn("briefing: 保存取消状态失败", "err", err)
			}
			return true
		}
		superAgent.OnSessionReset = func(ctx context.Context, event *entity.SensoryEvent) error {
			if event.Role != entity.RolePartner {
				return nil
			}
			briefingService.Cancel(event.Platform, event.ChannelID, event.IsGroup)
			return briefingService.Store.Viewed(ctx, event.Platform, event.ChannelID, event.IsGroup, "")
		}
		superAgent.OnPrivateInteraction = briefingService.NotifyPrivateInteraction
	}

	if err := superAgent.Start(rootCtx); err != nil {
		slog.Error("启动 SuperAgent 失败", "err", err)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if cleanupErr := superAgent.Stop(cleanupCtx); cleanupErr != nil {
			slog.Error("启动失败后的 SuperAgent 清理未能完成", "err", cleanupErr)
		}
		cleanupCancel()
		return 1
	}
	if briefingService != nil {
		briefingService.Start()
	}

	var ociSchedulerDone chan struct{}
	if cfg.OCI.Enabled {
		if qqChannel == nil || ownerOpenID == "" {
			slog.Warn("OCI 定时日报和告警未启动：需要启用 QQ 并配置 qq.owner_openid")
		} else {
			ociSchedulerDone = make(chan struct{})
			go func() {
				defer close(ociSchedulerDone)
				oci.RunScheduler(rootCtx, cfg.OCI, ownerOpenID, channelRegistry, ociClient, database)
			}()
		}
	}

	// 启动健康检查与单实例锁监听服务
	server := newHealthServer(rootCtx, cfg, qqChannel, telegramChannel, mindRouter, briefingService, ociSchedulerDone)
	serveErrCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP 健康检查服务已启动", "port", cfg.Server.Port)
		serveErrCh <- server.Serve(listener)
	}()

	// 监听操作系统中断信号并执行优雅休眠
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	var serveFailure error
	select {
	case <-sigCh:
	case serveErr := <-serveErrCh:
		if serveErr == nil {
			serveFailure = fmt.Errorf("HTTP 服务意外停止")
		} else if !errors.Is(serveErr, http.ErrServerClosed) {
			serveFailure = fmt.Errorf("HTTP 服务异常停止: %w", serveErr)
		}
	}
	signal.Stop(sigCh)

	slog.Info("收到停机信号，正在让 SuperAgent 优雅休眠...")
	rootCancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
	defer shutdownCancel()
	if briefingService != nil {
		if err := briefingService.Shutdown(shutdownCtx); err != nil {
			slog.Warn("早报服务未能在停机期限内收敛", "err", err)
		}
	}
	if ociSchedulerDone != nil {
		select {
		case <-ociSchedulerDone:
		case <-shutdownCtx.Done():
			slog.Warn("OCI 定时巡检未能在停机期限内完全收敛", "err", shutdownCtx.Err())
		}
	}

	if err := superAgent.Stop(shutdownCtx); err != nil {
		slog.Warn("通道未能在停机期限内完全收敛", "err", err)
	}
	if err := memEngine.Shutdown(shutdownCtx); err != nil {
		slog.Warn("后台记忆任务未能在停机期限内完全收敛", "err", err)
	}
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP 服务关闭异常", "err", err)
	}
	slog.Info(fmt.Sprintf("%s超级数字生命体已安全休眠", config.BotName()))
	if serveFailure != nil {
		slog.Error("健康检查服务停止，进程将以失败状态退出", "err", serveFailure)
		return 1
	}
	return 0
}
