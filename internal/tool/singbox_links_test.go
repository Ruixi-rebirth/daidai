package tool

import (
	"context"
	"strings"
	"testing"

	"daidai/internal/entity"
)

func TestFormatSingboxLinks(t *testing.T) {
	raw := `# VLESS + Reality (TCP 443)
vless://uuid@1.2.3.4:443?flow=xtls-rprx-vision#node

# Hysteria 2 (UDP 443)
hysteria2://pwd@1.2.3.4:443/?sni=bing.com#hy2
`
	formatted := FormatSingboxLinks(raw)
	if !strings.Contains(formatted, "### Sing-box 代理节点配置") {
		t.Fatalf("missing title: %s", formatted)
	}
	if !strings.Contains(formatted, "```text\nvless://uuid@1.2.3.4:443") {
		t.Fatalf("missing code block: %s", formatted)
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
