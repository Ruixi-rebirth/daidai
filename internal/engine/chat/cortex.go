package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"daidai/internal/core"
	"daidai/internal/engine/vision"
	"daidai/internal/entity"
)

// Cortex 具体双轨认知大脑实现 (实现 entity.Cognition)
type Cortex struct {
	llm          core.LLMProvider
	persona      *entity.Persona
	memory       entity.Memory
	hands        entity.Hands
	vision       *vision.Engine
	workspaces   *entity.WorkspaceManager
	workingLimit int
}

// CortexConfig 认知大脑初始化配置
type CortexConfig struct {
	LLM          core.LLMProvider
	Persona      *entity.Persona
	Memory       entity.Memory
	Hands        entity.Hands
	Vision       *vision.Engine
	Workspaces   *entity.WorkspaceManager
	WorkingLimit int
}

// NewCortex 按配置实例化认知大脑
func NewCortex(cfg CortexConfig) *Cortex {
	limit := cfg.WorkingLimit
	if limit <= 0 {
		limit = 30
	}
	return &Cortex{
		llm:          cfg.LLM,
		persona:      cfg.Persona,
		memory:       cfg.Memory,
		hands:        cfg.Hands,
		vision:       cfg.Vision,
		workspaces:   cfg.Workspaces,
		workingLimit: limit,
	}
}

// SetWorkingLimit 设置滑动工作记忆消息条数上限
func (c *Cortex) SetWorkingLimit(limit int) {
	if limit > 0 {
		c.workingLimit = limit
	}
}

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
	if c.hands == nil {
		return c.MindStatus()
	}
	res, err := c.hands.Execute(context.Background(), "switch_model", json.RawMessage(`{"action":"list"}`))
	if err != nil || res == nil || res.Text == "" {
		return c.MindStatus()
	}
	return res.Text
}

