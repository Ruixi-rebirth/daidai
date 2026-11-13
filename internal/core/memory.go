package core

import "time"

// MaxFactContentRunes bounds persisted manual facts and their prompt footprint.
const MaxFactContentRunes = 200

// Fact 用户长期记忆事实条目
type Fact struct {
	ID             int64
	UserID         string    // 归属用户
	ScopeType      string    // global 或 group
	ScopeID        string    // 群专属事实的群 ID
	ConvID         string    // 兼容旧调用：事实范围 ID
	Content        string    // 提炼的事实文本，如 "喜欢吃辣，住在上海"
	Embedding      []float32 // 可选稠密语义向量
	EmbeddingModel string    // 生成向量的编码器配置哈希
	UpdatedAt      time.Time
}

func (f Fact) ScopeLabel() string {
	if f.ScopeType == "group" {
		return "本群个人"
	}
	return "私聊全局"
}
