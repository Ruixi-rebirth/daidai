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
	"time"

	"daidai/internal/core"
)

var (
	// chromiumSem 限制全系统最多并发运行 2 个 Chromium 实例，保护宿主机内存
	chromiumSem = make(chan struct{}, 2)
)

// ScreenshotTool captures the initial visible browser viewport for a public URL.
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
	return "截取指定网页的初始可视区域，仅用于用户明确要求获取网页截图；此工具只返回截图，不分析页面内容。读取、总结正文请用 read_link。"
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

const (
	screenshotBrowserTimeout     = 20 * time.Second
	screenshotBrowserLoadTimeout = 15 * time.Second
	screenshotVirtualTimeBudget  = 5 * time.Second
)

func safeLogURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "[invalid URL]"
	}
	return u.Scheme + "://" + u.Hostname()
}

func (t *ScreenshotTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args screenshotArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid screenshot_web args: %w", err)
	}

	targetURL, err := core.NormalizeExternalURL(args.URL)
	if err != nil {
		return &core.ToolResult{Text: err.Error()}, nil
	}

	// 严格校验外部公网安全性，阻断内网探测
	if err := core.ValidateSafeExternalURL(ctx, targetURL); err != nil {
		slog.Warn("screenshot_web: 拦截不安全的外部链接", "host", safeLogURL(targetURL), "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("链接安全检查未通过: %v", err)}, nil
	}

	shotCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	slog.Info("screenshot_web: 开始截取网页可视区域", "host", safeLogURL(targetURL))

	// 截取固定浏览器视口，即页面初始可视区域，不拼接页面下方内容。
	browserCtx, browserCancel := context.WithTimeout(shotCtx, screenshotBrowserTimeout)
	pngBytes, shotErr := captureChromiumScreenshot(browserCtx, targetURL)
	browserCancel()
	if shotErr == nil && len(pngBytes) > 0 {
		slog.Info("screenshot_web: Chromium 网页截图成功", "host", safeLogURL(targetURL), "size", len(pngBytes))

		return &core.ToolResult{
			ImageData:    pngBytes,
			ImageOnly:    true,
			MediaHistory: "[已发送网页截图]",
		}, nil
	}

	slog.Warn("screenshot_web: Chromium 可视区域截图未完成", "host", safeLogURL(targetURL), "errorType", fmt.Sprintf("%T", shotErr))
	return &core.ToolResult{Text: fmt.Sprintf("网页可视区域截图未能完成：%v", shotErr)}, nil
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

	privateDir, err := os.MkdirTemp("", "daidai_chrome_*")
	if err != nil {
		return nil, fmt.Errorf("create private chromium directory failed: %w", err)
	}
	defer func() { _ = os.RemoveAll(privateDir) }()
	shotPath := filepath.Join(privateDir, "screenshot.png")
	userDataDir := filepath.Join(privateDir, "profile")
	if err := os.Mkdir(userDataDir, 0700); err != nil {
		return nil, fmt.Errorf("create private chromium profile failed: %w", err)
	}

	cmdArgs := []string{
		"--headless=new",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		"--window-size=1280,900",
		"--force-device-scale-factor=1",
		fmt.Sprintf("--timeout=%d", screenshotBrowserLoadTimeout.Milliseconds()),
		fmt.Sprintf("--virtual-time-budget=%d", screenshotVirtualTimeBudget.Milliseconds()),
		"--disable-background-networking",
		"--disable-sync",
		"--user-agent=Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36",
	}
	cmdArgs = append(cmdArgs, fmt.Sprintf("--user-data-dir=%s", userDataDir))

	// Chromium 直接联网无法在页面内跳转时可靠执行 SSRF 校验，因此仅在受控 WARP 代理可用时启用。
	if core.OutboundProxySOCKS5URL() == "" {
		return nil, fmt.Errorf("受控 WARP 代理未就绪，跳过 Chromium 直连截图")
	}
	proxyListener, err := startProtectedScreenshotProxy(ctx)
	if err != nil {
		return nil, fmt.Errorf("start protected screenshot proxy: %w", err)
	}
	defer func() { _ = proxyListener.Close() }()
	cmdArgs = append(cmdArgs,
		fmt.Sprintf("--proxy-server=socks5://%s", proxyListener.Addr().String()),
		"--proxy-bypass-list=<-loopback>",
	)

	cmdArgs = append(cmdArgs, fmt.Sprintf("--screenshot=%s", shotPath), targetURL)

	cmd := core.CommandProcessGroup(ctx, chromeBin, cmdArgs...)
	cmd.WaitDelay = 2 * time.Second

	stderrBuf := core.NewLimitedBuffer(64 << 10)
	cmd.Stderr = stderrBuf

	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(stderrBuf.String())
		if runes := []rune(stderr); len(runes) > 4000 {
			stderr = string(runes[:4000]) + "..."
		}
		if stderrBuf.Truncated() {
			stderr += " [stderr 已截断]"
		}
		return nil, fmt.Errorf("chromium execution failed: %s (%w)", stderr, err)
	}

	data, err := readFileLimited(shotPath, maxGeneratedImageBytes)
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
