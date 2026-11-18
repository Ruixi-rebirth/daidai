package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"daidai/internal/core"
	"daidai/internal/provider/llm"
)

// ReadLinkTool reads a user supplied page and performs the requested task.
// It prioritizes instant direct HTTP fetching via ProtectedHTTPClient,
// falling back to the read-only AGY sandbox when direct fetching is unavailable.
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
	if len([]rune(task)) > core.MaxUserMessageRunes {
		return &core.ToolResult{Text: fmt.Sprintf("网页处理要求不能超过 %d 个字符，请概括具体目标后重试。", core.MaxUserMessageRunes)}, nil
	}

	// 1. 优先尝试 Go 原生受保护客户端直接抓取（毫秒级极速响应，自带防 SSRF/DNS 重绑定）
	directCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	content, fetchErr := fetchDirectWebPage(directCtx, target)
	cancel()
	if fetchErr == nil && strings.TrimSpace(content) != "" {
		slog.Info("read_link: 原生直接抓取成功", "url", target, "runes", utf8.RuneCountInString(content))
		return &core.ToolResult{
			Text: fmt.Sprintf("【已直接读取网页正文（URL: %s，任务要求：%s）】：\n%s", target, task, content),
		}, nil
	}
	if fetchErr != nil {
		slog.Warn("read_link: 原生直接抓取失败，回退到 AGY 沙箱抓取", "url", target, "err", fetchErr)
	}

	// 2. 原生读取不适用或受阻时，回退到受控沙箱
	return t.search.run(ctx, webRequest{URL: target, Task: task})
}

// fetchDirectWebPage attempts to fetch and extract readable text from targetURL
// using ProtectedHTTPClient. It returns the extracted text or an error.
func fetchDirectWebPage(ctx context.Context, targetURL string) (string, error) {
	u, err := url.Parse(targetURL)
	if err != nil {
		return "", err
	}

	client := core.NewProtectedHTTPClient(12 * time.Second)

	reqURL := targetURL
	host := strings.ToLower(u.Hostname())
	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")

	// GitHub commit 快捷路径：/commit/<sha> -> 获取 .patch 纯文本补丁
	isGitHubCommit := (host == "github.com" || host == "www.github.com") &&
		len(pathParts) >= 4 && pathParts[2] == "commit" && len(pathParts[3]) >= 7

	if isGitHubCommit {
		reqURL = strings.TrimSuffix(targetURL, "/") + ".patch"
	}

	// GitHub blob 文件快捷路径：/blob/branch/... -> raw.githubusercontent.com
	if (host == "github.com" || host == "www.github.com") &&
		len(pathParts) >= 4 && pathParts[2] == "blob" {
		reqURL = fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", pathParts[0], pathParts[1], strings.Join(pathParts[3:], "/"))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,*/*;q=0.7")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	// 限制最大读取 512KB，防止超大文件耗尽内存
	lr := io.LimitReader(resp.Body, 512<<10)
	body, err := io.ReadAll(lr)
	if err != nil {
		return "", err
	}

	if isGitHubCommit {
		text := string(body)
		if utf8.RuneCountInString(text) > 20000 {
			text = string([]rune(text)[:20000]) + "\n\n...[Git Diff 内容过长，已截断]"
		}
		return text, nil
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/markdown") {
		text := string(body)
		if utf8.RuneCountInString(text) > 20000 {
			text = string([]rune(text)[:20000]) + "\n\n...[文本内容过长，已截断]"
		}
		return text, nil
	}

	return extractHTMLContent(body)
}

// extractHTMLContent parses HTML bytes and extracts the page title and clean visible text.
func extractHTMLContent(raw []byte) (string, error) {
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}

	var title string
	var articleNode *html.Node
	var bodyNode *html.Node

	for n := range root.Descendants() {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if title == "" && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					title = strings.TrimSpace(n.FirstChild.Data)
				}
			case "article", "main":
				if articleNode == nil {
					articleNode = n
				}
			case "body":
				if bodyNode == nil {
					bodyNode = n
				}
			}
		}
	}

	targetNode := articleNode
	if targetNode == nil {
		targetNode = bodyNode
	}
	if targetNode == nil {
		targetNode = root
	}

	var sb strings.Builder
	if title != "" {
		sb.WriteString("【页面标题】：")
		sb.WriteString(title)
		sb.WriteString("\n\n【页面正文】：\n")
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if sb.Len() > 25000 {
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "footer", "header", "form", "svg", "noscript", "iframe":
				return
			case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "li", "tr", "br":
				defer sb.WriteString("\n")
			}
		}
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				sb.WriteString(text)
				sb.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(targetNode)

	res := strings.TrimSpace(sb.String())
	lines := strings.Split(res, "\n")
	var cleaned []string
	blankCount := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			blankCount++
			if blankCount <= 1 {
				cleaned = append(cleaned, "")
			}
		} else {
			blankCount = 0
			cleaned = append(cleaned, trimmed)
		}
	}
	res = strings.Join(cleaned, "\n")
	if utf8.RuneCountInString(res) < 50 {
		return "", fmt.Errorf("extracted content too short")
	}
	return res, nil
}
