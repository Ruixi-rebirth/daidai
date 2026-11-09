package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	WarpSocks5Addr = "127.0.0.1:40000"
	WarpHTTPAddr   = "127.0.0.1:40001"
)

var (
	ipGuardMu    sync.RWMutex
	sensitiveIPs = make(map[string]struct{})
	sortedIPList []string

	warpReady    atomic.Bool
	warpEgressMu sync.RWMutex
	warpEgressIP string
)

// RegisterSensitiveIP 登记需要全局拦截脱敏的本机公网 IP 地址
func RegisterSensitiveIP(raw string) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return
	}
	ip := net.ParseIP(candidate)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
		return
	}

	canonical := ip.String()
	ipGuardMu.Lock()
	defer ipGuardMu.Unlock()

	sensitiveIPs[canonical] = struct{}{}
	if candidate != canonical {
		sensitiveIPs[candidate] = struct{}{}
	}

	sortedIPList = sortedIPList[:0]
	for k := range sensitiveIPs {
		sortedIPList = append(sortedIPList, k)
	}
	sort.Slice(sortedIPList, func(i, j int) bool {
		return len(sortedIPList[i]) > len(sortedIPList[j])
	})
}

// GetSensitiveIPs 获取当前已登记的本机公网 IP 列表（仅供伙伴在私聊中查看真实状态使用）
func GetSensitiveIPs() []string {
	ipGuardMu.RLock()
	defer ipGuardMu.RUnlock()
	list := make([]string, len(sortedIPList))
	copy(list, sortedIPList)
	return list
}

// RedactSensitiveIPs 将文本中出现的本机真实公网 IP 替换为脱敏占位符，防止群聊或非伙伴会话泄露宿主机公网地址
func RedactSensitiveIPs(text string) string {
	if text == "" {
		return ""
	}
	ipGuardMu.RLock()
	list := make([]string, len(sortedIPList))
	copy(list, sortedIPList)
	ipGuardMu.RUnlock()

	out := text
	for _, ipStr := range list {
		if ipStr != "" && strings.Contains(out, ipStr) {
			out = strings.ReplaceAll(out, ipStr, "[REDACTED_PUBLIC_IP]")
		}
	}
	return out
}

// IsWarpReady 返回本地 Cloudflare WARP 隔离代理是否已就绪
func IsWarpReady() bool {
	return warpReady.Load()
}

// OutboundProxyHTTPURL 返回本地 Cloudflare WARP 的 HTTP 代理地址（未就绪时返回空字符串）
func OutboundProxyHTTPURL() string {
	if warpReady.Load() {
		return "http://" + WarpHTTPAddr
	}
	return ""
}

// OutboundProxySOCKS5URL 返回本地 Cloudflare WARP 的 SOCKS5 代理地址（未就绪时返回空字符串）
func OutboundProxySOCKS5URL() string {
	if warpReady.Load() {
		return "socks5://" + WarpSocks5Addr
	}
	return ""
}

// WarpStatus 返回当前 Cloudflare WARP 出站隔离隧道的状态摘要
func WarpStatus() string {
	if !warpReady.Load() {
		return "未就绪"
	}
	warpEgressMu.RLock()
	egress := warpEgressIP
	warpEgressMu.RUnlock()
	if egress != "" {
		return fmt.Sprintf("已启用 (SOCKS5: %s / HTTP: %s, WARP 出口: %s)", WarpSocks5Addr, WarpHTTPAddr, egress)
	}
	return fmt.Sprintf("已启用 (SOCKS5: %s / HTTP: %s)", WarpSocks5Addr, WarpHTTPAddr)
}

// NewProtectedHTTPClient 创建带 Cloudflare WARP 代理路由的受保护 HTTP 客户端（用于对外网页抓取、链接出图等）
func NewProtectedHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	transport := &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			if warpReady.Load() {
				return url.Parse("http://" + WarpHTTPAddr)
			}
			return http.ProxyFromEnvironment(req)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// InitPublicIPGuard 扫描本机公网地址、登记脱敏护栏，并在后台自动拉起 Cloudflare WARP 本地代理隧道 (wireproxy)
func InitPublicIPGuard(ctx context.Context) {
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
				RegisterSensitiveIP(ip.String())
			}
		}
	}

	// 1. 后台直连探测宿主机真实 NAT 出口公网 IPv4 / IPv6
	go func() {
		probeEndpoints := []string{
			"https://api4.ipify.org",
			"https://api64.ipify.org",
			"https://icanhazip.com",
		}
		directClient := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy: nil, // 强制直连以准确获取本机真实公网出口 IP
			},
		}
		for _, ep := range probeEndpoints {
			reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, ep, nil)
			if err != nil {
				cancel()
				continue
			}
			resp, err := directClient.Do(req)
			if err != nil {
				cancel()
				continue
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
			resp.Body.Close()
			cancel()

			if ip := net.ParseIP(strings.TrimSpace(string(body))); ip != nil && !ip.IsPrivate() && !ip.IsLoopback() {
				RegisterSensitiveIP(ip.String())
			}
		}
	}()

	// 2. 后台自动初始化并守护 Cloudflare WARP 本地代理隧道 (wireproxy)
	go startWarpProxySupervisor(ctx)
}

