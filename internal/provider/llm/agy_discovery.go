package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"daidai/internal/core"
)

func (p *AGYMindProvider) DiscoverModels(ctx context.Context) []ModelInfo {
	models, err := p.DiscoverModelsWithError(ctx)
	if err != nil {
		slog.Warn("agy_mind: 动态模型探测失败", "err", err)
	}
	return models
}

func (p *AGYMindProvider) DiscoverModelsWithError(ctx context.Context) ([]ModelInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.RLock()
	command := p.command
	channel := p.channelName
	p.mu.RUnlock()

	discCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := core.CommandProcessGroup(discCtx, command, "models")
	cmd.WaitDelay = 2 * time.Second

	outBuf := core.NewLimitedBuffer(1 << 20)
	errBuf := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("run %s models: %w", command, err)
	}
	if outBuf.Truncated() {
		return nil, fmt.Errorf("%s models output exceeds 1 MiB limit", command)
	}

	models := ParseCLIModelsOutput(outBuf.String(), channel)
	if len(models) == 0 {
		return nil, fmt.Errorf("%s models returned no recognizable models", command)
	}
	p.mu.Lock()
	if p.model == "" {
		p.model = models[0].ID
	}
	if p.effort == "" && models[0].DefaultEffort != "" {
		p.effort = models[0].DefaultEffort
	}
	p.mu.Unlock()
	slog.Info("agy_mind: 成功从 CLI 动态探测可用模型列表", "command", command, "count", len(models))
	return models, nil
}

type agyUsageBucket struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Description       string  `json:"description"`
	Window            string  `json:"window"`
	RemainingFraction float64 `json:"remaining_fraction"`
	ResetTime         string  `json:"reset_time"`
}

type agyUsageGroup struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Buckets     []agyUsageBucket `json:"buckets"`
}

type agyUsageEnvelope struct {
	Status   string `json:"status"`
	Response string `json:"response"`
	Command  struct {
		Name string `json:"name"`
		Data struct {
			Description string          `json:"description"`
			Groups      []agyUsageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// FormatAGYUsageOutput 将 `agy -p "/usage" --output-format json` 响应解析并格式化为结构化 Markdown
func FormatAGYUsageOutput(raw []byte) (string, error) {
	var env agyUsageEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("解析 AGY 配额 JSON 失败: %w", err)
	}
	status := strings.ToUpper(strings.TrimSpace(env.Status))
	if status != "" && status != "SUCCESS" {
		detail := strings.TrimSpace(env.Response)
		if detail == "" {
			detail = status
		}
		if runes := []rune(detail); len(runes) > 4000 {
			detail = string(runes[:4000]) + "…"
		}
		return "", fmt.Errorf("AGY 配额查询返回非成功状态 %q: %s", status, detail)
	}

	groups := env.Command.Data.Groups
	if len(groups) == 0 {
		if strings.TrimSpace(env.Response) != "" {
			return strings.TrimSpace(env.Response), nil
		}
		return "", fmt.Errorf("未从 AGY 响应中提取到有效配额组数据")
	}

	var sb strings.Builder
	sb.WriteString("### AGY 实时配额与剩余使用量 (Quota Pools)\n\n")

	for i, g := range groups {
		fmt.Fprintf(&sb, "#### %d. %s\n", i+1, g.Name)
		if g.Description != "" {
			fmt.Fprintf(&sb, "- 说明：%s\n", g.Description)
		}
		for _, b := range g.Buckets {
			remPct := b.RemainingFraction * 100
			resetDesc := ""
			if b.ResetTime != "" {
				if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
					resetDesc = fmt.Sprintf("，刷新时间: %s", t.Local().Format("2006-01-02 15:04:05"))
				} else {
					resetDesc = fmt.Sprintf("，刷新时间: %s", b.ResetTime)
				}
			}
			hint := ""
			if b.Description != "" {
				hint = fmt.Sprintf(" (%s)", b.Description)
			}
			fmt.Fprintf(&sb, "- **%s**：剩余 **%.1f%%**%s%s\n", b.Name, remPct, resetDesc, hint)
		}
		sb.WriteString("\n")
	}

	return strings.TrimSpace(sb.String()), nil
}

// GetUsage 查询 AGY 底层当前各大配额池的实时剩余使用量与重置倒计时 (实现 UsageDiscoverer)
func (p *AGYMindProvider) GetUsage(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	p.mu.RLock()
	command := p.command
	cwd := p.workingDir
	p.mu.RUnlock()

	if err := CheckAGYAuth(); err != nil {
		return "", err
	}

	usageCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	cmd := core.CommandProcessGroup(usageCtx, command, "-p", "/usage", "--output-format", "json")
	if cwd != "" {
		cmd.Dir = cwd
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Env = append(os.Environ(), "HOME="+home)
	}
	cmd.WaitDelay = 2 * time.Second

	outBuf := core.NewLimitedBuffer(256 << 10)
	errBuf := core.NewLimitedBuffer(256 << 10)
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("执行 %s -p /usage 失败: %w", command, err)
	}
	if outBuf.Truncated() {
		return "", fmt.Errorf("执行 %s -p /usage 失败：输出超过 256 KiB 上限", command)
	}

	return FormatAGYUsageOutput(outBuf.Bytes())
}

