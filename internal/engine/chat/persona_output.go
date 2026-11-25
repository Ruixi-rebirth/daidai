package chat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"daidai/assets"
	"daidai/internal/config"
	"daidai/internal/core"
	"daidai/internal/entity"
)

var (
	faceTagPattern         = regexp.MustCompile(`[\[【]\s*脸\s*[:：]\s*([^\]】\n]{1,12}?)\s*[\]】]`)
	leakedPromptPattern    = regexp.MustCompile(`【(?:你的视觉观察|此刻内在节律|近期口癖去重|熟悉度分寸|对方近期情绪底色|你心里对 TA 的印象)[^】\n]*】?\s*`)
	leadingReplyTagPattern = regexp.MustCompile(`^\[(?:对\s*[^\]\n]{1,40}?\s*说|对\s+[^\]\n]{1,40}|回\s+[^\]\n]{1,40}|回复\s+[^\]\n]{1,40})\]\s*[:：]\s*`)
	fullWidthParenPattern  = regexp.MustCompile(`（([^（）\n]{1,24})）`)
	halfWidthParenPattern  = regexp.MustCompile(`\(([^()\n]{1,24})\)`)
	asteriskActionPattern  = regexp.MustCompile(`\*([^*\n]{1,18})\*`)

	aiSelfDisclosurePattern = regexp.MustCompile(
		`(?i)我(?:是|只是|不过是|其实是|作为)(?:一个|个|一名|款)?\s*(?:ai|人工智能|聊天机器人|聊天程序|机器人|智能助手|大?语言模型|对话模型|虚拟助手)` +
			`|(?i)作为(?:一个|个|一名|款)?\s*(?:ai|人工智能|机器人|大?语言模型|智能助手)` +
			`|我没有(?:真正的|真实的)?(?:情感|感情|意识|灵魂|实体)` +
			`|我(?:被|是被)(?:训练|编程|设计)出来`,
	)
	aiAccusationPattern = regexp.MustCompile(
		`(?i)(?:你|妳|呆呆).{0,8}(?:是|是不是|像|有点).{0,8}(?:ai|bot|机器人|人机|大模型|语言模型)` +
			`|(?i)(?:ai|bot|机器人|人机|大模型|语言模型).{0,6}(?:吧|吗|么|嘛|呀)`,
	)
	aiSelfProofPattern = regexp.MustCompile(
		`(?i)你才.{0,8}(?:ai|bot|机器人|人机)` +
			`|(?i)你全家.{0,8}(?:ai|bot|机器人|人机)` +
			`|我.{0,12}(?:是真人|是普通人|是人类)` +
			`|(?i)我.{0,6}(?:不是|才不是).{0,10}(?:ai|bot|机器人|人机|大模型|语言模型)` +
			`|(?:我有感情|我会累|证明自己|证明我是人)`,
	)
)

var aiDismissalPool = [...]string{"又来这套", "行吧随你怎么想", "一天天净想这些", "换点新鲜的聊呗", "随你啦"}

var stageActionCues = [...]string{
	"懒懒", "小声", "轻声", "低声", "歪头", "低头", "抬头", "摇头",
	"叹气", "轻叹", "打哈欠", "打了个哈欠", "哈欠", "伸懒腰", "伸了个懒腰",
	"揉眼", "揉了揉眼", "翻白眼", "翻了个白眼", "瞥了一眼", "瞥你一眼", "懒得瞥",
	"抱着猫", "抱紧猫", "撸着猫", "缩进毯子", "缩回被窝", "脸红", "耳根发红",
	"嘟囔", "扶额", "捂脸", "摊手", "深吸一口气", "默默看",
}

type personaTicGroup struct {
	label  string
	tokens []string
}

var personaTicGroups = [...]personaTicGroup{
	{label: "猫/撸猫", tokens: []string{"撸猫", "抱猫", "抱着猫", "小猫", "猫趴"}},
	{label: "补番/看番", tokens: []string{"补番", "追番", "看番", "新番"}},
	{label: "窝着/瘫着", tokens: []string{"窝着", "窝在", "瘫着", "瘫在", "躺平", "缩在毯子"}},
	{label: "犯困/低电量", tokens: []string{"低电量", "没电了", "好困", "打哈欠", "犯困"}},
	{label: "发呆/放空", tokens: []string{"发呆", "放空", "走神"}},
	{label: "窗边/听雨", tokens: []string{"窗边", "窗台", "听雨"}},
	{label: "好怪/无聊套话", tokens: []string{"你好怪", "好怪", "真怪", "无聊"}},
}

