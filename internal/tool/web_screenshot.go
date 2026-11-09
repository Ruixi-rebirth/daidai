package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/render"
	"daidai/internal/entity"
)

var (
	// chromiumSem 限制全系统最多并发运行 2 个 Chromium 实例，保护宿主机内存
	chromiumSem = make(chan struct{}, 2)
)

// ScreenshotTool 外部网页出图与截图工具 (实现 core.Tool)
// 支持 Chromium 无头浏览器全保真截图（强制绑定本地 WARP SOCKS5 代理隔离出口）；
// 若网页反爬或环境无图形支持，自动利用 Typst 渲染高清网页文摘卡片长图优雅回退
type ScreenshotTool struct {
	timeout time.Duration
}

// NewScreenshotTool 实例化网页截图工具
func NewScreenshotTool() *ScreenshotTool {
	return &ScreenshotTool{
		timeout: 25 * time.Second,
	}
}

func (t *ScreenshotTool) Name() string {
	return "screenshot_web"
}

func (t *ScreenshotTool) Description() string {
	return "网页快照与出图工具：对指定的公网网页进行快速截图，并同时提取页面正文供模型总结。单次对话最多处理 2 个链接；若用户提供了多个链接，只提取处理前 2 个核心链接，超出部分略过。返回网页截图图片与页面正文。"
}

func (t *ScreenshotTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "需要截图或出图的完整 HTTP/HTTPS 网页链接，例如 https://github.com",
			},
		},
		"required": []string{"url"},
	}
}

type screenshotArgs struct {
	URL string `json:"url"`
}

func (t *ScreenshotTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args screenshotArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid screenshot_web args: %w", err)
	}

	targetURL := strings.TrimSpace(args.URL)
	if targetURL == "" {
		return &core.ToolResult{Text: "网页链接不能为空。"}, nil
	}

	// 补全协议头
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://" + targetURL
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil || parsedURL.Host == "" {
		return &core.ToolResult{Text: fmt.Sprintf("无效的网页链接: %s", targetURL)}, nil
	}

	// 严格校验外部公网安全性，阻断内网探测
	if err := core.ValidateSafeExternalURL(ctx, targetURL); err != nil {
		slog.Warn("screenshot_web: 拦截不安全的外部链接", "url", targetURL, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("链接安全检查未通过: %v", err)}, nil
	}

	shotCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	slog.Info("screenshot_web: 开始进行网页出图与正文提取", "url", targetURL)

	// 获取网页正文内容供模型总结
	content, title, _ := FetchURLContent(shotCtx, targetURL)
	event := entity.SensoryEventFromContext(ctx)
	if event == nil || !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}
	content = safeTruncateRune(content, 4000)

	// 优先尝试 Chromium 无头浏览器直接截图
	pngBytes, shotErr := captureChromiumScreenshot(shotCtx, targetURL)
	if shotErr == nil && len(pngBytes) > 0 {
		slog.Info("screenshot_web: Chromium 网页截图成功", "url", targetURL, "size", len(pngBytes))

		var summarySb strings.Builder
		summarySb.WriteString(fmt.Sprintf("已成功获取网页截图: %s\n", targetURL))
		if title != "" {
			summarySb.WriteString(fmt.Sprintf("【网页标题】：%s\n", title))
		}
		if content != "" {
			summarySb.WriteString(fmt.Sprintf("【网页核心内容】：\n%s\n", content))
		}
		summarySb.WriteString("\n请基于上述网页内容与标题，用2~3句自然亲切的话提炼核心信息（这是什么 + 核心亮点/要点），此回复将与截取的网页长图一同发出。")

		return &core.ToolResult{
			Text:      summarySb.String(),
			ImageData: pngBytes,
		}, nil
	}

	slog.Warn("screenshot_web: Chromium 截图未完成，自动降级为网页文摘卡片渲染", "url", targetURL, "err", shotErr)

	// 优雅降级：抓取网页正文并通过 Typst 渲染精美长图
	cardBytes, cardErr := renderPageFallbackCard(shotCtx, targetURL)
	if cardErr != nil {
		slog.Error("screenshot_web: 网页文摘卡片渲染亦失败", "url", targetURL, "err", cardErr)
		return &core.ToolResult{
			Text: fmt.Sprintf("网页出图失败: %v", cardErr),
		}, nil
	}

	var fallbackSb strings.Builder
	fallbackSb.WriteString(fmt.Sprintf("网页截图已自动转由高清文摘排版卡片呈现: %s\n", targetURL))
	if title != "" {
		fallbackSb.WriteString(fmt.Sprintf("【网页标题】：%s\n", title))
	}
	if content != "" {
		fallbackSb.WriteString(fmt.Sprintf("【网页核心内容】：\n%s\n", content))
	}
	fallbackSb.WriteString("\n请基于上述网页内容提炼2~3句核心要点，与排版卡片一同回复。")

	return &core.ToolResult{
		Text:      fallbackSb.String(),
		ImageData: cardBytes,
	}, nil
}

