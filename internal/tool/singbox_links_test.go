package tool

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func TestFormatSingboxLinks(t *testing.T) {
	jsonRaw := `{
  "uuid": "c0316efc-fd54-4bf7-8e63-d036ad0b492e",
  "subscription_urls": {
    "ipv4": "http://138.2.68.244:2080/sub/test-uuid",
    "ipv6": "http://[2603:c024:...]:2080/sub/test-uuid"
  },
  "node_count": 2,
  "nodes": [
    {
      "tag": "OCI-VLESS-Reality",
      "type": "vless",
      "protocol": "tcp",
      "host": "138.2.68.244",
      "port": 443,
      "url": "vless://test-uuid@138.2.68.244:443?flow=xtls-rprx-vision#OCI-VLESS"
    },
    {
      "tag": "OCI-Hysteria2-443",
      "type": "hysteria2",
      "protocol": "udp",
      "host": "138.2.68.244",
      "port": 443,
      "url": "hysteria2://test-pwd@138.2.68.244:443/?sni=bing.com#OCI-Hy2"
    }
  ]
}`
	jsonFormatted := FormatSingboxLinks(jsonRaw)
	if !strings.Contains(jsonFormatted, "### 📱 Sing-box 客户端一键订阅") {
		t.Fatalf("missing subscription title in JSON format: %s", jsonFormatted)
	}
	if !strings.Contains(jsonFormatted, "http://138.2.68.244:2080/sub/test-uuid") {
		t.Fatalf("missing ipv4 subscription: %s", jsonFormatted)
	}
	if !strings.Contains(jsonFormatted, "http://[2603:c024:...]:2080/sub/test-uuid") {
		t.Fatalf("missing ipv6 subscription: %s", jsonFormatted)
	}
	if !strings.Contains(jsonFormatted, "OCI-VLESS-Reality") || !strings.Contains(jsonFormatted, "OCI-Hysteria2-443") {
		t.Fatalf("missing node tags: %s", jsonFormatted)
	}
	if !strings.Contains(jsonFormatted, "```text\nvless://test-uuid@138.2.68.244:443") {
		t.Fatalf("missing node URL code block: %s", jsonFormatted)
	}
	result := &core.ToolResult{
		Text:              jsonFormatted,
		FixedPresentation: true,
	}
	if !result.FixedPresentation {
		t.Fatal("expected FixedPresentation=true for node links")
	}
}

func TestSingboxLinksTool_Permissions(t *testing.T) {
	immune := entity.NewImmuneSystem("partner_1")
	tool := NewSingboxLinksTool(immune)

	// 1. Group rejection
	groupEv := &entity.SensoryEvent{
		Role:    entity.RolePartner,
		IsGroup: true,
	}
	groupCtx := entity.WithSensoryEvent(context.Background(), groupEv)
	res, err := tool.Execute(groupCtx, nil)
	if err != nil || !strings.Contains(res.Text, "群聊中不便展示") {
		t.Fatalf("expected group rejection, got %v, res=%+v", err, res)
	}

	// 2. Stranger rejection
	strangerEv := &entity.SensoryEvent{
		Role:    entity.RoleGuest,
		IsGroup: false,
	}
	strangerCtx := entity.WithSensoryEvent(context.Background(), strangerEv)
	resGuest, err := tool.Execute(strangerCtx, nil)
	if err != nil || !strings.Contains(resGuest.Text, "仅限核心搭档查询") {
		t.Fatalf("expected stranger rejection, got %v, res=%+v", err, resGuest)
	}
}
