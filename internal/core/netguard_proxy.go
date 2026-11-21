package core

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
