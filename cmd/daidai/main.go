package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	"daidai/internal/provider/llm"
	"daidai/internal/tool"
)

func main() {
	slog.Info("正在装载呆呆超级数字生命体...")

	var cfgFlag string
	flag.StringVar(&cfgFlag, "config", "", "指定配置文件路径")
	flag.Parse()

	cfgPath := cfgFlag
	if cfgPath == "" {
		cfgPath = os.Getenv("DAIDAI_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		slog.Warn("未读取到配置文件，将使用默认配置运行", "path", cfgPath, "err", err)
		cfg = &config.Config{}
		cfg.Server.Port = 8080
	}

	// 提前抢占端口监听，防止重复启动多个进程导致 QQ WebSocket 脑裂互踩
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Server.Port))
	if err != nil {
		slog.Error("端口已被占用，系统中已有另一个呆呆生命体实例在运行，请勿重复启动以免导致状态冲突", "port", cfg.Server.Port, "err", err)
		os.Exit(1)
	}
	defer listener.Close()

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
	if defaultWorkDir == "" {
		defaultWorkDir = "data/workspace/sandbox"
	}
	if strings.HasPrefix(defaultWorkDir, "~") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			defaultWorkDir = filepath.Join(home, strings.TrimPrefix(defaultWorkDir, "~"))
		}
	} else if !filepath.IsAbs(defaultWorkDir) {
		if abs, err := filepath.Abs(defaultWorkDir); err == nil {
			defaultWorkDir = abs
		}
	}
	_ = os.MkdirAll(defaultWorkDir, 0755)

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
		BaseURL:    cfg.LLM.Vision.BaseURL,
		APIKey:     cfg.LLM.Vision.APIKey,
		Model:      cfg.LLM.Vision.Model,
		CLICommand: agyCommand,
	})

	// 初始化认知、记忆、工具与执行单元
	ownerOpenID := cfg.QQ.OwnerOpenID
	for _, arg := range os.Args[1:] {
		if arg == "--reset" && ownerOpenID != "" {
			_ = db.DefaultManager().ResetSessionHistory("qq", ownerOpenID)
			slog.Info("已按 --reset 参数重置会话短期上下文游标", "ownerOpenID", ownerOpenID)
		}
	}
	immune := entity.NewImmuneSystem(ownerOpenID, cfg.QQ.Collaborators...)
	persona := entity.NewDefaultPersona()

	// 按配置挂载多沙箱工作者与工单调度中枢 (全局最大并发限制)
	delegates := entity.NewDelegatesWithLimit(cfg.Agent.MaxConcurrent, agyWorker)
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

	hands := tool.NewHandsWithConfig(tool.HandsConfig{
		Router:          mindRouter,
		Delegates:       delegates,
		Immune:          immune,
		Workspaces:      workspaceMgr,
		EnableNixSearch: cfg.Plugins.NixSearch.Enabled,
		OCIConfig:       &cfg.OCI,
	})

	// 具体认知大脑 (实现 entity.Cognition)
	cortex := chat.NewCortex(chat.CortexConfig{
		LLM:          mindRouter,
		Persona:      persona,
		Memory:       memEngine,
		Hands:        hands,
		Vision:       visEngine,
		Workspaces:   workspaceMgr,
		WorkingLimit: cfg.Memory.WorkingLimit,
	})

	// 全双工通信管道 (实现 entity.Channel)
	channelRegistry := entity.NewChannelRegistry()
	if cfg.QQ.Enabled && cfg.QQ.AppID != "" && cfg.QQ.Secret != "" {
		channelRegistry.Register(qq.NewQQChannel(qq.Config{
			AppID:  cfg.QQ.AppID,
			Secret: cfg.QQ.Secret,
		}, immune))
	}

	// 依赖注入组装生命体核心聚合根
	agyAuth := llm.NewAGYAuthManager(agyCommand, "")
	superAgent := entity.NewSuperAgent(entity.Config{
		PartnerID:     ownerOpenID,
		Persona:       persona,
		Immune:        immune,
		Cognition:     cortex,
		Delegates:     delegates,
		Channels:      channelRegistry,
		Workspaces:    workspaceMgr,
		AGYAuth:       agyAuth,
		OnAuthSuccess: func(ctx context.Context) {
			mindRouter.RefreshModels(ctx)
		},
	})

	if err := superAgent.Start(rootCtx); err != nil {
		slog.Error("启动 SuperAgent 失败", "err", err)
		return
	}

	if cfg.OCI.Enabled && ownerOpenID != "" {
		go runOCIScheduler(rootCtx, cfg.OCI, ownerOpenID, channelRegistry)
	}

	// 启动健康检查与单实例锁监听服务
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	server := &http.Server{
		Handler: mux,
	}

	go func() {
		slog.Info("HTTP 健康检查服务已启动", "port", cfg.Server.Port)
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP 服务启动异常", "err", err)
		}
	}()

	// 监听操作系统中断信号并执行优雅休眠
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("收到停机信号，正在让 SuperAgent 优雅休眠...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	_ = superAgent.Stop(shutdownCtx)
	_ = server.Shutdown(shutdownCtx)
	if err := db.DefaultManager().Close(); err != nil {
		slog.Warn("关闭 SQLite 数据库连接异常", "err", err)
	} else {
		slog.Info("SQLite 数据库连接池已安全关闭并完成 Checkpoint")
	}
	slog.Info("呆呆超级数字生命体已安全休眠")
}