// captureChromiumScreenshot 执行无头 Chromium 截图，强制绑定 Cloudflare WARP 本地代理出口
func captureChromiumScreenshot(ctx context.Context, targetURL string) ([]byte, error) {
	chromeBin := findChromiumBinary()
	if chromeBin == "" {
		return nil, fmt.Errorf("chromium binary not found in PATH")
	}

	select {
	case chromiumSem <- struct{}{}:
		defer func() { <-chromiumSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tmpDir := os.TempDir()
	nowNano := time.Now().UnixNano()
	shotPath := filepath.Join(tmpDir, fmt.Sprintf("daidai_shot_%d.png", nowNano))
	defer func() {
		_ = os.Remove(shotPath)
	}()

	cmdArgs := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,900",
		"--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	}

	// 强制走本地 Cloudflare WARP SOCKS5 代理隧道（若已就绪）
	socks5Proxy := core.OutboundProxySOCKS5URL()
	if socks5Proxy == "" && core.IsWarpReady() {
		socks5Proxy = "socks5://" + core.WarpSocks5Addr
	}
	if socks5Proxy != "" {
		cmdArgs = append(cmdArgs, fmt.Sprintf("--proxy-server=%s", socks5Proxy))
	}

	cmdArgs = append(cmdArgs, fmt.Sprintf("--screenshot=%s", shotPath), targetURL)

	cmd := exec.CommandContext(ctx, chromeBin, cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("chromium execution failed: %s (%w)", strings.TrimSpace(stderrBuf.String()), err)
	}

	data, err := os.ReadFile(shotPath)
	if err != nil {
		return nil, fmt.Errorf("read screenshot file failed: %w", err)
	}
	if len(data) < 8 || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		return nil, fmt.Errorf("invalid png output from chromium")
	}

	return data, nil
}

func findChromiumBinary() string {
	candidates := []string{"chromium", "google-chrome-stable", "google-chrome", "chromium-browser"}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil && p != "" {
			return p
		}
	}
	return ""
}

// renderPageFallbackCard 提取网页标题与正文并通过 Typst 排版渲染为优雅的预览长图
func renderPageFallbackCard(ctx context.Context, targetURL string) ([]byte, error) {
	content, title, err := FetchURLContent(ctx, targetURL)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse(targetURL)
	domain := ""
	if u != nil {
		domain = u.Host
	}

	if title == "" {
		title = domain
	}

	// 敏感 IP 脱敏
	event := entity.SensoryEventFromContext(ctx)
	if event == nil || !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}

	summary := safeTruncateRune(content, 1200)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))
	sb.WriteString(fmt.Sprintf("> 来源域名: `%s`  \n> 原文链接: <%s>\n\n", domain, targetURL))
	sb.WriteString("## 网页核心内容\n\n")
	sb.WriteString(summary)

	return render.RenderCard(ctx, sb.String())
}
