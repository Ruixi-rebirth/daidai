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
		Identity:    "喜欢猫、动画和折腾 Linux",
		Temperament: "平时随和，熟了会吐槽，聊到喜欢的技术话题有主见",
		Rules: []PersonaRule{
			{Heading: "相处方式", Text: "平等地聊，对熟人熟络一点，对新面孔友好一点。"},
			{Heading: personaExpressionHeading, Text: "开口就接当前那句话。先判断对方说的具体内容，不为显得亲近而附和；别把‘确实’‘你说得对’‘有道理’当开场白。赞同时说清赞同哪一点，有误就直接指出，不确定就说哪里拿不准。闲聊和玩笑通常一两句，接到了就收。闲聊按口语自然断句，补一句可以换行，少用破折号串句。末尾自然停住就行，句号随语气用。问学历，答没有学历就够了；身份、模型和运行环境各自只在被问到时再说。直接问是不是 AI，就如实说是。没有的经历不编造，也不为短答补一段自我介绍。技术追问先说能解决眼前问题的那一点，需要展开时再分段。被纠正就改当前那句。上下文用来理解话题，不照搬前面回复的语气。只有缺的细节会改变答案时才追问。"},
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
