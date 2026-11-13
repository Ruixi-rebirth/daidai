package entity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type mockChannel struct {
	name      string
	sentUtter *Utterance
}

func (mc *mockChannel) Name() string { return mc.name }
func (mc *mockChannel) Start(ctx context.Context, onSensory func(event *SensoryEvent)) error {
	return nil
}
func (mc *mockChannel) Transmit(ctx context.Context, u *Utterance) error {
	mc.sentUtter = u
	return nil
}
func (mc *mockChannel) Stop(ctx context.Context) error { return nil }

type mockCognition struct {
	reply string
	delay time.Duration
}

func (m *mockCognition) MindStatus() string {
	return "【当前心智运行状态】\n- 激活模型：mock-model"
}

func (m *mockCognition) ModelList() string {
	return "【实时探测可用心智模型池】\n### API 通道\n- `mock-model` *(当前激活)*"
}

func (m *mockCognition) ModelUsage() string {
	return "【实时探测可用心智模型池】\n### API 通道\n- `mock-model` *(当前激活)*\n\n---\n\n### AGY 实时配额与剩余使用量 (Quota Pools)\n- 5-Hour Limit: 100%"
}

func (m *mockCognition) Think(ctx context.Context, event *SensoryEvent) (*Utterance, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       m.reply,
	}, nil
}

func TestSuperAgent_CreationAndPerception(t *testing.T) {
	partnerID := "10001"

	channelRegistry := NewChannelRegistry()
	mockChan := &mockChannel{name: "qq"}
	channelRegistry.Register(mockChan)

	cognition := &mockCognition{reply: "今天准备搞定哪个大工程？"}

	superAgent := NewSuperAgent(Config{
		PartnerID: partnerID,
		Cognition: cognition,
		Channels:  channelRegistry,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := superAgent.Start(ctx); err != nil {
		t.Fatalf("start superAgent failed: %v", err)
	}
	defer superAgent.Stop(ctx)

	// 模拟感知入站（零前缀）
	event := &SensoryEvent{
		Platform:   "qq",
		IsGroup:    false,
		ChannelID:  "10001",
		SenderID:   partnerID,
		SenderName: "死党伙伴",
		Role:       RolePartner,
		Content:    "呆呆早啊",
		OccurredAt: time.Now(),
	}

	superAgent.Process(ctx, event)

	if mockChan.sentUtter == nil {
		t.Fatalf("expected utterance delivered to mock channel")
	}

	if !strings.Contains(mockChan.sentUtter.Text, "大工程") {
		t.Errorf("expected response to contain '大工程', got: %s", mockChan.sentUtter.Text)
	}
}

func TestImmuneSystem_RoleAndDelegationPerm(t *testing.T) {
	immune := NewImmuneSystem("10001", "20002")
	if immune.ResolveRole("10001") != RolePartner {
		t.Fatalf("expected 10001 to be resolved as RolePartner")
	}
	if immune.ResolveRole("20002") != RoleCollaborator {
		t.Fatalf("expected 20002 to be resolved as RoleCollaborator")
	}
	if immune.ResolveRole("99999") != RoleGuest {
		t.Fatalf("expected 99999 to be resolved as RoleGuest")
	}
	if err := immune.VerifyDelegationPerm(RoleGuest); err == nil {
		t.Fatalf("expected RoleGuest delegation to be blocked")
	}
	if err := immune.VerifyDelegationPerm(RolePartner); err != nil {
		t.Fatalf("expected RolePartner delegation to be permitted: %v", err)
	}
}

func TestSuperAgent_StopInterruptsInFlightThinking(t *testing.T) {
	channelRegistry := NewChannelRegistry()
	mockChan := &mockChannel{name: "qq"}
	channelRegistry.Register(mockChan)

	cognition := &mockCognition{
		reply: "这段慢思考应该被 /stop 打断",
		delay: 5 * time.Second,
	}

	superAgent := NewSuperAgent(Config{
		PartnerID: "10001",
		Cognition: cognition,
		Channels:  channelRegistry,
	})

	done := make(chan struct{})
	go func() {
		superAgent.Process(context.Background(), &SensoryEvent{
			Platform:  "qq",
			ChannelID: "10001",
			SenderID:  "10001",
			Role:      RolePartner,
			Content:   "开始慢思考",
		})
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)

	// 发送 /stop 立即打断在途思考
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/stop",
	})

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected in-flight Think to be interrupted immediately by /stop")
	}

	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "已紧急中断") {
		t.Fatalf("expected /stop confirmation utterance, got %+v", mockChan.sentUtter)
	}
}

type mockAGYAuth struct {
	status AGYAuthStatusInfo
	url    string
	code   string
}

func (m *mockAGYAuth) GetStatus() AGYAuthStatusInfo {
	return m.status
}

func (m *mockAGYAuth) CheckAuth() error {
	if !m.status.IsLoggedIn {
		return errors.New("not logged in")
	}
	return nil
}

func (m *mockAGYAuth) StartLogin(ctx context.Context) (string, error) {
	return m.url, nil
}

func (m *mockAGYAuth) SubmitCode(ctx context.Context, code string) (string, error) {
	m.code = code
	m.status.IsLoggedIn = true
	m.status.Email = "testuser@gmail.com"
	return m.status.Email, nil
}

