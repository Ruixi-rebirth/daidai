package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"daidai/internal/core"
	"daidai/internal/provider/llm"
)

// ReadLinkTool reads a user supplied page and performs the requested task
// through the existing read-only AGY web sandbox.
type ReadLinkTool struct{ search *WebSearchTool }

func NewReadLinkToolWithCommand(router *llm.MindRouter, command string) *ReadLinkTool {
	return &ReadLinkTool{search: NewWebSearchToolWithCommand(router, command)}
}
func (t *ReadLinkTool) Name() string { return "read_link" }
func (t *ReadLinkTool) Description() string {
	return "读取用户给出的网页正文，并根据用户原话完成总结、回答问题或提取信息。必须打开指定链接并以页面正文为依据；打不开时明确说明。多个链接逐个调用并综合结果。此工具不截图。"
}
func (t *ReadLinkTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"url":  map[string]any{"type": "string", "description": "用户提供的完整 HTTP/HTTPS 网页链接"},
		"task": map[string]any{"type": "string", "description": "结合用户整句话填写要对网页做的事，例如总结并回答某个问题"},
	}, "required": []string{"url", "task"}}
}
func (t *ReadLinkTool) Execute(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	var args struct {
		URL  string `json:"url"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid read_link args: %w", err)
	}
	target, err := normalizeExternalURL(args.URL)
	if err != nil {
		return &core.ToolResult{Text: "网页链接无效：" + err.Error()}, nil
	}
	task := strings.TrimSpace(args.Task)
	if task == "" {
		task = "概括页面主要内容"
	}
	if len([]rune(task)) > 500 {
		task = string([]rune(task)[:500])
	}
	query, _ := json.Marshal(map[string]string{"query": target + " 用户任务：" + task})
	return t.search.Execute(ctx, query)
}
