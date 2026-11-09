package core

import "time"

// Fact 用户长期记忆事实条目
type Fact struct {
	ID        int64
	UserID    string    // 归属用户
	ConvID    string    // 来源会话
	Content   string    // 提炼的事实文本，如 "喜欢吃辣，住在上海"
	Embedding []float32 // 可选稠密语义向量 (BAAI/bge-m3)
	UpdatedAt time.Time
}
