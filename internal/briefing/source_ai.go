package briefing

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
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

// arxivIDPattern matches canonical modern (e.g. "2301.07041", "2411.12345v2")
// and legacy arXiv paper identifiers (e.g. "cs.CV/0612001", "math/0309136").
var arxivIDPattern = regexp.MustCompile(`^(?:[0-9]{4}\.[0-9]{4,5}|[a-zA-Z\-]+(?:\.[a-zA-Z]{2})?/[0-9]{7})(?:v[0-9]+)?$`)

func validPaperID(id string) bool {
	return arxivIDPattern.MatchString(id)
}
