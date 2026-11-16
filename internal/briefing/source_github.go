package briefing

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"github.com/yuin/goldmark"
	"net/url"
	"time"
)

type ghRepo struct {
	ID                      int64
	FullName                string `json:"full_name"`
	HTMLURL                 string `json:"html_url"`
	Description, Language   string
	Stars                   int       `json:"stargazers_count"`
	CreatedAt               time.Time `json:"created_at"`
	PushedAt                time.Time `json:"pushed_at"`
	Private, Archived, Fork bool
}

func (c *Collector) github(ctx context.Context) ([]Item, error) {
	now := time.Now().UTC()
	queries := []string{"created:>=" + now.Add(-72*time.Hour).Format(time.RFC3339) + " stars:>50 is:public archived:false fork:false", "topic:nixos pushed:>=" + now.Add(-24*time.Hour).Format(time.RFC3339) + " is:public archived:false fork:false"}
	var out []Item
	var sourceErr error
	seen := map[int64]bool{}
	for index, q := range queries {
		var v struct {
			Items      []ghRepo
			Incomplete bool `json:"incomplete_results"`
		}
		err := c.json(ctx, queryURL(c.GitHub, "/search/repositories", url.Values{"q": {q}, "sort": {"stars"}, "order": {"desc"}, "per_page": {"10"}}), &v)
		if err != nil {
			sourceErr = err
			continue
		}
		if v.Incomplete {
			sourceErr = fmt.Errorf("GitHub results incomplete")
		}
		for _, r := range v.Items {
			if seen[r.ID] || r.Private || r.Archived || r.Fork {
				continue
			}
			seen[r.ID] = true
			at := r.CreatedAt
			tag := "三日内新项目"
			if index == 1 {
				at = r.PushedAt
				tag = "近期维护"
			}
			kind := "project_description"
			if r.Description == "" {
				kind = "title_only"
			}
			out = append(out, Item{ID: fmt.Sprintf("github:%d", r.ID), RepoID: r.ID, Source: "github", Title: clipped(r.FullName, 200), URL: r.HTMLURL, Evidence: clipped(r.Description, 1400), EvidenceKind: kind, Meta: fmt.Sprintf("%s · %d Star · %s", clipped(r.Language, 20), r.Stars, tag), Score: r.Stars, PublishedAt: at, NewProject: index == 0})
		}
	}
	return out, sourceErr
}
func (c *Collector) readme(ctx context.Context, i Item) Item {
	if i.Source != "github" {
		return c.enrich(ctx, i)
	}
	u, err := url.Parse(i.URL)
	if err != nil {
		return i
	}
	var v struct{ Content, Encoding string }
	if c.json(ctx, c.GitHub+"/repos"+u.Path+"/readme", &v) != nil || v.Encoding != "base64" {
		return i
	}
	b, err := base64.StdEncoding.DecodeString(v.Content)
	if err == nil && len(b) > 0 {
		var rendered bytes.Buffer
		if goldmark.Convert(b, &rendered) != nil {
			return i
		}
		i.Evidence = clipped(i.Evidence+"\nREADME 摘录："+fragmentText(rendered.String()), 1400)
		i.EvidenceKind = "readme_excerpt"
	}
	return i
}
