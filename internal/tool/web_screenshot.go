package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

var (
	// chromiumSem 限制全系统最多并发运行 2 个 Chromium 实例，保护宿主机内存
	chromiumSem        = make(chan struct{}, 2)
	screenshotProxySem = make(chan struct{}, 64)
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
	return "网页可视区域截图工具：截取并随回复发送指定网页初始可视区域。仅当用户明确要求截图、看页面长什么样、检查布局或视觉设计时调用；单纯发链接、要求总结正文或回答网页内容问题时不要调用本工具，改用 search_web 读取链接并完成用户原话中的任务。截图不读取或总结网页正文。"
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
	maxExternalURLBytes          = 8 << 10
	screenshotBrowserTimeout     = 20 * time.Second
	screenshotBrowserLoadTimeout = 15 * time.Second
	screenshotVirtualTimeBudget  = 3 * time.Second
)

func normalizeExternalURL(raw string) (string, error) {
	target := strings.TrimSpace(raw)
	if target == "" {
		return "", fmt.Errorf("网页链接不能为空")
	}
	if len(target) > maxExternalURLBytes {
		return "", fmt.Errorf("网页链接不能超过 %d 字节", maxExternalURLBytes)
	}
	schemeSeparator := strings.Index(target, "://")
	firstPathSlash := strings.IndexByte(target, '/')
	if schemeSeparator < 0 || (firstPathSlash >= 0 && schemeSeparator > firstPathSlash) {
		target = "https://" + target
	}
	if len(target) > maxExternalURLBytes {
		return "", fmt.Errorf("网页链接不能超过 %d 字节", maxExternalURLBytes)
	}

	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("无效的网页链接: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("网页链接只支持 HTTP/HTTPS")
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("网页链接缺少有效主机名")
	}
	if u.User != nil {
		return "", fmt.Errorf("网页链接不能包含用户名或密码")
	}
	return u.String(), nil
}

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

	targetURL, err := normalizeExternalURL(args.URL)
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
	event := entity.SensoryEventFromContext(ctx)
	displayURL := targetURL
	if event == nil || !event.IsPartnerPrivate() {
		displayURL = core.RedactSensitiveIPs(displayURL)
	}

	// 截取固定浏览器视口，即页面初始可视区域，不拼接页面下方内容。
	browserCtx, browserCancel := context.WithTimeout(shotCtx, screenshotBrowserTimeout)
	pngBytes, shotErr := captureChromiumScreenshot(browserCtx, targetURL)
	browserCancel()
	if shotErr == nil && len(pngBytes) > 0 {
		slog.Info("screenshot_web: Chromium 网页截图成功", "host", safeLogURL(targetURL), "size", len(pngBytes))

		return &core.ToolResult{
			Text:      fmt.Sprintf("网页初始可视区域截图成功，已随回复发送：%s", displayURL),
			ImageData: pngBytes,
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

	cmd := exec.CommandContext(ctx, chromeBin, cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
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

// startProtectedScreenshotProxy provides Chromium a per-run SOCKS5 endpoint.
// Every CONNECT destination is resolved, checked against the SSRF denylist,
// and pinned before it is sent through WARP.
func startProtectedScreenshotProxy(ctx context.Context) (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case screenshotProxySem <- struct{}{}:
				go func() {
					defer func() { <-screenshotProxySem }()
					handleProtectedScreenshotSOCKS5(ctx, conn)
				}()
			default:
				_ = conn.Close()
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
		case <-acceptDone:
		}
	}()
	return listener, nil
}

func handleProtectedScreenshotSOCKS5(ctx context.Context, client net.Conn) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer func() { _ = client.Close() }()
	stopCancellation := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopCancellation()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))
	var greeting [2]byte
	if _, err := io.ReadFull(client, greeting[:]); err != nil || greeting[0] != 5 || greeting[1] == 0 {
		return
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(client, methods); err != nil {
		return
	}
	noAuth := false
	for _, method := range methods {
		if method == 0 {
			noAuth = true
			break
		}
	}
	if !noAuth {
		_, _ = client.Write([]byte{5, 0xff})
		return
	}
	if _, err := client.Write([]byte{5, 0}); err != nil {
		return
	}
	var request [4]byte
	if _, err := io.ReadFull(client, request[:]); err != nil || request[0] != 5 || request[1] != 1 || request[2] != 0 {
		writeScreenshotSOCKSReply(client, 7)
		return
	}
	host, err := readScreenshotSOCKSHost(client, request[3])
	if err != nil {
		writeScreenshotSOCKSReply(client, 8)
		return
	}
	var portBytes [2]byte
	if _, err := io.ReadFull(client, portBytes[:]); err != nil {
		return
	}
	port := fmt.Sprint(int(portBytes[0])<<8 | int(portBytes[1]))
	_ = client.SetDeadline(time.Time{})
	upstream, err := core.DialProtectedExternalTCP(ctx, host, port)
	if err != nil {
		writeScreenshotSOCKSReply(client, 2)
		return
	}
	defer func() { _ = upstream.Close() }()
	if _, err := client.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	finished := make(chan struct{}, 2)
	copyStream := func(dst, src net.Conn) {
		_, _ = io.Copy(dst, src)
		if closeWriter, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		finished <- struct{}{}
	}
	go copyStream(upstream, client)
	go copyStream(client, upstream)
	for copied := 0; copied < 2; copied++ {
		select {
		case <-ctx.Done():
			return
		case <-finished:
		}
	}
}

func readScreenshotSOCKSHost(conn net.Conn, addressType byte) (string, error) {
	switch addressType {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return "", err
		}
		address := make([]byte, int(length[0]))
		if len(address) == 0 {
			return "", fmt.Errorf("empty domain")
		}
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return string(address), nil
	case 4:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	default:
		return "", fmt.Errorf("unsupported address type %d", addressType)
	}
}

func writeScreenshotSOCKSReply(conn net.Conn, status byte) {
	_, _ = conn.Write([]byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
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
