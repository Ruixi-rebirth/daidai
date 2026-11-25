package runtime

import (
	"context"
	"daidai/internal/core"
	"daidai/internal/db"
	"daidai/internal/entity"
	"errors"
	"strings"
	"testing"
	"time"
)

type mockChannel struct {
	name      string
	sentUtter *entity.Utterance
}

func (mc *mockChannel) Name() string { return mc.name }
func (mc *mockChannel) Start(ctx context.Context, onSensory func(event *entity.SensoryEvent)) error {
	return nil
}
func (mc *mockChannel) Transmit(ctx context.Context, u *entity.Utterance) error {
	mc.sentUtter = u
	return nil
}
func (mc *mockChannel) Stop(ctx context.Context) error { return nil }

type mockCognition struct {
	reply    string
	delay    time.Duration
	thinkErr error
}

func TestAskBeforeUnrequestedAttachmentProcessing(t *testing.T) {
	channel := &mockChannel{name: "qq"}
	telegramChannel := &mockChannel{name: "telegram"}
	channels := entity.NewChannelRegistry()
	channels.Register(channel)
	channels.Register(telegramChannel)
	agent := &SuperAgent{Channels: channels}
	event := &entity.SensoryEvent{
		Platform: "qq", IsGroup: true, ChannelID: "group", SenderID: "member",
		Role: entity.RoleGuest, Images: []*core.Attachment{{URL: "private-image-url"}},
	}

	if !agent.askBeforeUnrequestedAttachmentProcessing(context.Background(), event, "") {
		t.Fatal("attachment-only message should be handled before cognition")
	}
	if channel.sentUtter == nil || !strings.Contains(channel.sentUtter.Text, "还没有查看或读取") {
		t.Fatalf("expected an explicit attachment processing question, got %+v", channel.sentUtter)
	}
	if strings.Contains(channel.sentUtter.Text, "private-image-url") {
		t.Fatal("attachment URL leaked into the prompt")
	}

	channel.sentUtter = nil
	if agent.askBeforeUnrequestedAttachmentProcessing(context.Background(), event, "请描述这张图") {
		t.Fatal("an explicit attachment request should continue to cognition")
	}
	if channel.sentUtter != nil {
		t.Fatal("explicit attachment request should not receive the generic question")
	}

	missing := &entity.SensoryEvent{Platform: "telegram", IsGroup: true, ChannelID: "group", OmittedFiles: 2}
	if !agent.askBeforeUnrequestedAttachmentProcessing(context.Background(), missing, "") ||
		telegramChannel.sentUtter == nil || !strings.Contains(telegramChannel.sentUtter.Text, "2 个文件") {
		t.Fatalf("omitted attachments should still be reported: %+v", telegramChannel.sentUtter)
	}
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

func (m *mockCognition) Think(ctx context.Context, event *entity.SensoryEvent) (*entity.Utterance, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if m.thinkErr != nil {
		return nil, m.thinkErr
	}
	return &entity.Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       m.reply,
	}, nil
}

func TestSuperAgent_ReportsCognitionFailure(t *testing.T) {
	channel := &mockChannel{name: "qq"}
	channels := entity.NewChannelRegistry()
	channels.Register(channel)
	agent := NewSuperAgent(Config{
		PartnerID: "owner",
		Cognition: &mockCognition{thinkErr: errors.New("model unavailable")},
		Channels:  channels,
	})

	agent.Process(context.Background(), &entity.SensoryEvent{
		Platform: "qq", ChannelID: "owner", SenderID: "owner", Role: entity.RolePartner,
		Content: "请帮我处理",
	})
	if channel.sentUtter == nil || !strings.Contains(channel.sentUtter.Text, "这次处理没有完成") {
		t.Fatalf("cognition failure should receive a truthful fallback: %+v", channel.sentUtter)
	}
}

