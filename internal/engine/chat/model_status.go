package chat

import (
	"context"
	"encoding/json"
	"fmt"

	"daidai/internal/entity"
)

// MindStatus 返回当前激活的心智运行状态摘要
func (c *Cortex) MindStatus() string {
	if c.llm == nil {
		return ""
	}
	if formatter, ok := c.llm.(interface{ FormatCurrentStatus() string }); ok {
		return formatter.FormatCurrentStatus()
	}
	return fmt.Sprintf("【当前心智运行状态】\n- 激活模型：%s", c.llm.Name())
}

// ModelList 返回实时心智模型池与各通道列表
func (c *Cortex) ModelList() string {
	return c.ModelListContext(context.Background())
}

func (c *Cortex) ModelListContext(ctx context.Context) string {
	if c.hands == nil {
		return c.MindStatus()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := c.hands.Execute(entity.WithSystemToolAccess(ctx, "switch_model"), "switch_model", json.RawMessage(`{"action":"list"}`))
	if err != nil || res == nil || res.Text == "" {
		return c.MindStatus()
	}
	return res.Text
}

// ModelUsage 返回实时模型通道与配额用量详情
func (c *Cortex) ModelUsage() string {
	return c.ModelUsageContext(context.Background())
}

func (c *Cortex) ModelUsageContext(ctx context.Context) string {
	if c.hands == nil {
		return c.MindStatus()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := c.hands.Execute(entity.WithSystemToolAccess(ctx, "switch_model"), "switch_model", json.RawMessage(`{"action":"usage"}`))
	if err != nil || res == nil || res.Text == "" {
		return c.ModelListContext(ctx)
	}
	return res.Text
}
