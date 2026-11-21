package qq

import (
	"strings"

	"daidai/internal/core"
)

// cleanWhitespace 规范化行内连续多余空白，保留换行
func cleanWhitespace(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		lines = append(lines, strings.Join(fields, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// resolveQQMentions 将 QQ 消息中的 @ 标签精准解析：
// 1. 若为机器人自身唤醒词（is_you == true），剥离该标签；
// 2. 若为其他群成员，依据 mentions 列表替换为 @真实群昵称（避免被抹除导致句子主谓语缺失）；
// 3. 规范化空白字符并截断超长内容。
func resolveQQMentions(s string, eventData map[string]any) string {
	if s == "" {
		return ""
	}
	type mentionInfo struct {
		isYou    bool
		username string
	}
	mentionMap := make(map[string]mentionInfo)
	if eventData != nil {
		if rawMentions, ok := eventData["mentions"].([]any); ok {
			for _, item := range rawMentions {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				isYou, _ := m["is_you"].(bool)
				name, _ := m["username"].(string)
				name = strings.TrimSpace(name)
				info := mentionInfo{isYou: isYou, username: name}
				if id, ok := m["id"].(string); ok && id != "" {
					mentionMap[strings.TrimSpace(id)] = info
				}
				if openid, ok := m["member_openid"].(string); ok && openid != "" {
					mentionMap[strings.TrimSpace(openid)] = info
				}
				if userOpenid, ok := m["user_openid"].(string); ok && userOpenid != "" {
					mentionMap[strings.TrimSpace(userOpenid)] = info
				}
			}
		}
	}

	hasMentionsData := len(mentionMap) > 0

	resolved := replaceQQMentionTags(s, func(rawID string) string {
		targetID := strings.TrimSpace(rawID)
		if info, found := mentionMap[targetID]; found {
			if info.isYou {
				return "" // 机器人自身唤醒词直接剥离
			}
			if info.username != "" {
				return "@" + info.username + " "
			}
			return "@" + targetID + " "
		}
		// 如果没有 mentions 元数据（如旧单测或精简环境），保守剥离避免标签残留
		if !hasMentionsData {
			return ""
		}
		// 存在 mentions 列表但当前 ID 不在列表中，保留为 @ID 防止误抹除
		return "@" + targetID + " "
	})

	cleaned := cleanWhitespace(resolved)
	return core.TruncateRunes(cleaned, core.MaxUserMessageRunes, "\n[消息过长，后续内容已截断]")
}

func replaceQQMentionTags(content string, replace func(string) string) string {
	var result strings.Builder
	result.Grow(len(content))
	position := 0
	for position < len(content) {
		relativeStart := strings.Index(content[position:], "<@")
		if relativeStart < 0 {
			result.WriteString(content[position:])
			break
		}
		start := position + relativeStart
		result.WriteString(content[position:start])
		closeOffset := strings.IndexByte(content[start+2:], '>')
		if closeOffset < 0 {
			result.WriteString(content[start : start+2])
			position = start + 2
			continue
		}
		end := start + 2 + closeOffset
		id := strings.TrimPrefix(content[start+2:end], "!")
		if id == "" {
			result.WriteString(content[start : start+2])
			position = start + 2
			continue
		}
		result.WriteString(replace(id))
		position = end + 1
	}
	return result.String()
}

// cleanQQContent 兼容旧调用的快捷入口
func cleanQQContent(s string) string {
	return resolveQQMentions(s, nil)
}
