package briefing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPublishedNumbersSurviveNixSectionMovingLast(t *testing.T) {
	e := &Edition{Items: []Item{{ID: "nix", Number: "01", Source: "nixos", Title: "Nix公告", URL: "https://example.com/nix"}, {ID: "hn", Number: "02", Source: "hn", Title: "HN新闻", URL: "https://example.com/hn"}}}
	ordered := e.OrderedItems()
	if ordered[0].ID != "hn" || ordered[0].Number != "02" || ordered[1].ID != "nix" || ordered[1].Number != "01" || e.Items[0].ID != "nix" {
		t.Fatal("presentation rewrote published numbering or archive")
	}
	if text := e.Text(); strings.Index(text, "HN新闻") > strings.Index(text, "Nix公告") {
		t.Fatal("text fallback did not follow reading order")
	}
	if index := e.Index(); strings.Index(index, "02 · HN新闻") > strings.Index(index, "01 · Nix公告") {
		t.Fatal("link index did not follow card reading order")
	}
}

func TestNixCandidatesKeepOfficialAnnouncementsDespiteCommunityPopularity(t *testing.T) {
	items := []Item{{ID: "nixos:blog:https://example.com/release", Source: "nixos", URL: "https://example.com/release"}}
	for n := 0; n < 10; n++ {
		items = append(items, Item{ID: fmt.Sprintf("nixos:topic:%d", n), Source: "nixos", Score: 10000 + n})
	}
	got := nixCandidates(items)
	if len(got) != 8 || got[0].ID != items[0].ID {
		t.Fatal("official release was crowded out by community popularity")
	}
}

func TestNixUsesCuratedCategoriesAndSkipsOldReviewQueues(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog":
			_, _ = fmt.Fprint(w, `<rss><channel/></rss>`)
		case "/c/announcements/8.json":
			_, _ = fmt.Fprintf(w, `{"topic_list":{"topics":[{"id":1,"title":"新版本公告","created_at":%q,"last_posted_at":%q,"pinned":true}]}}`, now, now)
		case "/c/dev/14.json":
			_, _ = fmt.Fprintf(w, `{"topic_list":{"topics":[{"id":2,"title":"PRs ready for review","created_at":%q,"last_posted_at":%q,"like_count":9999},{"id":3,"title":"新的开发设计","created_at":%q,"last_posted_at":%q}]}}`, old, now, now, now)
		case "/c/howto/15.json":
			_, _ = fmt.Fprintf(w, `{"topic_list":{"topics":[{"id":4,"title":"完整教程","created_at":%q,"last_posted_at":%q}]}}`, now, now)
		case "/t/1.json", "/t/3.json", "/t/4.json":
			_, _ = fmt.Fprint(w, `{"post_stream":{"posts":[{"cooked":"<p>有明确技术内容的正文</p>"}]}}`)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewCollector("")
	c.Client, c.NixBlog, c.NixForum = server.Client(), server.URL+"/blog", server.URL
	items, err := c.nix(context.Background())
	if err != nil || len(items) != 3 {
		t.Fatalf("wrong curated result: %+v %v", items, err)
	}
	for _, item := range items {
		if item.ID == "nixos:topic:2" || item.EvidenceKind != "source_body" {
			t.Fatal("old queue or unread content selected")
		}
	}
}

func TestSourceStatusDoesNotDependOnDisplayOrder(t *testing.T) {
	c := fixtureCollector(t)
	c.HN += "/missing"
	_, states, _, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range states {
		want := "ok"
		if state.Source == "hn" {
			want = "error"
		}
		if state.Status != want {
			t.Fatalf("source status was assigned by column position: %+v", state)
		}
	}
}

func TestOldAnnouncementDoesNotReuseOldFirstPostAsFreshEvidence(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/blog":
			_, _ = fmt.Fprint(w, `<rss><channel/></rss>`)
		case "/c/announcements/8.json":
			_, _ = fmt.Fprintf(w, `{"topic_list":{"topics":[{"id":5,"title":"原有公告","created_at":%q,"last_posted_at":%q}]}}`, old, now)
		case "/c/dev/14.json", "/c/howto/15.json":
			_, _ = fmt.Fprint(w, `{"topic_list":{"topics":[]}}`)
		case "/t/5.json":
			_, _ = fmt.Fprint(w, `{"post_stream":{"posts":[{"cooked":"<p>OLD_FIRST_POST_FACT</p>"}],"stream":[11,12,13]}}`)
		case "/t/5/posts.json":
			_, _ = fmt.Fprintf(w, `{"post_stream":{"posts":[{"id":13,"post_number":9,"created_at":%q,"cooked":"<p>本次新的技术变更</p>"}]}}`, now)
		default:
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := NewCollector("")
	c.Client, c.NixBlog, c.NixForum = server.Client(), server.URL+"/blog", server.URL
	items, err := c.nix(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("old announcement unavailable: %+v %v", items, err)
	}
	item := items[0]
	if item.ID != "nixos:post:13" || item.EvidenceKind != "recent_replies" || strings.Contains(item.Evidence, "OLD_FIRST_POST_FACT") || !strings.Contains(item.Evidence, "本次新的技术变更") {
		t.Fatalf("old first post was presented as fresh evidence: %+v", item)
	}
}
