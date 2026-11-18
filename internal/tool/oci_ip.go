package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

type hostIPManager interface {
	AddIP(ctx context.Context, ip string, maskBits int, iface string) error
	DelIP(ctx context.Context, ip string, maskBits int, iface string) error
}

type defaultHostIPManager struct{}

func (m *defaultHostIPManager) AddIP(ctx context.Context, ip string, maskBits int, iface string) error {
	cidr := fmt.Sprintf("%s/%d", ip, maskBits)
	return sudoIP(ctx, "addr", "add", cidr, "dev", iface)
}

func (m *defaultHostIPManager) DelIP(ctx context.Context, ip string, maskBits int, iface string) error {
	cidr := fmt.Sprintf("%s/%d", ip, maskBits)
	return sudoIP(ctx, "addr", "del", cidr, "dev", iface)
}

func sudoIP(ctx context.Context, args ...string) error {
	sudoPath, err := exec.LookPath("sudo")
	if err != nil {
		sudoPath = "/run/wrappers/bin/sudo"
	}
	ipPath, err := exec.LookPath("ip")
	if err != nil {
		ipPath = "/run/current-system/sw/bin/ip"
	}
	cmdArgs := append([]string{"-n", ipPath}, args...)
	cmd := exec.CommandContext(ctx, sudoPath, cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(out))
		if len(args) >= 2 && args[0] == "addr" && args[1] == "del" && strings.Contains(outStr, "Cannot assign requested address") {
			return nil
		}
		return fmt.Errorf("sudo ip %s 失败 (%w): %s", strings.Join(args, " "), err, outStr)
	}
	return nil
}

// OCIIPTool Oracle Cloud 预留公网 IP 管理与换绑工具
type OCIIPTool struct {
	client      *OCIClient
	immune      *entity.ImmuneSystem
	operationMu chan struct{}
	qualityHTTP *http.Client
	hostIP      hostIPManager
	portLister  portLister
}

func NewOCIIPTool(client *OCIClient, immune *entity.ImmuneSystem) *OCIIPTool {
	return &OCIIPTool{
		client:      client,
		immune:      immune,
		operationMu: make(chan struct{}, 1),
		qualityHTTP: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		hostIP:      &defaultHostIPManager{},
		portLister:  systemListeningPorts,
	}
}

func (t *OCIIPTool) Name() string {
	return "oci_ip"
}

func (t *OCIIPTool) Description() string {
	return "OCI 预留公网 IP 管理：status 查询；roll 申请并保留未绑定候选；check 查询信誉画像、本机 TCP 监听和 UDP 绑定套接字，并从国内探针逐项检查适用的 TCP 监听端口。未绑定候选在检查期间临时绑定到辅助私网 IP，完成后确认解绑并清理辅助资源，但保留候选 IP；UDP 套接字仅作本机信息，不代表服务端口或公网可用；bind 单独换绑；cleanup 删除闲置资源。必须实际调用才能报告操作完成。画像未知不等于风险高；探针失败不等于 IP 不可用或被墙；不承诺网段质量。"
}

func (t *OCIIPTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "status 查询；cleanup 删除闲置 IP；roll 申请并保留候选（不自动绑定）；check 读取本机 TCP 监听和 UDP 绑定套接字，并从国内探针验证适用的 TCP 监听端口；bind 换绑",
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
				"description": "roll 可选风险分上限：默认 100 表示不筛选、不请求评分服务；0 只接受已知评分 0。仅用户明确要求时设置小于 100 的值；未知评分会停止筛选。",
			},
			"no_abuser": map[string]any{
				"type":        "boolean",
				"description": "roll 是否排除第三方标记的公开代理，默认 false；仅用户明确要求时启用，无法确认标记会停止筛选。",
			},
			"port": map[string]any{"type": "integer", "description": "check 可选的单个 TCP 端口；不指定时检查服务器当前监听且适用于目标私网 IP 的全部 TCP 端口，并列出 UDP 监听端口", "minimum": 1, "maximum": 65535},
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
	Port      *int     `json:"port"`
}

