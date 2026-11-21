package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"daidai/internal/core"
)

func (t *OCIIPTool) handleRoll(ctx context.Context, prefixes []string, maxRisk int, noAbuser bool) (*core.ToolResult, error) {
	maxAttempts := 15
	var rejected []string
	var prefixOctets [][]int
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) != prefix || prefix == "" {
			return nil, fmt.Errorf("IP 前缀不能为空或包含首尾空白")
		}
		parts := strings.Split(prefix, ".")
		if parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		if len(parts) > 4 {
			return nil, fmt.Errorf("IP 前缀最多包含四段 IPv4 数字")
		}
		octets := make([]int, 0, len(parts))
		for _, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != part {
				return nil, fmt.Errorf("IP 前缀必须由 0 到 255 的 IPv4 数字段组成")
			}
			octets = append(octets, n)
		}
		prefixOctets = append(prefixOctets, octets)
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		displayName := fmt.Sprintf("reserved-ip-%d-%d", time.Now().UnixNano(), attempt)
		createURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps", t.client.region)
		createBody := map[string]string{
			"compartmentId": t.client.compartmentID,
			"lifetime":      "RESERVED",
			"displayName":   displayName,
		}

		resp, b, err := t.client.Do(ctx, http.MethodPost, createURL, createBody)
		if err != nil {
			cleaned, reconcileErr := t.cleanupReservationByName(displayName)
			if reconcileErr != nil {
				return nil, fmt.Errorf("OCI 创建预留 IP 请求结果不确定；按名称回查/清理失败 (%v)，请检查 OCI 控制台: %w", reconcileErr, err)
			}
			if cleaned {
				return nil, fmt.Errorf("OCI 创建预留 IP 请求结果不确定；已按名称回查并清理候选资源，请检查 OCI 状态后重试: %w", err)
			}
			return nil, fmt.Errorf("OCI 创建预留 IP 请求结果不确定；回查未发现同名资源，为避免重复创建已停止重试，请检查 OCI 控制台: %w", err)
		}
		if resp.StatusCode >= http.StatusInternalServerError {
			cleaned, reconcileErr := t.cleanupReservationByName(displayName)
			if reconcileErr != nil {
				return nil, fmt.Errorf("OCI 创建预留 IP 返回 HTTP %d，结果不确定；回查/清理失败 (%v)，请检查 OCI 控制台: %s", resp.StatusCode, reconcileErr, summarizeOCIResponse(b))
			}
			if cleaned {
				return nil, fmt.Errorf("OCI 创建预留 IP 返回 HTTP %d；已按名称回查并清理候选资源，请检查 OCI 状态后重试: %s", resp.StatusCode, summarizeOCIResponse(b))
			}
			return nil, fmt.Errorf("OCI 创建预留 IP 返回 HTTP %d，结果不确定；回查未发现同名资源，为避免重复创建已停止重试，请检查 OCI 控制台: %s", resp.StatusCode, summarizeOCIResponse(b))
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("OCI 创建预留 IP 失败 (HTTP %d): %s", resp.StatusCode, summarizeOCIResponse(b))
		}

		var created ociPublicIPItem
		if err := json.Unmarshal(b, &created); err != nil || created.ID == "" {
			cleaned, reconcileErr := t.cleanupReservationByName(displayName)
			if reconcileErr != nil {
				return nil, fmt.Errorf("OCI 创建预留 IP 成功但响应缺少资源 ID；回查/清理失败 (%v)，请检查 OCI 控制台", reconcileErr)
			}
			if cleaned {
				return nil, fmt.Errorf("OCI 创建预留 IP 成功但响应缺少资源 ID；已按名称回查并清理候选资源，请检查 OCI 状态")
			}
			return nil, fmt.Errorf("OCI 创建预留 IP 成功但响应缺少资源 ID；回查未发现同名资源，无法确认其状态，请检查 OCI 控制台")
		}
		if created.IPAddress == "" || !isPublicIPAvailable(&created) {
			_, stateErr := t.waitPublicIPLifecycle(ctx, created.ID, "AVAILABLE", "")
			if stateErr == nil {
				var item *ociPublicIPItem
				item, stateErr = t.getPublicIPByID(ctx, created.ID)
				if item == nil && stateErr == nil {
					stateErr = fmt.Errorf("资源已消失")
				}
				if item != nil {
					created = *item
				}
				if stateErr == nil && (!isPublicIPAvailable(&created) || created.IPAddress == "") {
					stateErr = fmt.Errorf("地址或可用状态未确认")
				}
			}
			if stateErr != nil || created.IPAddress == "" {
				if cleanupErr := t.releaseRollCandidate(created.ID); cleanupErr != nil {
					return nil, fmt.Errorf("候选分配状态无法确认（%v），资源 %s 清理失败: %w", stateErr, created.ID, cleanupErr)
				}
				return nil, fmt.Errorf("OCI 已接受创建，但地址分配未就绪；本轮候选已释放（%v）", stateErr)
			}
		}
		candidateIP := created.IPAddress

		// 检查前缀
		matchedPrefix := len(prefixes) == 0
		for _, p := range prefixOctets {
			if matchesIPv4Prefix(candidateIP, p) {
				matchedPrefix = true
				break
			}
		}

		reason := "前缀不匹配"
		incomplete := false
		if matchedPrefix {
			rep := t.checkIPReputation(ctx, candidateIP, maxRisk, noAbuser)
			if err := ctx.Err(); err != nil {
				if cleanupErr := t.releaseRollCandidate(created.ID); cleanupErr != nil {
					return nil, fmt.Errorf("刷取已中断，候选 IP %s 清理失败: %w", candidateIP, cleanupErr)
				}
				return nil, err
			}
			reason, incomplete = strings.Join(rep.Reasons, "；"), rep.Incomplete
			if rep.Passed {
				if err := ctx.Err(); err != nil {
					if cleanupErr := t.releaseRollCandidate(created.ID); cleanupErr != nil {
						return nil, fmt.Errorf("刷取已取消，候选 IP %s 清理失败: %w", candidateIP, cleanupErr)
					}
					return nil, err
				}

				fraud := "未筛选"
				if rep.FraudScore >= 0 {
					fraud = fmt.Sprintf("%d/100", rep.FraudScore)
				}
				proxy := "未筛选"
				if rep.ProxyKnown {
					proxy = "否"
				}
				history := "无"
				if len(rejected) > 0 {
					history = strings.Join(rejected, "；")
				}
				statusLine := "- 状态：未绑定；尚未做连通探测；当前绑定未改动。"
				return &core.ToolResult{Text: fmt.Sprintf("### OCI 已申请并保留候选预留 IP\n- 候选：%s\n- OCI 创建成功：%d 次；已释放此前候选：%d 个\n- Scamalytics 评分：%s；公开代理标记：%s\n%s\n- 淘汰记录：%s\n画像与连通诊断仅供参考，不保证特定业务长期可用。需要换绑时请明确指定此 IP。",
					candidateIP, attempt, len(rejected), fraud, proxy, statusLine, history)}, nil
			}
		}

		// 不符合要求：立即删除释放该候选 IP
		if err := t.releaseRollCandidate(created.ID); err != nil {
			return nil, fmt.Errorf("候选 IP %s 未通过筛选且释放失败，可能继续产生费用: %w", candidateIP, err)
		}
		rejected = append(rejected, fmt.Sprintf("第 %d 个：%s", attempt, reason))
		slog.Info("OCI 候选已释放", "attempt", attempt, "reason", reason, "observation_incomplete", incomplete)
		if incomplete {
			return &core.ToolResult{Text: fmt.Sprintf("OCI 创建成功 %d 次，本轮候选已全部释放，当前绑定未改动。因筛选数据不可用已停止，不继续消耗候选。记录：%s。不能据此推断地址池枯竭或候选风险高。", attempt, strings.Join(rejected, "；"))}, nil
		}
		if attempt < maxAttempts {
			if err := waitContext(ctx, 2*time.Second); err != nil {
				return nil, err
			}
		}
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("OCI 创建成功 %d 次，本轮候选已全部释放，未找到满足指定筛选条件的候选；当前绑定未改动。记录：%s。不代表地址池枯竭，也不保证再次申请会获得不同地址。", maxAttempts, strings.Join(rejected, "；")),
	}, nil
}

func matchesIPv4Prefix(address string, prefix []int) bool {
	ip := net.ParseIP(address).To4()
	if ip == nil || len(prefix) > len(ip) {
		return false
	}
	for i, octet := range prefix {
		if int(ip[i]) != octet {
			return false
		}
	}
	return true
}

// releaseRollCandidate cleans up a resource created by this roll operation.
// Once creation succeeded, cancellation must not strand a billable candidate.
func (t *OCIIPTool) releaseRollCandidate(publicIPID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
	defer cancel()
	return t.releaseReservedIP(ctx, publicIPID)
}
