package briefing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"daidai/internal/core"
	"golang.org/x/net/html"
)

type Collector struct {
	Client                              *http.Client
	GitHubToken                         string
	NixBlog, NixForum, HN, GitHub, V2EX string
	slots                               chan struct{}
}

func NewCollector(token string) *Collector {
	return &Collector{Client: core.NewProtectedHTTPClient(12 * time.Second), GitHubToken: token, NixBlog: "https://nixos.org/blog/announcements-rss.xml", NixForum: "https://discourse.nixos.org", HN: "https://hacker-news.firebaseio.com/v0", GitHub: "https://api.github.com", V2EX: "https://www.v2ex.com/api", slots: make(chan struct{}, 6)}
}
func (c *Collector) get(ctx context.Context, raw string) ([]byte, string, error) {
	if canonical(raw) == "" {
		return nil, "", fmt.Errorf("invalid source URL")
	}
	select {
	case c.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	defer func() { <-c.slots }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Daidai/0.2 (+https://github.com/Ruixi-rebirth/daidai)")
	req.Header.Set("Accept", "application/json, application/rss+xml, text/html;q=0.8")
	if req.URL.Hostname() == "api.github.com" && req.URL.Scheme == "https" && c.GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.GitHubToken)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("source request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("source status %d", resp.StatusCode)
	}
	b, err := core.ReadBounded(resp.Body, 2<<20)
	return b, resp.Header.Get("Content-Type"), err
}
func (c *Collector) json(ctx context.Context, u string, out any) error {
	b, _, err := c.get(ctx, u)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

type sourceResult struct {
	items []Item
	state SourceState
}

func (c *Collector) Collect(ctx context.Context) ([]Item, []SourceState, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	results := make([]sourceResult, 4)
	fetchers := []struct {
		source string
		fetch  func(context.Context) ([]Item, error)
	}{{"nixos", c.nix}, {"hn", c.hackerNews}, {"github", c.github}, {"v2ex", c.v2ex}}
	var wg sync.WaitGroup
	for index, source := range fetchers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := source.fetch(ctx)
			status := "ok"
			if err != nil {
				status = "error"
				if len(items) > 0 {
					status = "partial"
				}
			}
			results[index] = sourceResult{items, SourceState{Source: source.source, Status: status, Count: len(items), ObservedAt: time.Now()}}
		}()
	}
	wg.Wait()
	cutoff := time.Now()
	var all []Item
	var states []SourceState
	successful := false
	for _, r := range results {
		if r.state.Status != "error" {
			successful = true
		}
		states = append(states, r.state)
		for _, i := range r.items {
			i.ObservedAt = r.state.ObservedAt
			at := i.PublishedAt
			window := 24 * time.Hour
			if i.OldTopic {
				at = i.ActiveAt
			}
			if i.Source == "github" && i.NewProject {
				window = 72 * time.Hour
			}
			if at.IsZero() || at.After(cutoff) || at.Before(cutoff.Add(-window)) {
				continue
			}
			if i.URL = canonical(i.URL); i.URL == "" || len(i.URL) > 2048 || len(i.ID) > 2048 {
				continue
			}
			i.DiscussionURL = canonical(i.DiscussionURL)
			all = append(all, i)
		}
	}
	if !successful {
		return nil, states, cutoff, fmt.Errorf("all briefing sources failed")
	}
	return all, states, cutoff, nil
}
func parallelItems(ctx context.Context, items []Item, fn func(context.Context, Item) Item) []Item {
	out := make([]Item, len(items))
	var wg sync.WaitGroup
	jobs := make(chan int)
	for n := 0; n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if ctx.Err() == nil {
					out[index] = fn(ctx, items[index])
				} else {
					out[index] = items[index]
				}
			}
		}()
	}
	for index := range items {
		select {
		case jobs <- index:
		case <-ctx.Done():
			out[index] = items[index]
		}
	}
	close(jobs)
	wg.Wait()
	return out
}
func rank(items []Item, n int) []Item {
	sort.SliceStable(items, func(a, b int) bool {
		aa := strings.HasPrefix(items[a].ID, "nixos:blog:")
		bb := strings.HasPrefix(items[b].ID, "nixos:blog:")
		if aa != bb {
			return aa
		}
		if items[a].Score != items[b].Score {
			return items[a].Score > items[b].Score
		}
		return items[a].PublishedAt.After(items[b].PublishedAt)
	})
	if len(items) > n {
		items = items[:n]
	}
	return items
}
func clipped(s string, n int) string {
	return core.TruncateRunes(strings.Join(strings.Fields(s), " "), n, "")
}

// pageEvidence deliberately prefers semantic article containers or metadata;
// generic page navigation is never passed off as an article body.
func pageEvidence(b []byte) (string, string) {
	root, err := html.Parse(bytes.NewReader(b))
	if err != nil {
		return "", "title_only"
	}
	var description string
	var article *html.Node
	for n := range root.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}
		if (n.Data == "article" || n.Data == "main") && article == nil {
			article = n
		}
		if n.Data == "meta" {
			key, value := "", ""
			for _, a := range n.Attr {
				switch a.Key {
				case "name", "property":
					key = a.Val
				case "content":
					value = a.Val
				}
			}
			if key == "description" || key == "og:description" {
				description = value
			}
		}
	}
	if article != nil {
		if text := nodeText(article); len([]rune(text)) >= 80 {
			return clipped(text, 1400), "article_excerpt"
		}
	}
	if strings.TrimSpace(description) != "" {
		return clipped(description, 1400), "page_description"
	}
	return "", "title_only"
}
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if b.Len() > 16<<10 {
			return
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "footer", "header", "form", "noscript":
				return
			}
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			b.WriteByte(' ')
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return clipped(b.String(), 1400)
}
func fragmentText(raw string) string {
	root, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	return nodeText(root)
}
func (c *Collector) enrich(ctx context.Context, i Item) Item {
	if i.Evidence != "" {
		return i
	}
	b, kind, err := c.get(ctx, i.URL)
	if err != nil {
		return i
	}
	if strings.Contains(kind, "text/html") {
		i.Evidence, i.EvidenceKind = pageEvidence(b)
	} else if strings.Contains(kind, "text/plain") {
		i.Evidence = clipped(string(b), 1400)
		i.EvidenceKind = "article_excerpt"
	}
	return i
}
func queryURL(base, path string, q url.Values) string {
	return strings.TrimRight(base, "/") + path + "?" + q.Encode()
}