// ipReputationReport 单个 IP 评估质量报告
type ipReputationReport struct {
	IP         string
	Passed     bool
	Reasons    []string
	Country    string
	City       string
	ASN        string
	IsProxy    bool
	ProxyKnown bool
	Incomplete bool
	FraudScore int
}

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
	globalpingPollInterval    = time.Second
	globalpingMaxPolls        = 20
	maxIPQualityResponseBytes = 1 << 20
	publicIPPollInterval      = 2 * time.Second
	publicIPPollTimeout       = 90 * time.Second
)

type vnicHostInfo struct {
	vnicID          string
	macAddr         string
	subnetCidrBlock string
	primaryPrivID   string
	primaryPrivIP   string
	ifaceName       string
	maskBits        int
}

func findInterfaceByMAC(macStr, privIP string) string {
	ifaces, err := net.Interfaces()
	if err == nil {
		if macStr != "" {
			for _, iface := range ifaces {
				if strings.EqualFold(iface.HardwareAddr.String(), macStr) {
					return iface.Name
				}
			}
		}
		if privIP != "" {
			for _, iface := range ifaces {
				addrs, err := iface.Addrs()
				if err != nil {
					continue
				}
				for _, addr := range addrs {
					if strings.HasPrefix(addr.String(), privIP+"/") {
						return iface.Name
					}
				}
			}
		}
	}
	return "enp0s6"
}

func (t *OCIIPTool) resolveVNICInfo(ctx context.Context) (*vnicHostInfo, error) {
	var vnics []imdsVnicMeta
	if err := FetchIMDS(ctx, "vnics", &vnics); err != nil {
		return nil, fmt.Errorf("读取 OCI 实例网卡元数据失败: %w", err)
	}
	if len(vnics) == 0 || vnics[0].VnicID == "" {
		return nil, fmt.Errorf("OCI 实例元数据中没有可用的主网卡 OCID")
	}

	info := &vnicHostInfo{
		vnicID:          vnics[0].VnicID,
		macAddr:         vnics[0].MacAddr,
		subnetCidrBlock: vnics[0].SubnetCidrBlock,
		primaryPrivIP:   vnics[0].PrivateIP,
		maskBits:        24,
	}

	if info.subnetCidrBlock != "" {
		if _, ipNet, err := net.ParseCIDR(info.subnetCidrBlock); err == nil {
			if ones, _ := ipNet.Mask.Size(); ones > 0 && ones <= 32 {
				info.maskBits = ones
			}
		}
	}

	info.ifaceName = findInterfaceByMAC(info.macAddr, info.primaryPrivIP)

	privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
		t.client.region, url.QueryEscape(info.vnicID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil)
	if err != nil {
		return nil, fmt.Errorf("查询主网卡私网 IP 失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询主网卡私网 IP 返回 HTTP %d: %s", resp.StatusCode, summarizeOCIResponse(b))
	}
	var privList []struct {
		ID        string `json:"id"`
		IsPrimary bool   `json:"isPrimary"`
		IPAddress string `json:"ipAddress"`
	}
	if err := json.Unmarshal(b, &privList); err != nil {
		return nil, fmt.Errorf("解析主网卡私网 IP 响应失败: %w", err)
	}
	primaryID := ""
	for _, p := range privList {
		if !p.IsPrimary {
			continue
		}
		if p.ID == "" {
			return nil, fmt.Errorf("主网卡私网 IP 响应将主地址标记为缺少 OCID")
		}
		if primaryID != "" {
			return nil, fmt.Errorf("主网卡私网 IP 响应包含多个主地址，拒绝猜测目标")
		}
		primaryID = p.ID
		if p.IPAddress != "" {
			info.primaryPrivIP = p.IPAddress
		}
	}
	if primaryID == "" {
		return nil, fmt.Errorf("主网卡私网 IP 响应未标出主地址，拒绝猜测目标")
	}
	info.primaryPrivID = primaryID
	return info, nil
}

