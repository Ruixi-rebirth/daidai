package core

import (
	"strings"
	"testing"
)

func TestSplitMarkdownChunks_BasicAndCodeBlock(t *testing.T) {
	text := "这是第一行\n```go\nfunc main() {\n\tprintln(\"hello world\")\n}\n```\n这是结束行"
	chunks := SplitMarkdownChunks(text, 25)

	if len(chunks) <= 1 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	for i, c := range chunks {
		// 验证每个分块内部的代码围栏是配对闭合的
		fenceCount := strings.Count(c, "```")
		if fenceCount%2 != 0 {
			t.Errorf("chunk #%d has unclosed code fence: %s", i, c)
		}
	}

	// 验证单行无换行超长文本能按字符安全切分
	longSingleLine := strings.Repeat("这是一段没有换行符的超长中文段落测试文本。", 10)
	singleChunks := SplitMarkdownChunks(longSingleLine, 50)
	if len(singleChunks) < 4 {
		t.Fatalf("expected long single line to be split into >= 4 chunks, got %d", len(singleChunks))
	}
	for i, sc := range singleChunks {
		if len([]rune(sc)) > 50 {
			t.Errorf("chunk #%d exceeded rune limit 50: got %d runes", i, len([]rune(sc)))
		}
	}
}
