package oci

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/core"
)

func (t *OCIIPTool) handleBind(ctx context.Context, targetIP string, deleteOld bool) (*core.ToolResult, error) {
	privID, err := t.ResolvePrivateIPID(ctx)
	if err != nil {
		return nil, err
	}

	// 1. 查找目标预留 IP 对象
	ips, err := t.listReservedPublicIPs(ctx)
	if err != nil {
		return nil, err
	}

	var targetObj *ociPublicIPItem
	if targetIP != "" {
		for i := range ips {
			if ips[i].IPAddress == targetIP {
				targetObj = &ips[i]
				break
			}
		}
	} else {
		var unbounds []*ociPublicIPItem
		for i := range ips {
			if isPublicIPAvailable(&ips[i]) {
				unbounds = append(unbounds, &ips[i])
			}
		}
		if len(unbounds) == 1 {
			targetObj = unbounds[0]
		} else if len(unbounds) > 1 {
			var ipStrs []string
			for _, u := range unbounds {
				ipStrs = append(ipStrs, u.IPAddress)
			}
			return &core.ToolResult{
				Text: fmt.Sprintf("当前存在多个候选未绑定 IP (`%s`)，请指定目标 IP (`oci_ip(action=\"bind\", ip=\"...\")`)", strings.Join(ipStrs, ", ")),
			}, nil
		}
	}

	if targetObj == nil {
		return &core.ToolResult{Text: "未找到待绑定的预留公网 IP，请先执行 roll 刷取。"}, nil
	}
	if targetObj.PrivateIPID == "" && !isPublicIPAvailable(targetObj) {
		return &core.ToolResult{Text: fmt.Sprintf("目标 IP `%s` 当前状态为 %s，无法确认可绑定；请等待 OCI 状态稳定后再试。", targetObj.IPAddress, publicIPStateLabel(targetObj))}, nil
	}
	if targetObj.PrivateIPID == privID && targetObj.LifecycleState != "ASSIGNED" {
		return &core.ToolResult{Text: fmt.Sprintf("目标 IP `%s` 当前状态为 %s，无法确认绑定稳定；请等待 OCI 状态稳定后再试。", targetObj.IPAddress, publicIPStateLabel(targetObj))}, nil
	}
	if targetObj.PrivateIPID != "" && targetObj.PrivateIPID != privID {
		return &core.ToolResult{Text: fmt.Sprintf("目标 IP `%s` 已绑定到其他私网 IP，拒绝将其从其他实例迁走。", targetObj.IPAddress)}, nil
	}

	// 2. 查当前绑定的公网 IP
	var oldPubID string
	var oldPubIP string
	curItem, err := t.getPublicIPByPrivateIP(ctx, privID)
	if err != nil {
		return nil, fmt.Errorf("无法确认当前公网 IP，停止换绑以避免破坏现有绑定: %w", err)
	}
	if curItem != nil {
		if curItem.LifecycleState != "ASSIGNED" || curItem.PrivateIPID != privID {
			return nil, fmt.Errorf("当前公网 IP %s 的绑定状态无法确认 (lifecycleState=%q, privateIpId=%q)，停止换绑以避免覆盖在途操作", curItem.IPAddress, curItem.LifecycleState, curItem.PrivateIPID)
		}
		oldPubID = curItem.ID
		oldPubIP = curItem.IPAddress
	}

	if oldPubID == targetObj.ID {
		return &core.ToolResult{Text: fmt.Sprintf("预留 IP `%s` 已经绑定在本机网卡上，无需重复操作。", targetObj.IPAddress)}, nil
	}

	// 3. 解绑旧 IP
	if oldPubID != "" {
		unbindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, oldPubID)
		unbindResp, unbindBody, unbindErr := t.client.Do(ctx, http.MethodPut, unbindURL, map[string]string{"privateIpId": ""})
		if unbindErr != nil {
			// 网络错误不代表 OCI 没有执行请求；尝试幂等恢复旧绑定，避免把不确定状态留给用户。
			rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
			if restored {
				return nil, fmt.Errorf("解绑旧 IP %s 的请求结果不确定，已重新确认并恢复旧绑定: %w", oldPubIP, unbindErr)
			}
			return nil, fmt.Errorf("解绑旧 IP %s 的请求结果不确定，恢复旧绑定失败 (%s)；请立即检查主网卡公网 IP: %w", oldPubIP, rollbackDetail, unbindErr)
		}
		if unbindResp.StatusCode != http.StatusOK {
			if unbindResp.StatusCode >= http.StatusInternalServerError {
				rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
				if restored {
					return nil, fmt.Errorf("解绑旧 IP %s 返回 HTTP %d，已重新确认并恢复旧绑定: %s", oldPubIP, unbindResp.StatusCode, strings.TrimSpace(string(unbindBody)))
				}
				return nil, fmt.Errorf("解绑旧 IP %s 返回 HTTP %d，恢复旧绑定失败 (%s)；请立即检查主网卡公网 IP: %s", oldPubIP, unbindResp.StatusCode, rollbackDetail, strings.TrimSpace(string(unbindBody)))
			}
			return nil, fmt.Errorf("解绑旧 IP %s 失败 (HTTP %d): %s；已停止换绑", oldPubIP, unbindResp.StatusCode, strings.TrimSpace(string(unbindBody)))
		}
		if _, waitErr := t.waitPublicIPLifecycle(context.Background(), oldPubID, "AVAILABLE", ""); waitErr != nil {
			rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
			if restored {
				return nil, fmt.Errorf("旧 IP %s 解绑未能确认 (%v)，已恢复旧绑定", oldPubIP, waitErr)
			}
			return nil, fmt.Errorf("旧 IP %s 解绑未能确认 (%v)，恢复旧绑定失败 (%s)；请立即检查主网卡公网 IP", oldPubIP, waitErr, rollbackDetail)
		}
	}

	// 4. 绑定新 IP
	if ctxErr := ctx.Err(); ctxErr != nil {
		if oldPubID == "" {
			return nil, fmt.Errorf("换绑已取消，新 IP 尚未绑定: %w", ctxErr)
		}
		rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
		if restored {
			return nil, fmt.Errorf("换绑已取消，新 IP 尚未绑定，已恢复旧 IP %s: %w", oldPubIP, ctxErr)
		}
		return nil, fmt.Errorf("换绑已取消，新 IP 尚未绑定；恢复旧 IP %s 失败 (%s)，请立即检查主网卡公网 IP: %w", oldPubIP, rollbackDetail, ctxErr)
	}
	bindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, targetObj.ID)
	bResp, _, err := t.client.Do(ctx, http.MethodPut, bindURL, map[string]string{"privateIpId": privID})
	var bindErr error
	bindUncertain := err != nil
	if err != nil {
		bindErr = err
	} else if bResp.StatusCode != http.StatusOK {
		bindErr = fmt.Errorf("HTTP %d", bResp.StatusCode)
		bindUncertain = bResp.StatusCode >= http.StatusInternalServerError
	}
	assignmentConfirmed := false
	if bindErr != nil && bindUncertain {
		// 网络错误和 5xx 都不能证明 OCI 没有执行 PUT。先观察目标资源的
		// 最终状态，避免在异步绑定仍进行时抢先把旧 IP 绑回去。
		terminal, stateErr := t.waitPublicIPLifecycle(context.Background(), targetObj.ID, "ASSIGNED", privID)
		if stateErr == nil {
			assignmentConfirmed = true
			bindErr = nil
		} else if !terminal {
			return nil, fmt.Errorf("新 IP %s 的绑定请求结果不确定，且无法确认最终状态 (%v)；未执行旧 IP 回滚以避免冲突，请检查 OCI 状态后再操作", targetObj.IPAddress, stateErr)
		}
	}
	if bindErr != nil {
		if oldPubID == "" {
			return nil, fmt.Errorf("绑定新预留 IP %s 失败: %w", targetObj.IPAddress, bindErr)
		}
		rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
		if restored {
			return nil, fmt.Errorf("绑定新 IP %s 失败，已恢复旧 IP %s: %w", targetObj.IPAddress, oldPubIP, bindErr)
		}
		return nil, fmt.Errorf("绑定新 IP %s 失败，恢复旧 IP %s 也失败 (%s)；请立即检查主网卡公网 IP。新绑定错误: %w", targetObj.IPAddress, oldPubIP, rollbackDetail, bindErr)
	}
	// Once OCI accepts an update, caller cancellation cannot cancel the cloud
	// operation. Reconcile its final state independently before reporting it.
	if !assignmentConfirmed {
		terminal, stateErr := t.waitPublicIPLifecycle(context.Background(), targetObj.ID, "ASSIGNED", privID)
		if stateErr != nil {
			if terminal && oldPubID != "" {
				rollbackDetail, restored := t.restorePublicIP(oldPubID, privID)
				if restored {
					return nil, fmt.Errorf("新 IP %s 的异步绑定失败 (%v)，已恢复旧 IP %s", targetObj.IPAddress, stateErr, oldPubIP)
				}
				return nil, fmt.Errorf("新 IP %s 的异步绑定失败 (%v)，恢复旧 IP %s 也失败 (%s)；请立即检查主网卡公网 IP", targetObj.IPAddress, stateErr, oldPubIP, rollbackDetail)
			}
			return nil, fmt.Errorf("OCI 已接受新 IP %s 的绑定请求，但尚未确认最终状态 (%v)；请先检查 OCI 状态，不要重复提交绑定", targetObj.IPAddress, stateErr)
		}
	}

	// 5. 若配置了 deleteOld，清理旧预留 IP
	deleteOldOK := false
	deleteOldError := ""
	if deleteOld && oldPubID != "" {
		if delErr := t.releaseReservedIP(context.Background(), oldPubID); delErr != nil {
			deleteOldError = delErr.Error()
		} else {
			deleteOldOK = true
		}
	}

	bindMsg := fmt.Sprintf("### OCI IP 热换绑成功\n- **新公网 IP**: `%s` (已生效并接入主网卡)\n- **旧公网 IP**: `%s`%s",
		targetObj.IPAddress, oldPubIP, func() string {
			if deleteOldOK {
				return " (已自动释放删除)"
			}
			return ""
		}())
	if deleteOldError != "" {
		bindMsg += fmt.Sprintf("\n- **旧 IP 清理状态**: 新 IP 已绑定成功，但旧 IP 释放失败：%s", deleteOldError)
	}

	// 1. 同步更新 Cloudflare DNS 记录
	if t.cfConfig != nil && strings.TrimSpace(t.cfConfig.APIToken) != "" {
		targetRecord := strings.TrimSpace(t.cfConfig.SyncRecord)
		if targetRecord == "" {
			bindMsg += "\n- **Cloudflare DNS 未同步**: 警告：配置项 cloudflare.sync_record 未设置"
			slog.Warn("oci_ip_tool: cloudflare.sync_record 未设置，跳过 DNS 自动同步")
		} else if err := t.updateCloudflareDNS(ctx, targetObj.IPAddress); err != nil {
			bindMsg += fmt.Sprintf("\n- **Cloudflare DNS 同步失败**: %v（可随时对我说「重试同步 DNS」再次尝试更新）", err)
			slog.Error("oci_ip_tool: Cloudflare DNS 更新失败", "record", targetRecord, "ip", targetObj.IPAddress, "err", err)
		} else {
			bindMsg += fmt.Sprintf("\n- **Cloudflare DNS 同步**: 已自动将 `%s` 更新为新 IP `%s` (保持灰云/仅 DNS 模式，TTL 60s)", targetRecord, targetObj.IPAddress)
		}
	}

	// 2. 刷新本地静态订阅文件
	if _, lookErr := exec.LookPath("singbox-links"); lookErr == nil {
		exportCmd := exec.CommandContext(ctx, "/run/wrappers/bin/sudo", "singbox-links", "--export", targetObj.IPAddress)
		if exportOut, exportErr := exportCmd.CombinedOutput(); exportErr != nil {
			slog.Warn("oci_ip_tool: 刷新本地订阅文件失败", "err", exportErr, "out", string(exportOut))
		} else {
			bindMsg += "\n- **本地订阅文件**: 已自动刷新并生成包含新 IP 的最新节点配置"
		}
	}

	// 3. 若系统部署了 singbox-links，换绑成功后自动生成并附带新节点的代理配置
	if _, lookErr := exec.LookPath("singbox-links"); lookErr == nil {
		if linkOutput, err := runSingboxLinks(ctx, targetObj.IPAddress); err == nil && linkOutput != "" {
			bindMsg += "\n\n" + FormatSingboxLinks(linkOutput)
		} else if err != nil {
			slog.Warn("oci_ip_tool: IP 已完成换绑，但 Sing-box 配置生成失败", "errorType", fmt.Sprintf("%T", err))
		}
	}

	return &core.ToolResult{
		Text: bindMsg,
	}, nil
}

