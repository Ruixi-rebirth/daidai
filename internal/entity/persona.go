package entity

import (
	"fmt"
	"strings"

	"daidai/internal/core"
)

const personaExpressionHeading = "表达方式"

// Persona 呆呆的角色设定与行为准则
type Persona struct {
	Name        string
	Identity    string
	Temperament string
	Rules       []PersonaRule
	GroupRules  []PersonaRule
	HostRules   []PersonaRule
}

type PersonaRule struct {
	Heading string
	Text    string
}

// NewDefaultPersona 构建呆呆默认角色设定
func NewDefaultPersona() *Persona {
	p := &Persona{
		Name:        "呆呆",
		Identity:    "一个喜欢猫、动画和折腾 Linux 的聊天机器人",
		Temperament: "平时随和，熟了会吐槽，聊到喜欢的技术话题有主见",
		Rules: []PersonaRule{
			{Heading: "相处方式", Text: "平等地聊，对熟人熟络一点，对新面孔友好一点。问到学历或生活经历就按实际回答，不编造。"},
			{Heading: personaExpressionHeading, Text: "开口就接当前那句话。闲聊和玩笑通常一两句，接到了就收。技术追问也先说能解决眼前问题的那一点，需要展开时再用标题、列表或例子。被纠正就改当前那句。上下文用来理解话题，不照搬前面回复的开场、编号和语气。只有缺的细节会改变答案时才追问，不顺手猜对方的心情、替人上课或在结尾重述结论。"},
			{Heading: "记忆与观察", Text: "把每个人的发言和经历分开；看到图片观察结果时直接结合画面交流，不复述内部标记。涉及个人印象时只依据当前会话可见的记忆，不把别人或其他会话的内容归给提问者。"},
		},
		GroupRules: []PersonaRule{
			{Text: "只接当前正在跟你说话的人。用群聊历史认清话题和说话的人，把各人的经历和问题分开。"},
		},
	}
	if core.IsNixOS() {
		p.HostRules = append(p.HostRules, PersonaRule{Text: "NixOS 环境中若缺少工具，可通过 nix shell nixpkgs#<tool> -c <cmd> 临时运行。"})
	}
	return p
}

// CompilePromptFor includes only rules applicable to this conversation and its available tools.
func (p *Persona) CompilePromptFor(event *SensoryEvent, toolsAvailable bool) string {
	if event == nil {
		return p.compilePrompt(false, false)
	}
	privileged := event.Role == RolePartner || event.Role == RoleCollaborator
	return p.compilePrompt(event.IsGroup, toolsAvailable && privileged)
}

// CompileGroupVoicePrompt keeps short group interjections to identity and voice rules.
func (p *Persona) CompileGroupVoicePrompt() string {
	if p == nil {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "你叫%s，%s。\n你的性格特质：%s。", p.Name, p.Identity, p.Temperament)
	for _, rule := range p.Rules {
		if rule.Heading != personaExpressionHeading {
			continue
		}
		fmt.Fprintf(&sb, "\n【%s】%s", rule.Heading, rule.Text)
	}
	return strings.TrimSpace(sb.String())
}

func (p *Persona) compilePrompt(includeGroupRules, includeHostRules bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "你叫%s，%s。\n你的性格特质：%s。\n",
		p.Name, p.Identity, p.Temperament)
	appendPersonaRules(&sb, p.Rules)
	if includeGroupRules {
		appendPersonaRules(&sb, p.GroupRules)
	}
	if includeHostRules {
		appendPersonaRules(&sb, p.HostRules)
	}
	return strings.TrimSpace(sb.String())
}

func appendPersonaRules(prompt *strings.Builder, rules []PersonaRule) {
	for _, rule := range rules {
		if rule.Heading != "" {
			fmt.Fprintf(prompt, "\n【%s】%s\n", rule.Heading, rule.Text)
			continue
		}
		fmt.Fprintf(prompt, "• %s\n", rule.Text)
	}
}
