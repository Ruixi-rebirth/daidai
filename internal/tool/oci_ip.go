package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// OCIIPTool Oracle Cloud 预留公网 IP 管理与换绑工具
type OCIIPTool struct {
	client      *OCIClient
	immune      *entity.ImmuneSystem
	operationMu chan struct{}
}

func NewOCIIPTool(client *OCIClient, immune *entity.ImmuneSystem) *OCIIPTool {
	return &OCIIPTool{
		client:      client,
		immune:      immune,
		operationMu: make(chan struct{}, 1),
	}
}

func (t *OCIIPTool) Name() string {
	return "oci_ip"
}

func (t *OCIIPTool) Description() string {
	return "Oracle Cloud (OCI) 预留公网 IP 管理与换绑工具。支持查看当前公网与预留 IP 列表 (status)、一键释放/删除闲置预留 IP (cleanup)、按纯净度与欺诈分筛选刷取候选优质 IP (roll)、检测指定 IP 质量 (check) 以及将优质 IP 无损绑定至服务器 (bind)。当用户要求查看、刷取、换绑、释放或删除预留 IP 时，必须实际调用本工具执行，绝对禁止在未调用工具的情况下虚构回复已操作完成。"
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
				"description": "刷取 (roll) 时允许的最大 Scamalytics 欺诈风险评分 (0-100，越低越纯净，如 25；省略时默认为 50)",
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
	MaxRisk   *int     `json:"max_risk"`
	NoAbuser  *bool    `json:"no_abuser"`
}

// IPQualityReport 单个 IP 评估质量报告
type IPQualityReport struct {
	IP         string
	Passed     bool
	Reasons    []string
	Country    string
	City       string
	ASN        string
	IsProxy    bool
	FraudScore int
	GFWStatus  string
	GFWLatency int
}

type globalpingMeasurement struct {
	Status  string `json:"status"`
	Results []struct {
		Result struct {
			Stats struct {
				RCV float64 `json:"rcv"`
				AVG float64 `json:"avg"`
			} `json:"stats"`
		} `json:"result"`
	} `json:"results"`
}

const (
	globalpingPollInterval    = 500 * time.Millisecond
	globalpingMaxPolls        = 20
	maxIPQualityResponseBytes = 1 << 20
	publicIPPollInterval      = 2 * time.Second
	publicIPPollTimeout       = 90 * time.Second
)

// ResolvePrivateIPID 解析主私网 IP OCID
func (t *OCIIPTool) ResolvePrivateIPID(ctx context.Context) (string, error) {
	var vnics []imdsVnicMeta
	if err := FetchIMDS(ctx, "vnics", &vnics); err != nil {
		return "", fmt.Errorf("读取 OCI 实例网卡元数据失败: %w", err)
	}
	if len(vnics) == 0 || vnics[0].VnicID == "" {
		return "", fmt.Errorf("OCI 实例元数据中没有可用的主网卡 OCID")
	}
	privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
		t.client.region, url.QueryEscape(vnics[0].VnicID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil)
	if err != nil {
		return "", fmt.Errorf("查询主网卡私网 IP 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("查询主网卡私网 IP 返回 HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(b))
	}
	var privList []struct {
		ID        string `json:"id"`
		IsPrimary bool   `json:"isPrimary"`
	}
	if err := json.Unmarshal(b, &privList); err != nil {
		return "", fmt.Errorf("解析主网卡私网 IP 响应失败: %w", err)
	}
	primaryID := ""
	for _, p := range privList {
		if !p.IsPrimary {
			continue
		}
		if p.ID == "" {
			return "", fmt.Errorf("主网卡私网 IP 响应将主地址标记为缺少 OCID")
		}
		if primaryID != "" {
			return "", fmt.Errorf("主网卡私网 IP 响应包含多个主地址，拒绝猜测目标")
		}
		primaryID = p.ID
	}
	if primaryID == "" {
		return "", fmt.Errorf("主网卡私网 IP 响应未标出主地址，拒绝猜测目标")
	}
	return primaryID, nil
}

