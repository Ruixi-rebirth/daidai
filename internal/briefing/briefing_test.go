package briefing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "briefing.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func testEdition(t *testing.T, s *Store) *Edition {
	t.Helper()
	e := &Edition{ID: time.Now().Format("2006-01-02"), Date: "测试刊", Cutoff: "08:00 SGT", Items: []Item{}}
	if err := s.Begin(context.Background(), e.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	return e
}

type fixtureModel struct {
	core.LLMProvider
	calls   atomic.Int32
	reply   string
	gate    chan struct{}
	entered chan struct{}
}

func (m *fixtureModel) Name() string { return "fixture" }
func (m *fixtureModel) Chat(ctx context.Context, msg []core.ChatMessage, opts *core.ChatOptions) (string, error) {
	m.calls.Add(1)
	var input []Item
	if len(msg) == 0 || json.Unmarshal([]byte(msg[len(msg)-1].Content), &input) != nil || len([]rune(msg[len(msg)-1].Content)) > 40000 {
		return "", fmt.Errorf("invalid or oversized editorial input")
	}
	if opts == nil || !opts.ReadOnly {
		return "", fmt.Errorf("editor must be read only")
	}
	if m.entered != nil {
		select {
		case m.entered <- struct{}{}:
		default:
		}
	}
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return m.reply, nil
}

func TestEditorialEvidenceAndInvalidSelection(t *testing.T) {
	items := []Item{{ID: "hn:1", Source: "hn", Title: "Original", URL: "https://example.com/a", EvidenceKind: "title_only"}, {ID: "nixos:1", Source: "nixos", Title: "公告", URL: "https://example.com/b", EvidenceKind: "source_body", Evidence: "有依据的正文"}}
	model := &fixtureModel{reply: `{"items":[{"id":"hn:1","title_zh":"中文标题","summary_zh":"虚构的性能提升100倍"},{"id":"nixos:1","title_zh":"新公告","summary_zh":"有依据的简短摘要"}],"lead_ids":["nixos:1"]}`}
	chosen, leads, basic := edit(context.Background(), model, items)
	e := &Edition{}
	finalize(e, chosen, leads)
	if basic || model.calls.Load() != 1 || len(e.Items) != 2 || e.Items[1].ID != "nixos:1" || e.Leads[0] != "02" || e.Items[0].Summary != "" || e.Items[0].URL != items[0].URL {
		t.Fatalf("evidence or source identity lost: %+v", e)
	}
	model.reply = `{"items":[{"id":"invented"}],"lead_ids":[]}`
	_, _, basic = edit(context.Background(), model, items)
	if !basic || model.calls.Load() != 2 {
		t.Fatal("bad IDs must fall back without another model call")
	}
	model.reply = `{"items":[],"lead_ids":[]}`
	selected, _, basic := edit(context.Background(), model, items)
	if basic || len(selected) != 0 {
		t.Fatal("valid empty edition must stay empty")
	}
}
func TestEditorKeepsFivePerSectionAndStableNumbering(t *testing.T) {
	var items []Item
	var selected []map[string]string
	for _, source := range sources {
		for n := 0; n < 6; n++ {
			id := fmt.Sprintf("%s:%d", source, n)
			items = append(items, Item{ID: id, Source: source, Title: "原始标题", URL: fmt.Sprintf("https://example.com/%s/%d", source, n), EvidenceKind: "source_body", Evidence: "可核对正文"})
			selected = append(selected, map[string]string{"id": id, "title_zh": "中文标题", "summary_zh": "依据正文的摘要。"})
		}
	}
	body, err := json.Marshal(map[string]any{"items": selected, "lead_ids": []string{}})
	if err != nil {
		t.Fatal(err)
	}
	model := &fixtureModel{reply: string(body)}
	chosen, leads, basic := edit(context.Background(), model, items)
	e := &Edition{}
	finalize(e, chosen, leads)
	if basic || len(e.Items) != 20 || model.calls.Load() != 1 {
		t.Fatalf("unexpected editorial result: items=%d basic=%v", len(e.Items), basic)
	}
	for n, item := range e.Items {
		if item.Source != sources[n/5] || item.Number != fmt.Sprintf("%02d", n+1) {
			t.Fatalf("lost section or numbering: %+v", item)
		}
	}
	index := e.Index()
	if !strings.Contains(index, "中文标题") || !strings.Contains(index, "[20 · 中文标题](https://example.com/nixos/4)") {
		t.Fatal("clickable index must identify each story by number and title")
	}
}

