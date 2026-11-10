package entity

import (
	"context"
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
