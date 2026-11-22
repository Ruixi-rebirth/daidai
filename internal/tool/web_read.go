package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"

	"daidai/internal/core"
	"daidai/internal/entity"
	"daidai/internal/provider/llm"
)

// ReadLinkTool reads a user supplied page and performs the requested task.
// It asks AGY first and falls back to a protected HTTP fetch.
type ReadLinkTool struct{ search *WebSearchTool }

type webPageChunk struct {
	URL        string `json:"url"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	TotalRunes int    `json:"total_runes"`
	Complete   bool   `json:"complete"`
	Content    string `json:"content"`
}

func NewReadLinkToolWithCommand(router *llm.MindRouter, command string) *ReadLinkTool {
	return &ReadLinkTool{search: NewWebSearchToolWithCommand(router, command)}
}
func (t *ReadLinkTool) Name() string { return "read_link" }
func (t *ReadLinkTool) Description() string {
	return "读取指定网页正文并按用户原话完成任务；长文分页时用 next_offset 续读。处理多个网页时，先用已读正文回答，说明未读完或未读到的链接。不截图。"
}
func (t *ReadLinkTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"url":  map[string]any{"type": "string", "description": "用户提供的完整 HTTP/HTTPS 网页链接"},
		"task": map[string]any{"type": "string", "description": "可选；仅当该链接有独立于当前用户原话的处理要求时填写"},
		"offset": map[string]any{
			"type":        "integer",
			"description": "可选正文起始字符偏移量。仅在工具返回 next_offset 时，将其传入以续读同一网页。默认为 0",
		},
	}, "required": []string{"url"}}
}
func (t *ReadLinkTool) Execute(ctx context.Context, raw json.RawMessage) (*core.ToolResult, error) {
	if ctx == nil {
		return nil, fmt.Errorf("网页处理需要有效的请求上下文")
	}
	var args struct {
		URL    string `json:"url"`
		Task   string `json:"task"`
		Offset int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid read_link args: %w", err)
	}
	target, err := core.NormalizeExternalURL(args.URL)
	if err != nil {
		return &core.ToolResult{Text: "网页链接无效：" + err.Error()}, nil
	}
	task := strings.TrimSpace(args.Task)
	if task == "" {
		if event := entity.SensoryEventFromContext(ctx); event != nil {
			task = strings.TrimSpace(event.Content)
		}
		if task == "" {
			task = "概括页面主要内容"
		}
	}
	if len([]rune(task)) > core.MaxUserMessageRunes {
		return &core.ToolResult{Text: fmt.Sprintf("网页处理要求不能超过 %d 个字符，请概括具体目标后重试。", core.MaxUserMessageRunes)}, nil
	}

	start := max(0, args.Offset)
	if start == 0 {
		if err := core.ValidateSafeExternalURL(ctx, target); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return &core.ToolResult{Text: "无法验证网页链接为可访问的公开地址，请核对链接后重试。"}, nil
		}
	}

	// Leave time for the outer conversation to synthesize and send the answer.
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(6 * time.Minute)
	}
	readCtx, cancel := context.WithDeadline(ctx, deadline.Add(-20*time.Second))
	defer cancel()
	label := core.TruncateRunes(target, 240, "…")
	var result *core.ToolResult
	var readErr error
	if start == 0 && readCtx.Err() == nil {
		result, readErr = readPageWithFallback(readCtx, 45*time.Second,
			func(ctx context.Context) (*core.ToolResult, error) {
				parsed, err := t.search.run(ctx, webRequest{URL: target, Task: task})
				if err != nil {
					return nil, err
				}
				answer, err := readAGYPageAnswer(parsed)
				if err != nil {
					return nil, err
				}
				parsed.Response = answer
				return pageResult("网页："+label+"\n"+webSearchResultText(parsed), target, "已读到正文"), nil
			},
			func(ctx context.Context) (*core.ToolResult, error) { return readDirectPage(ctx, target, start) })
	} else if readCtx.Err() == nil {
		// Offsets belong to the direct-fetch text and must stay on that path.
		result, readErr = readDirectPage(readCtx, target, start)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if result != nil {
		return result, nil
	}
	if readCtx.Err() != nil {
		return pageResult("本轮网页读取时间不足，尚未读到所需正文："+label, target, "时间不足"), nil
	}
	if readErr != nil {
		slog.Warn("read_link: 未能读取正文", "host", safeLogURL(target), "err", readErr)
	}
	return pageResult("未能读取该网页正文："+label, target, "未读到所需正文"), nil
}

func pageResult(text, target, status string) *core.ToolResult {
	return &core.ToolResult{Text: text, PageProgress: &core.PageReadProgress{URL: target, Status: status}}
}

type pageReader func(context.Context) (*core.ToolResult, error)

func readPageWithFallback(ctx context.Context, delay time.Duration, agy, direct pageReader) (*core.ToolResult, error) {
	type outcome struct {
		result *core.ToolResult
		err    error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	run := func(read pageReader) <-chan outcome {
		// A cancelled losing reader can still publish its result and exit.
		out := make(chan outcome, 1)
		go func() {
			result, err := read(ctx)
			out <- outcome{result: result, err: err}
		}()
		return out
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	agyResult, startFallback := run(agy), timer.C
	var directResult <-chan outcome
	var lastErr error
	for agyResult != nil || directResult != nil || startFallback != nil {
		select {
		case <-ctx.Done():
			if agyResult != nil && ctx.Err() == context.DeadlineExceeded {
				slog.Info("read_link: AGY 正文读取到达本轮时间预算")
			}
			return nil, ctx.Err()
		case <-startFallback:
			startFallback = nil
			slog.Info("read_link: AGY 仍在读取，启动 Go 兜底")
			directResult = run(direct)
		case final := <-agyResult:
			agyResult = nil
			if final.err == nil && final.result != nil {
				return final.result, nil
			}
			lastErr = final.err
			slog.Info("read_link: AGY 未返回可用结果", "err", final.err)
			if startFallback != nil {
				startFallback = nil
				timer.Stop()
				directResult = run(direct)
			}
		case final := <-directResult:
			directResult = nil
			if final.err == nil && final.result != nil {
				slog.Info("read_link: Go 兜底读取到正文")
				return final.result, nil
			}
			lastErr = final.err
			slog.Info("read_link: Go 兜底没有可用正文", "err", final.err)
		}
	}
	return nil, lastErr
}

func readDirectPage(ctx context.Context, target string, start int) (*core.ToolResult, error) {
	directCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	content, err := fetchDirectWebPage(directCtx, target)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("网页没有可用正文")
	}
	runes := []rune(content)
	start = min(start, len(runes))
	end := min(start+16000, len(runes))
	chunk := webPageChunk{URL: target, Offset: start, TotalRunes: len(runes), Complete: end == len(runes), Content: string(runes[start:end])}
	if !chunk.Complete {
		chunk.NextOffset = end
	}
	return pageChunkResult(chunk)
}

func readAGYPageAnswer(result llm.AGYResult) (string, error) {
	if result.FailureDetail() != "" {
		return "", fmt.Errorf("AGY 执行失败")
	}
	if core.OutputLimitReached(result.FinishedReason) {
		return "", fmt.Errorf("AGY 输出达到上限")
	}
	body := strings.TrimSpace(result.Response)
	if fenced, ok := strings.CutPrefix(body, "```"); ok {
		language, rest, hasLine := strings.Cut(fenced, "\n")
		content, closed := strings.CutSuffix(strings.TrimSpace(rest), "```")
		if !hasLine || !closed || (strings.TrimSpace(language) != "" && !strings.EqualFold(strings.TrimSpace(language), "json")) {
			return "", fmt.Errorf("AGY 返回的正文结果格式不完整")
		}
		body = content
	}
	var page struct {
		Read   bool   `json:"read"`
		Answer string `json:"answer"`
	}
	if json.Unmarshal([]byte(body), &page) != nil {
		return "", fmt.Errorf("AGY 返回的正文结果格式不完整")
	}
	if !page.Read {
		return "", fmt.Errorf("AGY 未读到网页正文")
	}
	answer := strings.TrimSpace(page.Answer)
	if answer == "" {
		return "", fmt.Errorf("AGY 没有返回基于正文的回答")
	}
	return answer, nil
}