// Think 对感知事件进行认知推理并一次性合成全息生命表达体 (Utterance)
func (c *Cortex) Think(ctx context.Context, event *entity.SensoryEvent) (*entity.Utterance, error) {
	// 注入角色设定、现实时空锚点与长期记忆沉淀
	now := time.Now()
	weekdayCn := [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}[now.Weekday()]
	timeContext := fmt.Sprintf("\n【当前客观物理时空】：%s %s (本地时区: %s)\n",
		now.Format("2006-01-02 15:04:05"), weekdayCn, now.Location().String())

	if mindStatus := c.MindStatus(); mindStatus != "" {
		timeContext += mindStatus + "\n"
	}

	if c.workspaces != nil && event != nil && event.Role != entity.RoleGuest {
		wsPath, wsName := c.workspaces.GetCurrent(event.ChannelID)
		if c.workspaces.IsSandbox(wsPath) {
			timeContext += fmt.Sprintf("【当前工作空间】：%s (沙盒)\n", wsPath)
		} else {
			timeContext += fmt.Sprintf("【当前工作空间】：%s (工程: %s)\n", wsPath, wsName)
		}
	}
	if !event.IsPartnerPrivate() {
		timeContext += "【安全红线】：当前非伙伴私聊会话，严禁探测、查询或透露宿主机的本机公网 IP 地址。\n"
	}

	sysPrompt := ""
	if c.persona != nil {
		sysPrompt = c.persona.CompilePrompt()
	}
	sysPrompt += timeContext

	if c.memory != nil {
		if episodic := c.memory.RecallEpisodic(ctx, event); episodic != "" {
			sysPrompt += "\n" + episodic + "\n"
		}
	}

	messages := []core.ChatMessage{
		{Role: core.RoleSystem, Content: sysPrompt},
	}

	// 注入近期工作记忆上下文 (默认 30 条，保证长话题与深度探讨不忘前文)
	limit := c.workingLimit
	if limit <= 0 {
		limit = 30
	}
	if c.memory != nil {
		messages = append(messages, c.memory.WorkingContext(ctx, event, limit)...)
	}

	userContent := event.Content

	// 若物理通道携带了引用的上下文消息，按原生 Markdown 引用块语义呈现
	if event.ReplyContext != nil && event.ReplyContext.Content != "" {
		quoted := "> " + strings.ReplaceAll(strings.TrimSpace(event.ReplyContext.Content), "\n", "\n> ")
		userContent = quoted + "\n\n" + event.Content
	}

	// 多模态视觉感知注入与摘要留存
	if c.vision != nil && c.vision.IsAvailable() && len(event.Images) > 0 {
		var imgURLs []string
		for _, img := range event.Images {
			if img.URL != "" {
				imgURLs = append(imgURLs, img.URL)
			}
		}
		if len(imgURLs) > 0 {
			if caption, vErr := c.vision.Describe(ctx, imgURLs, event.Content); vErr == nil && caption != "" {
				event.VisionSummary = caption
				userContent += fmt.Sprintf("\n【多模态视觉观察：%s】", caption)
			} else if vErr != nil {
				slog.Warn("视觉多模态解析失败", "err", vErr)
			}
		}
	} else if len(event.Images) > 0 {
		userContent += "\n【感知提示：你发送了图片，但当前未检测到可用视觉解析中枢，暂无法直接解析图像画面内容】"
	}

	if event.IsGroup {
		speaker := strings.TrimSpace(event.SenderName)
		if speaker == "" {
			sid := strings.TrimSpace(event.SenderID)
			if len(sid) > 6 {
				sid = sid[:6]
			}
			if sid != "" {
				speaker = "群成员#" + sid
			} else {
				speaker = "群成员"
			}
		}
		userContent = fmt.Sprintf("[%s]: %s", speaker, userContent)
	}

	messages = append(messages, core.ChatMessage{
		Role:    core.RoleUser,
		Content: userContent,
	})

	readOnly := event.Role == entity.RoleGuest
	var toolDefs []core.ToolDefinition
	if c.hands != nil {
		allDefs := c.hands.Definitions()
		if readOnly {
			for _, td := range allDefs {
				switch td.Function.Name {
				case "delegate_ticket", "switch_model", "switch_workspace", "generate_image":
					continue
				default:
					toolDefs = append(toolDefs, td)
				}
			}
		} else {
			toolDefs = allDefs
		}
	}
	toolCtx := entity.WithSensoryEvent(ctx, event)
	var lastToolText string
	var directCardData []byte

	// 单次心智推理与多轮工具调用循环
	for round := 0; round < 4; round++ {
		activeTools := toolDefs
		if len(directCardData) > 0 {
			// 已生成待发送图像，本轮仅需模型给出一句自然配文，不再重复触发工具
			activeTools = nil
		}

		resp, err := c.llm.ChatWithTools(ctx, messages, activeTools, &core.ChatOptions{
			Temperature: 0.7,
			MaxTokens:   1024,
			ReadOnly:    readOnly,
		})
		if err != nil {
			return nil, fmt.Errorf("cortex reasoning failed: %w", err)
		}

		// 若未触发任何 Tool 调用，组装文本与（可能存在的）图像一并发送
		if len(resp.ToolCalls) == 0 {
			cleanText := sanitizeAssistantSpoken(resp.Content)
			utterance := &entity.Utterance{
				Platform:   event.Platform,
				IsGroup:    event.IsGroup,
				ChannelID:  event.ChannelID,
				QuoteMsgID: event.RawMessageID,
				Text:       cleanText,
				CardData:   directCardData,
			}

			if c.memory != nil {
				c.memory.Commit(ctx, event, cleanText)
			}
			return utterance, nil
		}

		messages = append(messages, core.ChatMessage{
			Role:      core.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		isRenderCard := false
		linkToolCalls := 0
		for _, tc := range resp.ToolCalls {
			slog.Info("cortex: 大脑触发工具调用", "name", tc.Function.Name, "args", tc.Function.Arguments)
			if tc.Function.Name == "render_card" {
				isRenderCard = true
			}

			if tc.Function.Name == "screenshot_web" || tc.Function.Name == "read_link" {
				linkToolCalls++
				if linkToolCalls > 2 {
					slog.Info("cortex: 拦截超限的链接处理调用（单次上限 2 个）", "name", tc.Function.Name)
					messages = append(messages, core.ChatMessage{
						Role:       core.RoleTool,
						ToolCallID: tc.ID,
						Name:       tc.Function.Name,
						Content:    "已达单次会话链接处理上限（最多处理 2 个链接）。本链接已跳过，请在回复末尾自然带一句：'一口气发这么多容易刷屏，先帮你捞了前两个～后面那几个如果有想深入看的直接单独甩给我就行。'",
					})
					continue
				}
			}

			if c.hands == nil {
				continue
			}

			toolRes, tErr := c.hands.Execute(toolCtx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if tErr != nil {
				messages = append(messages, core.ChatMessage{
					Role:       core.RoleTool,
					ToolCallID: tc.ID,
					Name:       tc.Function.Name,
					Content:    fmt.Sprintf("工具执行失败: %v", tErr),
				})
				continue
			}

			if len(toolRes.ImageData) > 0 && len(directCardData) == 0 {
				directCardData = toolRes.ImageData
			}

			outText := toolRes.Text
			if outText == "" {
				outText = "执行成功"
			}
			lastToolText = outText
			safeOut := safeTruncateOutput(outText, 4000)
			messages = append(messages, core.ChatMessage{
				Role:       core.RoleTool,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    safeOut,
			})
		}

		// 若是纯 Markdown 排版卡片渲染，卡片即主要产物，直接返回；
		// 网页截图与出图 (isRenderCard 为 false) 则继续流转到下一轮，由大脑消化提取到的页面标题与正文后输出完整自然总结
		if len(directCardData) > 0 && isRenderCard {
			cleanText := sanitizeAssistantSpoken(resp.Content)
			utter := &entity.Utterance{
				Platform:   event.Platform,
				IsGroup:    event.IsGroup,
				ChannelID:  event.ChannelID,
				QuoteMsgID: event.RawMessageID,
				Text:       cleanText,
				CardData:   directCardData,
			}
			if c.memory != nil {
				c.memory.Commit(ctx, event, cleanText)
			}
			return utter, nil
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// 若多轮工具调用收束后仍未自然发声，请求模型基于执行反馈做最终纯文本表达
	finalResp, fErr := c.llm.ChatWithTools(ctx, messages, nil, &core.ChatOptions{
		Temperature: 0.7,
		MaxTokens:   512,
		ReadOnly:    readOnly,
	})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	finalText := ""
	if fErr == nil && finalResp != nil && strings.TrimSpace(finalResp.Content) != "" {
		finalText = sanitizeAssistantSpoken(finalResp.Content)
	} else if lastToolText != "" {
		finalText = sanitizeAssistantSpoken(lastToolText)
	} else {
		finalText = "已处理完毕。"
	}

	utter := &entity.Utterance{
		Platform:   event.Platform,
		IsGroup:    event.IsGroup,
		ChannelID:  event.ChannelID,
		QuoteMsgID: event.RawMessageID,
		Text:       finalText,
		CardData:   directCardData,
	}
	if c.memory != nil {
		c.memory.Commit(ctx, event, utter.Text)
	}
	return utter, nil
}

// sanitizeAssistantSpoken 彻底清洗助手发言，剥离任何内部工具调用标记与调试残留
func sanitizeAssistantSpoken(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	markers := []string{"\n\n[工具 ", "\n[工具 ", "\n\n【工具 ", "\n【工具 "}
	for _, m := range markers {
		if idx := strings.Index(s, m); idx != -1 {
			s = strings.TrimSpace(s[:idx])
		}
	}
	if strings.HasPrefix(s, "[工具 ") || strings.HasPrefix(s, "【工具 ") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = strings.TrimSpace(s[idx:])
		} else {
			s = ""
		}
	}
	return s
}

// safeTruncateOutput 安全按 Unicode 字符 (rune) 截断超长工具或工单输出，严防切断 UTF-8 多字节中文字符导致乱码或 JSON 序列化失败
func safeTruncateOutput(text string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 4000
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	headLen := maxRunes * 3 / 4
	tailLen := maxRunes / 4
	head := string(runes[:headLen])
	tail := string(runes[len(runes)-tailLen:])
	return fmt.Sprintf("%s\n\n[... 中间约 %d 字符已折叠省略 ...]\n\n%s", head, len(runes)-maxRunes, tail)
}
