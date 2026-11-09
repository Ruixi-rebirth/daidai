package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/entity"
)

// ReadLinkTool 外部链接抓取与正文阅读工具 (实现 core.Tool)
// 优先走 Jina Reader 提取干净 Markdown，兜底走 WARP 隔离 HTTP 客户端；全程受 SSRF 阻断与 IP 脱敏保护
type ReadLinkTool struct {
	timeout time.Duration
}

// NewReadLinkTool 实例化链接阅读工具
func NewReadLinkTool() *ReadLinkTool {
	return &ReadLinkTool{
		timeout: 25 * time.Second,
	}
}

func (t *ReadLinkTool) Name() string {
	return "read_link"
}

func (t *ReadLinkTool) Description() string {
	return "抓取并阅读指定公网网页的纯文本与 Markdown 正文内容（纯文本模式，不生成截图长图）。单次对话最多处理 2 个链接。"
}

func (t *ReadLinkTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "需要抓取阅读的完整 HTTP/HTTPS 网页链接，例如 https://example.com/article",
			},
		},
		"required": []string{"url"},
	}
}

type readLinkArgs struct {
	URL string `json:"url"`
}

func (t *ReadLinkTool) Execute(ctx context.Context, rawArgs json.RawMessage) (*core.ToolResult, error) {
	var args readLinkArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("invalid read_link args: %w", err)
	}

	targetURL := strings.TrimSpace(args.URL)
	if targetURL == "" {
		return &core.ToolResult{Text: "网页链接不能为空。"}, nil
	}

	// 补全协议头
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		targetURL = "https://" + targetURL
	}

	parsedURL, err := url.Parse(targetURL)
	if err != nil || parsedURL.Host == "" {
		return &core.ToolResult{Text: fmt.Sprintf("无效的网页链接: %s", targetURL)}, nil
	}

	// 严格校验公网地址，阻断内网 SSRF
	if err := core.ValidateSafeExternalURL(ctx, targetURL); err != nil {
		slog.Warn("read_link: 拦截不安全的外部链接", "url", targetURL, "err", err)
		return &core.ToolResult{Text: fmt.Sprintf("链接安全检查未通过: %v", err)}, nil
	}

	readCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	slog.Info("read_link: 开始抓取网页内容", "url", targetURL)

	content, title, fetchErr := FetchURLContent(readCtx, targetURL)
	if fetchErr != nil {
		slog.Warn("read_link: 抓取网页失败", "url", targetURL, "err", fetchErr)
		return &core.ToolResult{Text: fmt.Sprintf("抓取网页内容失败: %v", fetchErr)}, nil
	}

	// 敏感 IP 脱敏
	event := entity.SensoryEventFromContext(ctx)
	if event == nil || !event.IsPartnerPrivate() {
		content = core.RedactSensitiveIPs(content)
	}

	content = safeTruncateRune(content, 10000)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("【网页阅读结果】\n- 链接: %s\n", targetURL))
	if title != "" {
		sb.WriteString(fmt.Sprintf("- 标题: %s\n", title))
	}
	sb.WriteString("\n")
	sb.WriteString(content)

	return &core.ToolResult{
		Text: sb.String(),
	}, nil
}

// FetchURLContent 获取外部 URL 的正文内容，优先 Jina Reader，备用 WARP HTTP 客户端
func FetchURLContent(ctx context.Context, targetURL string) (string, string, error) {
	// 策略一：尝试 Jina Reader 获取结构化 Markdown
	jinaContent, jinaTitle, err := fetchViaJina(ctx, targetURL)
	if err == nil && len(strings.TrimSpace(jinaContent)) >= 50 {
		return jinaContent, jinaTitle, nil
	}
	if err != nil {
		slog.Debug("read_link: Jina Reader 抓取未果，回退至原生 HTTP", "url", targetURL, "err", err)
	}

	// 策略二：回退原生 HTTP GET 抓取与文本解析
	return fetchViaNativeHTTP(ctx, targetURL)
}