func pageChunkResult(chunk webPageChunk) (*core.ToolResult, error) {
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("encode webpage text chunk: %w", err)
	}
	status := "已读到正文"
	if !chunk.Complete {
		status = "已读部分正文，可继续读取"
	}
	return pageResult(string(encoded), chunk.URL, status), nil
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == http.StatusPartialContent {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	if resp.Request != nil && resp.Request.URL.Path != req.URL.Path {
		switch strings.ToLower(strings.TrimSuffix(resp.Request.URL.Path, "/")) {
		case "/login", "/signin", "/sign-in", "/session", "/auth/login", "/users/sign_in":
			return "", fmt.Errorf("网页重定向到了登录页")
		}
	}

	// 限制最大读取 4MB，防止超大文件耗尽内存
	body, err := core.ReadBounded(resp.Body, 4<<20)
	if err != nil {
		return "", err
	}

	if isGitHubCommit {
		return string(body), nil
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/plain") || strings.Contains(contentType, "application/json") || strings.Contains(contentType, "text/markdown") {
		return string(body), nil
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
				if articleNode == nil && !insideHiddenHTML(n) {
					articleNode = n
				}
			case "body":
				if bodyNode == nil && !insideHiddenHTML(n) {
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
	hasPasswordInput := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if sb.Len() > 300000 {
			return
		}
		if n.Type == html.ElementNode {
			if hasHiddenHTMLAttribute(n) {
				return
			}
			switch n.Data {
			case "form":
				for child := range n.Descendants() {
					if child.Type == html.ElementNode && child.Data == "input" {
						for _, attr := range child.Attr {
							if attr.Key == "type" && strings.EqualFold(attr.Val, "password") && !insideHiddenHTML(child) {
								hasPasswordInput = true
							}
						}
					}
				}
				return
			case "script", "style", "nav", "footer", "header", "svg", "noscript", "iframe", "template":
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
	if hasPasswordInput && utf8.RuneCountInString(res) < 200 {
		return "", fmt.Errorf("网页只返回登录界面及少量说明")
	}
	if utf8.RuneCountInString(res) < 50 {
		return "", fmt.Errorf("extracted content too short")
	}
	if title != "" {
		res = "【页面标题】：" + title + "\n\n【页面正文】：\n" + res
	}
	return res, nil
}

func hasHiddenHTMLAttribute(node *html.Node) bool {
	for _, attr := range node.Attr {
		if attr.Namespace == "" && attr.Key == "hidden" {
			return true
		}
	}
	return false
}

func insideHiddenHTML(node *html.Node) bool {
	for current := node; current != nil; current = current.Parent {
		if hasHiddenHTMLAttribute(current) {
			return true
		}
	}
	return false
}
