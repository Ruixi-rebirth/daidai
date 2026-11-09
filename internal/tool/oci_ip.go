package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// OCIIPTool Oracle Cloud 预留公网 IP 管理与换绑工具
type OCIIPTool struct {
	client *OCIClient
	immune *entity.ImmuneSystem
}

func NewOCIIPTool(client *OCIClient, immune *entity.ImmuneSystem) *OCIIPTool {
	return &OCIIPTool{
		client: client,
		immune: immune,
	}
}

func (t *OCIIPTool) Name() string {
	return "oci_ip"
}

func (t *OCIIPTool) Description() string {
	return "Oracle Cloud (OCI) 预留公网 IP 管理与换绑工具。支持查看当前公网与预留 IP 列表 (status)、一键释放闲置预留 IP (cleanup)、按纯净度与欺诈分筛选刷取候选优质 IP (roll)、检测指定 IP 质量 (check) 以及将优质 IP 无损绑定至服务器 (bind)。换绑与清理等高危操作仅限核心搭档。"
}

func (t *OCIIPTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "执行动作：status (状态查询), cleanup (清理未绑定闲置IP), roll (筛选刷取优质IP), check (质量与国内连通检测), bind (换绑目标IP)",
				"enum":        []string{"status", "cleanup", "roll", "check", "bind"},
			},
			"ip": map[string]any{
				"type":        "string",
				"description": "可选目标公网 IP 地址 (用于 check, bind 或单个 cleanup)",
			},
			"delete_old": map[string]any{
				"type":        "boolean",
				"description": "换绑 (bind) 时是否在解绑旧预留 IP 后将其彻底释放删除 (默认 false)",
			},
			"prefix": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "刷取 (roll) 时期望匹配的 IP 前缀列表，例如 [\"138.2.\", \"146.56.\"]",
			},
			"max_risk": map[string]any{
				"type":        "integer",
				"description": "刷取 (roll) 时允许的最大 Scamalytics 欺诈风险评分 (0-100，越低越纯净，如 25)",
			},
			"no_abuser": map[string]any{
				"type":        "boolean",
				"description": "刷取 (roll) 时是否自动淘汰被标记为公开代理/高危节点的 IP (默认 true)",
			},
		},
		"required": []string{"action"},
	}
}

type ociIPArgs struct {
	Action    string   `json:"action"`
	IP        string   `json:"ip"`
	DeleteOld bool     `json:"delete_old"`
	Prefix    []string `json:"prefix"`
	MaxRisk   int      `json:"max_risk"`
	NoAbuser  *bool    `json:"no_abuser"`
}

// IPQualityReport 单个 IP 评估质量报告
type IPQualityReport struct {
	IP          string
	Passed      bool
	Reasons     []string
	Country     string
	City        string
	ASN         string
	IsProxy     bool
	FraudScore  int
	GFWStatus   string
	GFWLatency  int
}

// ResolvePrivateIPID 解析主私网 IP OCID
func (t *OCIIPTool) ResolvePrivateIPID(ctx context.Context) (string, error) {
	var vnics []imdsVnicMeta
	if err := FetchIMDS(ctx, "vnics", &vnics); err == nil && len(vnics) > 0 {
		vnicID := vnics[0].VnicID
		if vnicID != "" {
			privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
				t.client.region, url.QueryEscape(vnicID))
			if resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil); err == nil && resp.StatusCode == http.StatusOK {
				var privList []struct {
					ID        string `json:"id"`
					IsPrimary bool   `json:"isPrimary"`
				}
				if err := json.Unmarshal(b, &privList); err == nil && len(privList) > 0 {
					for _, p := range privList {
						if p.IsPrimary {
							return p.ID, nil
						}
					}
					return privList[0].ID, nil
				}
			}
		}
	}
	return "", fmt.Errorf("未能自动识别本机主私网 IP OCID")
}

