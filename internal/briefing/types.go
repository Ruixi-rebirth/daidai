package briefing

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

const maxItems = 20

var sources = []string{"nixos", "hn", "github", "v2ex"}
var limits = map[string]int{"nixos": 5, "hn": 5, "github": 5, "v2ex": 5}
var candidateLimits = map[string]int{"nixos": 8, "hn": 8, "github": 8, "v2ex": 8}
var labels = map[string]string{"nixos": "❄️ NixOS 生态", "hn": "🔥 Hacker News 精选", "github": "💻 GitHub 开源发现", "v2ex": "💬 V2EX 技术话题"}

type Item struct {
	ID            string    `json:"id"`
	Number        string    `json:"number"`
	Page          int       `json:"page"`
	Source        string    `json:"source"`
	Title         string    `json:"title"`
	OriginalTitle string    `json:"original_title"`
	Summary       string    `json:"summary"`
	Meta          string    `json:"meta"`
	URL           string    `json:"url"`
	DiscussionURL string    `json:"discussion_url,omitempty"`
	Evidence      string    `json:"evidence,omitempty"`
	EvidenceKind  string    `json:"evidence_kind"`
	PublishedAt   time.Time `json:"published_at"`
	ActiveAt      time.Time `json:"active_at,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	Score         int       `json:"score"`
	RepoID        int64     `json:"repo_id,omitempty"`
	OldTopic      bool      `json:"old_topic,omitempty"`
	NewProject    bool      `json:"new_project,omitempty"`
}
type SourceState struct {
	Source     string    `json:"source"`
	Status     string    `json:"status"`
	Count      int       `json:"count"`
	ObservedAt time.Time `json:"observed_at"`
}
type Edition struct {
	ID           string        `json:"edition_id"`
	Date         string        `json:"date"`
	Weekday      string        `json:"weekday"`
	Cutoff       string        `json:"cutoff"`
	CutoffAt     time.Time     `json:"cutoff_at"`
	Timezone     string        `json:"timezone"`
	Items        []Item        `json:"items"`
	Leads        []string      `json:"leads"`
	SourceStates []SourceState `json:"sources"`
	Basic        bool          `json:"basic"`
	Model        string        `json:"model"`
}
type Subscription struct {
	Platform   string
	Recipient  string
	IsGroup    bool
	Owner      string
	Enabled    bool
	LastViewed string
	PausedDay  string
}
type Delivery struct {
	Edition   string
	Platform  string
	Recipient string
	IsGroup   bool
	Status    string
	Attempts  int
	Next      time.Time
	Delivered string
	Remaining string
	ErrorKind string
	Notified  bool
}

func canonical(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return ""
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || k == "fbclid" || k == "gclid" {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
func identity(i Item) string {
	if u, err := url.Parse(i.URL); err == nil && strings.EqualFold(u.Hostname(), "github.com") {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) == 2 {
			return "github:" + strings.ToLower(strings.Join(parts, "/"))
		}
	}
	if i.RepoID != 0 {
		return fmt.Sprintf("repo:%d", i.RepoID)
	}
	return canonical(i.URL)
}
func (e *Edition) PageText(page int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 极客早报 · %s\n\n截止 %s\n", e.ID, e.Cutoff)
	if e.Basic {
		b.WriteString("\n本期为基础速览，未生成完整 AI 编辑摘要。\n")
	}
	last := ""
	for _, i := range e.Items {
		if page != 0 && i.Page != page {
			continue
		}
		if i.Source != last {
			fmt.Fprintf(&b, "\n## %s\n", labels[i.Source])
			last = i.Source
		}
		fmt.Fprintf(&b, "\n### %s · %s\n", i.Number, escape(i.Title))
		if i.Summary != "" {
			fmt.Fprintf(&b, "\n%s\n", escape(i.Summary))
		}
		fmt.Fprintf(&b, "\n%s\n\n原文：%s\n", escape(i.Meta), i.URL)
		if i.DiscussionURL != "" {
			fmt.Fprintf(&b, "讨论：%s\n", i.DiscussionURL)
		}
	}
	if len(e.Items) == 0 {
		b.WriteString("\n本期没有筛选到值得推荐的新内容。\n")
	}
	return b.String()
}
func (e *Edition) Index() string {
	var b strings.Builder
	fmt.Fprintf(&b, "极客早报 · %s\n", e.ID)
	for _, i := range e.Items {
		fmt.Fprintf(&b, "\n[%s 原文](%s)", i.Number, i.URL)
		if i.DiscussionURL != "" {
			fmt.Fprintf(&b, " · [讨论](%s)", i.DiscussionURL)
		}
	}
	return b.String()
}
func escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "`", "\\`", "<", "&lt;", ">", "&gt;").Replace(s)
}
