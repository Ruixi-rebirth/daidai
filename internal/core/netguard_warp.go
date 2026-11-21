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
	"strings"
	"time"
)

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
		defer directClient.CloseIdleConnections()
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
	defer warpReady.Store(false)
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

		warpReady.Store(false)
		cmd := CommandProcessGroup(ctx, "wireproxy", "-s", "-c", confPath)

		if err := cmd.Start(); err != nil {
			slog.Warn("netguard: 启动 wireproxy 失败", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
				continue
			}
		}

		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		monitorWarpProxy(ctx, exited)
		<-exited

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// The owner of the process also owns readiness. No startup probe may outlive
// that process and publish a stale ready state for a replacement process.
func monitorWarpProxy(ctx context.Context, exited <-chan struct{}) {
	defer warpReady.Store(false)
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-exited:
			return
		case <-ticker.C:
			ready := checkPortOpen(WarpHTTPAddr) && checkPortOpen(WarpSocks5Addr)
			wasReady := warpReady.Swap(ready)
			if ready {
				probeWarpEgress(ctx)
				if !wasReady {
					slog.Info("netguard: Cloudflare WARP 本地代理隔离隧道已就绪", "socks5", WarpSocks5Addr, "http", WarpHTTPAddr)
				}
				ticker.Reset(15 * time.Second)
			} else {
				ticker.Reset(300 * time.Millisecond)
			}
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
	defer cli.CloseIdleConnections()
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