func TestSuperAgent_CreationAndPerception(t *testing.T) {
	partnerID := "10001"

	channelRegistry := entity.NewChannelRegistry()
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
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cleanupCancel()
		if err := superAgent.Stop(cleanupCtx); err != nil {
			t.Error(err)
		}
	})

	// 模拟感知入站（零前缀）
	event := &entity.SensoryEvent{
		Platform:   "qq",
		IsGroup:    false,
		ChannelID:  "10001",
		SenderID:   partnerID,
		SenderName: "死党伙伴",
		Role:       entity.RolePartner,
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
	immune := entity.NewImmuneSystem("10001", "20002")
	if immune.ResolveRole("10001") != entity.RolePartner {
		t.Fatalf("expected 10001 to be resolved as entity.RolePartner")
	}
	if immune.ResolveRole("20002") != entity.RoleCollaborator {
		t.Fatalf("expected 20002 to be resolved as entity.RoleCollaborator")
	}
	if immune.ResolveRole("99999") != entity.RoleGuest {
		t.Fatalf("expected 99999 to be resolved as entity.RoleGuest")
	}
	immune.AddPrivateUser("30003")
	if immune.ResolveRole("30003") != entity.RoleFriend {
		t.Fatalf("expected 30003 to be resolved as entity.RoleFriend")
	}
	if err := immune.VerifyDelegationPerm(entity.RoleGuest); err == nil {
		t.Fatalf("expected entity.RoleGuest delegation to be blocked")
	}
	if err := immune.VerifyDelegationPerm(entity.RoleFriend); err == nil {
		t.Fatalf("expected entity.RoleFriend delegation to be blocked")
	}
	if err := immune.VerifyDelegationPerm(entity.RoleCollaborator); err == nil {
		t.Fatalf("expected entity.RoleCollaborator delegation to be blocked")
	}
	if err := immune.VerifyDelegationPerm(entity.RolePartner); err != nil {
		t.Fatalf("expected entity.RolePartner delegation to be permitted: %v", err)
	}
	if err := immune.VerifyFaceGenPerm(entity.RoleGuest); err == nil {
		t.Fatalf("expected entity.RoleGuest face generation to be blocked")
	}
	if err := immune.VerifyFaceGenPerm(entity.RoleFriend); err == nil {
		t.Fatalf("expected entity.RoleFriend face generation to be blocked")
	}
	if err := immune.VerifyFaceGenPerm(entity.RoleCollaborator); err != nil {
		t.Fatalf("expected entity.RoleCollaborator face generation to be permitted: %v", err)
	}
}

func TestSuperAgent_StopInterruptsInFlightThinking(t *testing.T) {
	channelRegistry := entity.NewChannelRegistry()
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
		superAgent.Process(context.Background(), &entity.SensoryEvent{
			Platform:  "qq",
			ChannelID: "10001",
			SenderID:  "10001",
			Role:      entity.RolePartner,
			Content:   "开始慢思考",
		})
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)

	// 发送 /stop 立即打断在途思考
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
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
	status entity.AGYAuthStatusInfo
	url    string
	code   string
}

