package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"daidai/internal/core"
	"daidai/internal/entity"
)

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
	return "获取当前服务器的 Sing-box 代理节点配置链接 (包含 VLESS Reality 与 Hysteria 2 订阅串)。当搭档询问代理节点、获取科学上网链接或换绑 IP 需要配置时调用。仅限核心搭档私聊调用以防节点凭据泄露。"
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
	if event != nil {
		if event.IsGroup {
			return &core.ToolResult{Text: "安全拦截：代理节点链接包含敏感连接凭证，禁止在群聊中发送，请通过私聊发起查询。"}, nil
		}
		if t.immune != nil && event.Role != entity.RolePartner {
			return &core.ToolResult{Text: "权限拦截：Sing-box 代理节点链接仅限核心搭档查询。"}, nil
		}
	}

	var args singboxLinksArgs
	if len(rawArgs) > 0 {
		_ = json.Unmarshal(rawArgs, &args)
	}

	targetIP := strings.TrimSpace(args.IP)
	var cmd *exec.Cmd
	if targetIP != "" {
		cmd = exec.CommandContext(ctx, "singbox-links", targetIP)
	} else {
		cmd = exec.CommandContext(ctx, "singbox-links")
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return &core.ToolResult{
			Text: fmt.Sprintf("获取 Sing-box 节点失败 (命令执行异常): %s (%v)", strings.TrimSpace(string(out)), err),
		}, nil
	}

	return &core.ToolResult{
		Text: FormatSingboxLinks(string(out)),
	}, nil
}

// FormatSingboxLinks 将 singbox-links 命令行原始输出格式化为优雅的 Markdown 代码块
func FormatSingboxLinks(raw string) string {
	var lines []string
	lines = append(lines, "### Sing-box 代理节点配置")
	for _, rawLine := range strings.Split(strings.TrimSpace(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			lines = append(lines, fmt.Sprintf("\n**%s**", strings.TrimSpace(strings.TrimPrefix(line, "#"))))
		} else {
			lines = append(lines, fmt.Sprintf("```text\n%s\n```", line))
		}
	}
	return strings.Join(lines, "\n")
}
