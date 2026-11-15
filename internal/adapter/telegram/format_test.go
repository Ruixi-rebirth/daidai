package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestMarkdownFormattingAndUTF16Offsets(t *testing.T) {
	result := formatMarkdown("😀 **粗体** 与 *斜体*、`x < y`，[文档](https://example.com/a?q=1&b=2)。\n\n> 引用内容\n\n```go\nfmt.Println(\"你好\")\n```\n")
	if strings.Contains(result.Text, "**") || strings.Contains(result.Text, "```") || !strings.Contains(result.Text, "x < y") {
		t.Fatalf("bad text: %q", result.Text)
	}
	expected := map[string]string{"bold": "粗体", "italic": "斜体", "code": "x < y", "text_link": "文档", "blockquote": "引用内容\n\n", "pre": "fmt.Println(\"你好\")"}
	encoded := utf16.Encode([]rune(result.Text))
	for _, entity := range result.Entities {
		content := string(utf16.Decode(encoded[entity.Offset : entity.Offset+entity.Length]))
		want, ok := expected[entity.Type]
		if !ok || content != want {
			t.Fatalf("unexpected entity: %+v %q", entity, content)
		}
		if entity.Type == "bold" && entity.Offset != 3 {
			t.Fatal("emoji offset was counted as Unicode runes instead of UTF-16")
		}
		if entity.Type == "pre" && entity.Language != "go" {
			t.Fatal("code language lost")
		}
		delete(expected, entity.Type)
	}
	if len(expected) > 0 {
		t.Fatalf("missing formatting: %v", expected)
	}
}
func TestFormattedSplittingPreservesCodeAndEmoji(t *testing.T) {
	value := formatMarkdown("```go\n" + strings.Repeat("😀变量=1\n", 500) + "```\n")
	chunks := splitFormatted(value, 1800)
	if len(chunks) < 2 {
		t.Fatal("long code not split")
	}
	var joined strings.Builder
	for _, chunk := range chunks {
		if utf16Length(chunk.Text) > 1800 {
			t.Fatal("chunk exceeds UTF-16 limit")
		}
		joined.WriteString(chunk.Text)
		if len(chunk.Entities) != 1 || chunk.Entities[0].Type != "pre" || chunk.Entities[0].Offset != 0 || chunk.Entities[0].Length != utf16Length(chunk.Text) {
			t.Fatalf("code formatting lost across split: %+v", chunk.Entities)
		}
	}
	if joined.String() != value.Text {
		t.Fatal("split lost content")
	}
}
func TestMarkdownCannotInjectTelegramFormatting(t *testing.T) {
	result := formatMarkdown("<b>原样 HTML</b> &amp; [危险链接](javascript:alert) **普通 `代码` 文本**")
	if !strings.Contains(result.Text, "<b>原样 HTML</b> &") {
		t.Fatalf("literal content changed: %s", result.Text)
	}
	for _, entity := range result.Entities {
		if entity.Type == "text_link" {
			t.Fatal("unsafe link emitted")
		}
		if entity.Type == "bold" {
			for _, code := range result.Entities {
				if code.Type == "code" && entity.Offset < code.Offset+code.Length && code.Offset < entity.Offset+entity.Length {
					t.Fatal("bold overlaps code, rejected by Telegram")
				}
			}
		}
	}
}
func TestSendingTextUsesEntitiesAndRetainsQuoteSource(t *testing.T) {
	c := testChannel(t)
	c.http.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		var payload struct {
			Text      string       `json:"text"`
			Entities  []textEntity `json:"entities"`
			ParseMode string       `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Text != "😀 清晰" || len(payload.Entities) != 1 || payload.Entities[0].Offset != 3 || payload.ParseMode != "" {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		return response(`{"ok":true,"result":{"message_id":70}}`), nil
	})
	if err := c.sendText(context.Background(), "42", "😀 **清晰**", 0, ""); err != nil {
		t.Fatal(err)
	}
	quote, err := c.resolveQuote(context.Background(), &message{ID: 70}, "42", false, 0)
	if err != nil || quote.Content != "😀 **清晰**" {
		t.Fatal("original quote source lost")
	}
}
