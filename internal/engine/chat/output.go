package chat

import "fmt"

// truncateToolOutput 根据工具性质智能按 Unicode 字符 (rune) 截断，严防切断 UTF-8 多字节中文字符
func truncateToolOutput(toolName, text string) string {
	runes := []rune(text)
	// 网页内容保持连续，控制单次进入后续模型上下文的正文规模。
	if toolName == "read_link" {
		const maxWebRunes = 18000
		if len(runes) <= maxWebRunes {
			return text
		}
		return fmt.Sprintf("%s\n\n[... 网页正文篇幅极大（共 %d 字符），已截取前 %d 字符核心正文供分析 ...]", string(runes[:maxWebRunes]), len(runes), maxWebRunes)
	}

	// 其它工具（命令行执行、日志、工单等）：默认 8,000 字符，保留头尾关键报错与状态
	const maxDefaultRunes = 8000
	if len(runes) <= maxDefaultRunes {
		return text
	}
	headLen := maxDefaultRunes * 3 / 4
	tailLen := maxDefaultRunes / 4
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	return fmt.Sprintf("%s\n\n[... 中间约 %d 字符日志已省略 ...]\n\n%s", head, len(runes)-maxDefaultRunes, tail)
}