// CheckIPQuality 检测 IP 归属地、Scamalytics 评分与国内连通性
func (t *OCIIPTool) CheckIPQuality(ctx context.Context, ip string, maxRisk int, noAbuser bool) *IPQualityReport {
	rep := &IPQualityReport{
		IP:         ip,
		Passed:     true,
		FraudScore: -1,
		GFWStatus:  "未检测",
	}

	// 1. ip-api.com 查询属地与代理画像
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://ip-api.com/json/%s?fields=status,country,countryCode,city,as,proxy", ip), nil)
	client := &http.Client{Timeout: 5 * time.Second}
	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		var data struct {
			Status      string `json:"status"`
			Country     string `json:"country"`
			CountryCode string `json:"countryCode"`
			City        string `json:"city"`
			AS          string `json:"as"`
			Proxy       bool   `json:"proxy"`
		}
		if json.NewDecoder(resp.Body).Decode(&data) == nil && data.Status == "success" {
			rep.Country = fmt.Sprintf("%s (%s)", data.Country, data.CountryCode)
			rep.City = data.City
			rep.ASN = data.AS
			rep.IsProxy = data.Proxy
			if noAbuser && data.Proxy {
				rep.Passed = false
				rep.Reasons = append(rep.Reasons, "被标记为公开代理/高风险节点 (proxy=true)")
			}
		}
	}

	// 2. Scamalytics 欺诈分
	scamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://scamalytics.com/ip/%s", ip), nil)
	scamReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	if scamResp, err := client.Do(scamReq); err == nil {
		defer scamResp.Body.Close()
		var b strings.Builder
		buf := make([]byte, 8192)
		for {
			n, _ := scamResp.Body.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			} else {
				break
			}
		}
		html := b.String()
		scoreRe := regexp.MustCompile(`"score"\s*:\s*"(\d+)"`)
		if m := scoreRe.FindStringSubmatch(html); len(m) > 1 {
			var sc int
			_, _ = fmt.Sscanf(m[1], "%d", &sc)
			rep.FraudScore = sc
			if maxRisk > 0 && sc > maxRisk {
				rep.Passed = false
				rep.Reasons = append(rep.Reasons, fmt.Sprintf("Scamalytics 欺诈分 %d > 阈值 %d", sc, maxRisk))
			}
		}
	}

	// 3. 国内 TCP 连通性测试 (端口 443 探针)
	overseasConn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "443"), 2*time.Second)
	if err == nil {
		_ = overseasConn.Close()
		// 通过 Globalping 探针检测国内节点
		gpBody := map[string]any{
			"type":      "ping",
			"target":    ip,
			"locations": []map[string]string{{"country": "CN"}},
			"limit":     3,
			"measurementOptions": map[string]any{
				"protocol": "TCP",
				"port":     443,
				"packets":  2,
			},
		}
		gpJSON, _ := json.Marshal(gpBody)
		gpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.globalping.io/v1/measurements", strings.NewReader(string(gpJSON)))
		gpReq.Header.Set("Content-Type", "application/json")
		if gpResp, err := client.Do(gpReq); err == nil {
			defer gpResp.Body.Close()
			var gpInit struct {
				ID string `json:"id"`
			}
			if json.NewDecoder(gpResp.Body).Decode(&gpInit) == nil && gpInit.ID != "" {
				time.Sleep(2 * time.Second)
				measReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.globalping.io/v1/measurements/"+gpInit.ID, nil)
				if mResp, err := client.Do(measReq); err == nil {
					defer mResp.Body.Close()
					var mData struct {
						Results []struct {
							Result struct {
								Stats struct {
									RCV float64 `json:"rcv"`
									AVG float64 `json:"avg"`
								} `json:"stats"`
							} `json:"result"`
						} `json:"results"`
					}
					if json.NewDecoder(mResp.Body).Decode(&mData) == nil && len(mData.Results) > 0 {
						var okCount int
						var totalLatency float64
						for _, r := range mData.Results {
							if r.Result.Stats.RCV > 0 {
								okCount++
								totalLatency += r.Result.Stats.AVG
							}
						}
						if okCount > 0 {
							rep.GFWStatus = fmt.Sprintf("正常 (%d/%d 节点连通)", okCount, len(mData.Results))
							rep.GFWLatency = int(totalLatency / float64(okCount))
						} else {
							rep.GFWStatus = "国内 TCP 阻断 (可能被墙)"
							rep.Passed = false
							rep.Reasons = append(rep.Reasons, "国内探针 TCP 443 100% 丢包")
						}
					}
				}
			}
		}
	} else {
		rep.GFWStatus = "端口未开放 (无法测国内连通)"
	}

	if len(rep.Reasons) == 0 {
		rep.Reasons = append(rep.Reasons, "全部检测项正常通过")
	}

	return rep
}