func (t *OCIIPTool) listReservedPublicIPs(ctx context.Context) ([]ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps?scope=REGION&compartmentId=%s&lifetime=RESERVED",
		t.client.region, url.QueryEscape(t.client.compartmentID))
	ips, err := listOCIResources[ociPublicIPItem](ctx, t.client, endpoint)
	if err != nil {
		return nil, fmt.Errorf("读取 OCI 预留 IP 列表失败: %w", err)
	}
	return ips, nil
}

func (t *OCIIPTool) getPublicIPByPrivateIP(ctx context.Context, privateIPID string) (*ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/actions/getByPrivateIpId", t.client.region)
	resp, body, err := t.client.Do(ctx, http.MethodPost, endpoint, map[string]string{"privateIpId": privateIPID})
	if err != nil {
		return nil, fmt.Errorf("查询主网卡当前公网 IP 失败: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询主网卡当前公网 IP 返回 HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPublicIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析主网卡当前公网 IP 响应失败: %w", err)
	}
	if item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("主网卡当前公网 IP 响应缺少资源 ID 或地址")
	}
	return &item, nil
}

func (t *OCIIPTool) getPublicIPByID(ctx context.Context, publicIPID string) (*ociPublicIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询预留 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPublicIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析预留 IP %s 响应失败: %w", publicIPID, err)
	}
	if item.ID == "" {
		return nil, fmt.Errorf("预留 IP %s 响应缺少资源 ID", publicIPID)
	}
	return &item, nil
}

func isPublicIPAvailable(item *ociPublicIPItem) bool {
	if item == nil || item.PrivateIPID != "" {
		return false
	}
	return item.LifecycleState == "AVAILABLE" || item.LifecycleState == "UNASSIGNED"
}

func publicIPStateLabel(item *ociPublicIPItem) string {
	if item == nil || strings.TrimSpace(item.LifecycleState) == "" {
		return "未知"
	}
	return item.LifecycleState
}