func (m *mockAGYAuth) GetStatus() entity.AGYAuthStatusInfo {
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
	channelRegistry := entity.NewChannelRegistry()
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
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
		Content:   "/agy status",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "尚未登录") {
		t.Fatalf("expected not logged in status, got: %+v", mockChan.sentUtter)
	}

	// 2. 发起登录
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
		Content:   "/agy login",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "https://accounts.google.com") {
		t.Fatalf("expected login URL in utterance, got: %+v", mockChan.sentUtter)
	}

	// 3. 提交授权码
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
		Content:   "/agy code 4/0Atestcode123",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "testuser@gmail.com") {
		t.Fatalf("expected success with email in utterance, got: %+v", mockChan.sentUtter)
	}
	if mockAuth.code != "4/0Atestcode123" {
		t.Fatalf("expected code passed to auth, got: %s", mockAuth.code)
	}

	// 4. 再次查询状态，已是在线
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
		Content:   "/agy status",
	})
	if mockChan.sentUtter == nil || !strings.Contains(mockChan.sentUtter.Text, "已登录在线") {
		t.Fatalf("expected online status, got: %+v", mockChan.sentUtter)
	}

	// 5. 验证注销
	superAgent.Process(context.Background(), &entity.SensoryEvent{
		Platform:  "qq",
		ChannelID: "10001",
		SenderID:  "10001",
		Role:      entity.RolePartner,
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
	send func(*entity.Utterance) error
}

type draftContinuationCognition struct {
	entity.Cognition
	requests []string
}

func (c *draftContinuationCognition) ContinueDraft(_ context.Context, _ *entity.SensoryEvent, id string) (*entity.Utterance, string, error) {
	c.requests = append(c.requests, id)
	return nil, "", nil
}

func TestContinuePrioritizesUnsentTextAndRespectsDraftID(t *testing.T) {
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "private", SenderID: "owner", Role: entity.RolePartner}
	channel := &mockChannel{name: "qq"}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	cognition := &draftContinuationCognition{}
	a := &SuperAgent{Channels: registry, Cognition: cognition}
	a.setReplyContinuation(ctx, event.SenderID, &entity.Utterance{Platform: event.Platform, ChannelID: event.ChannelID}, "尚未送达的正文")
	a.setReplyContinuation(ctx, event.SenderID, &entity.Utterance{Platform: event.Platform, ChannelID: event.ChannelID, DraftID: "draft-2"}, "草稿二未送达的正文")

	event.Content = "/continue\tdraft-2"
	a.Process(ctx, event)
	if len(cognition.requests) != 0 || channel.sentUtter == nil || channel.sentUtter.Text != "草稿二未送达的正文" {
		t.Fatalf("selected draft generated a new segment instead of resending: requests=%v sent=%+v", cognition.requests, channel.sentUtter)
	}

	event.Content = "/continue missing"
	a.Process(ctx, event)
	if len(cognition.requests) != 1 || cognition.requests[0] != "missing" ||
		channel.sentUtter == nil || !strings.Contains(channel.sentUtter.Text, "草稿") {
		t.Fatalf("explicit draft selection failed: requests=%v sent=%+v", cognition.requests, channel.sentUtter)
	}

	event.Content = "继续"
	a.Process(ctx, event)
	if len(cognition.requests) != 1 || channel.sentUtter == nil || channel.sentUtter.Text != "尚未送达的正文" {
		t.Fatalf("pending text did not take priority: requests=%v sent=%+v", cognition.requests, channel.sentUtter)
	}
}

func (c *failingReplyChannel) Transmit(_ context.Context, u *entity.Utterance) error {
	return c.send(u)
}

func TestReplyContinuationDelivery(t *testing.T) {
	ctx := context.Background()
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "group", IsGroup: true, SenderID: "owner"}
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	a := &SuperAgent{Channels: registry}
	u := &entity.Utterance{Platform: "qq", ChannelID: "group", IsGroup: true, Text: "尚未送达的正文"}
	commits := 0
	u.OnDelivered = func(context.Context) { commits++ }
	channel.send = func(*entity.Utterance) error { return errors.New("offline") }
	if _, err := a.transmitUtterance(ctx, u, "owner"); err == nil {
		t.Fatal("expected send failure")
	}
	other := *event
	other.SenderID = "other"
	if _, ok := a.takeReplyContinuation(&other, ""); ok {
		t.Fatal("another user received the continuation")
	}
	// A failed retry must restore the original exactly once.
	a.handleContinueCommand(ctx, event, "继续")
	got, ok := a.takeReplyContinuation(event, "")
	if !ok || got.text != u.Text {
		t.Fatalf("failed retry lost or duplicated reply: %q", got.text)
	}
	a.restoreReplyContinuation(ctx, event, got)
	channel.send = func(*entity.Utterance) error {
		return &entity.PartialDeliveryError{Err: errors.New("offline"), DeliveredText: "尚未", UndeliveredText: "送达的正文"}
	}
	a.handleContinueCommand(ctx, event, "继续")
	got, _ = a.takeReplyContinuation(event, "")
	if got.text != "送达的正文" {
		t.Fatalf("partial retry retained delivered text: %q", got.text)
	}
	if commits != 0 {
		t.Fatal("partial delivery committed the reply")
	}
	a.restoreReplyContinuation(ctx, event, got)
	channel.send = func(u *entity.Utterance) error {
		if u.Text != got.text {
			t.Fatalf("unexpected retry: %q", u.Text)
		}
		if u.TargetUserID != event.SenderID || len(u.MentionUserIDs) != 1 || u.MentionUserIDs[0] != event.SenderID {
			t.Fatalf("retry did not address the requester: %+v", u)
		}
		return nil
	}
	a.handleContinueCommand(ctx, event, "继续")
	if _, ok := a.takeReplyContinuation(event, ""); ok {
		t.Fatal("successful reply still pending")
	}
	if commits != 1 {
		t.Fatalf("successful retry committed %d times", commits)
	}
}