// ResolvePrivateIPID 解析主私网 IP OCID
func (t *OCIIPTool) ResolvePrivateIPID(ctx context.Context) (string, error) {
	info, err := t.resolveVNICInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.primaryPrivID, nil
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

func (t *OCIIPTool) getPrivateIPByID(ctx context.Context, privateIPID string) (*ociPrivateIPItem, error) {
	endpoint := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps/%s", t.client.region, url.PathEscape(privateIPID))
	resp, body, err := t.client.Do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("查询私网 IP %s 失败: %w", privateIPID, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询私网 IP %s 返回 HTTP %d: %s", privateIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPrivateIPItem
	if err := json.Unmarshal(body, &item); err != nil {
		return nil, fmt.Errorf("解析私网 IP %s 响应失败: %w", privateIPID, err)
	}
	if item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("私网 IP %s 响应缺少资源 ID 或地址", privateIPID)
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

// checkIPReputation evaluates only requested reputation filters, never connectivity.
// A missing required observation stops rolling; it is not a rejected IP.
func (t *OCIIPTool) checkIPReputation(ctx context.Context, ip string, maxRisk int, noAbuser bool) *ipReputationReport {
	rep := &ipReputationReport{IP: ip, Passed: true, FraudScore: -1}
	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil || !parsedIP.IsGlobalUnicast() || parsedIP.IsPrivate() {
		rep.Passed, rep.Incomplete = false, true
		rep.Reasons = append(rep.Reasons, "目标不是有效的公网单播 IP 地址")
		return rep
	}
	rep.IP = parsedIP.String()
	if noAbuser {
		body, err := t.fetchIPObservation(ctx, "http://ip-api.com/json/"+rep.IP+"?fields=status,country,countryCode,city,as,proxy")
		var data struct {
			Status      string
			Country     string
			CountryCode string
			City        string
			AS          string
			Proxy       *bool
		}
		if err == nil {
			if json.Unmarshal(body, &data) != nil || data.Status != "success" || data.Proxy == nil {
				err = fmt.Errorf("响应缺少有效代理标记")
			}
		}
		if err != nil {
			rep.Passed, rep.Incomplete = false, true
			rep.Reasons = append(rep.Reasons, "ip-api 代理画像不可用："+err.Error())
		} else {
			rep.ProxyKnown = true
			rep.Country, rep.City, rep.ASN, rep.IsProxy = data.Country+" ("+data.CountryCode+")", data.City, data.AS, *data.Proxy
			if rep.IsProxy {
				rep.Passed = false
				rep.Reasons = append(rep.Reasons, "ip-api 标记 proxy=true，不满足排除公开代理的要求")
			}
		}
	}
	if maxRisk < 100 {
		body, err := t.fetchIPObservation(ctx, "https://scamalytics.com/ip/"+rep.IP)
		if err == nil {
			score, ok := parseScamalyticsScore(body)
			if !ok {
				err = fmt.Errorf("响应中没有有效评分")
			} else {
				rep.FraudScore = score
				if score > maxRisk {
					rep.Passed = false
					rep.Reasons = append(rep.Reasons, fmt.Sprintf("Scamalytics 欺诈分 %d > 上限 %d", score, maxRisk))
				}
			}
		}
		if err != nil {
			rep.Passed, rep.Incomplete = false, true
			rep.Reasons = append(rep.Reasons, "Scamalytics 评分不可用："+err.Error())
		}
	}
	return rep
}

func (t *OCIIPTool) fetchIPObservation(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; daidai/oci-ip)")
	resp, err := t.qualityHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return readLimitedBody(resp.Body, maxIPQualityResponseBytes)
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

type ociPrivateIPItem struct {
	ID          string `json:"id"`
	IPAddress   string `json:"ipAddress"`
	VnicID      string `json:"vnicId"`
	DisplayName string `json:"displayName"`
	IsPrimary   bool   `json:"isPrimary"`
}

func (t *OCIIPTool) createSecondaryPrivateIP(ctx context.Context, vnicID, displayName string) (*ociPrivateIPItem, error) {
	createURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps", t.client.region)
	createBody := map[string]string{
		"vnicId":      vnicID,
		"displayName": displayName,
	}
	resp, body, err := t.client.Do(ctx, http.MethodPost, createURL, createBody)
	if err != nil {
		return nil, fmt.Errorf("创建辅助私网 IP 请求失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("创建辅助私网 IP 失败 (HTTP %d): %s", resp.StatusCode, summarizeOCIResponse(body))
	}
	var item ociPrivateIPItem
	if err := json.Unmarshal(body, &item); err != nil || item.ID == "" || item.IPAddress == "" {
		return nil, fmt.Errorf("解析辅助私网 IP 响应失败: %w", err)
	}
	return &item, nil
}

func (t *OCIIPTool) deleteSecondaryPrivateIP(ctx context.Context, privateIPID string) error {
	delURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps/%s", t.client.region, url.PathEscape(privateIPID))
	resp, body, err := t.client.Do(ctx, http.MethodDelete, delURL, nil)
	if err != nil {
		return fmt.Errorf("删除辅助私网 IP %s 失败: %w", privateIPID, err)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("删除辅助私网 IP %s 返回 HTTP %d: %s", privateIPID, resp.StatusCode, summarizeOCIResponse(body))
}

func (t *OCIIPTool) cleanupOrphanedProbePrivateIPs(ctx context.Context, vnicID string, iface string, maskBits int) {
	privURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/privateIps?vnicId=%s",
		t.client.region, url.QueryEscape(vnicID))
	resp, b, err := t.client.Do(ctx, http.MethodGet, privURL, nil)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	var privList []ociPrivateIPItem
	if err := json.Unmarshal(b, &privList); err != nil {
		return
	}
	for _, item := range privList {
		if !item.IsPrimary && strings.HasPrefix(item.DisplayName, "probe-") {
			slog.Info("清理残留的 probe 辅助私网 IP", "id", item.ID, "ip", item.IPAddress)
			_ = t.deleteSecondaryPrivateIP(ctx, item.ID)
			if iface != "" && item.IPAddress != "" && t.hostIP != nil {
				_ = t.hostIP.DelIP(ctx, item.IPAddress, maskBits, iface)
			}
		}
	}
}

func (t *OCIIPTool) bindPublicIP(ctx context.Context, publicIPID, privateIPID string) error {
	bindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodPut, bindURL, map[string]string{"privateIpId": privateIPID})
	if err != nil {
		return fmt.Errorf("绑定公网 IP %s 到私网 IP %s 失败: %w", publicIPID, privateIPID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("绑定公网 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	return nil
}

func (t *OCIIPTool) unbindPublicIP(ctx context.Context, publicIPID string) error {
	unbindURL := fmt.Sprintf("https://iaas.%s.oraclecloud.com/20160918/publicIps/%s", t.client.region, url.PathEscape(publicIPID))
	resp, body, err := t.client.Do(ctx, http.MethodPut, unbindURL, map[string]string{"privateIpId": ""})
	if err != nil {
		return fmt.Errorf("解绑预留公网 IP %s 失败: %w", publicIPID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("解绑预留公网 IP %s 返回 HTTP %d: %s", publicIPID, resp.StatusCode, summarizeOCIResponse(body))
	}
	return nil
}

func (t *OCIIPTool) probeCandidateConnectivity(ctx context.Context, candidateItem *ociPublicIPItem, requestedPort *int) (details string, resultErr error) {
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

	// 清理可能残留的历史 probe 辅助私网 IP
	t.cleanupOrphanedProbePrivateIPs(ctx, vnicInfo.vnicID, vnicInfo.ifaceName, vnicInfo.maskBits)

	// 1. 在 OCI 创建测试用辅助私网 IP
	displayName := fmt.Sprintf("probe-%d", time.Now().UnixNano())
	secPriv, err := t.createSecondaryPrivateIP(ctx, vnicInfo.vnicID, displayName)
	if err != nil {
		return "", fmt.Errorf("创建辅助私网 IP 失败: %w", err)
	}
	secPrivID := secPriv.ID
	secPrivIP := secPriv.IPAddress

	// Check must leave the reserved candidate intact. Only the temporary
	// public-to-private association and secondary private IP are cleaned up.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), publicIPPollTimeout+15*time.Second)
		defer cancel()

		if unbindErr := t.unbindPublicIP(cleanupCtx, candidateItem.ID); unbindErr != nil {
			slog.Warn("oci_ip_tool: 临时探测 IP 解绑请求失败，继续核对 OCI 最终状态", "errorType", fmt.Sprintf("%T", unbindErr))
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
		if t.hostIP != nil {
			if err := t.hostIP.DelIP(cleanupCtx, secPrivIP, vnicInfo.maskBits, vnicInfo.ifaceName); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("从网卡 %s 删除临时地址 %s 失败: %w", vnicInfo.ifaceName, secPrivIP, err))
			}
		}
		if cleanupErr := errors.Join(cleanupErrs...); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
			details += "\n临时绑定已确认解除，但辅助资源清理不完整；候选公网 IP 仍保留，请检查 OCI 状态。"
		}
	}()

	// 2. 本机网卡添加辅助私网 IP
	if t.hostIP != nil {
		if err := t.hostIP.AddIP(ctx, secPrivIP, vnicInfo.maskBits, vnicInfo.ifaceName); err != nil {
			return "", fmt.Errorf("向本机网卡 %s 添加辅助私网 IP %s 失败: %w", vnicInfo.ifaceName, secPrivIP, err)
		}
	}

	// 3. 将候选公网 IP 绑定到辅助私网 IP (绝对不影响主私网 IP 与当前公网 IP)
	if err := t.bindPublicIP(ctx, candidateItem.ID, secPrivID); err != nil {
		return "", fmt.Errorf("候选公网 IP 临时绑定到辅助私网失败: %w", err)
	}

	// 等待候选公网 IP 到达 ASSIGNED
	if _, waitErr := t.waitPublicIPLifecycle(ctx, candidateItem.ID, "ASSIGNED", secPrivID); waitErr != nil {
		return "", fmt.Errorf("等待候选公网 IP 临时绑定生效失败: %w", waitErr)
	}

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
		result, err := t.handleStatus(ctx)
		return redactOCIIPResult(ctx, result), err
	case "cleanup":
		result, err := t.handleCleanup(ctx, strings.TrimSpace(args.IP))
		return redactOCIIPResult(ctx, result), err
	case "check":
		if args.Port != nil && (*args.Port < 1 || *args.Port > 65535) {
			return nil, fmt.Errorf("port 必须在 1 到 65535 之间")
		}
		result, err := t.handleCheck(ctx, strings.TrimSpace(args.IP), args.Port)
		return redactOCIIPResult(ctx, result), err
	case "bind":
		result, err := t.handleBind(ctx, strings.TrimSpace(args.IP), args.DeleteOld)
		return redactOCIIPResult(ctx, result), err
	case "roll":
		noAbuser := false
		if args.NoAbuser != nil {
			noAbuser = *args.NoAbuser
		}
		maxRisk := 100
		if args.MaxRisk != nil {
			if *args.MaxRisk < 0 || *args.MaxRisk > 100 {
				return nil, fmt.Errorf("max_risk 必须在 0 到 100 之间")
			}
			maxRisk = *args.MaxRisk
		}
		result, err := t.handleRoll(ctx, args.Prefix, maxRisk, noAbuser)
		return redactOCIIPResult(ctx, result), err
	default:
		return nil, fmt.Errorf("unknown action: %s", action)
	}
}

func redactOCIIPResult(ctx context.Context, result *core.ToolResult) *core.ToolResult {
	if result == nil {
		return nil
	}
	if event := entity.SensoryEventFromContext(ctx); event == nil || !event.IsPartnerPrivate() {
		result.Text = core.RedactSensitiveIPs(result.Text)
		result.FallbackText = core.RedactSensitiveIPs(result.FallbackText)
	}
	return result
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

func (t *OCIIPTool) handleRoll(ctx context.Context, prefixes []string, maxRisk int, noAbuser bool) (*core.ToolResult, error) {
	maxAttempts := 15
	var rejected []string
	for _, prefix := range prefixes {
		if strings.TrimSpace(prefix) != prefix || prefix == "" {
			return nil, fmt.Errorf("IP 前缀不能为空或包含首尾空白")
		}
		parts := strings.Split(strings.TrimSuffix(prefix, "."), ".")
		if len(parts) > 4 {
			return nil, fmt.Errorf("IP 前缀最多包含四段 IPv4 数字")
		}
		for _, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n > 255 || strconv.Itoa(n) != part {
				return nil, fmt.Errorf("IP 前缀必须由 0 到 255 的 IPv4 数字段组成")
			}
		}
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
		for _, p := range prefixes {
			if strings.HasPrefix(candidateIP, p) {
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
