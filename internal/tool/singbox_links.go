package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

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
	if event == nil || t.immune == nil {
		return &core.ToolResult{Text: "无法校验当前会话权限，拒绝读取 Sing-box 节点凭据。"}, nil
	}
	if event.IsGroup {
		return &core.ToolResult{Text: "Sing-box 节点链接包含密钥凭证，群聊中不便展示。请直接在私聊窗口向我发送'查看singbox节点'，我会在私聊中发给你。"}, nil
	}
	if event.Role != entity.RolePartner {
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
		return &core.ToolResult{Text: fmt.Sprintf("获取 Sing-box 节点失败：%v", err)}, nil
	}

	return &core.ToolResult{
		Text: FormatSingboxLinks(output),
	}, nil
}

func runSingboxLinks(ctx context.Context, targetIP string) (string, error) {
	if ctx == nil {
		return "", fmt.Errorf("singbox-links requires a context")
	}
	commandCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{}
	if targetIP != "" {
		args = append(args, targetIP)
	}
	cmd := exec.CommandContext(commandCtx, "singbox-links", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	output := core.NewLimitedBuffer(1 << 20)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Run(); err != nil {
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
