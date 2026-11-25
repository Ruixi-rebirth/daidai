package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/core"
)

func (t *OCIIPTool) updateCloudflareDNS(ctx context.Context, newIP string) error {
	if t.cfConfig == nil {
		return fmt.Errorf("未在配置文件中启用或配置 cloudflare 模块")
	}
	apiToken := strings.TrimSpace(t.cfConfig.APIToken)
	if apiToken == "" {
		return fmt.Errorf("配置项 cloudflare.api_token 不能为空")
	}
	syncRecord := strings.TrimSpace(t.cfConfig.SyncRecord)
	if syncRecord == "" {
		return fmt.Errorf("配置项 cloudflare.sync_record 不能为空 (未指定需同步的 DNS 域名)")
	}
	zoneName := strings.TrimSpace(t.cfConfig.Zone)
	if zoneName == "" {
		return fmt.Errorf("配置项 cloudflare.zone 不能为空 (未指定域名区域)")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Second}

	// 1. 获取 Zone ID
	zoneID := zoneName
	if len(zoneID) != 32 || strings.Contains(zoneID, ".") {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://api.cloudflare.com/client/v4/zones?name="+url.QueryEscape(zoneName), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+apiToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("查询 Cloudflare Zone 失败: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var zoneResp struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Result []struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&zoneResp); err != nil {
			return fmt.Errorf("解析 Zone 响应失败: %w", err)
		}
		if !zoneResp.Success || len(zoneResp.Result) == 0 {
			errMsg := "未找到匹配的 Zone"
			if len(zoneResp.Errors) > 0 {
				errMsg = zoneResp.Errors[0].Message
			}
			return fmt.Errorf("获取 Zone ID 失败: %s", errMsg)
		}
		zoneID = zoneResp.Result[0].ID
	}

	// 2. 查询 DNS 记录 ID (必须限定 type=A，绝对不影响 AAAA 记录)
	queryURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=A", zoneID, url.QueryEscape(syncRecord))
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, queryURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("查询 DNS 记录失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var recordResp struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&recordResp); err != nil {
		return fmt.Errorf("解析 DNS 记录响应失败: %w", err)
	}
	if !recordResp.Success || len(recordResp.Result) == 0 {
		errMsg := fmt.Sprintf("未在 Zone %s 中找到名为 %s 的 A 记录", zoneName, syncRecord)
		if len(recordResp.Errors) > 0 {
			errMsg = recordResp.Errors[0].Message
		}
		return fmt.Errorf("获取 DNS 记录失败: %s", errMsg)
	}
	recordID := recordResp.Result[0].ID

	// 3. 更新 DNS 记录为新公网 IP (保持 proxied: false 灰云，TTL 60s)
	updateURL := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zoneID, recordID)
	payload := map[string]any{
		"type":    "A",
		"name":    syncRecord,
		"content": newIP,
		"ttl":     60,
		"proxied": false,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err = http.NewRequestWithContext(reqCtx, http.MethodPatch, updateURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err = client.Do(req)
	if err != nil {
		return fmt.Errorf("请求更新 DNS 记录失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var updateResp struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&updateResp); err != nil {
		return fmt.Errorf("解析 DNS 更新响应失败: %w", err)
	}
	if !updateResp.Success {
		errMsg := "DNS 更新失败"
		if len(updateResp.Errors) > 0 {
			errMsg = updateResp.Errors[0].Message
		}
		return fmt.Errorf("DNS 更新失败: %s", errMsg)
	}

	return nil
}

func (t *OCIIPTool) handleSyncDNS(ctx context.Context, targetIP string) (*core.ToolResult, error) {
	if t.cfConfig == nil {
		return &core.ToolResult{Text: "未在配置文件中配置 cloudflare 模块。请在配置文件中配置 cloudflare.api_token、sync_record 和 zone。"}, nil
	}
	if strings.TrimSpace(t.cfConfig.APIToken) == "" {
		return &core.ToolResult{Text: "配置项 cloudflare.api_token 为空，请先配置 Cloudflare API Token。"}, nil
	}
	if strings.TrimSpace(t.cfConfig.SyncRecord) == "" {
		return &core.ToolResult{Text: "配置项 cloudflare.sync_record 为空，请指定需同步的 DNS 域名。"}, nil
	}
	if strings.TrimSpace(t.cfConfig.Zone) == "" {
		return &core.ToolResult{Text: "配置项 cloudflare.zone 为空，请指定域名区域。"}, nil
	}

	ip := strings.TrimSpace(targetIP)
	if ip == "" {
		privID, err := t.ResolvePrivateIPID(ctx)
		if err == nil && privID != "" {
			if curItem, err := t.getPublicIPByPrivateIP(ctx, privID); err == nil && curItem != nil {
				ip = curItem.IPAddress
			}
		}
		if ip == "" {
			return nil, fmt.Errorf("未指定目标 IP，且无法从 OCI 主网卡自动获取当前公网 IP；请显式指定 ip 参数")
		}
	}

	if err := t.updateCloudflareDNS(ctx, ip); err != nil {
		return &core.ToolResult{
			Text: fmt.Sprintf("### Cloudflare DNS 同步失败 ❌\n- **目标域名**: `%s`\n- **目标 IP**: `%s`\n- **错误原因**: %v\n\n您可以随时再次让我重试同步。", t.cfConfig.SyncRecord, ip, err),
		}, nil
	}

	subMsg := ""
	if _, lookErr := exec.LookPath("singbox-links"); lookErr == nil {
		exportCmd := exec.CommandContext(ctx, "/run/wrappers/bin/sudo", "singbox-links", "--export", ip)
		if exportOut, exportErr := exportCmd.CombinedOutput(); exportErr != nil {
			subMsg = fmt.Sprintf("\n- **本地订阅文件**: 刷新失败 (%v: %s)", exportErr, string(exportOut))
		} else {
			subMsg = "\n- **本地订阅文件**: 已自动刷新并生成包含新 IP 的最新配置"
		}
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("### Cloudflare DNS 同步成功 ✅\n- **目标域名**: `%s`\n- **A 记录已更新为**: `%s` (保持灰云/仅 DNS 模式，TTL 60s)%s", t.cfConfig.SyncRecord, ip, subMsg),
	}, nil
}