func fetchViaJina(ctx context.Context, targetURL string) (string, string, error) {
	jinaURL := "https://r.jina.ai/" + targetURL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jinaURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; DaidaiBot/1.0)")
	req.Header.Set("X-Return-Format", "markdown")
	req.Header.Set("X-Timeout", "15")

	client := core.NewProtectedHTTPClient(18 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("jina status code: %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", err
	}

	body := strings.TrimSpace(string(bodyBytes))
	title := ""

	// 提取标题行（如 "Title: ..." 或以 "# " 开头）
	lines := strings.Split(body, "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "Title: ") {
			title = strings.TrimPrefix(trimmed, "Title: ")
			break
		} else if strings.HasPrefix(trimmed, "# ") && title == "" {
			title = strings.TrimPrefix(trimmed, "# ")
			break
		}
	}

	return body, title, nil
}

func fetchViaNativeHTTP(ctx context.Context, targetURL string) (string, string, error) {
	client := core.NewProtectedHTTPClient(15 * time.Second)
	curURL := targetURL

	var resp *http.Response
	for hop := 0; hop < 4; hop++ {
		// 每次重定向均重新校验目标地址公网安全性
		if err := core.ValidateSafeExternalURL(ctx, curURL); err != nil {
			return "", "", fmt.Errorf("redirect target unsafe: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, curURL, nil)
		if err != nil {
			return "", "", err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

		resp, err = client.Transport.RoundTrip(req)
		if err != nil {
			return "", "", err
		}

		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", "", fmt.Errorf("redirect with empty location")
			}
			base, _ := url.Parse(curURL)
			next, err := base.Parse(loc)
			if err != nil {
				return "", "", fmt.Errorf("invalid redirect location: %w", err)
			}
			curURL = next.String()
			continue
		}
		break
	}

	if resp == nil {
		return "", "", fmt.Errorf("failed to get response")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("http response status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", "", err
	}

	title, text := extractHTMLTitleAndText(string(bodyBytes))
	return text, title, nil
}

// extractHTMLTitleAndText 使用纯标准库状态机提取网页标题与正文纯文本（跳过 script/style/svg/nav 等干扰标签）
func extractHTMLTitleAndText(htmlStr string) (string, string) {
	var (
		titleBuf strings.Builder
		textBuf  strings.Builder
		inTag    bool
		tagBuf   strings.Builder
		skipTag  string // 正在跳过的标签，如 script, style, svg, noscript
		inTitle  bool
	)

	skipTags := map[string]bool{
		"script":   true,
		"style":    true,
		"svg":      true,
		"noscript": true,
		"header":   true,
		"footer":   true,
		"nav":      true,
	}

	chars := []rune(htmlStr)
	for i := 0; i < len(chars); i++ {
		r := chars[i]
		if r == '<' {
			inTag = true
			tagBuf.Reset()
			continue
		}
		if r == '>' && inTag {
			inTag = false
			tagContent := strings.ToLower(strings.TrimSpace(tagBuf.String()))
			isClosing := strings.HasPrefix(tagContent, "/")
			tagName := strings.TrimPrefix(tagContent, "/")
			if idx := strings.IndexAny(tagName, " \t\r\n/"); idx != -1 {
				tagName = tagName[:idx]
			}

			if isClosing {
				if tagName == skipTag {
					skipTag = ""
				}
				if tagName == "title" {
					inTitle = false
				}
				if tagName == "p" || tagName == "div" || tagName == "br" || tagName == "h1" || tagName == "h2" || tagName == "h3" || tagName == "li" {
					textBuf.WriteRune('\n')
				}
			} else {
				if skipTags[tagName] && skipTag == "" {
					skipTag = tagName
				}
				if tagName == "title" {
					inTitle = true
				}
			}
			continue
		}

		if inTag {
			tagBuf.WriteRune(r)
			continue
		}

		if skipTag != "" {
			continue
		}

		if inTitle {
			titleBuf.WriteRune(r)
			continue
		}

		textBuf.WriteRune(r)
	}

	title := html.UnescapeString(strings.TrimSpace(titleBuf.String()))

	// 清洗正文中的多余空行与空白字符
	rawLines := strings.Split(textBuf.String(), "\n")
	var cleanedLines []string
	for _, line := range rawLines {
		t := strings.TrimSpace(html.UnescapeString(line))
		if t != "" {
			cleanedLines = append(cleanedLines, t)
		}
	}

	return title, strings.Join(cleanedLines, "\n")
}

func safeTruncateRune(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "\n\n... (正文超出长度，已截断)"
}
