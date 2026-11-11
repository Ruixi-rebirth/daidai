package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	WarpSocks5Addr      = "127.0.0.1:40000"
	WarpHTTPAddr        = "127.0.0.1:40001"
	maxWarpProfileBytes = 1 << 20
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

	sensitive := make(map[netip.Addr]struct{}, len(list))
	for _, ipStr := range list {
		ip, err := netip.ParseAddr(ipStr)
		if err == nil {
			sensitive[ip.Unmap()] = struct{}{}
		}
	}
	if len(sensitive) == 0 {
		return text
	}

	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); {
		if !startsIPToken(text[i]) {
			out.WriteByte(text[i])
			i++
			continue
		}

		end := i + 1
		for end < len(text) && isIPTokenByte(text[end]) {
			end++
		}
		candidate := text[i:end]
		if start, tokenEnd, found := findSensitiveIPToken(candidate, sensitive); found {
			out.WriteString(candidate[:start])
			out.WriteString("[REDACTED_PUBLIC_IP]")
			out.WriteString(candidate[tokenEnd:])
			i = end
			continue
		}
		out.WriteString(candidate)
		i = end
	}
	return out.String()
}

func findSensitiveIPToken(candidate string, sensitive map[netip.Addr]struct{}) (int, int, bool) {
	if addr, end, ok := parseIPTokenWithTrailingPunctuation(candidate); ok {
		if _, found := sensitive[addr.Unmap()]; found {
			return 0, end, true
		}
	}

	// A label such as "v6:" can be consumed as part of the hex token. Retry
	// only after a bounded number of colon separators to find an adjacent IP.
	start := 0
	for attempt := 0; attempt < 8; attempt++ {
		separator := strings.IndexByte(candidate[start:], ':')
		if separator < 0 {
			break
		}
		start += separator + 1
		if start >= len(candidate) {
			break
		}
		addr, end, ok := parseIPTokenWithTrailingPunctuation(candidate[start:])
		if ok {
			if _, found := sensitive[addr.Unmap()]; found {
				return start, start + end, true
			}
		}
	}
	return 0, 0, false
}

func parseIPTokenWithTrailingPunctuation(token string) (netip.Addr, int, bool) {
	if addr, ok := parseIPToken(token); ok {
		return addr, len(token), true
	}
	end := len(token)
	for end > 0 && (token[end-1] == '.' || token[end-1] == ':') {
		end--
	}
	if end < len(token) {
		if addr, ok := parseIPToken(token[:end]); ok {
			return addr, end, true
		}
	}
	return netip.Addr{}, 0, false
}

func startsIPToken(b byte) bool {
	return b == '[' || b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func isIPTokenByte(b byte) bool {
	return b == '[' || b == ']' || b == ':' || b == '.' ||
		b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F'
}

func parseIPToken(token string) (netip.Addr, bool) {
	if addrPort, err := netip.ParseAddrPort(token); err == nil {
		return addrPort.Addr(), true
	}
	if addr, err := netip.ParseAddr(token); err == nil {
		return addr, true
	}
	if strings.HasPrefix(token, "[") && strings.HasSuffix(token, "]") {
		addr, err := netip.ParseAddr(token[1 : len(token)-1])
		if err == nil {
			return addr, true
		}
	}
	return netip.Addr{}, false
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

// NewProtectedHTTPClient 创建带 Cloudflare WARP 代理路由与防 DNS 重绑定 (TOCTOU) 的受保护 HTTP 客户端（用于对外网页抓取、链接出图等）
func NewProtectedHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			// Resolve and validate at the final dial boundary. When WARP is active,
			// send the selected IP (not the hostname) through SOCKS5 so wireproxy
			// cannot perform a second, unchecked DNS lookup for a rebound address.
			ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("safe dial resolve failed for host %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("no IP resolved for host %s", host)
			}
			for _, ip := range ips {
				if isForbiddenTargetIP(ip) {
					return nil, fmt.Errorf("forbidden target IP %s for host %s", ip.String(), host)
				}
			}
			safeAddr := net.JoinHostPort(ips[0].String(), port)
			if warpReady.Load() {
				return dialSOCKS5Pinned(ctx, dialer, WarpSocks5Addr, safeAddr)
			}
			// 直连时也只连接刚刚校验过的 IP，杜绝连接时发生二次解析劫持。
			return dialer.DialContext(ctx, network, safeAddr)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("stopped after 5 redirects")
			}
			return ValidateSafeExternalURL(req.Context(), req.URL.String())
		},
	}
}

