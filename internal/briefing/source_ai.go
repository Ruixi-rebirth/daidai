package briefing

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type hfDailyPaper struct {
	Paper struct {
		ID               string    `json:"id"`
		Title            string    `json:"title"`
		Summary          string    `json:"summary"`
		PublishedAt      time.Time `json:"publishedAt"`
		SubmittedOnDaily time.Time `json:"submittedOnDailyAt"`
		Upvotes          int       `json:"upvotes"`
		GitHubStars      int       `json:"githubStars"`
	} `json:"paper"`
}

func (c *Collector) aiPapers(ctx context.Context) ([]Item, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-24 * time.Hour)
	dates := []string{cutoff.Format("2006-01-02"), now.Format("2006-01-02")}
	if dates[0] == dates[1] {
		dates = dates[1:]
	}
	seen := make(map[string]struct{})
	var out []Item
	var sourceErr error
	succeeded := false
	for _, date := range dates {
		query := url.Values{
			"date":  {date},
			"limit": {"100"},
			"p":     {"0"},
			"sort":  {"publishedAt"},
		}
		var feed []hfDailyPaper
		if err := c.json(ctx, queryURL(c.Papers, "/api/daily_papers", query), &feed); err != nil {
			sourceErr = err
			continue
		}
		succeeded = true
		for _, entry := range feed {
			paper := entry.Paper
			id := strings.TrimSpace(paper.ID)
			submitted := paper.SubmittedOnDaily.UTC()
			if !validPaperID(id) || submitted.IsZero() || submitted.Before(cutoff) || submitted.After(now) {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			title := clipped(paper.Title, 200)
			evidence := clipped(paper.Summary, 1400)
			if title == "" || evidence == "" {
				continue
			}
			paperURL := (&url.URL{Scheme: "https", Host: "arxiv.org", Path: "/abs/" + id}).String()
			meta := fmt.Sprintf("Hugging Face Daily Papers · %d 赞", paper.Upvotes)
			if paper.GitHubStars > 0 {
				meta += fmt.Sprintf(" · 关联仓库 %d Star", paper.GitHubStars)
			}
			out = append(out, Item{
				ID:           "ai:" + id,
				Source:       "ai",
				Title:        title,
				URL:          paperURL,
				Evidence:     evidence,
				EvidenceKind: "paper_abstract",
				Meta:         meta,
				PublishedAt:  submitted,
				Score:        paper.Upvotes,
			})
		}
	}
	if !succeeded && sourceErr != nil {
		return nil, sourceErr
	}
	return rank(out, candidateLimits["ai"]), sourceErr
}

func validPaperID(id string) bool {
	if id == "" || len(id) > 40 || strings.Contains(id, "..") || strings.HasPrefix(id, "/") || strings.HasSuffix(id, "/") {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '/' || r == '-' {
			continue
		}
		return false
	}
	return true
}
