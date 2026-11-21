package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"daidai/internal/config"
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
	cfConfig    *config.CloudflareConfig
	operationMu chan struct{}
	qualityHTTP *http.Client
	hostIP      hostIPManager
	portLister  portLister
}

func NewOCIIPTool(client *OCIClient, immune *entity.ImmuneSystem) *OCIIPTool {
	return NewOCIIPToolWithCloudflare(client, immune, nil)
}

func NewOCIIPToolWithCloudflare(client *OCIClient, immune *entity.ImmuneSystem, cfConfig *config.CloudflareConfig) *OCIIPTool {
	return &OCIIPTool{
		client:      client,
		immune:      immune,
		cfConfig:    cfConfig,
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
	return "管理 OCI 预留公网 IP 与 Cloudflare A 记录。画像未知不等于风险高；探针失败不等于 IP 不可用或被墙，也不保证网段质量。"
}

func (t *OCIIPTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "status 查询；cleanup 删除闲置 IP；roll 申请并保留候选（不自动绑定）；check 读取本机 TCP 监听和 UDP 绑定套接字，并从国内探针验证适用的 TCP 监听端口；bind 换绑；sync_dns 手动同步或重试将 Cloudflare DNS 的 A 记录更新为目标公网 IP（默认当前主网卡 IP）",
				"enum":        []string{"status", "cleanup", "roll", "check", "bind", "sync_dns"},
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
	case "sync_dns":
		result, err := t.handleSyncDNS(ctx, strings.TrimSpace(args.IP))
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
