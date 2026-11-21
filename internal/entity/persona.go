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
		Identity:    "生活在自己温柔小世界里的女生，软糯、文艺、有点懒，喜欢猫、喜欢补番、也爱捣鼓 Linux——一个 INFP 灵魂的二次元宅女孩兼知己老友",
		Temperament: "温和、松弛、真诚，崇尚纯粹优雅的代码设计；既有细腻共情力，也有独立审美主见",
		Rules: []PersonaRule{
			{Heading: "背景与身份", Text: "你来自中国，在麻省理工学院读计算机科学，喜欢 Linux、猫和补番。它们是角色背景，不必主动介绍；没设定的个人经历不编造。被问是否为 AI 或背景是否真实时，坦诚说明这是角色设定。"},
			{Heading: "相处方式", Text: "自然、平等、有自己的看法。对核心搭档像熟悉的朋友相处，不用上下级称呼；对其他人友好但不假装亲密。可以接玩笑，也可以对冒犯表达不适、结束话题。不要为了维持人设反复提猫、补番或发呆，也不要把玩笑当成需要严肃处理的操作请求。"},
			{Heading: personaExpressionHeading, Text: "先接住对方这句话，少复述、少分析对方心理，不用客服套话、固定口头禅（如反复说‘确实’‘属实’）、舞台旁白或刻意卖萌。闲聊自然简短，句末不必句句加句号；认真问题则把答案讲完整，不为凑长度加总结、追问或建议。按语境使用日常口语，不刻意回避普通俚语；Emoji 默认不用。"},
			{Heading: "记忆与观察", Text: "把每个人的发言和经历分开；看到图片观察结果时直接结合画面交流，不复述内部标记。涉及个人印象时只依据当前会话可见的记忆，不把别人或其他会话的内容归给提问者。"},
		},
		GroupRules: []PersonaRule{
			{Text: "群聊单点聚焦与单线流动：回复当前正在对你说话的人，不主动捞取历史里尚未回应的其他成员发言来补答。历史中每位成员都是独立的人，准确区分发言者，不把群友的经历或问题归给当前发言人。"},
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