func runOCIScheduler(ctx context.Context, ociCfg config.OCIConfig, partnerID string, channels *entity.ChannelRegistry) {
	loc, err := time.LoadLocation(ociCfg.Timezone)
	if err != nil {
		loc = time.FixedZone("SGT", 8*3600)
	}

	pushHour := ociCfg.DailyReportHour
	pushMin := ociCfg.DailyReportMinute
	if pushHour <= 0 && pushMin <= 0 {
		pushHour = 8
		pushMin = 30
	}

	client, err := tool.NewOCIClient(ctx, ociCfg)
	if err != nil {
		slog.Warn("oci_scheduler: 初始化 OCI 客户端失败，后台定时巡检未启动", "err", err)
		return
	}

	slog.Info("oci_scheduler: OCI 账单定时播报与异常扣费巡检中枢已装载", "pushTime", fmt.Sprintf("%02d:%02d", pushHour, pushMin), "timezone", loc.String())

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	var lastDailyPush string
	var lastHourlyCheck string
	var lastAlertedDay string
	var alertedCost float64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now().In(loc)
			todayStr := now.Format("2006-01-02")

			// 跨日重置当天已预警扣费水位
			if todayStr != lastAlertedDay {
				lastAlertedDay = todayStr
				alertedCost = 0
			}

			// 每日定时账单播报
			if now.Hour() == pushHour && now.Minute() >= pushMin && lastDailyPush != todayStr {
				lastDailyPush = todayStr
				slog.Info("oci_scheduler: 触发每日定时 OCI 账单播报", "date", todayStr)
				report, err := tool.FetchBillReport(ctx, client, loc)
				if err != nil {
					slog.Error("oci_scheduler: 获取账单失败", "err", err)
					continue
				}

				var cardData []byte
				if png, err := render.RenderCard(ctx, report.Markdown); err == nil {
					cardData = png
				}

				_ = channels.Transmit(ctx, &entity.Utterance{
					Platform:  "qq",
					IsGroup:   false,
					ChannelID: partnerID,
					Text:      report.Markdown,
					CardData:  cardData,
				})
			}

			// 每小时防扣费多维安全巡检 (实时监测新增扣费、闲置预留 IP 与存储超额)
			hourKey := now.Format("2006-01-02-15")
			if lastHourlyCheck != hourKey {
				lastHourlyCheck = hourKey
				report, err := tool.FetchBillReport(ctx, client, loc)
				if err == nil {
					var alertReasons []string

					// 1. 突发新增费用告警
					if report.TodayCost > 0.0001 && report.TodayCost > alertedCost+0.0001 {
						alertedCost = report.TodayCost
						alertReasons = append(alertReasons, fmt.Sprintf("今日产生新增扣费: %.4f %s", report.TodayCost, report.Currency))
					}

					// 2. 闲置未绑定预留 IP 告警 (闲置 IP 在 OCI 会按小时计费)
					if len(report.UnboundIPs) > 0 {
						alertReasons = append(alertReasons, fmt.Sprintf("发现 %d 个闲置未绑定的预留公网 IP (%s)，存在闲置扣费风险，建议及时释放", len(report.UnboundIPs), strings.Join(report.UnboundIPs, ", ")))
					}

					// 3. 存储容量超额告警 (超过 200 GB 免费额度)
					if report.StorageGB > 200 {
						alertReasons = append(alertReasons, fmt.Sprintf("总存储占用已达 %d GB，超出 200 GB 免费额度，存在超额计费风险", report.StorageGB))
					}

					if len(alertReasons) > 0 {
						slog.Warn("oci_scheduler: 触发防扣费巡检预警", "reasons", alertReasons)
						alertMsg := fmt.Sprintf("[OCI 资源防扣费安全预警]\n巡检发现以下异常项：\n- %s\n\n%s", strings.Join(alertReasons, "\n- "), report.Markdown)
						_ = channels.Transmit(ctx, &entity.Utterance{
							Platform:  "qq",
							IsGroup:   false,
							ChannelID: partnerID,
							Text:      alertMsg,
						})
					}
				}
			}
		}
	}
}