// ParseCLIModelsOutput 解析 `agy models` 标准输出，自动聚合模型 ID、展示名称与支持的思考强度列表
func ParseCLIModelsOutput(rawOutput, channel string) []ModelInfo {
	if channel == "" {
		channel = "AGY"
	}

	var order []string
	byID := make(map[string]*ModelInfo)

	lines := strings.Split(rawOutput, "\n")
	for _, line := range lines {
		// 清理终端 spinner 控制字符 (\r) 并截取最后一段有效文本
		if idx := strings.LastIndex(line, "\r"); idx != -1 {
			line = line[idx+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Fetching") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		rawID := fields[0]
		displayName := strings.TrimSpace(line[len(rawID):])

		baseID := rawID
		effort := ""
		openParen := strings.LastIndex(displayName, "(")
		if openParen > 0 && strings.HasSuffix(displayName, ")") {
			effortLabel := strings.TrimSpace(displayName[openParen+1 : len(displayName)-1])
			if isSupportedEffortLabel(effortLabel) {
				candidateID, candidateEffort := SplitModelAndEffort(rawID)
				if candidateEffort != "" && strings.EqualFold(candidateEffort, effortLabel) {
					baseID = candidateID
					effort = candidateEffort
					displayName = strings.TrimSpace(displayName[:openParen])
				}
			}
		}
		if baseID == "" {
			continue
		}

		if openParen := strings.LastIndex(displayName, "("); openParen > 0 && strings.HasSuffix(displayName, ")") {
			parentheticalID := strings.TrimSpace(displayName[openParen+1 : len(displayName)-1])
			if strings.EqualFold(parentheticalID, baseID) {
				displayName = strings.TrimSpace(displayName[:openParen])
			}
		}
		if displayName == "" {
			displayName = baseID
		}

		info, exists := byID[baseID]
		if !exists {
			info = &ModelInfo{
				ID:      baseID,
				Channel: channel,
				Name:    displayName,
			}
			byID[baseID] = info
			order = append(order, baseID)
		}

		if effort != "" {
			info.SupportsEffort = true
			hasEffort := false
			for _, e := range info.SupportedEfforts {
				if e == effort {
					hasEffort = true
					break
				}
			}
			if !hasEffort {
				info.SupportedEfforts = append(info.SupportedEfforts, effort)
			}
			if info.DefaultEffort == "" || effort == "medium" {
				info.DefaultEffort = effort
			}
		}
	}

	result := make([]ModelInfo, 0, len(order))
	for _, id := range order {
		result = append(result, *byID[id])
	}
	return result
}

func isSupportedEffortLabel(label string) bool {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// SetModel 动态设置当前模型
