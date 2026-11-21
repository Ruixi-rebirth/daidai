package core

import (
	"net"
	"net/netip"
	"sort"
	"strings"
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