func (t *OCIIPTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args ociIPArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid oci_ip args: %w", err)
	}

	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		action = "status"
	}

	// 权限保护：对换绑、申请与删除闲置等破坏性操作进行身份校验
	event := entity.SensoryEventFromContext(ctx)
	if action == "cleanup" || action == "roll" || action == "bind" {
		if t.immune != nil && event != nil {
			if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
				return &core.ToolResult{Text: fmt.Sprintf("权限拦截：%s 操作仅限搭档或协作者发起", action)}, nil
			}
		}
	}

	switch action {
	case "status":
		return t.handleStatus(ctx)
	case "cleanup":
		return t.handleCleanup(ctx, strings.TrimSpace(args.IP))
	case "check":
		return t.handleCheck(ctx, strings.TrimSpace(args.IP))
	case "bind":
		return t.handleBind(ctx, strings.TrimSpace(args.IP), args.DeleteOld)
	case "roll":
		noAbuser := true
		if args.NoAbuser != nil {
			noAbuser = *args.NoAbuser
		}
		maxRisk := args.MaxRisk
		if maxRisk <= 0 {
			maxRisk = 50
		}
		return t.handleRoll(ctx, args.Prefix, maxRisk, noAbuser)
	default:
		return nil, fmt.Errorf("unknown action: %s", action)
	}
}

func (t *OCIIPTool) handleStatus(ctx context.Context) (*core.ToolResult, error) {
	privID, _ := t.ResolvePrivateIPID(ctx)

	pipURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		t.client.region, url.QueryEscape(t.client.compartmentID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, pipURL, nil)
	if err != nil {
		return nil, fmt.Errorf("list public IPs error: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list public IPs returned code %d: %s", resp.StatusCode, string(b))
	}

	var ips []ociPublicIPItem
	_ = json.Unmarshal(b, &ips)

	var curPublicIP string
	if privID != "" {
		curURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/actions/getByPrivateIpId", t.client.region)
		if curResp, curB, err := t.client.Do(ctx, http.MethodPost, curURL, map[string]string{"privateIpId": privID}); err == nil && curResp.StatusCode == http.StatusOK {
			var curItem ociPublicIPItem
			if json.Unmarshal(curB, &curItem) == nil {
				curPublicIP = curItem.IPAddress
			}
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
			if item.PrivateIPID != "" {
				if item.PrivateIPID == privID {
					stateTag = "已绑定本机"
				} else {
					stateTag = "已绑定其他实例"
				}
			}
			lines = append(lines, fmt.Sprintf("- `%s`: %s (%s)", item.IPAddress, stateTag, item.DisplayName))
		}
	}

	return &core.ToolResult{Text: strings.Join(lines, "\n")}, nil
}