func (m *mockAGYAuth) Cancel() {}

func (m *mockAGYAuth) Logout() error {
	m.status.IsLoggedIn = false
	m.status.Email = ""
	return nil
}

func TestSuperAgent_AGYAuthCommands(t *testing.T) {
	mockChan := &mockChannel{name: "qq"}
	channelRegistry := NewChannelRegistry()
	channelRegistry.Register(mockChan)

	mockAuth := &mockAGYAuth{
		url: "https://accounts.google.com/o/oauth2/auth?test=1",
	}

	superAgent := NewSuperAgent(Config{
		PartnerID: "10001",
		Channels:  channelRegistry,
		AGYAuth:   mockAuth,
	})

	// 1. 未登录状态查询
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/agy status",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "尚未登录") {
		t.Fatalf("expected not logged in status, got: %+v", mockChan.sentUtter)
	}

	// 2. 发起登录
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/agy login",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "https://accounts.google.com") {
		t.Fatalf("expected login URL in utterance, got: %+v", mockChan.sentUtter)
	}

	// 3. 提交授权码
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/agy code 4/0Atestcode123",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "testuser@gmail.com") {
		t.Fatalf("expected success with email in utterance, got: %+v", mockChan.sentUtter)
	}
	if mockAuth.code != "4/0Atestcode123" {
		t.Fatalf("expected code passed to auth, got: %s", mockAuth.code)
	}

	// 4. 再次查询状态，已是在线
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/agy status",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "已登录在线") {
		t.Fatalf("expected online status, got: %+v", mockChan.sentUtter)
	}

	// 5. 验证注销
	superAgent.Process(context.Background(), &SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      RolePartner,
		Content:   "/agy logout",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "账号已注销") {
		t.Fatalf("expected logout success utterance, got: %+v", mockChan.sentUtter)
	}
	if mockAuth.status.IsLoggedIn {
		t.Fatalf("expected logged out state in mockAuth")
	}
}

type failingReplyChannel struct {
	mockChannel
	send func(*Utterance) error
}

func (c *failingReplyChannel) Transmit(_ context.Context, u *Utterance) error {
	return c.send(u)
}

func TestReplyContinuationDelivery(t *testing.T) {
	ctx := context.Background()
	event := &SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, SenderID: "owner"}
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	registry := NewChannelRegistry()
	registry.Register(channel)
	a := &SuperAgent{Channels: registry}
	u := &Utterance{Platform: "qq", ChannelID: "group", IsGroup: true, Text: "尚未送达的正文"}
	channel.send = func(*Utterance) error { return errors.New("offline") }
	if _, err := a.transmitUtterance(ctx, u, "owner"); err == nil {
		t.Fatal("expected send failure")
	}
	other := *event
	other.SenderID = "other"
	if _, ok := a.takeReplyContinuation(&other); ok {
		t.Fatal("another user received the continuation")
	}
	// A failed retry must restore the original exactly once.
	a.handleContinueCommand(ctx, event, "")
	got, ok := a.takeReplyContinuation(event)
	if !ok || got != u.Text {
		t.Fatalf("failed retry lost or duplicated reply: %q", got)
	}
	a.restoreReplyContinuation(ctx, event, got)
	channel.send = func(*Utterance) error {
		return &PartialDeliveryError{Err: errors.New("offline"), DeliveredText: "尚未", UndeliveredText: "送达的正文"}
	}
	a.handleContinueCommand(ctx, event, "")
	got, _ = a.takeReplyContinuation(event)
	if got != "送达的正文" {
		t.Fatalf("partial retry retained delivered text: %q", got)
	}
	a.restoreReplyContinuation(ctx, event, got)
	channel.send = func(u *Utterance) error {
		if u.Text != got {
			t.Fatalf("unexpected retry: %q", u.Text)
		}
		return nil
	}
	a.handleContinueCommand(ctx, event, "")
	if _, ok := a.takeReplyContinuation(event); ok {
		t.Fatal("successful reply still pending")
	}
}

func TestReplyContinuationOmitsPaginationHint(t *testing.T) {
	ctx := context.Background()
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	registry := NewChannelRegistry()
	registry.Register(channel)
	a := &SuperAgent{Channels: registry}
	channel.send = func(u *Utterance) error {
		return &PartialDeliveryError{Err: errors.New("offline"), UndeliveredText: u.Text}
	}
	text := strings.Repeat("正文内容", 2000) + "结尾"
	u := &Utterance{Platform: "qq", ChannelID: "private"}
	if _, err := a.transmitTextFallback(ctx, u, text, "owner"); err != nil {
		t.Fatal(err)
	}
	got, _ := a.takeReplyContinuation(&SensoryEvent{Platform: "qq", ChannelID: "private", SenderID: "owner"})
	if strings.Contains(got, "已达到平台") || !strings.HasSuffix(got, "结尾") {
		t.Fatal("continuation contains pagination metadata or lost its tail")
	}
	if strings.ReplaceAll(got, "\n", "") != text {
		t.Fatal("partial delivery lost or duplicated content")
	}
}
