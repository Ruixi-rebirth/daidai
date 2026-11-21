package entity

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPaginateTicketOutputPreservesLogs(t *testing.T) {
	logs := "启动\n~~~go\n" + strings.Repeat("测", 29) + "\n~~~\n  结束"
	pages := paginateTicketOutput(logs, 12)
	if len(pages) < 2 || !strings.HasSuffix(pages[0], "\n") || strings.Join(pages, "") != logs {
		t.Fatalf("unexpected pages: %q", pages)
	}
	for _, page := range pages {
		if !utf8.ValidString(page) || utf8.RuneCountInString(page) > 12 {
			t.Fatalf("invalid page: %q", page)
		}
	}
}