func (t *OCIIPTool) handleCleanup(ctx context.Context, targetIP string) (*core.ToolResult, error) {
	pipURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		t.client.region, url.QueryEscape(t.client.compartmentID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, pipURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list public IPs returned code %d", resp.StatusCode)
	}
	var ips []ociPublicIPItem
	_ = json.Unmarshal(b, &ips)

	var released []string
	for _, item := range ips {
		if item.PrivateIPID == "" {
			if targetIP == "" || targetIP == item.IPAddress {
				delURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, item.ID)
				delResp, _, err := t.client.Do(ctx, http.MethodDelete, delURL, nil)
				if err == nil && (delResp.StatusCode == http.StatusOK || delResp.StatusCode == http.StatusNoContent) {
					released = append(released, item.IPAddress)
				}
			}
		}
	}

	if len(released) == 0 {
		if targetIP != "" {
			return &core.ToolResult{Text: fmt.Sprintf("未找到待释放的未绑定预留 IP `%s` (可能已绑定或不存在)", targetIP)}, nil
		}
		return &core.ToolResult{Text: "当前没有未绑定的闲置预留 IP，无需清理。"}, nil
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("### 预留 IP 清理完成\n- **共释放**: `%d` 个闲置 IP\n- **已清理**: `%s`",
			len(released), strings.Join(released, ", ")),
	}, nil
}

func (t *OCIIPTool) handleCheck(ctx context.Context, targetIP string) (*core.ToolResult, error) {
	if targetIP == "" {
		privID, _ := t.ResolvePrivateIPID(ctx)
		if privID != "" {
			curURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/actions/getByPrivateIpId", t.client.region)
			if curResp, curB, err := t.client.Do(ctx, http.MethodPost, curURL, map[string]string{"privateIpId": privID}); err == nil && curResp.StatusCode == http.StatusOK {
				var curItem ociPublicIPItem
				if json.Unmarshal(curB, &curItem) == nil {
					targetIP = curItem.IPAddress
				}
			}
		}
	}
	if targetIP == "" {
		return nil, fmt.Errorf("未指定检测 IP 且未能获取当前公网 IP")
	}

	rep := t.CheckIPQuality(ctx, targetIP, 50, true)
	statusText := "优质通过"
	if !rep.Passed {
		statusText = "未通过"
	}

	fraudStr := "未知"
	if rep.FraudScore >= 0 {
		fraudStr = fmt.Sprintf("%d/100", rep.FraudScore)
	}
	proxyStr := "否"
	if rep.IsProxy {
		proxyStr = "是 (风险代理)"
	}

	lines := []string{
		fmt.Sprintf("### OCI IP 质量画像: `%s` [%s]", rep.IP, statusText),
		fmt.Sprintf("- **原生属地**: `%s %s`", rep.Country, rep.City),
		fmt.Sprintf("- **ASN 运营商**: `%s`", rep.ASN),
		fmt.Sprintf("- **公开代理标记**: `%s`", proxyStr),
		fmt.Sprintf("- **Scamalytics 欺诈分**: `%s`", fraudStr),
		fmt.Sprintf("- **国内 TCP (443)**: `%s`", rep.GFWStatus),
	}
	if len(rep.Reasons) > 0 {
		lines = append(lines, fmt.Sprintf("- **评估结论**: %s", strings.Join(rep.Reasons, "；")))
	}

	return &core.ToolResult{Text: strings.Join(lines, "\n")}, nil
}

