package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// SingboxExport 对应 singbox-links --json 输出的结构化数据
type SingboxExport struct {
	UUID             string            `json:"uuid"`
	SubscriptionURLs map[string]string `json:"subscription_urls"`
	NodeCount        int               `json:"node_count"`
	Nodes            []SingboxNode     `json:"nodes"`
}

// SingboxNode 单个 Sing-box 出站节点信息
type SingboxNode struct {
	Tag      string `json:"tag"`
	Type     string `json:"type"`
	Protocol string `json:"protocol"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	URL      string `json:"url"`
}

// SingboxLinksTool 获取当前 Sing-box 代理节点链接 (VLESS Reality 与 Hysteria 2)
type SingboxLinksTool struct {
	immune *entity.ImmuneSystem
}

func NewSingboxLinksTool(immune *entity.ImmuneSystem) *SingboxLinksTool {
	return &SingboxLinksTool{
		immune: immune,
	}
}

func (t *SingboxLinksTool) Name() string {
	return "singbox_links"
}

func (t *SingboxLinksTool) Description() string {
	return "获取当前服务器的 Sing-box 代理节点配置链接 (包含 VLESS Reality 与 Hysteria 2 订阅串)。当搭档询问代理节点、获取科学上网链接或换绑 IP 需要配置时调用。"
}

func (t *SingboxLinksTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ip": map[string]any{
				"type":        "string",
				"description": "可选指定公网 IP 地址。默认自动探测当前服务器公网 IP",
			},
		},
	}
}

type singboxLinksArgs struct {
	IP string `json:"ip"`
}

func (t *SingboxLinksTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	event := entity.SensoryEventFromContext(ctx)
	if event == nil || t.immune == nil {
		return &core.ToolResult{Text: "无法校验当前会话权限，拒绝读取 Sing-box 节点凭据。"}, nil
	}
	if event.IsGroup {
		return &core.ToolResult{Text: "Sing-box 节点链接包含密钥凭证，群聊中不便展示。请直接在私聊窗口向我发送'查看singbox节点'，我会在私聊中发给你。"}, nil
	}
	if !entity.ToolAllowed(t.Name(), event) {
		return &core.ToolResult{Text: "权限拦截：Sing-box 代理节点链接仅限核心搭档查询。"}, nil
	}

	var args singboxLinksArgs
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid singbox_links args: %w", err)
		}
	}

	output, err := runSingboxLinks(ctx, strings.TrimSpace(args.IP))
	if err != nil {
		return &core.ToolResult{
			Text:              fmt.Sprintf("获取 Sing-box 节点失败：%v", err),
			FixedPresentation: true,
		}, nil
	}

	return &core.ToolResult{
		Text:              FormatSingboxLinks(output),
		FixedPresentation: true,
	}, nil
}

func runSingboxLinks(ctx context.Context, targetIP string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("singbox-links requires a context")
	}
	commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"--json"}
	if targetIP != "" {
		args = append(args, targetIP)
	}
	cmd := core.CommandProcessGroup(commandCtx, "singbox-links", args...)
	cmd.WaitDelay = 2 * time.Second
	output := core.NewLimitedBuffer(1 << 20)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
		outStr := strings.TrimSpace(output.String())
		if outStr != "" {
			return "", fmt.Errorf("command failed (%w): %s", err, outStr)
		}
		return "", fmt.Errorf("command failed: %w", err)
	}
	if output.Truncated() {
		return "", fmt.Errorf("command output exceeded 1 MiB limit")
	}
	result := strings.TrimSpace(output.String())
	if result == "" {
		return "", fmt.Errorf("command returned no node links")
	}
	return result, nil
}

// FormatSingboxLinks 将 singbox-links --json 输出解析格式化为优雅的 Markdown
func FormatSingboxLinks(raw string) string {
	rawTrimmed := strings.TrimSpace(raw)
	if rawTrimmed == "" {
		return "未获取到 Sing-box 节点配置。"
	}

	var export SingboxExport
	if err := json.Unmarshal([]byte(rawTrimmed), &export); err != nil {
		return fmt.Sprintf("解析 Sing-box 节点数据失败：%v", err)
	}

	return formatSingboxJSON(export)
}

func formatSingboxJSON(export SingboxExport) string {
	var sb strings.Builder
	sb.WriteString("### 📱 Sing-box 客户端一键订阅\n")
	sb.WriteString("> 复制下方链接粘贴到客户端（Sing-box / Clash Verge / Shadowrocket / Box4HP 等）即可自动拉取全部最新节点：\n\n")

	if len(export.SubscriptionURLs) > 0 {
		if pub, ok := export.SubscriptionURLs["public"]; ok && strings.TrimSpace(pub) != "" {
			sb.WriteString("- **通用 HTTPS 订阅 (推荐)**：\n```text\n")
			sb.WriteString(strings.TrimSpace(pub))
			sb.WriteString("\n```\n")
		}
		if ipv4, ok := export.SubscriptionURLs["ipv4"]; ok && strings.TrimSpace(ipv4) != "" {
			sb.WriteString("- **IPv4 直连订阅**：\n```text\n")
			sb.WriteString(strings.TrimSpace(ipv4))
			sb.WriteString("\n```\n")
		}
		if ipv6, ok := export.SubscriptionURLs["ipv6"]; ok && strings.TrimSpace(ipv6) != "" {
			sb.WriteString("- **IPv6 直连订阅**：\n```text\n")
			sb.WriteString(strings.TrimSpace(ipv6))
			sb.WriteString("\n```\n")
		}
		for k, v := range export.SubscriptionURLs {
			if k != "public" && k != "ipv4" && k != "ipv6" && strings.TrimSpace(v) != "" {
				fmt.Fprintf(&sb, "- **%s 订阅**：\n```text\n%s\n```\n", strings.ToUpper(k), strings.TrimSpace(v))
			}
		}
	} else {
		sb.WriteString("暂无可用订阅链接。\n")
	}

	if len(export.Nodes) > 0 {
		fmt.Fprintf(&sb, "\n---\n### 🌐 独立代理节点（共 %d 个）\n", len(export.Nodes))
		sb.WriteString("> 点击下方代码块可一键长按复制单节点链接：\n\n")

		for _, node := range export.Nodes {
			tag := node.Tag
			if tag == "" {
				tag = fmt.Sprintf("%s-%d", node.Type, node.Port)
			}
			protoInfo := node.Type
			if node.Protocol != "" && !strings.EqualFold(node.Protocol, node.Type) {
				protoInfo = fmt.Sprintf("%s / %s", node.Type, strings.ToUpper(node.Protocol))
			}
			if node.Port > 0 {
				protoInfo = fmt.Sprintf("%s %d", protoInfo, node.Port)
			}
			fmt.Fprintf(&sb, "**%s** (%s)\n```text\n%s\n```\n\n", tag, protoInfo, strings.TrimSpace(node.URL))
		}
	}

	return strings.TrimSpace(sb.String())
}
