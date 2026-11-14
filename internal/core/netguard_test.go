package core

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNetGuard_RedactSensitiveIPsAndValidateMediaURL(t *testing.T) {
	RegisterSensitiveIP("203.0.113.88")
	RegisterSensitiveIP("2001:db8::1a2b")

	raw := "服务器连接成功，当前节点地址 203.0.113.88:8080，IPv6 为 [2001:db8::1a2b]"
	redacted := RedactSensitiveIPs(raw)
	if strings.Contains(redacted, "203.0.113.88") || strings.Contains(redacted, "2001:db8::1a2b") {
		t.Fatalf("expected public IPs to be redacted, got: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_PUBLIC_IP]") {
		t.Fatalf("expected redaction placeholder, got: %s", redacted)
	}

	// 验证伙伴私聊状态可看见真实 IP，非伙伴私聊状态隐藏真实 IP
	partnerStatus := FormatSystemStatusWithPerm(true)
	if !strings.Contains(partnerStatus, "203.0.113.88") {
		t.Errorf("expected partner private status to contain real public IP, got:\n%s", partnerStatus)
	}
	guestStatus := FormatSystemStatusWithPerm(false)
	if strings.Contains(guestStatus, "203.0.113.88") {
		t.Errorf("expected guest status to hide real public IP, got:\n%s", guestStatus)
	}

	if err := ValidateTrustedMediaURL("https://multimedia.nt.qq.com.cn/download?appid=123"); err != nil {
		t.Errorf("expected official qq media domain to be allowed, got: %v", err)
	}
	if err := ValidateTrustedMediaURL("https://gchat.qpic.cn/gchatpic_new/123/0"); err != nil {
		t.Errorf("expected qpic.cn media domain to be allowed, got: %v", err)
	}
	if err := ValidateTrustedMediaURL("https://qqbot.ugcimg.cn/download?file=voice.wav"); err != nil {
		t.Errorf("expected ugcimg.cn media domain to be allowed, got: %v", err)
	}
	if err := ValidateTrustedMediaURL("https://ifconfig.me/ip"); err == nil {
		t.Errorf("expected third-party IP probe domain to be rejected")
	}
	if err := ValidateTrustedMediaURL("http://127.0.0.1:8080/healthz"); err == nil {
		t.Errorf("expected literal IP SSRF target to be rejected")
	}

	// 验证通用外部链接 SSRF 防护
	ctx := context.Background()
	if err := ValidateSafeExternalURL(ctx, "http://127.0.0.1:8080/secret"); err == nil {
		t.Errorf("expected loopback URL to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://192.168.1.1/admin"); err == nil {
		t.Errorf("expected private LAN URL to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://169.254.169.254/latest/meta-data"); err == nil {
		t.Errorf("expected link-local metadata URL to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://100.100.100.200/latest/meta-data"); err == nil {
		t.Errorf("expected CGNAT cloud metadata URL to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://0.0.0.1/test"); err == nil {
		t.Errorf("expected 0.0.0.0/8 current network to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://224.0.0.1/"); err == nil {
		t.Errorf("expected multicast IP to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://[::1]/secret"); err == nil {
		t.Errorf("expected IPv6 loopback to be blocked")
	}
	if err := ValidateSafeExternalURL(ctx, "http://[::ffff:127.0.0.1]/secret"); err == nil {
		t.Errorf("expected IPv4-mapped IPv6 loopback to be blocked")
	}

	// 验证 ProtectedHTTPClient 的重定向安全校验
	client := NewProtectedHTTPClient(5 * time.Second)
	if client.CheckRedirect == nil {
		t.Fatalf("expected CheckRedirect to be configured on ProtectedHTTPClient")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1/admin", nil)
	if err := client.CheckRedirect(req, []*http.Request{req}); err == nil {
		t.Errorf("expected CheckRedirect to reject internal target")
	}
}
