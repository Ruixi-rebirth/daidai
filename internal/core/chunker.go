package core

import (
	"strings"
	"unicode/utf8"
)

const (
	MaxUserMessageRunes   = 12000
	MaxQuotedMessageRunes = 4000
	MaxVisionSummaryRunes = 2000
)

type markdownFence struct {
	marker  byte
	length  int
	opening string
}

func parseMarkdownFence(line string) (markdownFence, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || (trimmed[0] != '`' && trimmed[0] != '~') {
		return markdownFence{}, false
	}
	marker := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	if length < 3 {
		return markdownFence{}, false
	}
	if marker == '`' && strings.Contains(trimmed[length:], "`") {
		return markdownFence{}, false
	}
	return markdownFence{
		marker:  marker,
		length:  length,
		opening: trimmed,
	}, true
}

func closesMarkdownFence(line string, fence markdownFence) bool {
	trimmed := strings.TrimSpace(line)
	length := 0
	for length < len(trimmed) && trimmed[length] == fence.marker {
		length++
	}
	return length >= fence.length && strings.TrimSpace(trimmed[length:]) == ""
}

// TruncateRunes clips text without splitting a UTF-8 character and includes the marker within the limit.
func TruncateRunes(text string, limit int, marker string) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	textRunes := []rune(text)
	markerRunes := []rune(marker)
	if len(markerRunes) > limit {
		markerRunes = markerRunes[:limit]
	}
	keep := limit - len(markerRunes)
	return string(textRunes[:keep]) + string(markerRunes)
}

// SplitMarkdownChunks 将 Markdown 文本按字符数 (rune) 上限分块，保持反引号/波浪号代码围栏闭合并支持单行超长切分
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
		maxLineRunes := limit - 4 // leave room for a closing code fence
		if maxLineRunes <= 0 {
			maxLineRunes = limit
		}
		if len(runes) <= maxLineRunes {
			lines = append(lines, rl)
			continue
		}
		step := maxLineRunes - 10
		if step <= 0 {
			step = maxLineRunes
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
	var codeFence markdownFence

	flushChunk := func() {
		if currentRunes == 0 {
			return
		}
		if inCodeBlock {
			closingFence := strings.Repeat(string(codeFence.marker), codeFence.length)
			current.WriteString("\n" + closingFence)
			chunks = append(chunks, current.String())
			current.Reset()
			current.WriteString(codeFence.opening)
			current.WriteString("\n")
			currentRunes = utf8.RuneCountInString(codeFence.opening) + 1
		} else {
			chunks = append(chunks, current.String())
			current.Reset()
			currentRunes = 0
		}
	}

	for _, line := range lines {
		lineRunes := utf8.RuneCountInString(line)

		openingFence, opensFence := parseMarkdownFence(line)
		closesFence := inCodeBlock && closesMarkdownFence(line, codeFence)
		closeReserve := 0
		if inCodeBlock {
			closeReserve = codeFence.length + 1 // newline and matching closing fence
		} else if opensFence {
			closeReserve = openingFence.length + 1
		}
		if currentRunes+lineRunes+1+closeReserve > limit && currentRunes > 0 {
			flushChunk()
		}

		if opensFence && !inCodeBlock {
			inCodeBlock = true
			codeFence = openingFence
		} else if closesFence {
			inCodeBlock = false
			codeFence = markdownFence{}
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
			current.WriteString("\n" + strings.Repeat(string(codeFence.marker), codeFence.length))
		}
		chunks = append(chunks, current.String())
	}
	return chunks
}
