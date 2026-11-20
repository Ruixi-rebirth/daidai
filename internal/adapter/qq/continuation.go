package qq

import (
	"strings"
	"time"
	"unicode"

	"daidai/internal/config"
	"daidai/internal/entity"
)

const (
	defaultContinuationTurns = 2
	defaultContinuationTTL   = 45 * time.Second
	maxInterleavedMessages   = 2
)

// groupActiveSession 记录群聊中某位用户与呆呆的免@临时对话上下文
type groupActiveSession struct {
	speakerID        string    // 目标用户 ID
	remainingTurns   int       // 剩余免@轮次 (默认 2)
	expiresAt        time.Time // 会话超时时间 (默认交互后 45s)
	interleavedCount int       // 中间穿插的其它群友消息条数 (最多容忍 2 条)
}

// recordGroupActiveSession 显式唤醒、插话发声或引用回复呆呆时，建立或重置会话
func (q *QQChannel) recordGroupActiveSession(groupID, speakerID string) {
	if q == nil || groupID == "" {
		return
	}
	q.groupActiveSessionsMu.Lock()
	defer q.groupActiveSessionsMu.Unlock()

	if q.groupActiveSessions == nil {
		q.groupActiveSessions = make(map[string]*groupActiveSession)
	}

	q.groupActiveSessions[groupID] = &groupActiveSession{
		speakerID:        speakerID,
		remainingTurns:   defaultContinuationTurns,
		expiresAt:        time.Now().Add(defaultContinuationTTL),
		interleavedCount: 0,
	}

	// 惰性容量清理，避免长时间不活跃群聊积累无用键
	if len(q.groupActiveSessions) > 2048 {
		now := time.Now()
		for id, sess := range q.groupActiveSessions {
			if sess == nil || now.After(sess.expiresAt) {
				delete(q.groupActiveSessions, id)
			}
		}
	}
}

// clearGroupActiveSession 手动注销群聊活跃会话
func (q *QQChannel) clearGroupActiveSession(groupID string) {
	if q == nil || groupID == "" {
		return
	}
	q.groupActiveSessionsMu.Lock()
	defer q.groupActiveSessionsMu.Unlock()

	if q.groupActiveSessions != nil {
		delete(q.groupActiveSessions, groupID)
	}
}