func dialSOCKS5Pinned(ctx context.Context, dialer *net.Dialer, proxyAddr, targetAddr string) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial WARP SOCKS5 proxy: %w", err)
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancellation()
	keepConn := false
	defer func() {
		if !keepConn {
			_ = conn.Close()
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set WARP SOCKS5 deadline: %w", err)
	}

	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return nil, fmt.Errorf("negotiate WARP SOCKS5 authentication: %w", err)
	}
	var greeting [2]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return nil, fmt.Errorf("read WARP SOCKS5 greeting: %w", err)
	}
	if greeting != [2]byte{5, 0} {
		return nil, fmt.Errorf("WARP SOCKS5 proxy rejected unauthenticated connection")
	}

	targetHost, targetPort, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return nil, err
	}
	targetIP := net.ParseIP(targetHost)
	if targetIP == nil {
		return nil, fmt.Errorf("refusing to send an unpinned hostname to WARP SOCKS5")
	}
	portNumber, err := strconv.ParseUint(targetPort, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid target port %q: %w", targetPort, err)
	}

	request := []byte{5, 1, 0}
	if ipv4 := targetIP.To4(); ipv4 != nil {
		request = append(request, 1)
		request = append(request, ipv4...)
	} else {
		request = append(request, 4)
		request = append(request, targetIP.To16()...)
	}
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(portNumber))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("send pinned WARP SOCKS5 CONNECT request: %w", err)
	}

	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, fmt.Errorf("read WARP SOCKS5 CONNECT response: %w", err)
	}
	if reply[0] != 5 || reply[1] != 0 {
		return nil, fmt.Errorf("WARP SOCKS5 CONNECT failed with reply code %d", reply[1])
	}
	var bindAddrLen int
	switch reply[3] {
	case 1:
		bindAddrLen = net.IPv4len
	case 4:
		bindAddrLen = net.IPv6len
	case 3:
		var length [1]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil, fmt.Errorf("read WARP SOCKS5 domain reply: %w", err)
		}
		bindAddrLen = int(length[0])
	default:
		return nil, fmt.Errorf("WARP SOCKS5 returned unknown address type %d", reply[3])
	}
	if _, err := io.CopyN(io.Discard, conn, int64(bindAddrLen+2)); err != nil {
		return nil, fmt.Errorf("read WARP SOCKS5 bind address: %w", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear WARP SOCKS5 deadline: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !stopCancellation() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, context.Canceled
	}
	keepConn = true
	return conn, nil
}

