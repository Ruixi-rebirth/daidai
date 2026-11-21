package qq

import (
	"fmt"
	"time"
)

const passiveReplyBudgetSweepInterval = time.Minute

type passiveReplyBudget struct {
	used      int
	reserved  int
	limit     int
	expiresAt time.Time
}

func (q *QQChannel) reservePassiveReplyBudget(msgID string, count, limit int, window time.Duration) (*passiveReplyBudget, int, bool, error) {
	if msgID == "" {
		return nil, count, false, nil
	}
	if limit <= 0 || window <= 0 {
		return nil, 0, false, fmt.Errorf("invalid QQ passive reply budget: limit=%d window=%s", limit, window)
	}
	now := time.Now()
	q.replyBudgetsMu.Lock()
	if q.replyBudgets == nil {
		q.replyBudgets = make(map[string]*passiveReplyBudget)
	}
	if !now.Before(q.replyBudgetSweepAt) {
		for key, budget := range q.replyBudgets {
			if !now.Before(budget.expiresAt) && budget.reserved == 0 {
				delete(q.replyBudgets, key)
			}
		}
		q.replyBudgetSweepAt = now.Add(passiveReplyBudgetSweepInterval)
	}
	budget := q.replyBudgets[msgID]
	if budget == nil {
		budget = &passiveReplyBudget{limit: limit, expiresAt: now.Add(window)}
		q.replyBudgets[msgID] = budget
	} else if budget.limit != limit {
		q.replyBudgetsMu.Unlock()
		return nil, 0, false, fmt.Errorf("QQ message reply limit changed while processing one inbound message")
	}
	if !now.Before(budget.expiresAt) {
		if budget.reserved == 0 {
			budget.used = 0
			budget.expiresAt = now.Add(window)
		}
	}
	if budget.used+budget.reserved+count > budget.limit {
		available := budget.limit - budget.used - budget.reserved
		if available <= 0 {
			used := budget.used + budget.reserved
			q.replyBudgetsMu.Unlock()
			return nil, 0, false, fmt.Errorf("QQ 入站消息的被动回复额度已使用或预留 %d/%d 条；本次未发送，请用新消息继续请求", used, budget.limit)
		}
		budget.reserved += available
		q.replyBudgetsMu.Unlock()
		return budget, available, true, nil
	}
	budget.reserved += count
	q.replyBudgetsMu.Unlock()
	return budget, count, false, nil
}

func (q *QQChannel) commitPassiveReplyAttempt(budget *passiveReplyBudget) {
	if budget == nil {
		return
	}
	q.replyBudgetsMu.Lock()
	if budget.reserved > 0 {
		budget.reserved--
		budget.used++
	}
	q.replyBudgetsMu.Unlock()
}

func (q *QQChannel) restoreUnsentPassiveReply(budget *passiveReplyBudget) {
	if budget == nil {
		return
	}
	q.replyBudgetsMu.Lock()
	if budget.used > 0 {
		budget.used--
		budget.reserved++
	}
	q.replyBudgetsMu.Unlock()
}

func (q *QQChannel) reserveAdditionalPassiveReply(budget *passiveReplyBudget) bool {
	if budget == nil {
		return true
	}
	q.replyBudgetsMu.Lock()
	defer q.replyBudgetsMu.Unlock()
	if budget.used+budget.reserved >= budget.limit {
		return false
	}
	budget.reserved++
	return true
}

func (q *QQChannel) releasePassiveReplyReservations(budget *passiveReplyBudget, count int) {
	if budget == nil || count <= 0 {
		return
	}
	q.replyBudgetsMu.Lock()
	budget.reserved -= count
	if budget.reserved < 0 {
		budget.reserved = 0
	}
	q.replyBudgetsMu.Unlock()
}
