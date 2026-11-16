package entity

import (
	"fmt"
	"strings"
)

// ImageDeliveryText accounts for shared sources only after all pages carrying
// that source were delivered. Empty sources represent genuine images.
func ImageDeliveryText(sources []string, delivered int) (complete, pending string) {
	last := map[string]int{}
	for i, source := range sources {
		if source = strings.TrimSpace(source); source != "" {
			last[source] = i
		}
	}
	seen := map[string]bool{}
	var done, todo []string
	missing := 0
	for i, source := range sources {
		source = strings.TrimSpace(source)
		if source == "" {
			if i >= delivered {
				missing++
			}
			continue
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		if last[source] < delivered {
			done = append(done, source)
		} else {
			todo = append(todo, source)
		}
	}
	if missing > 0 {
		todo = append(todo, fmt.Sprintf("[%d 张图片未发送完成；图片无法通过文字续发，请重新请求发送或生成。]", missing))
	}
	return strings.Join(done, "\n\n"), strings.Join(todo, "\n\n")
}
