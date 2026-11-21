package briefing

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
)

const itemsPerSection = 5
const MaxEditionItems = len(sources) * itemsPerSection
const maxTitleRunes = 30
const maxSummaryRunes = 80
const maxSummarySentences = 2

var sources = [...]string{"hn", "ai", "github", "v2ex", "nixos"}
var candidateLimits = map[string]int{"nixos": 8, "hn": 8, "ai": 12, "github": 8, "v2ex": 8}
var labels = map[string]string{"nixos": "❄️ NixOS 生态", "hn": "🔥 Hacker News 精选", "ai": "🤖 AI 研究精选", "github": "💻 GitHub 开源发现", "v2ex": "💬 V2EX 技术话题"}

type Item struct {
	ID            string    `json:"id"`
	Number        string    `json:"number"`
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

func parsePublishedLink(raw string) *url.URL {
	link := strings.TrimSpace(raw)
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil ||
		strings.ContainsAny(link, "<>\\") || strings.IndexFunc(link, unicode.IsSpace) >= 0 {
		return nil
	}
	return u
}

func publishedLink(raw string) string {
	if parsePublishedLink(raw) == nil {
		return ""
	}
	return strings.TrimSpace(raw)
}

func canonical(raw string) string {
	u := parsePublishedLink(raw)
	if u == nil {
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
	if link := canonical(i.URL); link != "" {
		return link
	}
	return i.ID
}

// OrderedItems changes presentation without renumbering a published edition.
// Quotes using an existing article number keep referring to the same article.
func (e *Edition) OrderedItems() []Item {
	items := append([]Item(nil), e.Items...)
	position := func(source string) int {
		for n, value := range sources {
			if value == source {
				return n
			}
		}
		return len(sources)
	}
	sort.SliceStable(items, func(i, j int) bool { return position(items[i].Source) < position(items[j].Source) })
	return items
}

// LookupItem returns the published entry with the same links shown in the index.
func (e *Edition) LookupItem(number int) (Item, bool) {
	label := fmt.Sprintf("%02d", number)
	for _, item := range e.Items {
		if item.Number == label {
			item.URL = publishedLink(item.URL)
			item.DiscussionURL = publishedLink(item.DiscussionURL)
			return item, true
		}
	}
	return Item{}, false
}

func (e *Edition) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 极客早报 · %s\n\n截止 %s\n", e.ID, e.Cutoff)
	if e.Basic {
		b.WriteString("\n本期为基础速览，未生成完整 AI 编辑摘要。\n")
	}
	last := ""
	for _, i := range e.OrderedItems() {
		if i.Source != last {
			fmt.Fprintf(&b, "\n## %s\n", labels[i.Source])
			last = i.Source
		}
		fmt.Fprintf(&b, "\n### %s · %s\n", i.Number, escape(i.Title))
		if i.Summary != "" {
			fmt.Fprintf(&b, "\n%s\n", escape(i.Summary))
		}
		fmt.Fprintf(&b, "\n%s\n", escape(i.Meta))
		if link := publishedLink(i.URL); link != "" {
			fmt.Fprintf(&b, "\n原文：%s\n", link)
		}
		if discussion := publishedLink(i.DiscussionURL); discussion != "" {
			fmt.Fprintf(&b, "讨论：%s\n", discussion)
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
	for _, i := range e.OrderedItems() {
		title := fmt.Sprintf("%s · %s", i.Number, escape(i.Title))
		if link := publishedLink(i.URL); link != "" {
			fmt.Fprintf(&b, "\n[%s](%s)", title, markdownDestination(link))
		} else {
			fmt.Fprintf(&b, "\n%s", title)
		}
		if discussion := publishedLink(i.DiscussionURL); discussion != "" {
			fmt.Fprintf(&b, " · [讨论](%s)", markdownDestination(discussion))
		}
	}
	return b.String()
}

func markdownDestination(link string) string {
	if strings.ContainsAny(link, "()") {
		return "<" + link + ">"
	}
	return link
}

func escape(s string) string {
	return strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]", "*", "\\*", "_", "\\_", "`", "\\`", "<", "&lt;", ">", "&gt;").Replace(s)
}
