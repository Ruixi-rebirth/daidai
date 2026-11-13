package main

import (
	"context"
	"encoding/json"
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
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/engine/agent"
	"daidai/internal/engine/chat"
	"daidai/internal/engine/memory"
	"daidai/internal/engine/render"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
	"daidai/internal/media"
	"daidai/internal/provider/llm"
	"daidai/internal/tool"
)

const gracefulShutdownTimeout = 4 * time.Minute

type ociSchedulerState struct {
	LastDailySentDay          string          `json:"last_daily_sent_day"`
	DailyAttemptsDay          string          `json:"daily_attempts_day"`
	DailyAttempts             int             `json:"daily_attempts"`
	DailyLastAttemptAt        int64           `json:"daily_last_attempt_at"`
	LastCostSuccessBucket     string          `json:"last_cost_success_bucket"`
	LastCostAttemptBucket     string          `json:"last_cost_attempt_bucket"`
	LastCostAttemptAt         int64           `json:"last_cost_attempt_at"`
	LastResourceSuccessBucket string          `json:"last_resource_success_bucket"`
	LastResourceAttemptBucket string          `json:"last_resource_attempt_bucket"`
	LastResourceAttemptAt     int64           `json:"last_resource_attempt_at"`
	AlertedUTCDate            string          `json:"alerted_utc_date"`
	AlertedCost               float64         `json:"alerted_cost"`
	AlertedRisk               map[string]bool `json:"alerted_risk"`
}

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
			cfg = &config.Config{}
			cfg.Server.Port = 8080
		} else {
			slog.Error("配置文件无效，停止启动", "path", cfgPath, "err", err)
			return 1
		}
	}

	// 提前抢占端口监听，防止重复启动多个进程导致 QQ WebSocket 脑裂互踩
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.Port))
	if err != nil {
		slog.Error("端口已被占用，系统中已有另一个呆呆生命体实例在运行，请勿重复启动以免导致状态冲突", "port", cfg.Server.Port, "err", err)
		return 1
	}
	defer func() { _ = listener.Close() }()

	var ociClient *tool.OCIClient
	if cfg.OCI.Enabled {
		ociCtx, ociCancel := context.WithTimeout(context.Background(), 3*time.Second)
		ociClient, err = tool.NewOCIClient(ociCtx, cfg.OCI)
		ociCancel()
		if err != nil {
			slog.Error("OCI 已启用但客户端初始化失败，停止启动", "err", err)
			return 1
		}
	}

	defer func() {
		if err := db.DefaultManager().Close(); err != nil {
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

	defaultWorkDir := ""
	agyCommand := "agy"
	agyDesc := ""
	for _, r := range cfg.Agent.Runners {
		if r.Name == "agy" {
			if r.DefaultDir != "" {
				defaultWorkDir = r.DefaultDir
			}
			if r.Command != "" {
				agyCommand = r.Command
			}
			if r.Description != "" {
				agyDesc = r.Description
			}
			break
		}
	}
	if defaultWorkDir == "" && len(cfg.Agent.Runners) > 0 {
		defaultWorkDir = cfg.Agent.Runners[0].DefaultDir
	}
	defaultWorkDir = entity.ResolveSandboxDir(defaultWorkDir)
	if err := os.MkdirAll(defaultWorkDir, 0755); err != nil {
		slog.Error("初始化默认工作区失败", "path", defaultWorkDir, "err", err)
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
		SessionDB:   db.DefaultManager(),
		Platform:    "qq",
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

	memEngine := memory.NewMemoryEngine(apiProvider)
	embBaseURL := strings.TrimSpace(cfg.LLM.Embedding.BaseURL)
	if embBaseURL == "" {
		embBaseURL = "https://api.siliconflow.cn/v1"
	}
	embModel := strings.TrimSpace(cfg.LLM.Embedding.Model)
	if embModel == "" {
		embModel = "BAAI/bge-m3"
	}
	embAPIKey := strings.TrimSpace(cfg.LLM.Embedding.APIKey)
	if embAPIKey == "" {
		embAPIKey = strings.TrimSpace(os.Getenv("SILICONFLOW_API_KEY"))
	}
	if embAPIKey == "" {
		embAPIKey = strings.TrimSpace(os.Getenv("EMBEDDING_API_KEY"))
	}
	memEngine.SetEmbedder(llm.NewOpenAIProvider(llm.OpenAIConfig{
		BaseURL: embBaseURL,
		APIKey:  embAPIKey,
		Model:   embModel,
	}))

	visEngine := vision.NewEngine(vision.Config{
		BaseURL:        cfg.LLM.Vision.BaseURL,
		APIKey:         cfg.LLM.Vision.APIKey,
		Model:          cfg.LLM.Vision.Model,
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
		if err := db.DefaultManager().ResetSessionHistoryInChat("qq", "private", ownerOpenID); err != nil {
			slog.Error("重置会话历史失败，停止启动以避免误报成功", "ownerOpenID", ownerOpenID, "err", err)
			return 1
		}
		slog.Info("已按 --reset 参数重置会话短期上下文游标", "ownerOpenID", ownerOpenID)
	}
	database := db.DefaultManager()
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
			if role.Role == "collaborator" {
				immune.AddCollaborator(role.UserID)
			}
		}
	}
	persona := entity.NewDefaultPersona()

	// 按配置挂载多沙箱工作者与工单调度中枢 (全局最大并发限制)
	delegates := entity.NewDelegatesWithLimit(cfg.Agent.MaxConcurrent, agyWorker)
	executionTimeout, err := time.ParseDuration(cfg.Agent.ExecutionTimeout)
	if err != nil {
		slog.Error("解析 agent.execution_timeout 失败", "err", err)
		return 1
	}
	if err := delegates.SetExecutionTimeout(executionTimeout); err != nil {
		slog.Error("配置工单执行超时失败", "err", err)
		return 1
	}
	if err := delegates.AttachTicketStore(context.Background(), db.DefaultManager(), "qq"); err != nil {
		slog.Error("初始化工程工单历史失败", "err", err)
		return 1
	}
	for _, r := range cfg.Agent.Runners {
		if r.Name == "" || r.Name == "agy" || r.Command == "" {
			continue
		}
		delegates.Register(agent.NewCLIWorker(agent.CLIAgentConfig{
			Name:        r.Name,
			Description: r.Description,
			Command:     r.Command,
			Args:        r.Args,
			DefaultDir:  r.DefaultDir,
		}))
	}

	// 初始化通道级工作空间中枢 (SQLite 状态持久化 + 显式激活纳管)
	workspaceMgr := entity.NewWorkspaceManager(defaultWorkDir, db.DefaultManager())
	attachmentInbox, inboxErr := media.NewInbox("data/inbox")
	if inboxErr != nil {
		slog.Warn("初始化 QQ 文件附件暂存目录失败，文件处理能力不可用", "err", inboxErr)
	} else {
		attachmentInbox.RunCleanup(rootCtx)
	}

	hands := tool.NewHandsWithConfig(tool.HandsConfig{
		AGYCommand:      agyCommand,
		Router:          mindRouter,
		Delegates:       delegates,
		Immune:          immune,
		Workspaces:      workspaceMgr,
		AttachmentInbox: attachmentInbox,
		Database:        database,
		EnableNixSearch: cfg.Plugins.NixSearch.Enabled,
		OCIConfig:       &cfg.OCI,
		OCIClient:       ociClient,
	})

	// 具体认知大脑 (实现 entity.Cognition)
	cortex := chat.NewCortex(chat.CortexConfig{
		LLM:          mindRouter,
		Persona:      persona,
		Memory:       memEngine,
		Hands:        hands,
		Vision:       visEngine,
		Workspaces:   workspaceMgr,
		Inbox:        attachmentInbox,
		WorkingLimit: cfg.Memory.WorkingLimit,
	})

	// 全双工通信管道 (实现 entity.Channel)
	channelRegistry := entity.NewChannelRegistry()
	var qqChannel *qq.QQChannel
	if cfg.QQ.Enabled && cfg.QQ.AppID != "" && cfg.QQ.Secret != "" {
		qqChannel = qq.NewQQChannel(qq.Config{
			AppID:  cfg.QQ.AppID,
			Secret: cfg.QQ.Secret,
		}, immune)
		qqChannel.SetAttachmentInbox(attachmentInbox)
		qqChannel.SetAccessRequestStore(database)
		qqChannel.SetHistoryRecorder(cortex.RecordInbound)
		channelRegistry.Register(qqChannel)
	}

	// 依赖注入组装生命体核心聚合根
	agyAuth := llm.NewAGYAuthManager(agyCommand, "")
	superAgent := entity.NewSuperAgent(entity.Config{
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

	if err := superAgent.Start(rootCtx); err != nil {
		slog.Error("启动 SuperAgent 失败", "err", err)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if cleanupErr := superAgent.Stop(cleanupCtx); cleanupErr != nil {
			slog.Error("启动失败后的 SuperAgent 清理未能完成", "err", cleanupErr)
		}
		cleanupCancel()
		return 1
	}

	var ociSchedulerDone chan struct{}
	if cfg.OCI.Enabled {
		if qqChannel == nil || ownerOpenID == "" {
			slog.Warn("OCI 定时日报和告警未启动：需要启用 QQ 并配置 qq.owner_openid")
		} else {
			ociSchedulerDone = make(chan struct{})
			go func() {
				defer close(ociSchedulerDone)
				runOCIScheduler(rootCtx, cfg.OCI, ownerOpenID, channelRegistry, ociClient)
			}()
		}
	}

	// 启动健康检查与单实例锁监听服务
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok"))
		}
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		checks := map[string]string{
			"qq_gateway":    "disabled",
			"llm_route":     "unconfigured",
			"oci_scheduler": "disabled",
		}
		ready := true
		if cfg.QQ.Enabled {
			checks["qq_gateway"] = "not_ready"
			if qqChannel != nil && qqChannel.Ready() {
				checks["qq_gateway"] = "ready"
			} else {
				ready = false
			}
		}
		if mindRouter.HasConfiguredRoute() {
			checks["llm_route"] = "configured"
		} else {
			ready = false
		}
		if cfg.OCI.Enabled {
			checks["oci_scheduler"] = "not_started"
			if ociSchedulerDone == nil {
				ready = false
			} else {
				select {
				case <-ociSchedulerDone:
					checks["oci_scheduler"] = "stopped"
					ready = false
				default:
					checks["oci_scheduler"] = "running"
				}
			}
		}
		status := http.StatusOK
		if !ready {
			status = http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(struct {
				Ready  bool              `json:"ready"`
				Checks map[string]string `json:"checks"`
			}{Ready: ready, Checks: checks})
		}
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

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
	slog.Info("呆呆超级数字生命体已安全休眠")
	if serveFailure != nil {
		slog.Error("健康检查服务停止，进程将以失败状态退出", "err", serveFailure)
		return 1
	}
	return 0
}

func runOCIScheduler(ctx context.Context, ociCfg config.OCIConfig, recipient string, channels *entity.ChannelRegistry, client *tool.OCIClient) {
	if client == nil || channels == nil || recipient == "" {
		slog.Warn("oci_scheduler: 客户端、QQ 通道或接收者缺失，后台定时任务未启动")
		return
	}
	loc, err := time.LoadLocation(strings.TrimSpace(ociCfg.Timezone))
	if err != nil {
		loc = time.FixedZone("SGT", 8*3600)
	}
	pushHour, pushMinute := 8, 30
	if ociCfg.DailyReportHour != nil {
		pushHour = *ociCfg.DailyReportHour
	}
	if ociCfg.DailyReportMinute != nil {
		pushMinute = *ociCfg.DailyReportMinute
	}
	slog.Info("oci_scheduler: 定时账单和资源巡检已启动", "pushTime", fmt.Sprintf("%02d:%02d", pushHour, pushMinute), "timezone", loc.String())

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	state := ociSchedulerState{AlertedRisk: make(map[string]bool)}
	loadCtx, loadCancel := context.WithTimeout(ctx, 3*time.Second)
	storedState, err := db.DefaultManager().GetRuntimeState(loadCtx, "qq", "oci_scheduler")
	loadCancel()
	if err != nil {
		slog.Warn("oci_scheduler: 读取持久化调度状态失败，将使用空状态", "err", err)
	} else if storedState != "" {
		if err := json.Unmarshal([]byte(storedState), &state); err != nil {
			slog.Warn("oci_scheduler: 持久化调度状态格式无效，将使用空状态", "err", err)
			state = ociSchedulerState{}
		}
	}
	if state.AlertedRisk == nil {
		state.AlertedRisk = make(map[string]bool)
	}
	lastCostSuccess := state.LastCostSuccessBucket
	lastCostAttemptKey := state.LastCostAttemptBucket
	lastResourceSuccess := state.LastResourceSuccessBucket
	lastResourceAttemptKey := state.LastResourceAttemptBucket
	lastCostAttempt := time.UnixMilli(state.LastCostAttemptAt)
	lastResourceAttempt := time.UnixMilli(state.LastResourceAttemptAt)
	persistState := func() {
		data, err := json.Marshal(state)
		if err != nil {
			slog.Error("oci_scheduler: 序列化调度状态失败", "err", err)
			return
		}
		saveCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := db.DefaultManager().SetRuntimeState(saveCtx, "qq", "oci_scheduler", string(data)); err != nil {
			slog.Error("oci_scheduler: 保存调度状态失败", "err", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().In(loc)
			localDay := now.Format("2006-01-02")
			utcDay := now.UTC().Format("2006-01-02")
			if state.DailyAttemptsDay != localDay {
				state.DailyAttemptsDay = localDay
				state.DailyAttempts = 0
				state.DailyLastAttemptAt = 0
			}
			if utcDay != state.AlertedUTCDate {
				state.AlertedUTCDate = utcDay
				state.AlertedCost = 0
				state.AlertedRisk = make(map[string]bool)
			}

			resourceBucket := fmt.Sprintf("%s-%d", utcDay, now.UTC().Hour()/6)
			var scannedResources *tool.OCIBillReport
			dailyDue := now.Hour() > pushHour || (now.Hour() == pushHour && now.Minute() >= pushMinute)
			dailyRetryReady := state.DailyLastAttemptAt == 0 || now.Sub(time.UnixMilli(state.DailyLastAttemptAt)) >= 5*time.Minute
			if dailyDue && state.LastDailySentDay != localDay && state.DailyAttempts < 3 && dailyRetryReady {
				state.DailyAttempts++
				state.DailyLastAttemptAt = now.UnixMilli()
				lastResourceAttemptKey, lastResourceAttempt = resourceBucket, now
				state.LastResourceAttemptBucket = resourceBucket
				state.LastResourceAttemptAt = now.UnixMilli()
				persistState()
				report, err := tool.FetchBillReport(ctx, client, loc)
				if err != nil {
					if state.DailyAttempts >= 3 {
						slog.Error("oci_scheduler: 获取每日账单连续失败 3 次，停止当天重试", "err", err)
					} else {
						slog.Error("oci_scheduler: 获取每日账单失败，稍后重试", "attempt", state.DailyAttempts, "err", err)
					}
				} else {
					scannedResources = report
					lastResourceSuccess = resourceBucket
					state.LastResourceSuccessBucket = resourceBucket
					persistState()
					var card []byte
					cardCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
					card, err := render.RenderCard(cardCtx, report.Markdown)
					cancel()
					if err != nil {
						slog.Warn("oci_scheduler: 日报卡片渲染失败，将发送文本", "err", err)
						card = nil
					}
					message := report.Markdown
					dailyReportDeferred := false
					if len(card) > 0 {
						message = "📊 OCI 每日账单与资源巡检"
					} else if len(core.SplitMarkdownChunks(message, 1200)) > 5 {
						queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
						_, queueErr := db.DefaultManager().EnqueuePendingNotification(queueCtx, "qq", recipient,
							"oci-daily-"+localDay, "oci_daily", report.Markdown)
						queueCancel()
						if queueErr != nil {
							dailyReportDeferred = true
							slog.Error("oci_scheduler: 日报超过文本发送上限且完整报告未能入队，将稍后重试", "date", localDay, "err", queueErr)
							message = "【OCI 每日账单】报告较长，完整日报暂未能保存，系统会稍后重试。"
						} else {
							message = "【OCI 每日账单】报告较长且图片渲染不可用，完整日报已保存，将在你下次私聊时送达。"
						}
					}
					sendErr := channels.Transmit(ctx, &entity.Utterance{Platform: "qq", ChannelID: recipient, Proactive: true, Text: message, CardData: card})
					if sendErr != nil {
						pendingBody := report.Markdown
						var partialErr *entity.PartialDeliveryError
						if errors.As(sendErr, &partialErr) && strings.TrimSpace(partialErr.UndeliveredText) != "" {
							pendingBody = partialErr.UndeliveredText
						}
						queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
						_, queueErr := db.DefaultManager().EnqueuePendingNotification(queueCtx, "qq", recipient,
							"oci-daily-"+localDay, "oci_daily", pendingBody)
						queueCancel()
						if queueErr == nil {
							state.LastDailySentDay = localDay
							persistState()
							slog.Warn("oci_scheduler: 主动推送失败，日报已排队，将在用户下次私聊时送达", "date", localDay, "err", sendErr)
						} else if state.DailyAttempts >= 3 {
							slog.Error("oci_scheduler: 日报发送和待发队列保存均失败，停止当天重试", "sendErr", sendErr, "queueErr", queueErr)
						} else {
							slog.Error("oci_scheduler: 日报发送和待发队列保存均失败，稍后重试", "attempt", state.DailyAttempts, "sendErr", sendErr, "queueErr", queueErr)
						}
					} else {
						if !dailyReportDeferred {
							state.LastDailySentDay = localDay
							persistState()
							slog.Info("oci_scheduler: 每日账单已发送或完整报告已排队", "date", localDay)
						}
					}
				}
			}

			hourKey := now.UTC().Format("2006-01-02-15")
			var reasons []string
			var alertKeys []string
			var costAlertValue float64
			if lastCostSuccess != hourKey && (lastCostAttemptKey != hourKey || now.Sub(lastCostAttempt) >= 5*time.Minute) {
				lastCostAttemptKey, lastCostAttempt = hourKey, now
				state.LastCostAttemptBucket = hourKey
				state.LastCostAttemptAt = now.UnixMilli()
				persistState()
				cost, err := tool.FetchOCICostSummary(ctx, client)
				if err != nil {
					slog.Warn("oci_scheduler: 每小时费用检查失败", "err", err)
				} else {
					lastCostSuccess = hourKey
					state.LastCostSuccessBucket = hourKey
					persistState()
					if cost.TodayCost > 0.0001 && (state.AlertedCost == 0 || cost.TodayCost >= state.AlertedCost+ociCfg.CostAlertDelta) {
						alertKeys = append(alertKeys, "cost")
						costAlertValue = cost.TodayCost
						reasons = append(reasons, fmt.Sprintf("截至 UTC %s 当日累计费用 %.4f %s", utcDay, cost.TodayCost, cost.Currency))
					}
				}
			}

			resources := scannedResources
			if resources == nil && lastResourceSuccess != resourceBucket && (lastResourceAttemptKey != resourceBucket || now.Sub(lastResourceAttempt) >= 5*time.Minute) {
				lastResourceAttemptKey, lastResourceAttempt = resourceBucket, now
				state.LastResourceAttemptBucket = resourceBucket
				state.LastResourceAttemptAt = now.UnixMilli()
				persistState()
				fetched, err := tool.FetchBillReport(ctx, client, loc)
				if err != nil {
					slog.Warn("oci_scheduler: 六小时资源巡检失败", "err", err)
				} else {
					lastResourceSuccess = resourceBucket
					state.LastResourceSuccessBucket = resourceBucket
					persistState()
					resources = fetched
				}
			}
			if resources != nil {
				if len(resources.UnboundIPs) > 0 && !state.AlertedRisk["unbound-ip"] {
					alertKeys = append(alertKeys, "unbound-ip")
					reasons = append(reasons, fmt.Sprintf("发现 %d 个闲置未绑定预留公网 IP (示例: %s)", len(resources.UnboundIPs), tool.SummarizeIPList(resources.UnboundIPs, 10)))
				}
				if resources.StorageGB > 200 && !state.AlertedRisk["storage"] {
					alertKeys = append(alertKeys, "storage")
					reasons = append(reasons, fmt.Sprintf("存储占用 %d GB，超过 200 GB 免费额度", resources.StorageGB))
				}
			}
			if len(reasons) == 0 {
				continue
			}
			alert := fmt.Sprintf("[OCI 资源防扣费告警]\n- %s", strings.Join(reasons, "\n- "))
			sendErr := channels.Transmit(ctx, &entity.Utterance{Platform: "qq", ChannelID: recipient, Proactive: true, Text: alert})
			var queueErr error
			if sendErr != nil {
				queueCtx, queueCancel := context.WithTimeout(context.Background(), 5*time.Second)
				dedupeSuffix := strings.Join(alertKeys, "+")
				if costAlertValue > 0 {
					dedupeSuffix += fmt.Sprintf("-cost-%.4f", costAlertValue)
				}
				dedupeKey := "oci-alert-" + hourKey + "-" + dedupeSuffix
				_, queueErr = db.DefaultManager().EnqueuePendingNotification(queueCtx, "qq", recipient,
					dedupeKey, "oci_alert", alert)
				queueCancel()
				if queueErr != nil {
					for _, key := range alertKeys {
						switch key {
						case "cost":
							lastCostSuccess = ""
							state.LastCostSuccessBucket = ""
						default:
							lastResourceSuccess = ""
							state.LastResourceSuccessBucket = ""
						}
					}
					persistState()
					slog.Error("oci_scheduler: 告警发送和待发队列保存均失败", "sendErr", sendErr, "queueErr", queueErr)
				} else {
					slog.Warn("oci_scheduler: 主动推送失败，告警已排队，将在用户下次私聊时送达", "reasons", reasons, "err", sendErr)
				}
			} else {
				slog.Info("oci_scheduler: OCI 告警已主动发送", "reasons", reasons)
			}
			if sendErr == nil || queueErr == nil {
				for _, key := range alertKeys {
					if key == "cost" {
						state.AlertedCost = costAlertValue
					} else {
						state.AlertedRisk[key] = true
					}
				}
				persistState()
			}
		}
	}
}