func TestReplyContinuationOmitsPaginationHint(t *testing.T) {
	ctx := context.Background()
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	a := &SuperAgent{Channels: registry}
	channel.send = func(u *entity.Utterance) error {
		if !u.HasMoreText || u.DraftID != "draft-1" || strings.Contains(u.Text, "/continue") || len(u.ImageSources) != 0 || u.InlineNonTextImageCount != 0 {
			t.Fatalf("invalid paginated text payload: draft prompt or media metadata")
		}
		return &entity.PartialDeliveryError{Err: errors.New("offline"), UndeliveredText: u.Text}
	}
	text := strings.Repeat("正文内容", 2000) + "结尾"
	u := &entity.Utterance{Platform: "qq", ChannelID: "private", DraftID: "draft-1", ImageSources: []string{"图片来源"}, InlineNonTextImageCount: 1}
	if _, err := a.transmitTextFallback(ctx, u, text, "owner"); err != nil {
		t.Fatal(err)
	}
	got, _ := a.takeReplyContinuation(&entity.SensoryEvent{Platform: "qq", ChannelID: "private", SenderID: "owner"}, "draft-1")
	if strings.Contains(got.text, "已达到平台") || !strings.HasSuffix(got.text, "结尾") {
		t.Fatal("continuation contains pagination metadata or lost its tail")
	}
	if strings.ReplaceAll(got.text, "\n", "") != text {
		t.Fatal("partial delivery lost or duplicated content")
	}
}

func TestResetCommandUsesConfiguredDatabase(t *testing.T) {
	ctx := context.Background()
	store := db.NewManager(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	draft := db.ConversationDraft{ID: "existing", Platform: "qq", ChatType: "private", ConversationID: "chat", RequesterID: "owner", ContextJSON: "{}", UpdatedAt: time.Now()}
	if err := store.SaveConversationDraft(ctx, draft); err != nil {
		t.Fatal(err)
	}
	channel := &mockChannel{name: "qq"}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := NewSuperAgent(Config{Database: store, Channels: registry})
	agent.handleResetCommand(ctx, &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner", Role: entity.RolePartner}, "/reset")
	remaining, err := store.GetConversationDraft(ctx, draft.Platform, draft.ChatType, draft.ConversationID, draft.RequesterID, draft.ID)
	if err != nil || remaining != nil || channel.sentUtter == nil {
		t.Fatalf("configured store was not reset: draft=%+v, err=%v", remaining, err)
	}
}

type pendingCardRenderer struct{ entity.Cognition }

func (*pendingCardRenderer) RenderCard(context.Context, *entity.SensoryEvent, string) ([]byte, error) {
	return []byte("card"), nil
}

func TestPendingNotificationKeepsOnlyUnsentCardText(t *testing.T) {
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	sends := 0
	channel.send = func(*entity.Utterance) error {
		sends++
		return &entity.PartialDeliveryError{Err: errors.New("partial"), DeliveredText: "前段", UndeliveredText: "尾段"}
	}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := &SuperAgent{Channels: registry, Cognition: &pendingCardRenderer{}}
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner"}
	complete, remaining, err := agent.transmitPendingNotification(context.Background(), event, "通知", "前段尾段")
	if err != nil || complete || remaining != "尾段" || sends != 1 {
		t.Fatalf("partial notification was not retained correctly: complete=%v remaining=%q sends=%d err=%v", complete, remaining, sends, err)
	}
}

func TestPendingNotificationDoesNotImmediatelyResendAmbiguousCard(t *testing.T) {
	channel := &failingReplyChannel{mockChannel: mockChannel{name: "qq"}}
	sends := 0
	transportErr := errors.New("missing response")
	channel.send = func(*entity.Utterance) error {
		sends++
		return transportErr
	}
	registry := entity.NewChannelRegistry()
	registry.Register(channel)
	agent := &SuperAgent{Channels: registry, Cognition: &pendingCardRenderer{}}
	event := &entity.SensoryEvent{Platform: "qq", ChannelID: "chat", SenderID: "owner"}
	complete, _, err := agent.transmitPendingNotification(context.Background(), event, "通知", "等待发送")
	if complete || !errors.Is(err, transportErr) || sends != 1 {
		t.Fatalf("ambiguous card was retried immediately: complete=%v sends=%d err=%v", complete, sends, err)
	}
}
