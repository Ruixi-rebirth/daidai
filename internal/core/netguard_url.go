package core

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
)

func ValidateTrustedMediaURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("invalid media url: %w", err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "https" {
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
		".ugcimg.cn",
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
	// Prevent a public URL from routing back to this host through its public
	// interface or NAT address. These addresses are registered at startup and
	// may also be discovered asynchronously by the direct-egress probe.
	ipGuardMu.RLock()
	_, isHostIP := sensitiveIPs[addr.String()]
	ipGuardMu.RUnlock()
	if isHostIP {
		return true
	}
	for _, prefix := range forbiddenCIDRs {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

const maxExternalURLBytes = 8 << 10

// NormalizeExternalURL normalizes a web target without performing network validation.
func NormalizeExternalURL(raw string) (string, error) {
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

// ExternalURLBudgetKey identifies equivalent HTTP targets for per-turn tool
// budgets without changing the URL used for the actual request. Fragments are
// client-side only; host casing, default ports, and an empty path do not change
// the HTTP origin or request target.
func ExternalURLBudgetKey(raw string) (string, error) {
	target, err := NormalizeExternalURL(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.Fragment = ""
	u.RawFragment = ""
	return u.String(), nil
}

// NormalizeWebSearchQuery prepares the query sent to the search provider.
func NormalizeWebSearchQuery(raw string) string {
	var sb strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) {
			sb.WriteRune(' ')
		} else {
			sb.WriteRune(r)
		}
	}
	clean := strings.TrimSpace(sb.String())
	clean = strings.TrimLeft(clean, "/")
	clean = strings.TrimSpace(clean)
	if runes := []rune(clean); len(runes) > 1000 {
		clean = string(runes[:1000])
	}
	return clean
}