func isPartnerSystemQuery(event *entity.SensoryEvent, userRequest string) bool {
	if event == nil || event.Role != entity.RolePartner {
		return false
	}
	lower := strings.ToLower(userRequest)
	for _, kw := range []string{"模型", "model", "agy", "api", "渠道", "配额", "额度", "工单", "工作区", "workspace", "状态"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func transformOutsideCode(text string, fn func(string) string) string {
	if text == "" {
		return ""
	}
	var out strings.Builder
	out.Grow(len(text))
	n := len(text)
	i := 0
	proseStart := 0

	flushProse := func(end int) {
		if end > proseStart {
			out.WriteString(fn(text[proseStart:end]))
		}
	}

	for i < n {
		if strings.HasPrefix(text[i:], "```") {
			flushProse(i)
			endFence := strings.Index(text[i+3:], "```")
			if endFence < 0 {
				out.WriteString(text[i:])
				return out.String()
			}
			blockEnd := i + 3 + endFence + 3
			out.WriteString(text[i:blockEnd])
			i = blockEnd
			proseStart = i
			continue
		}
		if text[i] == '`' {
			endTick := strings.IndexByte(text[i+1:], '`')
			nl := strings.IndexByte(text[i+1:], '\n')
			if endTick >= 0 && (nl < 0 || endTick < nl) {
				flushProse(i)
				spanEnd := i + 1 + endTick + 1
				out.WriteString(text[i:spanEnd])
				i = spanEnd
				proseStart = i
				continue
			}
		}
		i++
	}
	flushProse(n)
	return out.String()
}

func isTheatricalStageDirection(inner string) bool {
	s := strings.TrimSpace(inner)
	if s == "" || utf8.RuneCountInString(s) > 20 {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) && r <= unicode.MaxASCII {
			return false
		}
		if unicode.IsDigit(r) || strings.ContainsRune("=/:;<>[]{}+-*`", r) {
			return false
		}
	}
	for _, cue := range stageActionCues {
		if strings.Contains(s, cue) {
			return true
		}
	}
	return false
}

func stripBotNamePrefix(text string) string {
	trimmed := strings.TrimLeft(text, " \t")
	if loc := leadingReplyTagPattern.FindStringIndex(trimmed); loc != nil {
		trimmed = strings.TrimLeft(trimmed[loc[1]:], " \t")
	}
	botName := strings.TrimSpace(config.BotName())
	if botName != "" {
		for _, prefix := range []string{
			"[" + botName + "]:", "[" + botName + "]：",
			botName + ":", botName + "：",
		} {
			if strings.HasPrefix(trimmed, prefix) {
				return strings.TrimLeft(trimmed[len(prefix):], " \t")
			}
		}
	}
	if trimmed != strings.TrimLeft(text, " \t") {
		return trimmed
	}
	return text
}

func pickDismissal(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return aiDismissalPool[int(sum[0])%len(aiDismissalPool)]
}

func sanitizePersonaText(text, userRequest string, skipIdentityGuard, isInterjection bool) (cleaned string, faceEmotion string, dropped bool) {
	if text == "" {
		return "", "", false
	}
	cleaned = stripBotNamePrefix(text)
	var nonCodeProse strings.Builder

	cleaned = transformOutsideCode(cleaned, func(seg string) string {
		seg = leakedPromptPattern.ReplaceAllString(seg, "")
		for _, m := range faceTagPattern.FindAllStringSubmatch(seg, -1) {
			if len(m) >= 2 {
				if canonical, ok := assets.ResolveBundledFace(m[1]); ok {
					faceEmotion = canonical
				}
			}
		}
		seg = faceTagPattern.ReplaceAllString(seg, "")
		seg = fullWidthParenPattern.ReplaceAllStringFunc(seg, func(match string) string {
			sub := fullWidthParenPattern.FindStringSubmatch(match)
			if len(sub) >= 2 && isTheatricalStageDirection(sub[1]) {
				return ""
			}
			return match
		})
		seg = halfWidthParenPattern.ReplaceAllStringFunc(seg, func(match string) string {
			sub := halfWidthParenPattern.FindStringSubmatch(match)
			if len(sub) >= 2 && isTheatricalStageDirection(sub[1]) {
				return ""
			}
			return match
		})
		seg = asteriskActionPattern.ReplaceAllStringFunc(seg, func(match string) string {
			sub := asteriskActionPattern.FindStringSubmatch(match)
			if len(sub) >= 2 && isTheatricalStageDirection(sub[1]) {
				return ""
			}
			return match
		})
		nonCodeProse.WriteString(seg)
		return seg
	})

	prose := strings.TrimSpace(nonCodeProse.String())
	if !skipIdentityGuard && prose != "" {
		if aiSelfDisclosurePattern.MatchString(prose) ||
			(aiAccusationPattern.MatchString(userRequest) && aiSelfProofPattern.MatchString(prose)) {
			if isInterjection {
				return "", "", true
			}
			return pickDismissal(userRequest + ":" + prose), "", false
		}
	}

	if cleaned != text {
		cleaned = strings.TrimSpace(cleaned)
	}
	return cleaned, faceEmotion, false
}

func (c *Cortex) attachPersonaSticker(ctx context.Context, event *entity.SensoryEvent, out *entity.Utterance, faceEmotion string, textOnly bool) bool {
	if out == nil || event == nil {
		return true
	}
	canAttach := faceEmotion != "" &&
		(event.Platform == "qq" || event.Platform == "telegram") &&
		!event.GroupInterjection &&
		!textOnly &&
		!out.FixedPresentation &&
		!out.HasMedia() &&
		!c.ShouldRenderCard(ctx, out.Text)

	if canAttach {
		if stickerData, canonical, ok := assets.ReadBundledSmallFace(faceEmotion); ok {
			out.Images = append(out.Images, stickerData)
			out.ImageSources = append(out.ImageSources, "")
			if strings.TrimSpace(out.Text) == "" {
				out.MediaHistory = fmt.Sprintf("[发送了表情：%s]", canonical)
				out.FallbackText = fmt.Sprintf("[%s]", canonical)
			}
		}
	}

	if strings.TrimSpace(out.Text) == "" && !out.HasMedia() {
		if event.GroupInterjection {
			return false
		}
		out.Text = "嗯？"
	}
	return true
}

// recentPersonaTicHint inspects already-loaded working history for repeated
// persona prop words so Daidai varies her phrasing naturally before generation.
func recentPersonaTicHint(history []core.ChatMessage, currentRequest string) string {
	if len(history) == 0 {
		return ""
	}
	var assistantRecent []string
	hasLeadingQuestionMark := false
	for i := len(history) - 1; i >= 0 && len(assistantRecent) < 6; i-- {
		if history[i].Role != core.RoleAssistant {
			continue
		}
		txt := strings.TrimSpace(leadingReplyTagPattern.ReplaceAllString(strings.TrimSpace(history[i].Content), ""))
		if txt == "" || utf8.RuneCountInString(txt) > 180 {
			continue
		}
		if strings.HasPrefix(txt, "？") || strings.HasPrefix(txt, "?") {
			hasLeadingQuestionMark = true
		}
		assistantRecent = append(assistantRecent, txt)
	}
	if len(assistantRecent) == 0 {
		return ""
	}
	joinedRecent := strings.Join(assistantRecent, "\n")
	var repeated []string
	if hasLeadingQuestionMark {
		repeated = append(repeated, "单问号起手（？开头）")
	}
	for _, group := range personaTicGroups {
		askedByUser := false
		for _, tok := range group.tokens {
			if strings.Contains(currentRequest, tok) {
				askedByUser = true
				break
			}
		}
		if askedByUser {
			continue
		}
		for _, tok := range group.tokens {
			if strings.Contains(joinedRecent, tok) {
				repeated = append(repeated, group.label)
				break
			}
		}
	}
	if len(repeated) == 0 {
		return ""
	}
	return fmt.Sprintf("【近期口癖去重】前几轮回复里你已经提过「%s」这类背景词，本轮请直接自然接话，不要重复提及。", strings.Join(repeated, "、"))
}