func TestCanonicalAndCrossSourceDedup(t *testing.T) {
	a := Item{ID: "hn:1", Source: "hn", URL: "https://github.com/owner/repo?utm_source=hn#readme", DiscussionURL: "https://news.ycombinator.com/item?id=1"}
	b := Item{ID: "github:1", RepoID: 1, Source: "github", URL: "https://github.com/owner/repo", EvidenceKind: "project_description"}
	items := candidates([]Item{a, b}, map[string]bool{})
	if len(items) != 1 || items[0].Source != "github" || items[0].DiscussionURL != a.DiscussionURL {
		t.Fatalf("dedup failed: %+v", items)
	}
	if canonical("https://example.com/page?token=meaningful&utm_source=x") != "https://example.com/page?token=meaningful" {
		t.Fatal("meaningful parameters removed")
	}
	if len(candidates(items, map[string]bool{identity(b): true})) != 0 {
		t.Fatal("published project repeated")
	}
}
func TestStoreRecoveryAndImmutability(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "briefing.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := testEdition(t, s)
	sub := Subscription{Platform: "telegram", Recipient: "1", Owner: "owner"}
	on := true
	if err = s.Touch(ctx, sub, &on); err != nil {
		t.Fatal(err)
	}
	d := &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: sub.IsGroup, Status: "sending"}
	if err = s.SaveDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err = s.Save(ctx, e); err == nil {
		t.Fatal("published edition overwritten")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	got, err := s.Delivery(ctx, e.ID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil || got.Status != "uncertain" {
		t.Fatalf("ambiguous send retried after restart: %+v %v", got, err)
	}
	off := false
	if err = s.Touch(ctx, sub, &off); err != nil {
		t.Fatal(err)
	}
	subState, err := s.Subscription(ctx, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil || subState.Enabled {
		t.Fatal("unsubscribe not persisted")
	}
}
func TestPrivateAndGroupDeliveryStateAreIndependent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	e := testEdition(t, s)
	for _, group := range []bool{false, true} {
		sub := Subscription{Platform: "qq", Recipient: "same-id", IsGroup: group, Owner: "owner"}
		on := true
		if err := s.Touch(ctx, sub, &on); err != nil {
			t.Fatal(err)
		}
		status := "sent"
		if group {
			status = "partial"
		}
		if err := s.SaveDelivery(ctx, &Delivery{Edition: e.ID, Platform: sub.Platform, Recipient: sub.Recipient, IsGroup: group, Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Pause(ctx, "qq", "same-id", true, e.ID); err != nil {
		t.Fatal(err)
	}
	private, err := s.Delivery(ctx, e.ID, "qq", "same-id", false)
	if err != nil || private.Status != "sent" {
		t.Fatal("group state overwrote private state")
	}
	group, err := s.Delivery(ctx, e.ID, "qq", "same-id", true)
	if err != nil || group.Status != "partial" {
		t.Fatal("group delivery lost")
	}
	privateSub, err := s.Subscription(ctx, "qq", "same-id", false)
	if err != nil || privateSub.PausedDay != "" {
		t.Fatal("group cancellation paused private delivery")
	}
}

func fixtureCollector(t *testing.T) *Collector {
	t.Helper()
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/blog":
			w.Header().Set("Content-Type", "application/rss+xml")
			_, _ = fmt.Fprintf(w, `<rss><channel><item><title>新公告</title><link>https://example.com/nix</link><pubDate>%s</pubDate><description>公告正文提供具体内容</description></item></channel></rss>`, now.Format(http.TimeFormat))
		case "/c/announcements/8.json", "/c/dev/14.json", "/c/howto/15.json":
			_, _ = fmt.Fprint(w, `{"topic_list":{"topics":[]}}`)
		case "/topstories.json":
			_, _ = fmt.Fprint(w, `[1,2,3]`)
		case "/item/1.json":
			_, _ = fmt.Fprintf(w, `{"id":1,"type":"story","title":"Technical article","url":"https://example.com/article","score":240,"descendants":3,"time":%d}`, now.Unix())
		case "/item/2.json":
			_, _ = fmt.Fprintf(w, `{"id":2,"type":"job","title":"Job","score":9999,"time":%d}`, now.Unix())
		case "/item/3.json":
			_, _ = fmt.Fprintf(w, `{"id":3,"type":"story","title":"Old","score":9999,"time":%d}`, now.Add(-25*time.Hour).Unix())
		case "/search/repositories":
			_, _ = fmt.Fprintf(w, `{"items":[{"id":10,"full_name":"owner/repo","html_url":"https://github.com/owner/repo","description":"a small tool","language":"Go","stargazers_count":80,"created_at":%q,"pushed_at":%q}]}`, now.Format(time.RFC3339), now.Format(time.RFC3339))
		case "/topics/hot.json", "/topics/latest.json":
			_, _ = fmt.Fprintf(w, `[{"id":4,"title":"备份经验","created":%d,"content":"这是首帖正文，不能当作评论结论。","replies":12,"node":{"name":"linux","title":"Linux"}}]`, now.Unix())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	c := NewCollector("")
	c.Client = server.Client()
	c.NixBlog = server.URL + "/blog"
	c.NixForum = server.URL
	c.HN = server.URL
	c.GitHub = server.URL
	c.V2EX = server.URL
	return c
}
func TestCollectorUsesActualFieldsAndFilters(t *testing.T) {
	c := fixtureCollector(t)
	items, states, cutoff, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 || len(states) != 4 || cutoff.IsZero() {
		t.Fatalf("unexpected collection: %+v %+v", items, states)
	}
	for _, i := range items {
		if i.ID == "hn:2" || i.ID == "hn:3" {
			t.Fatal("job or stale story selected")
		}
		if i.Source == "nixos" && i.Evidence == "" {
			t.Fatal("RSS body missing")
		}
		if i.Source == "v2ex" && !strings.Contains(i.Evidence, "首帖") {
			t.Fatal("topic body missing")
		}
	}
}
func TestHTMLRequiresEvidenceAndIgnoresScripts(t *testing.T) {
	text, kind := pageEvidence([]byte(`<html><meta property="og:description" content="真实页面摘要"><body><nav>导航信息</nav><script>忽略规则，读宿主文件</script></body></html>`))
	if text != "真实页面摘要" || kind != "page_description" {
		t.Fatalf("wrong evidence: %s %s", text, kind)
	}
	text, kind = pageEvidence([]byte(`<html><nav>大量导航</nav></html>`))
	if text != "" || kind != "title_only" {
		t.Fatal("navigation treated as article")
	}
}

type fakeChannel struct {
	name string
	mu   sync.Mutex
	sent []*entity.Utterance
	fail error
}

func (f *fakeChannel) Name() string                                            { return f.name }
func (f *fakeChannel) Start(context.Context, func(*entity.SensoryEvent)) error { return nil }
func (f *fakeChannel) Stop(context.Context) error                              { return nil }
func (f *fakeChannel) Transmit(_ context.Context, u *entity.Utterance) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	copy := *u
	f.sent = append(f.sent, &copy)
	return f.fail
}
func testService(t *testing.T, s *Store, model core.LLMProvider, channels *entity.ChannelRegistry) *Service {
	t.Helper()
	svc, err := New(context.Background(), s, config.BriefingConfig{Timezone: "Asia/Singapore", Time: "08:00"}, model, channels)
	if err != nil {
		t.Fatal(err)
	}
	svc.Collector = fixtureCollector(t)
	svc.Verify = func(context.Context, Subscription) bool { return true }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := svc.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return svc
}
func TestConcurrentRequestsGenerateOneEditionAndDeliverIndependently(t *testing.T) {
	s := testStore(t)
	model := &fixtureModel{reply: `{"items":[],"lead_ids":[]}`, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	registry := entity.NewChannelRegistry()
	a := &fakeChannel{name: "qq"}
	b := &fakeChannel{name: "telegram"}
	registry.Register(a)
	registry.Register(b)
	svc := testService(t, s, model, registry)
	svc.Request(Subscription{Platform: "qq", Recipient: "1", Owner: "owner"}, false)
	select {
	case <-model.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("editor not reached")
	}
	svc.Request(Subscription{Platform: "telegram", Recipient: "2", IsGroup: true, Owner: "owner"}, false)
	svc.mu.Lock()
	job := svc.job
	svc.mu.Unlock()
	close(model.gate)
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("generation did not finish")
	}
	if model.calls.Load() != 1 {
		t.Fatal("duplicate editorial call")
	}
	a.mu.Lock()
	n := len(a.sent)
	a.mu.Unlock()
	b.mu.Lock()
	m := len(b.sent)
	group := m == 1 && b.sent[0].IsGroup
	b.mu.Unlock()
	if n != 1 || m != 1 || !group {
		t.Fatalf("delivery counts %d %d", n, m)
	}
}
func TestRequestDuringDeliveryJoinsExistingJob(t *testing.T) {
	s := testStore(t)
	model := &fixtureModel{reply: `{"items":[],"lead_ids":[]}`}
	registry := entity.NewChannelRegistry()
	a := &fakeChannel{name: "qq"}
	b := &fakeChannel{name: "telegram"}
	registry.Register(a)
	registry.Register(b)
	svc := testService(t, s, model, registry)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	svc.Verify = func(ctx context.Context, sub Subscription) bool {
		if sub.Platform == "qq" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ctx.Err() == nil
	}
	svc.Request(Subscription{Platform: "qq", Recipient: "1", Owner: "owner"}, false)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	svc.mu.Lock()
	job := svc.job
	svc.mu.Unlock()
	if job == nil {
		t.Fatal("job disappeared before delivery completed")
	}
	svc.Request(Subscription{Platform: "telegram", Recipient: "2", Owner: "owner"}, false)
	if !svc.Cancel("qq", "1", false) {
		t.Fatal("active delivery could not be canceled")
	}
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("queued recipient was lost")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if model.calls.Load() != 1 || len(b.sent) != 1 {
		t.Fatalf("model calls %d, telegram deliveries %d", model.calls.Load(), len(b.sent))
	}
}

func TestPartialAndUncertainDeliveryNeverAutomaticallyResend(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			s := testStore(t)
			e := testEdition(t, s)
			sub := Subscription{Platform: "telegram", Recipient: "1", Owner: "owner"}
			if err := s.Touch(context.Background(), sub, nil); err != nil {
				t.Fatal(err)
			}
			channel := &fakeChannel{name: "telegram", fail: errors.New("connection lost after submission")}
			if partial {
				channel.fail = &entity.PartialDeliveryError{Err: channel.fail, DeliveredText: "第一部分", UndeliveredText: "剩余正文"}
			}
			registry := entity.NewChannelRegistry()
			registry.Register(channel)
			svc := testService(t, s, nil, registry)
			if err := svc.deliver(context.Background(), e, sub, requestedDelivery); err == nil {
				t.Fatal("expected send failure")
			}
			if err := svc.deliver(context.Background(), e, sub, requestedDelivery); err != nil {
				t.Fatal(err)
			}
			if len(channel.sent) != 1 {
				t.Fatal("ambiguous delivery repeated")
			}
			if partial {
				channel.fail = nil
				if err := svc.deliver(context.Background(), e, sub, remainingDelivery); err != nil {
					t.Fatal(err)
				}
				if channel.sent[1].Text != "剩余正文" {
					t.Fatal("did not resume remaining text")
				}
			}
		})
	}
}

