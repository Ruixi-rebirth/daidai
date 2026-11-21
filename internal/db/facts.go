package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"daidai/internal/core"
)

// ListVisibleFacts 私聊只返回当前用户的全局事实；群聊只返回当前用户在本群记录的个人事实，避免成员画像互相泄漏。
func (m *Manager) ListVisibleFacts(ctx context.Context, platform, userID, groupID string, isGroup bool) ([]*core.Fact, error) {
	if isGroup {
		if groupID == "" {
			return nil, nil
		}
		return m.listFacts(ctx, platform, userID, `AND scope_type = 'group' AND scope_id = ?`, groupID)
	}
	return m.listFacts(ctx, platform, userID, `AND scope_type = 'global'`, "")
}

// PageVisibleFacts returns one page of facts visible in the current conversation.
func (m *Manager) PageVisibleFacts(ctx context.Context, platform, userID, groupID string, isGroup bool, page, pageSize int) ([]*core.Fact, int, error) {
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, 0, fmt.Errorf("invalid visible fact page %d with page size %d", page, pageSize)
	}
	maxInt := int(^uint(0) >> 1)
	if page-1 > maxInt/pageSize {
		return nil, 0, fmt.Errorf("visible fact page %d is too large", page)
	}
	scopeFilter := `AND scope_type = 'global'`
	args := []any{NormPlatform(platform), userID}
	if isGroup {
		if strings.TrimSpace(groupID) == "" {
			return nil, 0, nil
		}
		scopeFilter = `AND scope_type = 'group' AND scope_id = ?`
		args = append(args, groupID)
	}
	database, err := m.GetConn(platform)
	if err != nil {
		return nil, 0, err
	}
	var total int
	countQuery := `SELECT COUNT(*) FROM user_facts WHERE platform = ? AND user_id = ? AND status = 'active' ` + scopeFilter
	if err := database.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	maxPage := total / pageSize
	if total%pageSize != 0 {
		maxPage++
	}
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		return []*core.Fact{}, total, nil
	}
	query := fmt.Sprintf(`
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, embedding_model
		FROM user_facts
		WHERE platform = ? AND user_id = ? AND status = 'active' %s
		ORDER BY updated_at DESC, id DESC LIMIT ? OFFSET ?`, core.MaxFactContentRunes+1, scopeFilter)
	pageArgs := append(append([]any(nil), args...), pageSize, (page-1)*pageSize)
	rows, err := database.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	facts := make([]*core.Fact, 0, pageSize)
	for rows.Next() {
		var fact core.Fact
		var updatedAt int64
		var embedding []byte
		if err := rows.Scan(&fact.ID, &fact.UserID, &fact.ScopeType, &fact.ScopeID, &fact.Content, &updatedAt, &embedding, &fact.EmbeddingModel); err != nil {
			return nil, 0, err
		}
		fact.UpdatedAt = time.UnixMilli(updatedAt)
		fact.Embedding = decodeFloat32Slice(embedding)
		facts = append(facts, &fact)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return facts, total, nil
}

