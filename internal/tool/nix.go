package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"daidai/internal/core"
)

type NixSearchTool struct{}

func NewNixSearchTool() *NixSearchTool {
	return &NixSearchTool{}
}

func (t *NixSearchTool) Name() string {
	return "search_nix_package"
}

func (t *NixSearchTool) Description() string {
	return "在 NixOS / nixpkgs 官方软件仓库中检索软件包。当用户询问某个软件包在 Nix 中是否存在、对应属性名是什么、当前可用版本或描述时调用。"
}

func (t *NixSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "要查询的 Nix 软件包名称或关键字，最多 200 字，例如 ripgrep, typst, neovim 等",
			},
		},
		"required": []string{"query"},
	}
}

type nixSearchArgs struct {
	Query string `json:"query"`
}

func (t *NixSearchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args nixSearchArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid search_nix_package args: %w", err)
	}

	query := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(args.Query), "-"))
	if query == "" {
		return nil, fmt.Errorf("search query cannot be empty")
	}
	if len([]rune(query)) > 200 {
		return &core.ToolResult{Text: "Nix 软件包搜索词不能超过 200 个字符。"}, nil
	}

	execCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(execCtx, "nix", "--extra-experimental-features", "nix-command flakes", "search", "nixpkgs", "--json", "--", regexp.QuoteMeta(query))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil && cmd.Process.Pid > 0 {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second

	outBuf := core.NewLimitedBuffer(4 << 20)
	errBuf := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	err := cmd.Run()
	if err != nil {
		return &core.ToolResult{
			Text: fmt.Sprintf("检索 Nix 软件包 [%s] 失败或未匹配到结果: %v", query, err),
		}, nil
	}
	if outBuf.Truncated() {
		return &core.ToolResult{Text: "Nix 搜索结果超过 4 MiB 上限，未能完整解析。"}, nil
	}

	var results map[string]struct {
		Pname       string `json:"pname"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}

	if err := json.Unmarshal(outBuf.Bytes(), &results); err != nil || len(results) == 0 {
		return &core.ToolResult{
			Text: fmt.Sprintf("未在 nixpkgs 中检索到与 [%s] 匹配的软件包。", query),
		}, nil
	}

	attrs := make([]string, 0, len(results))
	for attr := range results {
		attrs = append(attrs, attr)
	}
	sort.Strings(attrs)

	var lines []string
	for i, attr := range attrs {
		if i >= 8 {
			break
		}
		item := results[attr]
		name := item.Pname
		if name == "" {
			parts := strings.Split(attr, ".")
			name = parts[len(parts)-1]
		}
		ver := item.Version
		desc := strings.TrimSpace(item.Description)
		if desc == "" {
			desc = "暂无描述"
		}
		lines = append(lines, fmt.Sprintf("- 属性: %s | 包名: %s | 版本: %s | 描述: %s",
			truncateNixField(attr, 200), truncateNixField(name, 200), truncateNixField(ver, 100), truncateNixField(desc, 500)))
	}

	summary := fmt.Sprintf("在 nixpkgs 找到关于 [%s] 的相关软件包:\n%s", query, strings.Join(lines, "\n"))
	return &core.ToolResult{
		Text: summary,
	}, nil
}

func truncateNixField(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}