func (t *OCIIPTool) handleRoll(ctx context.Context, prefixes []string, maxRisk int, noAbuser bool) (*core.ToolResult, error) {
	maxAttempts := 15
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		displayName := fmt.Sprintf("reserved-ip-%d", time.Now().Unix())
		createURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps", t.client.region)
		createBody := map[string]string{
			"compartmentId": t.client.compartmentID,
			"lifetime":      "RESERVED",
			"displayName":   displayName,
		}

		resp, b, err := t.client.Do(ctx, http.MethodPost, createURL, createBody)
		if err != nil || resp.StatusCode != http.StatusOK {
			time.Sleep(3 * time.Second)
			continue
		}

		var created ociPublicIPItem
		_ = json.Unmarshal(b, &created)
		candidateIP := created.IPAddress
		if candidateIP == "" {
			continue
		}

		// 检查前缀
		matchedPrefix := len(prefixes) == 0
		for _, p := range prefixes {
			if strings.HasPrefix(candidateIP, p) {
				matchedPrefix = true
				break
			}
		}

		if matchedPrefix {
			rep := t.CheckIPQuality(ctx, candidateIP, maxRisk, noAbuser)
			if rep.Passed {
				return &core.ToolResult{
					Text: fmt.Sprintf("### OCI 抽中优质候选预留 IP\n- **候选 IP**: `%s` (尝试 `%d` 次)\n- **属地**: `%s`\n- **欺诈分**: `%d/100`\n- **国内连通**: `%s`\n\n发送 `/oci_bind` 或调用 `oci_ip(action=\"bind\", ip=\"%s\")` 即可热换绑到主机。",
						candidateIP, attempt, rep.Country, rep.FraudScore, rep.GFWStatus, candidateIP),
				}, nil
			}
		}

		// 不符合要求：立即删除释放该候选 IP
		delURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, created.ID)
		_, _, _ = t.client.Do(ctx, http.MethodDelete, delURL, nil)
		time.Sleep(2 * time.Second)
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("已尝试刷取 %d 次，暂未摇中完全满足前缀与欺诈分要求的预留 IP，未保留任何闲置资源。", maxAttempts),
	}, nil
}

func (t *OCIIPTool) handleBind(ctx context.Context, targetIP string, deleteOld bool) (*core.ToolResult, error) {
	privID, err := t.ResolvePrivateIPID(ctx)
	if err != nil {
		return nil, err
	}

	// 1. 查找目标预留 IP 对象
	pipURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		t.client.region, url.QueryEscape(t.client.compartmentID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, pipURL, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list public IPs returned code %d", resp.StatusCode)
	}
	var ips []ociPublicIPItem
	_ = json.Unmarshal(b, &ips)

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
			if ips[i].PrivateIPID == "" {
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

	// 2. 查当前绑定的公网 IP
	var oldPubID string
	var oldPubIP string
	curURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/actions/getByPrivateIpId", t.client.region)
	if curResp, curB, err := t.client.Do(ctx, http.MethodPost, curURL, map[string]string{"privateIpId": privID}); err == nil && curResp.StatusCode == http.StatusOK {
		var curItem ociPublicIPItem
		if json.Unmarshal(curB, &curItem) == nil {
			oldPubID = curItem.ID
			oldPubIP = curItem.IPAddress
		}
	}

	if oldPubID == targetObj.ID {
		return &core.ToolResult{Text: fmt.Sprintf("预留 IP `%s` 已经绑定在本机网卡上，无需重复操作。", targetObj.IPAddress)}, nil
	}

	// 3. 解绑旧 IP
	if oldPubID != "" {
		unbindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, oldPubID)
		_, _, _ = t.client.Do(ctx, http.MethodPut, unbindURL, map[string]string{"privateIpId": ""})
		time.Sleep(2 * time.Second)
	}

	// 4. 绑定新 IP
	bindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, targetObj.ID)
	bResp, _, err := t.client.Do(ctx, http.MethodPut, bindURL, map[string]string{"privateIpId": privID})
	if err != nil || bResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("绑定新预留 IP 异常: %w", err)
	}

	// 5. 若配置了 deleteOld，清理旧预留 IP
	if deleteOld && oldPubID != "" {
		delURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, oldPubID)
		_, _, _ = t.client.Do(ctx, http.MethodDelete, delURL, nil)
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("### OCI IP 热换绑成功\n- **新公网 IP**: `%s` (已生效并接入主网卡)\n- **旧公网 IP**: `%s`%s",
			targetObj.IPAddress, oldPubIP, func() string {
				if deleteOld && oldPubID != "" {
					return " (已自动释放删除)"
				}
				return ""
			}()),
	}, nil
}