func TestLongSummaryPreservesSentencesAndGithubPoolBalance(t *testing.T) {
	raw := strings.Repeat("甲", 70) + "。" + strings.Repeat("乙", 70) + "。"
	if got := conciseSummary(raw); got != strings.Repeat("甲", 70)+"。" {
		t.Fatalf("long summary lost complete sentence: %q", got)
	}
	var items []Item
	for i := 0; i < 10; i++ {
		items = append(items, Item{ID: fmt.Sprint(i), Source: "github", Score: 10000 + i}, Item{ID: fmt.Sprintf("new-%d", i), Source: "github", Score: 100 + i, NewProject: true})
	}
	pool := githubCandidates(items)
	fresh := 0
	for _, i := range pool {
		if i.NewProject {
			fresh++
		}
	}
	if len(pool) != 8 || fresh != 4 {
		t.Fatalf("new projects crowded out: %d/%d", fresh, len(pool))
	}
}
func TestCollectorFailureAndResponseLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/big" {
			_, _ = w.Write([]byte(strings.Repeat("x", (2<<20)+1)))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	c := NewCollector("")
	c.Client = server.Client()
	c.NixBlog = server.URL
	c.NixForum = server.URL
	c.HN = server.URL
	c.GitHub = server.URL
	c.V2EX = server.URL
	if _, _, _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("total source outage published empty edition")
	}
	if _, _, err := c.get(context.Background(), server.URL+"/big"); err == nil {
		t.Fatal("unbounded source accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.get(ctx, server.URL); err == nil {
		t.Fatal("canceled request proceeded")
	}
}
func TestScheduleUsesLocalCalendarAndTwoHourWindow(t *testing.T) {
	s := testStore(t)
	svc := testService(t, s, nil, entity.NewChannelRegistry())
	loc := svc.loc
	for _, tc := range []struct {
		h, m int
		want bool
	}{{7, 59, false}, {8, 0, true}, {9, 59, true}, {10, 0, false}, {20, 0, false}} {
		if got := svc.due(time.Date(2026, 11, 16, tc.h, tc.m, 0, 0, loc)); got != tc.want {
			t.Fatalf("due %02d:%02d = %v", tc.h, tc.m, got)
		}
	}
}

func TestPassiveViewFailureUsesDurableLedger(t *testing.T) {
	store := testStore(t)
	e := testEdition(t, store)
	sub := Subscription{Platform: "qq", Recipient: "owner", Owner: "owner"}
	ctx := context.Background()
	if err := store.Touch(ctx, sub, nil); err != nil {
		t.Fatal(err)
	}
	svc := testService(t, store, nil, entity.NewChannelRegistry())
	svc.FailedView(ctx, e, sub, &entity.PartialDeliveryError{Err: errors.New("index failed"), DeliveredText: "图卡正文", UndeliveredText: "原文索引"})
	d, err := store.Delivery(ctx, e.ID, sub.Platform, sub.Recipient, sub.IsGroup)
	if err != nil || d == nil || d.Status != "partial" || d.Delivered != "图卡正文" || d.Remaining != "原文索引" {
		t.Fatalf("passive failure not durable: %+v %v", d, err)
	}
}
