package briefing

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type nixTopic struct {
	ID          int64
	Slug, Title string
	Created     time.Time `json:"created_at"`
	Active      time.Time `json:"last_posted_at"`
	Likes       int       `json:"like_count"`
	Posts       int       `json:"posts_count"`
	Pinned      bool
	section     string
}
type nixPost struct {
	ID      int64
	Number  int       `json:"post_number"`
	Created time.Time `json:"created_at"`
	Cooked  string
}
type nixStream struct {
	Stream struct {
		Posts []nixPost
		IDs   []int64 `json:"stream"`
	} `json:"post_stream"`
}

func (c *Collector) nix(ctx context.Context) ([]Item, error) {
	var out []Item
	var sourceErr error
	success := false
	b, _, err := c.get(ctx, c.NixBlog)
	if err != nil {
		sourceErr = err
	} else {
		var feed struct {
			Items []struct {
				Title       string `xml:"title"`
				Link        string `xml:"link"`
				PubDate     string `xml:"pubDate"`
				Description string `xml:"description"`
				Content     string `xml:"encoded"`
			} `xml:"channel>item"`
		}
		if err = xml.Unmarshal(b, &feed); err != nil {
			sourceErr = err
		} else {
			success = true
			for _, v := range feed.Items {
				at, err := http.ParseTime(v.PubDate)
				if err != nil || time.Since(at) > 24*time.Hour {
					continue
				}
				body := v.Content
				if body == "" {
					body = v.Description
				}
				out = append(out, Item{ID: "nixos:blog:" + v.Link, Source: "nixos", Title: clipped(v.Title, 200), URL: v.Link, Evidence: fragmentText(body), EvidenceKind: "source_body", PublishedAt: at, Meta: "官方公告 · 新发布"})
			}
		}
	}
	seen := map[int64]bool{}
	var topics []nixTopic
	for _, path := range []string{"/c/announcements/8.json", "/c/dev/14.json", "/c/howto/15.json"} {
		var v struct {
			List struct{ Topics []nixTopic } `json:"topic_list"`
		}
		if err = c.json(ctx, c.NixForum+path, &v); err != nil {
			sourceErr = err
			continue
		}
		success = true
		for _, t := range v.List.Topics {
			if seen[t.ID] || (t.Pinned && path != "/c/announcements/8.json") {
				continue
			}
			seen[t.ID] = true
			// Only announcements may qualify through a substantive fresh reply.
			// Old review queues and replies to old guides are not daily news.
			if time.Since(t.Created) > 24*time.Hour && (path != "/c/announcements/8.json" || time.Since(t.Active) > 24*time.Hour) {
				continue
			}
			switch path {
			case "/c/announcements/8.json":
				t.section = "社区公告"
			case "/c/dev/14.json":
				t.section = "开发讨论"
			case "/c/howto/15.json":
				t.section = "教程"
			}
			topics = append(topics, t)
		}
	}
	jobs := make([]Item, len(topics))
	for index, t := range topics {
		jobs[index] = Item{ID: fmt.Sprintf("nixos:topic:%d", t.ID), Source: "nixos", Title: clipped(t.Title, 200), URL: fmt.Sprintf("https://discourse.nixos.org/t/%s/%d", url.PathEscape(t.Slug), t.ID), PublishedAt: t.Created, ActiveAt: t.Active, OldTopic: time.Since(t.Created) > 24*time.Hour, Score: t.Likes, Meta: fmt.Sprintf("%s首帖 · 累计 %d 个赞 · %d 条回复", t.section, t.Likes, max(0, t.Posts-1)), EvidenceKind: "title_only"}
	}
	jobs = rank(jobs, 8)
	jobs = parallelItems(ctx, jobs, func(ctx context.Context, i Item) Item {
		parts := strings.Split(i.ID, ":")
		id := parts[len(parts)-1]
		var v nixStream
		if c.json(ctx, c.NixForum+"/t/"+id+".json", &v) != nil {
			return i
		}
		if len(v.Stream.Posts) > 0 {
			i.Evidence = fragmentText(v.Stream.Posts[0].Cooked)
			i.EvidenceKind = "source_body"
		}
		if !i.OldTopic {
			return i
		}
		ids := v.Stream.IDs
		if len(ids) > 3 {
			ids = ids[len(ids)-3:]
		}
		q := url.Values{}
		for _, id := range ids {
			q.Add("post_ids[]", strconv.FormatInt(id, 10))
		}
		var recent nixStream
		if c.json(ctx, queryURL(c.NixForum, "/t/"+id+"/posts.json", q), &recent) != nil {
			return Item{}
		}
		var updates strings.Builder
		var last nixPost
		for _, p := range recent.Stream.Posts {
			if time.Since(p.Created) > 24*time.Hour {
				continue
			}
			fmt.Fprintf(&updates, "%s 新回复：%s\n", p.Created.Format(time.RFC3339), fragmentText(p.Cooked))
			if p.Created.After(last.Created) {
				last = p
			}
		}
		if last.ID == 0 {
			return Item{}
		}
		// Only fresh replies are evidence for an old topic's new development.
		// Supplying the old first post lets the editor retell old announcements
		// when today's reply is merely a greeting or acknowledgement.
		i.Evidence = clipped(updates.String(), 1400)
		i.EvidenceKind = "recent_replies"
		i.ActiveAt = last.Created
		i.URL += "/" + strconv.Itoa(last.Number)
		i.ID = fmt.Sprintf("nixos:post:%d", last.ID)
		i.Meta = fmt.Sprintf("旧帖新回复 · 累计 %d 个赞", i.Score)
		return i
	})
	for _, i := range jobs {
		if i.ID != "" {
			out = append(out, i)
		}
	}
	if !success && sourceErr == nil {
		sourceErr = fmt.Errorf("NixOS unavailable")
	}
	return out, sourceErr
}