func (t *OCIIPTool) restorePublicIP(publicIPID, privateIPID string) (string, bool) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
	defer cancel()
	current, err := t.getPublicIPByID(rollbackCtx, publicIPID)
	if err != nil {
		return fmt.Sprintf("检查旧 IP 当前状态失败: %v", err), false
	}
	if current == nil {
		return "旧预留 IP 不存在", false
	}
	if current.LifecycleState == "ASSIGNED" && current.PrivateIPID == privateIPID {
		return "", true
	}
	if current.LifecycleState == "ASSIGNING" && current.PrivateIPID == privateIPID {
		if _, err := t.waitPublicIPLifecycle(rollbackCtx, publicIPID, "ASSIGNED", privateIPID); err != nil {
			return fmt.Sprintf("旧 IP 正在恢复绑定但未能确认完成: %v", err), false
		}
		return "", true
	}
	if current.PrivateIPID != "" && current.PrivateIPID != privateIPID {
		return fmt.Sprintf("旧 IP 已绑定到其他私网地址 %s，拒绝抢占", current.PrivateIPID), false
	}
	if current.LifecycleState != "AVAILABLE" && current.LifecycleState != "UNASSIGNED" {
		if _, err := t.waitPublicIPLifecycle(rollbackCtx, publicIPID, "AVAILABLE", ""); err != nil {
			return fmt.Sprintf("旧 IP 尚未达到可恢复状态: %v", err), false
		}
	}
	url := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, publicIPID)
	resp, body, err := t.client.Do(rollbackCtx, http.MethodPut, url, map[string]string{"privateIpId": privateIPID})
	var requestErr error
	uncertain := err != nil
	if err != nil {
		requestErr = err
	} else if resp.StatusCode != http.StatusOK {
		requestErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
		uncertain = resp.StatusCode >= http.StatusInternalServerError
	}
	if requestErr != nil {
		if !uncertain {
			return requestErr.Error(), false
		}
		// A timeout or 5xx does not prove the rollback was rejected. Reconcile
		// the resource before reporting failure or allowing another operation.
		if _, stateErr := t.waitPublicIPLifecycle(rollbackCtx, publicIPID, "ASSIGNED", privateIPID); stateErr == nil {
			return "", true
		} else {
			return fmt.Sprintf("恢复绑定请求结果不确定 (%v)，状态核验失败 (%v)", requestErr, stateErr), false
		}
	}
	if _, err := t.waitPublicIPLifecycle(rollbackCtx, publicIPID, "ASSIGNED", privateIPID); err != nil {
		return err.Error(), false
	}
	return "", true
}
