package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

type globalpingMeasurement struct {
	Status  string `json:"status"`
	Results []struct {
		Result struct {
			Status        string `json:"status"`
			FailureSource string `json:"failureSource"`
			Stats         *struct {
				RCV *float64 `json:"rcv"`
				AVG *float64 `json:"avg"`
			} `json:"stats"`
		} `json:"result"`
	} `json:"results"`
}

const (
	globalpingPollInterval = time.Second
	globalpingMaxPolls     = 20
)

type probeResult struct {
	Protocol     string
	Port         int
	Summary      string
	Reachable    int
	Completed    int
	Total        int
	AvgLatencyMs int
	Err          error
}

func checkGlobalpingTCPProbe(ctx context.Context, client *http.Client, ip string, port int) probeResult {
	res := probeResult{Protocol: "TCP", Port: port}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	protoLabel := fmt.Sprintf("TCP %d", port)
	measurementOptions := map[string]any{"packets": 2, "protocol": "TCP", "port": port}

	requestBody := map[string]any{
		"type":               "ping",
		"target":             ip,
		"locations":          []map[string]any{{"country": "CN", "limit": 3}},
		"measurementOptions": measurementOptions,
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		res.Summary = fmt.Sprintf("%s：国内探针请求无法创建", protoLabel)
		res.Err = fmt.Errorf("编码 Globalping 探测请求失败: %w", err)
		return res
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.globalping.io/v1/measurements", bytes.NewReader(body))
	if err != nil {
		res.Summary = fmt.Sprintf("%s：国内探针请求无法创建", protoLabel)
		res.Err = fmt.Errorf("创建 Globalping 探测请求失败: %w", err)
		return res
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "daidai/oci-ip-check")
	response, err := client.Do(request)
	if err != nil {
		res.Summary = fmt.Sprintf("%s：国内探针服务不可用", protoLabel)
		res.Err = fmt.Errorf("globalping 未能接受国内连通探测: %w", err)
		return res
	}
	defer func() { _ = response.Body.Close() }()
	var created struct {
		ID string `json:"id"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		res.Summary = fmt.Sprintf("%s：国内探针创建失败 (HTTP %d)", protoLabel, response.StatusCode)
		res.Err = fmt.Errorf("globalping 创建测量返回 HTTP %d", response.StatusCode)
		return res
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&created); err != nil || created.ID == "" {
		res.Summary = fmt.Sprintf("%s：国内探针创建失败", protoLabel)
		res.Err = fmt.Errorf("globalping 未返回有效测量任务")
		return res
	}

	for attempt := 0; attempt < globalpingMaxPolls; attempt++ {
		if err := waitContext(ctx, globalpingPollInterval); err != nil {
			if ctx.Err() == context.Canceled {
				res.Summary = fmt.Sprintf("%s：国内连通探测已中断", protoLabel)
				res.Err = fmt.Errorf("国内连通探测已中断")
				return res
			}
			res.Summary = fmt.Sprintf("%s：国内连通探测超时", protoLabel)
			res.Err = fmt.Errorf("globalping 测量在 20 秒内未完成")
			return res
		}
		resultURL := "https://api.globalping.io/v1/measurements/" + url.PathEscape(created.ID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
		if err != nil {
			res.Summary = fmt.Sprintf("%s：国内探针请求无法创建", protoLabel)
			res.Err = fmt.Errorf("创建 Globalping 结果请求失败: %w", err)
			return res
		}
		req.Header.Set("User-Agent", "daidai/oci-ip-check")
		resp, err := client.Do(req)
		if err != nil {
			res.Summary = fmt.Sprintf("%s：国内探针服务不可用", protoLabel)
			res.Err = fmt.Errorf("读取 Globalping 测量结果失败: %w", err)
			return res
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			res.Summary = fmt.Sprintf("%s：国内探针结果读取失败 (HTTP %d)", protoLabel, resp.StatusCode)
			res.Err = fmt.Errorf("读取 Globalping 测量结果返回 HTTP %d", resp.StatusCode)
			return res
		}
		var measurement globalpingMeasurement
		err = json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&measurement)
		_ = resp.Body.Close()
		if err != nil {
			res.Summary = fmt.Sprintf("%s：国内探针结果无效", protoLabel)
			res.Err = fmt.Errorf("解析 Globalping 测量结果失败: %w", err)
			return res
		}
		if measurement.Status == "in-progress" || measurement.Status == "in_progress" {
			continue
		}
		if measurement.Status != "finished" || len(measurement.Results) == 0 {
			res.Summary = fmt.Sprintf("%s：国内探针结果无效", protoLabel)
			res.Err = fmt.Errorf("globalping 测量已结束但没有有效探测结果")
			return res
		}

		var reachable int
		completed := 0
		var totalLatency float64
		for _, result := range measurement.Results {
			stats := result.Result.Stats
			if result.Result.Status != "finished" || stats == nil || stats.RCV == nil {
				continue
			}
			completed++
			if *stats.RCV > 0 {
				reachable++
				if stats.AVG != nil {
					totalLatency += *stats.AVG
				}
			}
		}
		res.Reachable = reachable
		res.Completed = completed
		res.Total = len(measurement.Results)
		if reachable > 0 {
			res.AvgLatencyMs = int(totalLatency / float64(reachable))
		}

		if reachable == 0 {
			if completed == 0 {
				res.Summary = fmt.Sprintf("%s：探针未提供有效回包统计（有效节点 0/%d），结果不确定", protoLabel, len(measurement.Results))
				res.Err = fmt.Errorf("探针没有返回有效测量统计")
				return res
			}
			res.Summary = fmt.Sprintf("未观察到 %s 响应（有效节点 %d/%d）；可能是端口未监听、规则过滤、路由或探针问题，不能据此判断被墙", protoLabel, completed, len(measurement.Results))
			res.Err = fmt.Errorf("探测节点未收到响应；不能区分端口未监听、过滤、路由或探针问题")
			return res
		}
		probeCount := fmt.Sprintf("%d/%d 有效节点响应", reachable, completed)
		if completed != len(measurement.Results) {
			probeCount += fmt.Sprintf("（仅 %d/%d 个探针完成）", completed, len(measurement.Results))
		}
		if res.AvgLatencyMs > 0 {
			res.Summary = fmt.Sprintf("%s：%s (均延 %dms)", protoLabel, probeCount, res.AvgLatencyMs)
		} else {
			res.Summary = fmt.Sprintf("%s：%s", protoLabel, probeCount)
		}
		return res
	}
	if err := ctx.Err(); err != nil {
		res.Summary = fmt.Sprintf("%s：国内连通探测已中断", protoLabel)
		res.Err = fmt.Errorf("国内连通探测已中断: %w", err)
		return res
	}
	res.Summary = fmt.Sprintf("%s：国内连通探测未完成", protoLabel)
	res.Err = fmt.Errorf("globalping 测量在等待期限内未完成")
	return res
}

func checkGlobalpingTCP(ctx context.Context, client *http.Client, ip string, port int) (string, int, error) {
	res := checkGlobalpingTCPProbe(ctx, client, ip, port)
	return res.Summary, res.AvgLatencyMs, res.Err
}
func (t *OCIIPTool) probeCandidateConnectivity(ctx context.Context, candidateItem *ociPublicIPItem, requestedPort *int) (details string, resultErr error) {
	if t.hostIP == nil {
		return "", fmt.Errorf("本机网卡地址管理器未初始化，已停止候选 IP 探测")
	}

	vnicInfo, err := t.resolveVNICInfo(ctx)
	if err != nil {
		return "", fmt.Errorf("获取主网卡信息失败: %w", err)
	}

	select {
	case t.operationMu <- struct{}{}:
		defer func() { <-t.operationMu }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// The candidate snapshot may have gone stale while this probe waited for
	// another OCI mutation. Recheck under the shared operation lock before
	// creating any temporary VNIC resources.
	currentCandidate, err := t.getPublicIPByID(ctx, candidateItem.ID)
	if err != nil {
		return "", fmt.Errorf("重新确认候选公网 IP 状态失败，已停止临时探测: %w", err)
	}
	if !isPublicIPAvailable(currentCandidate) {
		state := publicIPStateLabel(currentCandidate)
		if currentCandidate != nil && currentCandidate.PrivateIPID != "" {
			return "", fmt.Errorf("候选公网 IP 已绑定到其他私网 IP，已停止临时探测 (state=%s)", state)
		}
		return "", fmt.Errorf("候选公网 IP 已不存在或状态不再可用，已停止临时探测 (state=%s)", state)
	}
	if currentCandidate.IPAddress != candidateItem.IPAddress {
		return "", fmt.Errorf("候选公网 IP 地址与原始查询结果不一致，已停止临时探测")
	}
	candidateItem = currentCandidate

	// 清理可能残留的历史 probe 辅助私网 IP
	if err := t.cleanupOrphanedProbePrivateIPs(ctx, vnicInfo.vnicID, vnicInfo.ifaceName, vnicInfo.maskBits); err != nil {
		return "", fmt.Errorf("上次探测残留未能安全清理，已停止本次检查: %w", err)
	}

	// 1. 在 OCI 创建测试用辅助私网 IP
	displayName := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	secPriv, err := t.createSecondaryPrivateIP(ctx, vnicInfo.vnicID, displayName)
	if err != nil {
		return "", fmt.Errorf("创建辅助私网 IP 失败: %w", err)
	}
	secPrivID := secPriv.ID
	secPrivIP := secPriv.IPAddress
	bindingAttempted := false
	bindingVerified := false

	// Check must leave the reserved candidate intact. Only the temporary
	// public-to-private association and secondary private IP are cleaned up.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()

		if bindingAttempted {
			current, stateErr := t.getPublicIPByID(cleanupCtx, candidateItem.ID)
			if stateErr != nil || current == nil {
				if stateErr == nil {
					stateErr = fmt.Errorf("候选公网 IP 已不存在")
				}
				cleanupErr := fmt.Errorf("无法确认候选公网 IP 的临时绑定归属；为避免解绑其他映射，保留辅助私网 IP %s 与本机地址 %s: %w", secPrivID, secPrivIP, stateErr)
				resultErr = errors.Join(resultErr, cleanupErr)
				details += "\n临时绑定归属无法确认，辅助资源已保留；请先执行 status 核对，未删除候选 IP。"
				return
			}
			if current.PrivateIPID != "" && current.PrivateIPID != secPrivID {
				cleanupErr := fmt.Errorf("候选公网 IP 已绑定到其他私网 IP %s；为避免破坏现有映射，保留辅助私网 IP %s 与本机地址 %s", current.PrivateIPID, secPrivID, secPrivIP)
				resultErr = errors.Join(resultErr, cleanupErr)
				details += "\n候选公网 IP 已指向其他私网，未执行解绑；辅助资源已保留，请先执行 status 核对。"
				return
			}
			if current.PrivateIPID == "" && !bindingVerified {
				cleanupErr := fmt.Errorf("绑定请求结果不确定且 OCI 尚未确认本次映射；为避免迟到的绑定指向已删除资源，保留辅助私网 IP %s 与本机地址 %s", secPrivID, secPrivIP)
				resultErr = errors.Join(resultErr, cleanupErr)
				details += "\nOCI 尚未确认临时绑定状态，辅助资源已保留；请先执行 status 核对，未删除候选 IP。"
				return
			}
			if current.PrivateIPID == secPrivID {
				if unbindErr := t.unbindPublicIP(cleanupCtx, candidateItem.ID); unbindErr != nil {
					slog.Warn("oci_ip_tool: 临时探测 IP 解绑请求失败，继续核对 OCI 最终状态", "errorType", fmt.Sprintf("%T", unbindErr))
				}
			}
		}
		if _, waitErr := t.waitPublicIPLifecycle(cleanupCtx, candidateItem.ID, "AVAILABLE", ""); waitErr != nil {
			cleanupErr := fmt.Errorf("确认候选公网 IP 已从辅助私网解绑失败；为避免破坏仍在使用的映射，保留辅助私网 IP %s 与本机地址 %s: %w", secPrivID, secPrivIP, waitErr)
			resultErr = errors.Join(resultErr, cleanupErr)
			details += "\n临时绑定清理未能确认，候选及辅助私网映射均保留，请先执行 status 核对；未删除候选 IP。"
			return
		}

		var cleanupErrs []error
		if err := t.deleteSecondaryPrivateIP(cleanupCtx, secPrivID); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("删除辅助私网 IP %s 失败: %w", secPrivID, err))
		}
		if err := t.hostIP.DelIP(cleanupCtx, secPrivIP, vnicInfo.maskBits, vnicInfo.ifaceName); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("从网卡 %s 删除临时地址 %s 失败: %w", vnicInfo.ifaceName, secPrivIP, err))
		}
		if cleanupErr := errors.Join(cleanupErrs...); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
			details += "\n临时绑定已确认解除，但辅助资源清理不完整；候选公网 IP 仍保留，请检查 OCI 状态。"
		}
	}()

	// 2. 本机网卡添加辅助私网 IP
	if err := t.hostIP.AddIP(ctx, secPrivIP, vnicInfo.maskBits, vnicInfo.ifaceName); err != nil {
		return "", fmt.Errorf("向本机网卡 %s 添加辅助私网 IP %s 失败: %w", vnicInfo.ifaceName, secPrivIP, err)
	}

	// 3. 将候选公网 IP 绑定到辅助私网 IP (绝对不影响主私网 IP 与当前公网 IP)
	bindingAttempted = true
	if err := t.bindPublicIP(ctx, candidateItem.ID, secPrivID); err != nil {
		return "", fmt.Errorf("候选公网 IP 临时绑定到辅助私网失败: %w", err)
	}

	// 等待候选公网 IP 到达 ASSIGNED
	if _, waitErr := t.waitPublicIPLifecycle(ctx, candidateItem.ID, "ASSIGNED", secPrivID); waitErr != nil {
		return "", fmt.Errorf("等待候选公网 IP 临时绑定生效失败: %w", waitErr)
	}
	bindingVerified = true

	// 留出 1.5 秒等待 OCI 虚拟路由器路由表同步生效
	if err := waitContext(ctx, 1500*time.Millisecond); err != nil {
		return "", err
	}

	// 4. 读取本机实际监听端口，并从国内探针逐个验证适用的 TCP 端口。
	portReport, reportErr := t.probeListeningPorts(ctx, candidateItem.IPAddress, secPrivIP, requestedPort)
	if reportErr != nil {
		return "", reportErr
	}
	details = "临时辅助私网测试（候选 IP 已绑定到网卡；主 IP 保持不变）\n" + portReport
	return details, nil
}
