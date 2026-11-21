package tool

import (
	"context"
	"fmt"
	"net"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

func (t *OCIIPTool) handleStatus(ctx context.Context) (*core.ToolResult, error) {
	privID, err := t.ResolvePrivateIPID(ctx)
	if err != nil {
		return nil, err
	}

	ips, err := t.listReservedPublicIPs(ctx)
	if err != nil {
		return nil, err
	}

	var curPublicIP string
	if privID != "" {
		curItem, err := t.getPublicIPByPrivateIP(ctx, privID)
		if err != nil {
			return nil, err
		}
		if curItem != nil {
			curPublicIP = curItem.IPAddress
		}
	}

	var lines []string
	lines = append(lines, "### OCI 预留 IP 状态")
	lines = append(lines, fmt.Sprintf("- **区域**: `%s`", t.client.region))
	if curPublicIP != "" {
		lines = append(lines, fmt.Sprintf("- **当前公网 IP**: `%s` (已绑定主网卡)", curPublicIP))
	} else {
		lines = append(lines, "- **当前公网 IP**: `未绑定`")
	}

	lines = append(lines, fmt.Sprintf("\n**预留 IP 列表 (%d 个)**", len(ips)))
	if len(ips) == 0 {
		lines = append(lines, "- 无预留 IP")
	} else {
		for _, item := range ips {
			stateTag := "闲置未绑定 (建议清理)"
			switch item.LifecycleState {
			case "AVAILABLE", "UNASSIGNED":
			case "ASSIGNED":
				stateTag = "已绑定但缺少私网 IP 标识"
				if item.PrivateIPID != "" {
					if item.PrivateIPID == privID {
						stateTag = "已绑定本机"
					} else {
						stateTag = "已绑定其他实例"
					}
				}
			default:
				if item.LifecycleState == "" {
					stateTag = "未知状态 (拒绝自动操作)"
				} else {
					stateTag = fmt.Sprintf("状态变更中: %s", item.LifecycleState)
				}
			}
			lines = append(lines, fmt.Sprintf("- `%s`: %s (%s)", item.IPAddress, stateTag, item.DisplayName))
		}
	}

	return &core.ToolResult{Text: strings.Join(lines, "\n")}, nil
}

func (t *OCIIPTool) handleCleanup(ctx context.Context, targetIP string) (*core.ToolResult, error) {
	ips, err := t.listReservedPublicIPs(ctx)
	if err != nil {
		return nil, err
	}

	var released []string
	var releaseFailures []string
	for _, item := range ips {
		if err := ctx.Err(); err != nil {
			releaseFailures = append(releaseFailures, "清理操作已取消，未处理完所有闲置 IP")
			break
		}
		if targetIP == "" || targetIP == item.IPAddress {
			if isPublicIPAvailable(&item) ||
				(item.PrivateIPID == "" && (item.LifecycleState == "TERMINATING" || item.LifecycleState == "TERMINATED")) {
				if err := t.releaseReservedIP(ctx, item.ID); err == nil {
					released = append(released, item.IPAddress)
				} else {
					releaseFailures = append(releaseFailures, fmt.Sprintf("%s: %v", item.IPAddress, err))
				}
			}
		}
	}

	if len(released) == 0 {
		if len(releaseFailures) > 0 {
			return &core.ToolResult{Text: "以下闲置 IP 释放失败，可能继续产生费用：\n- " + strings.Join(releaseFailures, "\n- ")}, nil
		}
		if targetIP != "" {
			return &core.ToolResult{Text: fmt.Sprintf("未找到待释放的未绑定预留 IP `%s` (可能已绑定或不存在)", targetIP)}, nil
		}
		return &core.ToolResult{Text: "当前没有未绑定的闲置预留 IP，无需清理。"}, nil
	}

	text := fmt.Sprintf("### 预留 IP 清理完成\n- **共释放**: `%d` 个闲置 IP\n- **已清理**: `%s`", len(released), strings.Join(released, ", "))
	if len(releaseFailures) > 0 {
		text += "\n- **释放失败** (可能继续产生费用): " + strings.Join(releaseFailures, "; ")
	}
	return &core.ToolResult{Text: text}, nil
}

func (t *OCIIPTool) handleCheck(ctx context.Context, targetIP string, requestedPort *int) (*core.ToolResult, error) {
	if targetIP == "" {
		privID, err := t.ResolvePrivateIPID(ctx)
		if err != nil {
			return nil, err
		}
		curItem, err := t.getPublicIPByPrivateIP(ctx, privID)
		if err != nil {
			return nil, err
		}
		if curItem != nil {
			targetIP = curItem.IPAddress
		}
	}
	if targetIP == "" {
		return nil, fmt.Errorf("未指定检测 IP 且未能获取当前公网 IP")
	}
	parsed := net.ParseIP(targetIP)
	if parsed == nil || !parsed.IsGlobalUnicast() || parsed.IsPrivate() {
		return nil, fmt.Errorf("目标不是有效的公网单播 IP 地址")
	}
	targetIP = parsed.String()
	if requestedPort == nil {
		ips, err := t.listReservedPublicIPs(ctx)
		if err != nil {
			return nil, err
		}
		owned := false
		for i := range ips {
			if ips[i].IPAddress == targetIP {
				owned = true
				break
			}
		}
		if !owned {
			return nil, fmt.Errorf("目标 IP 不是当前 OCI 配置区域内的预留 IP；扫描外部 IP 时必须指定 port")
		}
	}
	rep := t.checkIPReputation(ctx, targetIP, 50, true)
	ips, listErr := t.listReservedPublicIPs(ctx)
	var connectivity string
	var targetItem *ociPublicIPItem
	for i := range ips {
		if ips[i].IPAddress == targetIP {
			targetItem = &ips[i]
			break
		}
	}
	if listErr != nil {
		rep.Reasons = append(rep.Reasons, "OCI 绑定状态查询失败："+listErr.Error())
	}

	event := entity.SensoryEventFromContext(ctx)
	if targetItem != nil && (targetItem.PrivateIPID == "" || targetItem.LifecycleState == "AVAILABLE" || targetItem.LifecycleState == "UNASSIGNED") {
		// 目标是未绑定的候选预留 IP：权限校验并触发零中断辅助私网测试
		if t.immune == nil || event == nil {
			return &core.ToolResult{Text: "权限校验不可用，拒绝临时绑定候选 IP 进行探测"}, nil
		}
		if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
			return &core.ToolResult{Text: "权限拦截：探测未绑定候选 IP 需临时创建辅助私网，仅限搭档或协作者发起"}, nil
		}
		probeText, probeErr := t.probeCandidateConnectivity(ctx, targetItem, requestedPort)
		if probeErr != nil {
			connectivity = probeText
			if connectivity == "" {
				connectivity = fmt.Sprintf("临时辅助私网测试失败: %v", probeErr)
			} else {
				connectivity += "\n临时辅助私网测试未完整成功：" + probeErr.Error()
			}
			rep.Reasons = append(rep.Reasons, "连通诊断异常: "+probeErr.Error())
		} else {
			connectivity = probeText
		}
	} else {
		// 对已绑定的 OCI IP，按 OCI 私网地址筛选本机监听端口；外部 IP 仅允许指定端口诊断。
		bindDesc := "外部目标直接探测"
		privateIP := ""
		if targetItem != nil && targetItem.PrivateIPID != "" {
			bindDesc = "当前绑定实例"
			privateItem, privateErr := t.getPrivateIPByID(ctx, targetItem.PrivateIPID)
			if privateErr != nil {
				connectivity = "读取 OCI 私网地址失败：" + privateErr.Error()
				rep.Reasons = append(rep.Reasons, "连通诊断异常: "+privateErr.Error())
			} else if privateItem == nil {
				connectivity = "OCI 未找到该私网 IP；不能将本机监听端口对应到目标公网 IP"
			} else {
				vnicInfo, vnicErr := t.resolveVNICInfo(ctx)
				if vnicErr != nil {
					connectivity = "无法确认目标私网 IP 是否属于本机网卡：" + vnicErr.Error()
					rep.Reasons = append(rep.Reasons, "连通诊断异常: "+vnicErr.Error())
				} else if privateItem.VnicID == vnicInfo.vnicID {
					privateIP = privateItem.IPAddress
				} else if requestedPort == nil {
					connectivity = "该预留 IP 绑定到其他 VNIC；本机监听端口不适用于它，请指定实际服务端口"
				}
			}
		}
		if connectivity == "" {
			portReport, probeErr := t.probeListeningPorts(ctx, rep.IP, privateIP, requestedPort)
			if probeErr != nil {
				connectivity = probeErr.Error()
				rep.Reasons = append(rep.Reasons, "连通诊断异常: "+probeErr.Error())
			} else {
				connectivity = bindDesc + "\n" + portReport
			}
		}
	}

	statusText := "满足画像参考条件（风险分≤50、非公开代理）"
	if !rep.Passed {
		statusText = "不满足画像参考条件"
	}

	fraudStr := "未知"
	if rep.FraudScore >= 0 {
		fraudStr = fmt.Sprintf("%d/100", rep.FraudScore)
	}
	if rep.Incomplete {
		statusText = "画像未知或不完整"
	}
	proxyStr := "未知"
	if rep.ProxyKnown {
		proxyStr = "否"
	}
	if rep.IsProxy {
		proxyStr = "是 (风险代理)"
	}

	lines := []string{
		fmt.Sprintf("### OCI IP 画像与独立连通诊断: `%s` [%s]", rep.IP, statusText),
		fmt.Sprintf("- **第三方标记属地**: `%s %s`", rep.Country, rep.City),
		fmt.Sprintf("- **ASN 运营商**: `%s`", rep.ASN),
		fmt.Sprintf("- **公开代理标记**: `%s`", proxyStr),
		fmt.Sprintf("- **Scamalytics 欺诈分**: `%s`", fraudStr),
		fmt.Sprintf("- **国内端口诊断**: %s", connectivity),
	}
	if len(rep.Reasons) > 0 {
		lines = append(lines, fmt.Sprintf("- **评估结论**: %s", strings.Join(rep.Reasons, "；")))
	}

	return &core.ToolResult{Text: strings.Join(lines, "\n")}, nil
}
