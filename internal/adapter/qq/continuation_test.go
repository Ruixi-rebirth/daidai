package qq

import (
	"testing"
	"time"

	"daidai/internal/entity"
)

func TestEvaluateGroupContinuation_FullLifecycle(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_1"
	speakerID := "speaker_1"

	// 初始唤醒
	q.recordGroupActiveSession(groupID, speakerID)

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.remainingTurns != 2 {
		t.Fatalf("expected remainingTurns=2, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()

	// 第 1 轮追问 (免@)
	handled := q.evaluateGroupContinuation(groupID, speakerID, "那明天呢", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("expected turn 1 to be handled, got false")
	}

	q.groupActiveSessionsMu.Lock()
	sess = q.groupActiveSessions[groupID]
	if sess == nil || sess.remainingTurns != 1 {
		t.Fatalf("expected remainingTurns=1 after turn 1, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()

	// 第 2 轮追问 (免@)
	handled = q.evaluateGroupContinuation(groupID, speakerID, "后天会不会下雨？", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("expected turn 2 to be handled, got false")
	}

	// 2轮耗尽后，Session 应自然销毁
	q.groupActiveSessionsMu.Lock()
	sess = q.groupActiveSessions[groupID]
	if sess != nil {
		t.Fatalf("expected session to be cleaned up after turn 2, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()

	// 第 3 轮追问：由于没有重新 @，应被忽略
	handled = q.evaluateGroupContinuation(groupID, speakerID, "大后天呢", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("expected turn 3 to be rejected, got true")
	}
}

func TestEvaluateGroupContinuation_TTLExpiry(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_ttl"
	speakerID := "speaker_ttl"

	q.recordGroupActiveSession(groupID, speakerID)

	// 人工将会话修改为已超时
	q.groupActiveSessionsMu.Lock()
	q.groupActiveSessions[groupID].expiresAt = time.Now().Add(-1 * time.Second)
	q.groupActiveSessionsMu.Unlock()

	handled := q.evaluateGroupContinuation(groupID, speakerID, "那明天呢", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("expected expired session to reject continuation, got true")
	}

	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected expired session to be deleted from map")
	}
	q.groupActiveSessionsMu.Unlock()
}

func TestEvaluateGroupContinuation_InterleavingToleranceAndBreak(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_interleave"
	speakerID := "user_a"

	q.recordGroupActiveSession(groupID, speakerID)

	// 其它群友发言第 1 条（容忍）
	handled := q.evaluateGroupContinuation(groupID, "user_b", "哈哈今天真冷", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("other user message should not be handled as continuation")
	}

	// 其它群友发言第 2 条（容忍）
	handled = q.evaluateGroupContinuation(groupID, "user_c", "确实好冷", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("other user message should not be handled as continuation")
	}

	// 目标用户 A 在容忍阈值内发送追问（带连词“那”）
	handled = q.evaluateGroupContinuation(groupID, speakerID, "那羽绒服怎么挑？", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("user_a follow-up within tolerance should be handled")
	}

	// 验证插话计数清零
	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.interleavedCount != 0 {
		t.Fatalf("expected interleavedCount to reset to 0, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()

	// 连续 3 条其它群友发言 -> 熔断
	q.evaluateGroupContinuation(groupID, "user_b", "msg 1", map[string]any{}, nil, false)
	q.evaluateGroupContinuation(groupID, "user_c", "msg 2", map[string]any{}, nil, false)
	q.evaluateGroupContinuation(groupID, "user_d", "msg 3", map[string]any{}, nil, false)

	q.groupActiveSessionsMu.Lock()
	sess = q.groupActiveSessions[groupID]
	if sess != nil {
		t.Fatalf("expected session to be destroyed after 3 interleaved messages")
	}
	q.groupActiveSessionsMu.Unlock()

	// 用户 A 之后再说话不再触发续接
	handled = q.evaluateGroupContinuation(groupID, speakerID, "那怎么办", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("expected rejection after session was broken by interleaving")
	}
}

func TestEvaluateGroupContinuation_NegativeChecksBreakSession(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_neg"
	speakerID := "user_a"

	// 场景 1: @ 了其它群友
	q.recordGroupActiveSession(groupID, speakerID)
	mentionsOther := map[string]any{
		"mentions": []any{
			map[string]any{"user_openid": "user_other", "bot": false},
		},
	}
	handled := q.evaluateGroupContinuation(groupID, speakerID, "你觉得呢", mentionsOther, nil, false)
	if handled {
		t.Fatalf("mentioning other user should not be handled")
	}
	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected session to be destroyed when user @'d someone else")
	}
	q.groupActiveSessionsMu.Unlock()

	// 场景 2: 引用了其它群友的消息
	q.recordGroupActiveSession(groupID, speakerID)
	otherReplyCtx := &entity.SensoryEvent{Content: "其他人说的内容", RawMessageID: "msg_other"}
	handled = q.evaluateGroupContinuation(groupID, speakerID, "确实是这样", map[string]any{}, otherReplyCtx, false)
	if handled {
		t.Fatalf("quoting other user should not be handled")
	}
	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected session to be destroyed when user quoted someone else")
	}
	q.groupActiveSessionsMu.Unlock()

	// 场景 3: 触发了其他指令
	q.recordGroupActiveSession(groupID, speakerID)
	handled = q.evaluateGroupContinuation(groupID, speakerID, "/help", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("command prefix should not be handled")
	}
	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected session to be destroyed on command prefix")
	}
	q.groupActiveSessionsMu.Unlock()
}

func TestEvaluateGroupContinuation_FarewellTerminatesSession(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_farewell"
	speakerID := "user_a"

	q.recordGroupActiveSession(groupID, speakerID)

	// 用户在第 1 轮就表达收尾感谢
	handled := q.evaluateGroupContinuation(groupID, speakerID, "好的，多谢啦！", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("farewell should be handled to reply politeness")
	}

	// 但 Session 应立即被清理，防止后续自作多情
	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected session to be cleaned up immediately on farewell")
	}
	q.groupActiveSessionsMu.Unlock()

	// 紧接着再发言不再接话
	handled = q.evaluateGroupContinuation(groupID, speakerID, "随便说一句", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("should not handle message after farewell")
	}
}

func TestEvaluateGroupContinuation_QuoteBotExemption(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_quote"
	speakerID := "user_a"

	q.recordGroupActiveSession(groupID, speakerID)

	// 消耗 1 轮
	q.evaluateGroupContinuation(groupID, speakerID, "那明天呢", map[string]any{}, nil, false)

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess.remainingTurns != 1 {
		t.Fatalf("expected remainingTurns=1, got %d", sess.remainingTurns)
	}
	q.groupActiveSessionsMu.Unlock()

	// 用户引用回复呆呆的消息 (quotedBot = true)
	handled := q.evaluateGroupContinuation(groupID, speakerID, "这个报错怎么看？", map[string]any{}, nil, true)
	if !handled {
		t.Fatalf("quoting bot should always be handled")
	}

	// 引用呆呆应享受满血豁免重置
	q.groupActiveSessionsMu.Lock()
	sess = q.groupActiveSessions[groupID]
	if sess == nil || sess.remainingTurns != 2 {
		t.Fatalf("expected quoting bot to reset remainingTurns to 2, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()
}

func TestEvaluateGroupContinuation_InterjectionAndPartnerPrivilege(t *testing.T) {
	immune := entity.NewImmuneSystem("partner_openid")
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
		immune:              immune,
	}

	groupID := "test_group_interject"
	targetUser := "user_a"
	partnerUser := "partner_openid"
	otherUser := "user_c"

	// 呆呆插话给 user_a 建立会话
	q.recordGroupActiveSession(groupID, targetUser)

	// 场景 A：搭档（月）在干净消息流中直接接话 -> 享受特权，无需 @ 呆呆
	handled := q.evaluateGroupContinuation(groupID, partnerUser, "哈哈你又插话，别逗他了", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("partner should trigger continuation in clean stream even after interjection to another user")
	}

	// 场景 B：重新插话建立会话，路人群友发言 1 条
	q.recordGroupActiveSession(groupID, targetUser)
	handled = q.evaluateGroupContinuation(groupID, otherUser, "今晚吃牛肉火锅", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("other user should not trigger continuation")
	}

	// 此时有 1 条插话，搭档用追问词接话 -> 成功唤醒
	handled = q.evaluateGroupContinuation(groupID, partnerUser, "那你觉得应该怎么做？", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("partner with follow up question should handle interleaved continuation")
	}

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.speakerID != partnerUser || sess.remainingTurns != 1 {
		t.Fatalf("expected session to transfer to partner with remainingTurns=1, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()
}

func TestEvaluateGroupContinuation_OpenSessionFromChatter(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_open"

	// 呆呆主动发梦/闲聊 (speakerID为空，开放会话)
	q.recordGroupActiveSession(groupID, "")

	// 任何群友首个接话 -> 锁定会话并接续
	handled := q.evaluateGroupContinuation(groupID, "user_first", "梦到啥离谱的了说说看", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("open session should accept first responsive user")
	}

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.speakerID != "user_first" || sess.remainingTurns != 1 {
		t.Fatalf("expected open session to lock to user_first with remainingTurns=1, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()
}
