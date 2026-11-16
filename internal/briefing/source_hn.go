package briefing

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

func (c *Collector) hackerNews(ctx context.Context) ([]Item, error) {
	var ids []int64
	if err := c.json(ctx, c.HN+"/topstories.json", &ids); err != nil {
		return nil, err
	}
	if len(ids) > 60 {
		ids = ids[:60]
	}
	jobs := make([]Item, len(ids))
	for n, id := range ids {
		jobs[n].ID = strconv.FormatInt(id, 10)
	}
	var failed atomic.Int32
	items := parallelItems(ctx, jobs, func(ctx context.Context, i Item) Item {
		var v struct {
			ID                     int64
			Type, Title, URL, Text string
			Score, Descendants     int
			Time                   int64
			Deleted, Dead          bool
		}
		if err := c.json(ctx, c.HN+"/item/"+i.ID+".json", &v); err != nil {
			failed.Add(1)
			return Item{}
		}
		if v.ID == 0 || v.Type != "story" || v.Deleted || v.Dead || time.Since(time.Unix(v.Time, 0)) > 24*time.Hour {
			return Item{}
		}
		discussion := "https://news.ycombinator.com/item?id=" + i.ID
		link := v.URL
		if link == "" {
			link = discussion
		}
		body := fragmentText(v.Text)
		kind := "title_only"
		if body != "" {
			kind = "source_body"
		}
		return Item{ID: "hn:" + i.ID, Source: "hn", Title: clipped(v.Title, 200), URL: link, DiscussionURL: discussion, Score: v.Score, Meta: fmt.Sprintf("%d 分 · %d 条讨论", v.Score, v.Descendants), PublishedAt: time.Unix(v.Time, 0), Evidence: body, EvidenceKind: kind}
	})
	var out []Item
	for _, i := range items {
		if i.ID != "" {
			out = append(out, i)
		}
	}
	out = rank(out, 16)
	if failed.Load() > 0 {
		return out, fmt.Errorf("some HN details unavailable")
	}
	return out, nil
}
