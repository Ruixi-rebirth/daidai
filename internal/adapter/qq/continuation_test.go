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

func TestEvaluateGroupContinuation_TurnsExhaustion(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_turns"
	speakerID := "user_a"

	q.recordGroupActiveSession(groupID, speakerID)

	// 第 1 轮发言：无论用词如何，均正常接续并扣减轮次，不靠生硬死词表误杀
	handled := q.evaluateGroupContinuation(groupID, speakerID, "好的，多谢啦！", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("first turn follow-up should be handled")
	}

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.remainingTurns != 1 {
		t.Fatalf("expected session remainingTurns=1, got %v", sess)
	}
	q.groupActiveSessionsMu.Unlock()

	// 第 2 轮发言：正常接续，轮次耗尽后会话自然销毁
	handled = q.evaluateGroupContinuation(groupID, speakerID, "对了我还想问个细节", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("second turn follow-up should be handled")
	}

	q.groupActiveSessionsMu.Lock()
	if q.groupActiveSessions[groupID] != nil {
		t.Fatalf("expected session to be cleaned up after exhausting turns")
	}
	q.groupActiveSessionsMu.Unlock()

	// 轮次耗尽后，第 3 轮无 @ 发言不再接话
	handled = q.evaluateGroupContinuation(groupID, speakerID, "随便说一句", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("should not handle message after turns exhausted")
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

func TestEvaluateGroupContinuation_InterjectionTargetUser(t *testing.T) {
	q := &QQChannel{
		groupActiveSessions: make(map[string]*groupActiveSession),
	}

	groupID := "test_group_interject"
	targetUser := "user_a"
	otherUser := "user_c"

	// 呆呆插话给 user_a 建立会话
	q.recordGroupActiveSession(groupID, targetUser)

	// 其他人发言（即使是无 @ 接话）-> 不会抢夺会话，计入穿插
	handled := q.evaluateGroupContinuation(groupID, otherUser, "今晚吃牛肉火锅", map[string]any{}, nil, false)
	if handled {
		t.Fatalf("other user should not trigger continuation without @")
	}

	// 目标用户 (user_a) 在插话 1 条后追问 -> 成功接续
	handled = q.evaluateGroupContinuation(groupID, targetUser, "那我该买哪个牌子好呢？", map[string]any{}, nil, false)
	if !handled {
		t.Fatalf("target user with follow-up should continue conversation")
	}

	q.groupActiveSessionsMu.Lock()
	sess := q.groupActiveSessions[groupID]
	if sess == nil || sess.speakerID != targetUser || sess.remainingTurns != 1 {
		t.Fatalf("expected session remainingTurns=1 for target user, got %v", sess)
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