func (m *Manager) listFacts(ctx context.Context, platform, userID, scopeFilter, scopeID string) ([]*core.Fact, error) {
	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT id, user_id, scope_type, scope_id, substr(fact, 1, %d), updated_at, embedding, embedding_model
		FROM user_facts
		WHERE platform = ? AND user_id = ? AND status = 'active' ` + scopeFilter + `
		ORDER BY id ASC
	`
	query = fmt.Sprintf(query, core.MaxFactContentRunes+1)
	args := []any{p, userID}
	if scopeID != "" {
		args = append(args, scopeID)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var facts []*core.Fact
	for rows.Next() {
		var (
			id        int64
			uID       string
			scopeType string
			scID      string
			factText  string
			updatedMs int64
			embBlob   []byte
			embModel  string
		)
		if err := rows.Scan(&id, &uID, &scopeType, &scID, &factText, &updatedMs, &embBlob, &embModel); err != nil {
			return nil, err
		}
		facts = append(facts, &core.Fact{
			ID:             id,
			UserID:         uID,
			ScopeType:      scopeType,
			ScopeID:        scID,
			Content:        factText,
			Embedding:      decodeFloat32Slice(embBlob),
			EmbeddingModel: embModel,
			UpdatedAt:      time.UnixMilli(updatedMs),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

// FactMutation 长期记忆事实演进指令
type FactMutation struct {
	Action           string    `json:"action"`               // ADD, UPDATE 或 DELETE
	FactID           int64     `json:"fact_id,omitempty"`    // 当 UPDATE 或 DELETE 时指定的目标事实 ID
	SourceMessageIDs []int64   `json:"-"`                    // 提炼该事实所用的本轮原始入站消息
	EmbeddingModel   string    `json:"-"`                    // 生成向量的编码器配置哈希
	Fact             string    `json:"fact,omitempty"`       // 新增或更新后的事实陈述
	ExpectedFact     string    `json:"-"`                    // 非空时要求修改前事实仍与读取时一致
	ScopeType        string    `json:"scope_type,omitempty"` // global 或 group
	ScopeID          string    `json:"scope_id,omitempty"`   // 群号 (若是群专属事实)
	Quote            string    `json:"quote,omitempty"`      // 来源原话
	Embedding        []float32 `json:"-"`                    // 可选稠密语义向量
}

// ApplyFactMutations 事务内批量执行事实新增、原地更新与删除（保持 FactID 稳定并校验目标存在性）
func (m *Manager) ApplyFactMutations(ctx context.Context, platform, userID string, mutations []FactMutation) error {
	if len(mutations) == 0 {
		return nil
	}

	p := NormPlatform(platform)
	db, err := m.GetConn(p)
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	nowMs := time.Now().UnixMilli()

	for _, mut := range mutations {
		action := strings.ToUpper(strings.TrimSpace(mut.Action))
		if action != "ADD" && action != "UPDATE" && action != "DELETE" {
			return fmt.Errorf("不支持的记忆操作 %q", mut.Action)
		}
		scopeType := strings.TrimSpace(mut.ScopeType)
		if scopeType == "" {
			scopeType = "global"
		}
		scopeID := strings.TrimSpace(mut.ScopeID)
		switch scopeType {
		case "global":
			scopeID = ""
		case "group":
			if scopeID == "" {
				return fmt.Errorf("群专属记忆必须提供群 ID")
			}
		default:
			return fmt.Errorf("不支持的记忆范围类型 %q", scopeType)
		}

		switch action {
		case "ADD":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" {
				return fmt.Errorf("新增记忆事实不能为空")
			}
			if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
				return fmt.Errorf("记忆事实不能超过 %d 个字符", core.MaxFactContentRunes)
			}
			var existingID int64
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND fact = ? LIMIT 1",
				p, userID, scopeType, scopeID, factText,
			).Scan(&existingID)
			if err == nil && existingID > 0 {
				if err := insertFactSources(ctx, tx, existingID, mut.SourceMessageIDs, p, userID, scopeType, scopeID, nowMs); err != nil {
					return err
				}
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			result, err := tx.ExecContext(ctx,
				"INSERT INTO user_facts (platform, user_id, scope_type, scope_id, fact, status, quote, created_at, updated_at, embedding, embedding_model) VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?)",
				p, userID, scopeType, scopeID, factText, mut.Quote, nowMs, nowMs, encodeFloat32Slice(mut.Embedding), mut.EmbeddingModel,
			)
			if err != nil {
				return err
			}
			insertedFactID, err := result.LastInsertId()
			if err != nil {
				return err
			}
			if err := insertFactSources(ctx, tx, insertedFactID, mut.SourceMessageIDs, p, userID, scopeType, scopeID, nowMs); err != nil {
				return err
			}

		case "UPDATE":
			factText := strings.TrimSpace(mut.Fact)
			if factText == "" {
				return fmt.Errorf("更新记忆事实不能为空")
			}
			if utf8.RuneCountInString(factText) > core.MaxFactContentRunes {
				return fmt.Errorf("记忆事实不能超过 %d 个字符", core.MaxFactContentRunes)
			}
			if mut.FactID <= 0 {
				return fmt.Errorf("更新记忆事实必须提供有效 ID")
			}
			var duplicateID int64
			err := tx.QueryRowContext(ctx,
				"SELECT id FROM user_facts WHERE platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active' AND fact = ? AND id <> ? LIMIT 1",
				p, userID, scopeType, scopeID, factText, mut.FactID,
			).Scan(&duplicateID)
			if err == nil {
				return fmt.Errorf("更新后的记忆与同范围生效事实 ID %d 重复", duplicateID)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			query := "UPDATE user_facts SET fact = ?, quote = ?, updated_at = ?, embedding = ?, embedding_model = ? WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'"
			args := []any{factText, mut.Quote, nowMs, encodeFloat32Slice(mut.Embedding), mut.EmbeddingModel, mut.FactID, p, userID, scopeType, scopeID}
			if mut.ExpectedFact != "" {
				query += " AND fact = ?"
				args = append(args, mut.ExpectedFact)
			}
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				if mut.ExpectedFact != "" {
					return fmt.Errorf("ID 为 %d 的记忆事实已变化或不再生效，拒绝覆盖旧版本", mut.FactID)
				}
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM user_fact_sources WHERE fact_id = ?`, mut.FactID); err != nil {
				return err
			}
			if err := insertFactSources(ctx, tx, mut.FactID, mut.SourceMessageIDs, p, userID, scopeType, scopeID, nowMs); err != nil {
				return err
			}

		case "DELETE":
			if mut.FactID <= 0 {
				return fmt.Errorf("删除记忆事实必须提供有效 ID")
			}
			query := "UPDATE user_facts SET status = 'deleted', updated_at = ? WHERE id = ? AND platform = ? AND user_id = ? AND scope_type = ? AND scope_id = ? AND status = 'active'"
			args := []any{nowMs, mut.FactID, p, userID, scopeType, scopeID}
			if mut.ExpectedFact != "" {
				query += " AND fact = ?"
				args = append(args, mut.ExpectedFact)
			}
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if affected == 0 {
				if mut.ExpectedFact != "" {
					return fmt.Errorf("ID 为 %d 的记忆事实已变化或不再生效，拒绝删除新版本", mut.FactID)
				}
				return fmt.Errorf("未找到 ID 为 %d 的生效记忆事实", mut.FactID)
			}
		}
	}

	return tx.Commit()
}