// DialProtectedExternalTCP resolves and validates a public destination before
// opening a TCP connection through the local WARP SOCKS5 proxy. The resolved IP
// is sent to WARP so a second DNS lookup cannot bypass the validation.
func DialProtectedExternalTCP(ctx context.Context, host, port string) (net.Conn, error) {
	if !warpReady.Load() {
		return nil, fmt.Errorf("WARP proxy is not ready")
	}
	if host == "" || strings.ContainsAny(host, "\x00\r\n") {
		return nil, fmt.Errorf("invalid destination host")
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".lan") {
		return nil, fmt.Errorf("forbidden internal host %q", host)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, fmt.Errorf("invalid destination port %q", port)
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		ips, err = net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("safe dial resolve failed for host %s: %w", host, err)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP resolved for host %s", host)
	}
	for _, ip := range ips {
		if isForbiddenTargetIP(ip) {
			return nil, fmt.Errorf("forbidden target IP %s for host %s", ip.String(), host)
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return dialSOCKS5Pinned(ctx, dialer, WarpSocks5Addr, net.JoinHostPort(ips[0].String(), strconv.FormatUint(portNumber, 10)))
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
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
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
			_ = resp.Body.Close()
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
	warpDirInfo, err := os.Lstat(warpDir)
	if err != nil {
		return "", err
	}
	if !warpDirInfo.IsDir() || warpDirInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("WARP directory must be a real directory")
	}
	if err := os.Chmod(warpDir, 0700); err != nil {
		return "", fmt.Errorf("secure WARP directory permissions: %w", err)
	}

	confPath := filepath.Join(warpDir, "wireproxy.conf")
	profilePath := filepath.Join(warpDir, "wgcf-profile.conf")
	acctPath := filepath.Join(warpDir, "wgcf-account.toml")
	for _, path := range []string{confPath, profilePath, acctPath} {
		if err := securePrivateFile(path); err != nil {
			return "", fmt.Errorf("secure WARP credential file %s: %w", filepath.Base(path), err)
		}
	}
	if st, err := os.Lstat(confPath); err == nil && st.Size() > 0 {
		return confPath, nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}

	if _, err := os.Stat(profilePath); err != nil {
		if _, lookErr := exec.LookPath("wgcf"); lookErr != nil {
			return "", fmt.Errorf("wgcf not found in PATH")
		}
		if _, aErr := os.Stat(acctPath); aErr != nil {
			regCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			regCmd := exec.CommandContext(regCtx, "wgcf", "register", "--accept-tos")
			regCmd.Dir = warpDir
			regCmd.WaitDelay = 2 * time.Second
			regOutput := NewLimitedBuffer(256 << 10)
			regCmd.Stdout = regOutput
			regCmd.Stderr = regOutput
			regErr := regCmd.Run()
			cancel()
			if regErr != nil {
				return "", fmt.Errorf("wgcf register failed: %w (%s)", regErr, strings.TrimSpace(regOutput.String()))
			}
			if regOutput.Truncated() {
				return "", fmt.Errorf("wgcf register output exceeded 256 KiB limit")
			}
		}
		genCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		genCmd := exec.CommandContext(genCtx, "wgcf", "generate")
		genCmd.Dir = warpDir
		genCmd.WaitDelay = 2 * time.Second
		genOutput := NewLimitedBuffer(256 << 10)
		genCmd.Stdout = genOutput
		genCmd.Stderr = genOutput
		genErr := genCmd.Run()
		cancel()
		if genErr != nil {
			return "", fmt.Errorf("wgcf generate failed: %w (%s)", genErr, strings.TrimSpace(genOutput.String()))
		}
		if genOutput.Truncated() {
			return "", fmt.Errorf("wgcf generate output exceeded 256 KiB limit")
		}
	}

	for _, path := range []string{profilePath, acctPath} {
		if err := securePrivateFile(path); err != nil {
			return "", fmt.Errorf("secure generated WARP credential file %s: %w", filepath.Base(path), err)
		}
	}
	profileFile, err := os.Open(profilePath)
	if err != nil {
		return "", err
	}
	rawProfile, readErr := ReadBounded(profileFile, maxWarpProfileBytes)
	closeErr := profileFile.Close()
	if readErr != nil {
		return "", fmt.Errorf("read WARP profile: %w", readErr)
	}
	if closeErr != nil {
		return "", closeErr
	}

	fullConf := strings.TrimSpace(string(rawProfile)) + fmt.Sprintf("\n\n[Socks5]\nBindAddress = %s\n\n[http]\nBindAddress = %s\n", WarpSocks5Addr, WarpHTTPAddr)
	if err := os.WriteFile(confPath, []byte(fullConf), 0600); err != nil {
		return "", err
	}
	if err := securePrivateFile(confPath); err != nil {
		return "", fmt.Errorf("secure generated wireproxy configuration: %w", err)
	}
	return confPath, nil
}

func securePrivateFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular file")
	}
	return os.Chmod(path, 0600)
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
		if checkPortOpen(WarpHTTPAddr) && checkPortOpen(WarpSocks5Addr) {
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
				if checkPortOpen(WarpHTTPAddr) && checkPortOpen(WarpSocks5Addr) {
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
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
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
	defer func() { _ = resp.Body.Close() }()
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
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("media url cannot contain a username or password")
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
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("url cannot contain a username or password")
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
	if err != nil {
		return fmt.Errorf("failed to resolve host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("host %q resolved to no addresses", host)
	}
	for _, addr := range addrs {
		if isForbiddenTargetIP(addr.IP) {
			return fmt.Errorf("host %q resolves to forbidden internal IP %s", host, addr.IP.String())
		}
	}
	return nil
}

// forbiddenCIDRs 覆盖 RFC 保留地址、局域网私网、基准测试、云厂商内网元数据及组播/广播地址
var forbiddenCIDRs = []netip.Prefix{
	// IPv4 (RFC 1122, 1918, 6598, 3927, 2544, 5771, 1112)
	netip.MustParsePrefix("0.0.0.0/8"),       // 当前网络与 0.0.0.0 回环映射
	netip.MustParsePrefix("10.0.0.0/8"),      // 私有局域网
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT 与云厂商元数据 (如阿里云 100.100.100.200)
	netip.MustParsePrefix("127.0.0.0/8"),     // 本地回环
	netip.MustParsePrefix("169.254.0.0/16"),  // 链路本地与云元数据 (如 AWS/GCP 169.254.169.254)
	netip.MustParsePrefix("172.16.0.0/12"),   // 私有局域网
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF 协议分配
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 任播废弃网段
	netip.MustParsePrefix("192.168.0.0/16"),  // 私有局域网
	netip.MustParsePrefix("198.18.0.0/15"),   // 基准测试网段
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // 组播 Multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // 保留网段与广播 (含 255.255.255.255)

	// IPv6 (RFC 4291, 6052, 6666, 8215, 3056, 3849, 4193, 9602, 9637, 9780)
	netip.MustParsePrefix("::1/128"),        // 本地回环
	netip.MustParsePrefix("::/128"),         // 未指定地址
	netip.MustParsePrefix("64:ff9b::/96"),   // IPv4/IPv6 转换
	netip.MustParsePrefix("64:ff9b:1::/48"), // 本地 IPv4/IPv6 转换
	netip.MustParsePrefix("100::/64"),       // 丢弃前缀 (Discard-Only)
	netip.MustParsePrefix("100:0:0:1::/64"), // Dummy IPv6 Prefix，不可作为目标
	netip.MustParsePrefix("2001::/23"),      // IETF 协议分配 (整体按特殊用途地址拒绝)
	netip.MustParsePrefix("2001:2::/48"),    // 基准测试
	netip.MustParsePrefix("2001:db8::/32"),  // 文档示例专用
	netip.MustParsePrefix("2002::/16"),      // 6to4 转换前缀
	netip.MustParsePrefix("3fff::/20"),      // 文档示例专用
	netip.MustParsePrefix("5f00::/16"),      // SRv6 Segment Routing SID
	netip.MustParsePrefix("fc00::/7"),       // 唯一本地私网 ULA
	netip.MustParsePrefix("fe80::/10"),      // 链路本地
	netip.MustParsePrefix("ff00::/8"),       // 组播 Multicast
}

var globalIPv6Unicast = netip.MustParsePrefix("2000::/3")

func isForbiddenTargetIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr.IsLinkLocalUnicast() {
		return true
	}
	// 自动将 IPv4-mapped IPv6 (如 ::ffff:127.0.0.1) 还原为标准 IPv4
	addr = addr.Unmap()
	if addr.Is6() && !globalIPv6Unicast.Contains(addr) {
		return true
	}
	for _, prefix := range forbiddenCIDRs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