func ensureWireproxyConfig(ctx context.Context, warpDir string) (string, error) {
	if err := os.MkdirAll(warpDir, 0700); err != nil {
		return "", err
	}
	confPath := filepath.Join(warpDir, "wireproxy.conf")
	if st, err := os.Stat(confPath); err == nil && st.Size() > 0 {
		return confPath, nil
	}

	profilePath := filepath.Join(warpDir, "wgcf-profile.conf")
	if _, err := os.Stat(profilePath); err != nil {
		if _, lookErr := exec.LookPath("wgcf"); lookErr != nil {
			return "", fmt.Errorf("wgcf not found in PATH")
		}
		acctPath := filepath.Join(warpDir, "wgcf-account.toml")
		if _, aErr := os.Stat(acctPath); aErr != nil {
			regCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			regCmd := exec.CommandContext(regCtx, "wgcf", "register", "--accept-tos")
			regCmd.Dir = warpDir
			out, regErr := regCmd.CombinedOutput()
			cancel()
			if regErr != nil {
				return "", fmt.Errorf("wgcf register failed: %w (%s)", regErr, strings.TrimSpace(string(out)))
			}
		}
		genCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		genCmd := exec.CommandContext(genCtx, "wgcf", "generate")
		genCmd.Dir = warpDir
		out, genErr := genCmd.CombinedOutput()
		cancel()
		if genErr != nil {
			return "", fmt.Errorf("wgcf generate failed: %w (%s)", genErr, strings.TrimSpace(string(out)))
		}
	}

	rawProfile, err := os.ReadFile(profilePath)
	if err != nil {
		return "", err
	}

	fullConf := strings.TrimSpace(string(rawProfile)) + fmt.Sprintf("\n\n[Socks5]\nBindAddress = %s\n\n[http]\nBindAddress = %s\n", WarpSocks5Addr, WarpHTTPAddr)
	if err := os.WriteFile(confPath, []byte(fullConf), 0600); err != nil {
		return "", err
	}
	return confPath, nil
}

func startWarpProxySupervisor(ctx context.Context) {
	if _, err := exec.LookPath("wireproxy"); err != nil {
		slog.Info("netguard: 未检测到 wireproxy，跳过本地 WARP 代理隧道启动")
		return
	}

	warpDir := filepath.Join("data", "warp")
	confPath, err := ensureWireproxyConfig(ctx, warpDir)
	if err != nil {
		slog.Warn("netguard: 初始化 Cloudflare WARP 配置失败", "err", err)
		return
	}

	for {
		if ctx.Err() != nil {
			return
		}

		// 若端口已被外部或前序实例监听，直接探测验证
		if checkPortOpen(WarpHTTPAddr) {
			warpReady.Store(true)
			probeWarpEgress(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
				continue
			}
		}

		cmd := exec.CommandContext(ctx, "wireproxy", "-s", "-c", confPath)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil && cmd.Process.Pid > 0 {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}

		if err := cmd.Start(); err != nil {
			slog.Warn("netguard: 启动 wireproxy 失败", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		go func() {
			for i := 0; i < 15; i++ {
				if ctx.Err() != nil {
					return
				}
				time.Sleep(300 * time.Millisecond)
				if checkPortOpen(WarpHTTPAddr) {
					warpReady.Store(true)
					probeWarpEgress(ctx)
					slog.Info("netguard: Cloudflare WARP 本地代理隔离隧道已就绪", "socks5", WarpSocks5Addr, "http", WarpHTTPAddr)
					return
				}
			}
		}()

		_ = cmd.Wait()
		warpReady.Store(false)

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func checkPortOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func probeWarpEgress(ctx context.Context) {
	proxyURL, err := url.Parse("http://" + WarpHTTPAddr)
	if err != nil {
		return
	}
	cli := &http.Client{
		Timeout: 8 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
	}
	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://cloudflare.com/cdn-cgi/trace", nil)
	if err != nil {
		return
	}
	resp, err := cli.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))

	var ipVal, coloVal string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "ip=") {
			ipVal = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
		} else if strings.HasPrefix(line, "colo=") {
			coloVal = strings.TrimSpace(strings.TrimPrefix(line, "colo="))
		}
	}
	if ipVal != "" {
		summary := ipVal
		if coloVal != "" {
			summary = fmt.Sprintf("%s (%s)", ipVal, coloVal)
		}
		warpEgressMu.Lock()
		warpEgressIP = summary
		warpEgressMu.Unlock()
	}
}

// ValidateTrustedMediaURL 校验远程媒体 URL 是否属于受信任的官方平台图床且非内网地址，防止外部链接探测宿主机公网 IP 或触发 SSRF
func ValidateTrustedMediaURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid media url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}

	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return fmt.Errorf("empty media url host")
	}
	if net.ParseIP(host) != nil {
		return fmt.Errorf("literal IP media url is forbidden")
	}

	trustedSuffixes := []string{
		".qpic.cn",
		".gtimg.cn",
		".qq.com",
		".qq.com.cn",
		".myqcloud.com",
	}
	trusted := false
	for _, suf := range trustedSuffixes {
		if strings.HasSuffix(host, suf) || host == strings.TrimPrefix(suf, ".") {
			trusted = true
			break
		}
	}
	if !trusted {
		return fmt.Errorf("untrusted media host %q: refusing direct outbound connection to protect host public IP", host)
	}

	return nil
}

// ValidateSafeExternalURL 校验任意外部网页 URL（用于链接出图等场景），阻断一切回环地址、局域网私有地址与元数据地址，防止 SSRF 偷拍内网
func ValidateSafeExternalURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}

	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return fmt.Errorf("forbidden internal host: %q", host)
	}

	if ip := net.ParseIP(host); ip != nil {
		if isForbiddenTargetIP(ip) {
			return fmt.Errorf("forbidden private or loopback IP: %s", ip.String())
		}
		return nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}
	for _, addr := range addrs {
		if isForbiddenTargetIP(addr.IP) {
			return fmt.Errorf("host %q resolves to forbidden internal IP %s", host, addr.IP.String())
		}
	}
	return nil
}

func isForbiddenTargetIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		// 阻断 Carrier-grade NAT 100.64.0.0/10 与基准网络
		if ip[0] == 100 && (ip[1]&0xc0) == 64 {
			return true
		}
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}