// evaluateGroupContinuation 评估群聊中未带 @ 的消息是否为针对呆呆的免@续接追问
func (q *QQChannel) evaluateGroupContinuation(
	groupID, userID, content string,
	eventData map[string]any,
	replyCtx *entity.SensoryEvent,
	quotedBot bool,
) bool {
	if q == nil || groupID == "" || userID == "" {
		return false
	}
	q.groupActiveSessionsMu.Lock()
	defer q.groupActiveSessionsMu.Unlock()

	if q.groupActiveSessions == nil {
		return false
	}

	session, exists := q.groupActiveSessions[groupID]
	if !exists || session == nil {
		return false
	}

	now := time.Now()
	// 会话已超时：清理并拒绝接话
	if now.After(session.expiresAt) {
		delete(q.groupActiveSessions, groupID)
		return false
	}

	// 场景 1：其他人发言（非当前目标用户，且非开放会话）
	if userID != session.speakerID && session.speakerID != "" {
		session.interleavedCount++
		// 其它群友插话超过阈值 (>=3 条)：判定群聊焦点已彻底转移，熔断会话
		if session.interleavedCount > maxInterleavedMessages {
			delete(q.groupActiveSessions, groupID)
		}
		return false
	}

	// 若之前是开放会话（如主动闲聊/发梦），首个接话用户锁定为会话主体
	if session.speakerID == "" {
		session.speakerID = userID
	}

	// 场景 2：目标用户发言
	trimmed := strings.TrimSpace(content)

	// 【第一层：绝对硬规则过滤 (0ms / 0 Token)】
	// 1. @ 了别人，但没有 @ 呆呆 -> 明确转移交流目标，立即销毁会话并闭嘴
	if mentions, ok := eventData["mentions"].([]any); ok && len(mentions) > 0 {
		if !qqMessageMentionsBot(eventData) {
			delete(q.groupActiveSessions, groupID)
			return false
		}
	}
	// 2. 引用了别人（非呆呆）的消息 -> 明确与他人对话，立即销毁会话
	if replyCtx != nil && !quotedBot {
		delete(q.groupActiveSessions, groupID)
		return false
	}
	// 3. 以指令前缀开头 -> 其他机器人指令，销毁会话
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ".") {
		delete(q.groupActiveSessions, groupID)
		return false
	}

	// 引用呆呆：享完全豁免特权，满血重置
	if quotedBot {
		session.remainingTurns = defaultContinuationTurns
		session.expiresAt = now.Add(defaultContinuationTTL)
		session.interleavedCount = 0
		return true
	}

	// 明文包含呆呆称谓
	if (config.BotName() != "" && strings.Contains(content, config.BotName())) || isBotWakeNickname(trimmed) {
		session.remainingTurns--
		session.expiresAt = now.Add(defaultContinuationTTL)
		session.interleavedCount = 0
		if session.remainingTurns <= 0 {
			delete(q.groupActiveSessions, groupID)
		}
		return true
	}

	// 【第二层：时序拓扑与追问判定】
	if trimmed == "" || isPurePunctuationNoise(trimmed) {
		return false
	}

	// 场景 2A：中间没有任何人插话 (clean stream)
	// 目标用户的正常连续输入，正常扣减轮次并刷新超时时间
	if session.interleavedCount == 0 {
		session.remainingTurns--
		session.expiresAt = now.Add(defaultContinuationTTL)
		if session.remainingTurns <= 0 {
			delete(q.groupActiveSessions, groupID)
		}
		return true
	}

	// 场景 2B：中间有 1~2 条插话 (interleaved stream)
	// 只有明确具备追问特征（如包含问号、追问连词、疑问词或反馈）才接续，避免将转移话题后的无目标发言误当成回复
	if isFollowUpIndicator(trimmed) {
		session.remainingTurns--
		session.expiresAt = now.Add(defaultContinuationTTL)
		session.interleavedCount = 0
		if session.remainingTurns <= 0 {
			delete(q.groupActiveSessions, groupID)
		}
		return true
	}

	// 模糊歧义发言（中间有人插话且不带追问特征），不冒然插话，销毁会话退回静默
	delete(q.groupActiveSessions, groupID)
	return false
}

// isPurePunctuationNoise 判断是否为纯无意义符号/标点噪声（但保留问号问询）
func isPurePunctuationNoise(s string) bool {
	if strings.ContainsAny(s, "?？") {
		return false
	}
	for _, r := range s {
		if !unicode.IsPunct(r) && !unicode.IsSymbol(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// isBotWakeNickname 判断文本是否提及机器人的常用称谓/昵称
func isBotWakeNickname(s string) bool {
	lower := strings.ToLower(s)
	return strings.Contains(lower, "呆呆") ||
		strings.Contains(lower, "阿呆") ||
		strings.Contains(lower, "小呆") ||
		strings.Contains(lower, "机器人")
}

// isFollowUpIndicator 判断句子是否具备承接上文的追问连词、疑问词或状态反馈特征
func isFollowUpIndicator(s string) bool {
	lower := strings.ToLower(s)

	// 1. 包含问号
	if strings.ContainsAny(lower, "?？") {
		return true
	}

	// 2. 句首带有承接连词
	conjunctionPrefixes := []string{
		"那", "然后", "还有", "另外", "而且", "可是", "但是", "不过", "所以", "那么", "再",
	}
	for _, prefix := range conjunctionPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}

	// 3. 包含追问/疑问词或句式助词
	questionKeywords := []string{
		"怎么", "怎样", "如何", "为什么", "为啥", "哪", "几", "谁", "何时",
		"是不是", "对不对", "行不行", "好不好", "可以吗", "能不能", "可否", "行吗", "对吗", "是吗", "会吗",
		"吗", "呢", "么",
	}
	for _, kw := range questionKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}

	// 4. 包含排障与交互状态反馈词
	feedbackKeywords := []string{
		"报错", "失败", "成功", "不行", "不对", "没有", "找不到", "还是", "依然", "试了", "试过", "出了问题", "异常",
	}
	for _, kw := range feedbackKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}

	return false
}
