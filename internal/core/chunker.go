package core

import (
	"strings"
	"unicode/utf8"
)

// SplitMarkdownChunks 将 Markdown 文本按字符数 (rune) 上限分块，保持代码块闭合完整性并支持单行超长切分
func SplitMarkdownChunks(text string, limit int) []string {
	if limit <= 0 {
		limit = 1200
	}
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	rawLines := strings.Split(text, "\n")
	var lines []string
	for _, rl := range rawLines {
		runes := []rune(rl)
		if len(runes) <= limit || strings.HasPrefix(strings.TrimSpace(rl), "```") {
			lines = append(lines, rl)
			continue
		}
		step := limit - 10
		if step <= 0 {
			step = limit
		}
		for len(runes) > 0 {
			take := step
			if take > len(runes) {
				take = len(runes)
			}
			lines = append(lines, string(runes[:take]))
			runes = runes[take:]
		}
	}

	var chunks []string
	var current strings.Builder
	currentRunes := 0

	inCodeBlock := false
	codeFence := ""

	flushChunk := func() {
		if currentRunes == 0 {
			return
		}
		if inCodeBlock {
			current.WriteString("\n```")
			chunks = append(chunks, current.String())
			current.Reset()
			current.WriteString(codeFence)
			current.WriteString("\n")
			currentRunes = utf8.RuneCountInString(codeFence) + 1
		} else {
			chunks = append(chunks, current.String())
			current.Reset()
			currentRunes = 0
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lineRunes := utf8.RuneCountInString(line)

		isFence := strings.HasPrefix(trimmed, "```")
		if currentRunes+lineRunes+1 > limit && currentRunes > 0 {
			flushChunk()
		}

		if isFence {
			if !inCodeBlock {
				inCodeBlock = true
				codeFence = trimmed
			} else {
				inCodeBlock = false
				codeFence = ""
			}
		}

		if currentRunes > 0 && !strings.HasSuffix(current.String(), "\n") {
			current.WriteString("\n")
			currentRunes++
		}
		current.WriteString(line)
		currentRunes += lineRunes
	}

	if currentRunes > 0 {
		if inCodeBlock {
			current.WriteString("\n```")
		}
		chunks = append(chunks, current.String())
	}
	return chunks
}