// waitPublicIPLifecycle waits for an asynchronous OCI assignment transition.
// terminal is true when OCI reports a state that cannot satisfy the requested
// transition, so callers may safely decide whether a rollback is appropriate.
func (t *OCIIPTool) waitPublicIPLifecycle(ctx context.Context, publicIPID, expectedState, expectedPrivateIPID string) (terminal bool, err error) {
	pollCtx, cancel := context.WithTimeout(ctx, publicIPPollTimeout)
	defer cancel()
	ticker := time.NewTicker(publicIPPollInterval)
	defer ticker.Stop()
	// An AVAILABLE snapshot immediately after an ambiguous PUT may predate the
	// asynchronous assignment. Only treat it as a failed final state after OCI
	// has first reported ASSIGNING; otherwise keep polling and report uncertainty.
	observedAssigning := false

	for {
		item, getErr := t.getPublicIPByID(pollCtx, publicIPID)
		if getErr != nil {
			return false, fmt.Errorf("轮询预留 IP %s 状态失败: %w", publicIPID, getErr)
		}
		if item == nil {
			if expectedState == "TERMINATED" {
				return false, nil
			}
			return true, fmt.Errorf("预留 IP %s 查询结果为不存在", publicIPID)
		}
		available := expectedState == "AVAILABLE" && item.LifecycleState == "UNASSIGNED"
		if (item.LifecycleState == expectedState || available) && item.PrivateIPID == expectedPrivateIPID {
			return false, nil
		}
		if expectedState == "ASSIGNED" && item.LifecycleState == "ASSIGNING" {
			observedAssigning = true
		}
		if item.LifecycleState == "ASSIGNED" && item.PrivateIPID != expectedPrivateIPID {
			return true, fmt.Errorf("预留 IP %s 已绑定到其他私网 IP %s", publicIPID, item.PrivateIPID)
		}
		if expectedState == "ASSIGNED" && item.LifecycleState == "TERMINATED" {
			return true, fmt.Errorf("预留 IP %s 到达终态 %s，未绑定到目标私网 IP", publicIPID, item.LifecycleState)
		}
		if expectedState == "ASSIGNED" && observedAssigning && (item.LifecycleState == "AVAILABLE" || item.LifecycleState == "UNASSIGNED") {
			return true, fmt.Errorf("预留 IP %s 经 ASSIGNING 后回到 %s，未绑定到目标私网 IP", publicIPID, item.LifecycleState)
		}
		if expectedState == "AVAILABLE" && item.LifecycleState == "TERMINATED" {
			return true, fmt.Errorf("预留 IP %s 已终止，无法确认解绑状态", publicIPID)
		}

		select {
		case <-pollCtx.Done():
			return false, fmt.Errorf("等待预留 IP %s 到达 %s 超时或取消；当前状态 %s，privateIpId=%q: %w", publicIPID, expectedState, item.LifecycleState, item.PrivateIPID, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// CheckIPQuality 检测 IP 归属地、Scamalytics 评分与国内连通性
func (t *OCIIPTool) CheckIPQuality(ctx context.Context, ip string, maxRisk int, noAbuser bool) *IPQualityReport {
	rep := &IPQualityReport{
		IP:         ip,
		Passed:     true,
		FraudScore: -1,
		GFWStatus:  "未检测",
	}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || !parsedIP.IsGlobalUnicast() || parsedIP.IsPrivate() || parsedIP.IsLoopback() || parsedIP.IsLinkLocalUnicast() || parsedIP.IsUnspecified() {
		rep.Passed = false
		rep.Reasons = append(rep.Reasons, "目标不是有效的公网单播 IP 地址")
		return rep
	}
	ip = parsedIP.String()
	rep.IP = ip
	var locationVerified bool

	// 1. ip-api.com 查询属地与代理画像
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://ip-api.com/json/%s?fields=status,country,countryCode,city,as,proxy", ip), nil)
	client := &http.Client{
		Timeout: 5 * time.Second,
		// These fixed third-party API requests must not follow redirects to
		// arbitrary hosts or local metadata services.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if resp, err := client.Do(req); err == nil {
		defer func() { _ = resp.Body.Close() }()
		var data struct {
			Status      string `json:"status"`
			Country     string `json:"country"`
			CountryCode string `json:"countryCode"`
			City        string `json:"city"`
			AS          string `json:"as"`
			Proxy       *bool  `json:"proxy"`
		}
		body, readErr := readLimitedBody(resp.Body, maxIPQualityResponseBytes)
		if resp.StatusCode == http.StatusOK && readErr == nil && json.Unmarshal(body, &data) == nil && data.Status == "success" && data.Proxy != nil {
			locationVerified = true
			rep.Country = fmt.Sprintf("%s (%s)", data.Country, data.CountryCode)
			rep.City = data.City
			rep.ASN = data.AS
			rep.IsProxy = *data.Proxy
			if noAbuser && rep.IsProxy {
				rep.Passed = false
				rep.Reasons = append(rep.Reasons, "被标记为公开代理/高风险节点 (proxy=true)")
			}
		}
	}
	if noAbuser && !locationVerified {
		rep.Passed = false
		rep.Reasons = append(rep.Reasons, "无法确认 IP 是否为公开代理或高风险节点")
	}

	// 2. Scamalytics 欺诈分
	scamReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://scamalytics.com/ip/%s", ip), nil)
	scamReq.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	if scamResp, err := client.Do(scamReq); err == nil {
		defer func() { _ = scamResp.Body.Close() }()
		if scamResp.StatusCode == http.StatusOK {
			rawHTML, readErr := io.ReadAll(io.LimitReader(scamResp.Body, 1024*1024))
			if readErr == nil {
				if sc, ok := parseScamalyticsScore(rawHTML); ok {
					rep.FraudScore = sc
					if maxRisk >= 0 && sc > maxRisk {
						rep.Passed = false
						rep.Reasons = append(rep.Reasons, fmt.Sprintf("Scamalytics 欺诈分 %d > 阈值 %d", sc, maxRisk))
					}
				}
			}
		}
	}
	if maxRisk >= 0 && rep.FraudScore < 0 {
		rep.Passed = false
		rep.Reasons = append(rep.Reasons, "无法获取 Scamalytics 欺诈评分")
	}

	// 国内连通性应由国内探针直接判断，不能依赖运行本工具的主机能否连接目标。
	status, latency, probeErr := checkGlobalpingTCP(ctx, client, ip)
	rep.GFWStatus, rep.GFWLatency = status, latency
	if probeErr != nil {
		rep.Passed = false
		rep.Reasons = append(rep.Reasons, probeErr.Error())
	}

	if len(rep.Reasons) == 0 {
		rep.Reasons = append(rep.Reasons, "全部检测项正常通过")
	}

	return rep
}

func parseScamalyticsScore(body []byte) (int, bool) {
	const key = `"score"`
	for offset := 0; offset < len(body); {
		match := bytes.Index(body[offset:], []byte(key))
		if match < 0 {
			return 0, false
		}
		index := offset + match + len(key)
		for index < len(body) && isJSONSpace(body[index]) {
			index++
		}
		if index >= len(body) || body[index] != ':' {
			offset = index
			continue
		}
		index++
		for index < len(body) && isJSONSpace(body[index]) {
			index++
		}
		if index >= len(body) || body[index] != '"' {
			offset = index
			continue
		}
		index++
		digits := 0
		score := 0
		for index < len(body) && body[index] >= '0' && body[index] <= '9' {
			if score <= 100 {
				score = score*10 + int(body[index]-'0')
				if score > 100 {
					score = 101
				}
			}
			digits++
			index++
		}
		if digits > 0 && index < len(body) && body[index] == '"' && score <= 100 {
			return score, true
		}
		offset = index
	}
	return 0, false
}

func isJSONSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\r' || char == '\n'
}

func checkGlobalpingTCP(ctx context.Context, client *http.Client, ip string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	requestBody := map[string]any{
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
	body, err := json.Marshal(requestBody)
	if err != nil {
		return "国内探针请求无法创建", 0, fmt.Errorf("编码 Globalping 探测请求失败: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.globalping.io/v1/measurements", bytes.NewReader(body))
	if err != nil {
		return "国内探针请求无法创建", 0, fmt.Errorf("创建 Globalping 探测请求失败: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "daidai/oci-ip-check")
	response, err := client.Do(request)
	if err != nil {
		return "国内探针服务不可用", 0, fmt.Errorf("globalping 未能接受国内连通探测: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	var created struct {
		ID string `json:"id"`
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Sprintf("国内探针创建失败 (HTTP %d)", response.StatusCode), 0, fmt.Errorf("globalping 创建测量返回 HTTP %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64*1024)).Decode(&created); err != nil || created.ID == "" {
		return "国内探针创建失败", 0, fmt.Errorf("globalping 未返回有效测量任务")
	}

	for attempt := 0; attempt < globalpingMaxPolls; attempt++ {
		if err := waitContext(ctx, globalpingPollInterval); err != nil {
			if ctx.Err() == context.Canceled {
				return "国内连通探测已中断", 0, fmt.Errorf("国内连通探测已中断")
			}
			return "国内连通探测超时", 0, fmt.Errorf("globalping 测量在 20 秒内未完成")
		}
		resultURL := "https://api.globalping.io/v1/measurements/" + url.PathEscape(created.ID)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, resultURL, nil)
		if err != nil {
			return "国内探针请求无法创建", 0, fmt.Errorf("创建 Globalping 结果请求失败: %w", err)
		}
		request.Header.Set("User-Agent", "daidai/oci-ip-check")
		response, err := client.Do(request)
		if err != nil {
			return "国内探针服务不可用", 0, fmt.Errorf("读取 Globalping 测量结果失败: %w", err)
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return fmt.Sprintf("国内探针结果读取失败 (HTTP %d)", response.StatusCode), 0, fmt.Errorf("读取 Globalping 测量结果返回 HTTP %d", response.StatusCode)
		}
		var measurement globalpingMeasurement
		err = json.NewDecoder(io.LimitReader(response.Body, 1024*1024)).Decode(&measurement)
		_ = response.Body.Close()
		if err != nil {
			return "国内探针结果无效", 0, fmt.Errorf("解析 Globalping 测量结果失败: %w", err)
		}
		if measurement.Status == "in-progress" || measurement.Status == "in_progress" {
			continue
		}
		if measurement.Status == "" || len(measurement.Results) == 0 {
			return "国内探针结果无效", 0, fmt.Errorf("globalping 测量已结束但没有有效探测结果")
		}

		var reachable int
		var totalLatency float64
		for _, result := range measurement.Results {
			if result.Result.Stats.RCV > 0 {
				reachable++
				totalLatency += result.Result.Stats.AVG
			}
		}
		if reachable == 0 {
			return "国内 TCP 阻断 (可能被墙)", 0, fmt.Errorf("国内探针 TCP 443 全部未连通")
		}
		return fmt.Sprintf("正常 (%d/%d 节点连通)", reachable, len(measurement.Results)), int(totalLatency / float64(reachable)), nil
	}
	if err := ctx.Err(); err != nil {
		return "国内连通探测已中断", 0, fmt.Errorf("国内连通探测已中断: %w", err)
	}
	return "国内连通探测未完成", 0, fmt.Errorf("globalping 测量在等待期限内未完成")
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
		if t.immune == nil || event == nil {
			return &core.ToolResult{Text: "权限校验不可用，拒绝执行 OCI 变更"}, nil
		}
		if permErr := t.immune.VerifyDelegationPerm(event.Role); permErr != nil {
			return &core.ToolResult{Text: fmt.Sprintf("权限拦截：%s 操作仅限搭档或协作者发起", action)}, nil
		}
		// OCI 的绑定、解绑和释放是有状态的云端变更；本地串行化避免
		// cleanup/roll/bind 并发修改同一批预留 IP。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case t.operationMu <- struct{}{}:
			defer func() { <-t.operationMu }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
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
		maxRisk := 50
		if args.MaxRisk != nil {
			if *args.MaxRisk < 0 || *args.MaxRisk > 100 {
				return nil, fmt.Errorf("max_risk 必须在 0 到 100 之间")
			}
			maxRisk = *args.MaxRisk
		}
		return t.handleRoll(ctx, args.Prefix, maxRisk, noAbuser)
	default:
		return nil, fmt.Errorf("unknown action: %s", action)
	}
}

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

func (t *OCIIPTool) handleCheck(ctx context.Context, targetIP string) (*core.ToolResult, error) {
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
		candidateIP := created.IPAddress
		if candidateIP == "" {
			if err := t.releaseRollCandidate(created.ID); err != nil {
				return nil, fmt.Errorf("新建候选 IP 响应缺少地址，且资源 %s 清理失败: %w", created.ID, err)
			}
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
			if err := ctx.Err(); err != nil {
				if cleanupErr := t.releaseRollCandidate(created.ID); cleanupErr != nil {
					return nil, fmt.Errorf("刷取已中断，候选 IP %s 清理失败: %w", candidateIP, cleanupErr)
				}
				return nil, err
			}
			if rep.Passed {
				if err := ctx.Err(); err != nil {
					if cleanupErr := t.releaseRollCandidate(created.ID); cleanupErr != nil {
						return nil, fmt.Errorf("刷取已取消，候选 IP %s 清理失败: %w", candidateIP, cleanupErr)
					}
					return nil, err
				}
				return &core.ToolResult{
					Text: fmt.Sprintf("### OCI 抽中优质候选预留 IP\n- **候选 IP**: `%s` (尝试 `%d` 次)\n- **属地**: `%s`\n- **欺诈分**: `%d/100`\n- **国内连通**: `%s`\n\n发送 `/oci_bind` 或调用 `oci_ip(action=\"bind\", ip=\"%s\")` 即可热换绑到主机。",
						candidateIP, attempt, rep.Country, rep.FraudScore, rep.GFWStatus, candidateIP),
				}, nil
			}
		}

		// 不符合要求：立即删除释放该候选 IP
		if err := t.releaseRollCandidate(created.ID); err != nil {
			return nil, fmt.Errorf("候选 IP %s 未通过筛选且释放失败，可能继续产生费用: %w", candidateIP, err)
		}
		if err := waitContext(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}

	return &core.ToolResult{
		Text: fmt.Sprintf("已尝试刷取 %d 次，暂未摇中完全满足前缀与欺诈分要求的预留 IP，未保留任何闲置资源。", maxAttempts),
	}, nil
}

// releaseRollCandidate cleans up a resource created by this roll operation.
// Once creation succeeded, cancellation must not strand a billable candidate.
func (t *OCIIPTool) releaseRollCandidate(publicIPID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
	defer cancel()
	return t.releaseReservedIP(ctx, publicIPID)
}

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

	// 若系统部署了 singbox-links，换绑成功后自动生成并附带新节点的代理配置
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

func (t *OCIIPTool) cleanupReservationByName(displayName string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for attempt := 0; attempt < 5; attempt++ {
		ips, err := t.listReservedPublicIPs(ctx)
		if err != nil {
			return false, err
		}
		found := false
		for _, item := range ips {
			if item.DisplayName != displayName {
				continue
			}
			found = true
			if item.ID == "" {
				return false, fmt.Errorf("同名预留 IP 缺少资源 OCID")
			}
			if item.PrivateIPID != "" {
				return false, fmt.Errorf("同名预留 IP %s 已绑定私网地址 %s，拒绝自动删除", item.IPAddress, item.PrivateIPID)
			}
			if err := t.releaseReservedIP(ctx, item.ID); err != nil {
				return false, fmt.Errorf("释放同名预留 IP %s 失败: %w", item.IPAddress, err)
			}
		}
		if found {
			return true, nil
		}
		if attempt < 4 {
			if err := waitContext(ctx, time.Second); err != nil {
				return false, err
			}
		}
	}
	return false, nil
}

func (t *OCIIPTool) releaseReservedIP(ctx context.Context, publicIPID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := t.getPublicIPByID(ctx, publicIPID)
	if err != nil {
		return fmt.Errorf("删除前无法确认预留 IP %s 状态: %w", publicIPID, err)
	}
	if current == nil {
		return nil
	}
	if current.LifecycleState == "TERMINATED" {
		return nil
	}
	if current.LifecycleState == "TERMINATING" {
		if current.PrivateIPID != "" {
			return fmt.Errorf("拒绝等待删除仍绑定的预留 IP %s (privateIpId=%q)", publicIPID, current.PrivateIPID)
		}
		verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()
		if _, err := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); err != nil {
			return fmt.Errorf("预留 IP %s 已进入 TERMINATING，但未能确认删除完成: %w", publicIPID, err)
		}
		return nil
	}
	if !isPublicIPAvailable(current) {
		return fmt.Errorf("拒绝删除状态不明确或仍绑定的预留 IP %s (lifecycleState=%q, privateIpId=%q)", publicIPID, current.LifecycleState, current.PrivateIPID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	url := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, publicIPID)
	resp, body, err := t.client.Do(ctx, http.MethodDelete, url, nil)
	if err != nil {
		verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()
		if _, stateErr := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); stateErr == nil {
			return nil
		} else {
			return fmt.Errorf("删除预留 IP %s 的请求结果不确定，最终状态也未能确认 (%v): %w", publicIPID, stateErr, err)
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
	defer cancel()
	if _, err := t.waitPublicIPLifecycle(verifyCtx, publicIPID, "TERMINATED", ""); err != nil {
		return fmt.Errorf("删除请求已接受，但未能确认资源进入 TERMINATED: %w", err)
	}
	return nil
}

func waitContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
