package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestHandsRegistry_BasicsAndDeterministicOrder(t *testing.T) {
	reg := NewHandsWithConfig(HandsConfig{
		EnableNixSearch: true,
	})

	defs := reg.Definitions()
	if len(defs) != 8 {
		t.Fatalf("expected 8 definitions, got %d", len(defs))
	}

	// 验证确定性顺序（保证大模型 Prompt Cache 命中率）
	expectedOrder := []string{"render_card", "get_system_status", "manage_memory", "search_web", "read_link", "screenshot_web", "singbox_links", "search_nix_package"}
	for i, name := range expectedOrder {
		if defs[i].Function.Name != name {
			t.Errorf("expected tool #%d to be %s, got %s", i, name, defs[i].Function.Name)
		}
	}

	tool, ok := reg.Get("render_card")
	if !ok || tool == nil {
		t.Fatalf("expected render_card tool to exist")
	}

	if tool.Name() != "render_card" {
		t.Errorf("expected tool name 'render_card', got %s", tool.Name())
	}
}

func TestRenderTool_Execute(t *testing.T) {
	renderTool := NewRenderTool()
	rawArgs, _ := json.Marshal(map[string]string{
		"content": "# 测试卡片渲染\n- 这是由 Tool 生成的 Maple Mono 卡片",
	})

	res, err := renderTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("execute renderTool error: %v", err)
	}

	if len(res.ImageData) == 0 {
		t.Fatalf("expected non-empty rendered image data")
	}

	pngMagic := []byte{0x89, 'P', 'N', 'G'}
	if !bytes.HasPrefix(res.ImageData, pngMagic) {
		t.Errorf("image data is not a valid PNG")
	}
}

func TestSysInfoTool_Execute(t *testing.T) {
	sysTool := NewSysInfoTool()
	res, err := sysTool.Execute(context.Background(), json.RawMessage("{}"))
	if err != nil {
		t.Fatalf("execute sysTool error: %v", err)
	}

	if res.Text == "" {
		t.Fatalf("expected non-empty sysinfo text")
	}
}
