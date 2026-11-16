package briefing

import (
	"context"
	"fmt"
	"time"
)

type v2Topic struct {
	ID                  int64
	Title, URL, Content string
	Created             int64
	Replies             int
	Node                struct{ Name, Title string }
}

func (c *Collector) v2ex(ctx context.Context) ([]Item, error) {
	var topics []v2Topic
	err := c.json(ctx, c.V2EX+"/topics/hot.json", &topics)
	if err != nil {
		return nil, err
	}
	if len(topics) < 20 {
		var latest []v2Topic
		if fetchErr := c.json(ctx, c.V2EX+"/topics/latest.json", &latest); fetchErr == nil {
			topics = append(topics, latest...)
		} else {
			err = fetchErr
		}
	}
	seen := map[int64]bool{}
	var out []Item
	for _, v := range topics {
		if seen[v.ID] || v.ID == 0 {
			continue
		}
		seen[v.ID] = true
		switch v.Node.Name {
		case "all4all", "jobs", "job", "career", "qna", "life", "random", "free", "promotions", "invest":
			continue
		}
		if time.Since(time.Unix(v.Created, 0)) > 24*time.Hour {
			continue
		}
		kind := "source_body"
		if v.Content == "" {
			kind = "title_only"
		}
		out = append(out, Item{ID: fmt.Sprintf("v2ex:%d", v.ID), Source: "v2ex", Title: clipped(v.Title, 200), URL: fmt.Sprintf("https://www.v2ex.com/t/%d", v.ID), Evidence: clipped(v.Content, 1400), EvidenceKind: kind, Meta: fmt.Sprintf("%s · %d 条回复 · 首帖", clipped(v.Node.Title, 24), v.Replies), PublishedAt: time.Unix(v.Created, 0), Score: v.Replies})
	}
	return rank(out, 8), err
}
